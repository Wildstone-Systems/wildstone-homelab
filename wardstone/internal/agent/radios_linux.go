//go:build linux

package agent

// ListRadios (protocol minor 25) reads /dev/serial/by-id, /sys/class/tty
// and /sys/class/bluetooth directly — no udevadm, no lsusb, no shell-out
// (ADR-W003 §A4.5). Every path is derived from those fixed roots and the
// kernel's own names; nothing on the wire selects what is read.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// radioSysRoot and radioDevRoot are the sysfs and /dev roots. Package-level
// vars so tests can point them at a fixture tree built in t.TempDir().
// Paths reported to Keystone are always rewritten to /sys and /dev, so a
// fixture root never leaks into a result.
var (
	radioSysRoot = "/sys"
	radioDevRoot = "/dev"
)

// kernelDevName is what a tty or hci name may look like before it is
// joined into a path: a kernel name, never a traversal.
var kernelDevName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// byIDName is what a udev by-id alias may look like: udev replaces
// anything unsafe with "_" or a \xNN escape, and a name never holds "/".
// Aliases run long (vendor, model and serial number), hence 255.
var byIDName = regexp.MustCompile(`^[A-Za-z0-9_.:+,=@\\-]{1,255}$`)

// maxSysfsDepth bounds the walk from a device up to its USB ancestor. A
// real tree is under ten levels deep; the bound only guards a looping or
// malformed fixture.
const maxSysfsDepth = 24

func (a *Agent) listRadios() (*wsv1.OpResult_ListRadios, *wsv1.OpError) {
	radios := append(listSerialRadios(), listBluetoothRadios()...)
	sort.Slice(radios, func(i, j int) bool { return radios[i].GetId() < radios[j].GetId() })
	return &wsv1.OpResult_ListRadios{ListRadios: &wsv1.ListRadiosResult{
		Radios:            radios,
		PlatformSupported: true,
	}}, nil
}

// serialAliases maps a tty name to the sorted /dev/serial/by-id aliases
// that resolve to it. Only the link TARGET's base name is used; the
// links are read, never followed into the filesystem.
func serialAliases() map[string][]string {
	dir := filepath.Join(radioDevRoot, "serial", "by-id")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no udev, or no serial device plugged in
	}
	out := map[string][]string{}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 || !byIDName.MatchString(e.Name()) {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		tty := filepath.Base(target)
		if !kernelDevName.MatchString(tty) {
			continue
		}
		out[tty] = append(out[tty], "/dev/serial/by-id/"+e.Name())
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out
}

// listSerialRadios reports every tty that is USB-backed or has a by-id
// alias. The virtual consoles and the always-present 8250 UART stubs have
// neither, so they never appear.
func listSerialRadios() []*wsv1.Radio {
	aliases := serialAliases()
	names := map[string]bool{}
	for tty := range aliases {
		names[tty] = true
	}
	classDir := filepath.Join(radioSysRoot, "class", "tty")
	if entries, err := os.ReadDir(classDir); err == nil {
		for _, e := range entries {
			if kernelDevName.MatchString(e.Name()) {
				names[e.Name()] = true
			}
		}
	}

	var out []*wsv1.Radio
	for tty := range names {
		info := sysfsDeviceInfo(filepath.Join(classDir, tty, "device"))
		if info.usb == nil && len(aliases[tty]) == 0 {
			continue
		}
		r := &wsv1.Radio{
			KernelName: tty,
			DevPath:    "/dev/" + tty,
			ByIdPaths:  aliases[tty],
		}
		info.fill(r)
		r.Kind, r.KindReason = classifySerial(r.GetUsbVendorId(), r.GetUsbProductId())
		switch {
		case len(r.ByIdPaths) > 0:
			r.Id = "serial-by-id:" + filepath.Base(r.ByIdPaths[0])
		case r.GetSysfsPath() != "":
			r.Id = "sysfs:" + r.GetSysfsPath()
		default:
			r.Id = "sysfs:" + "/sys/class/tty/" + tty
		}
		out = append(out, r)
	}
	return out
}

// listBluetoothRadios reports each hciN controller. Connection entries
// (hci0:256) are skipped: they are links, not adapters.
func listBluetoothRadios() []*wsv1.Radio {
	classDir := filepath.Join(radioSysRoot, "class", "bluetooth")
	entries, err := os.ReadDir(classDir)
	if err != nil {
		return nil // no bluetooth subsystem loaded
	}
	var out []*wsv1.Radio
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "hci") || strings.Contains(name, ":") || !kernelDevName.MatchString(name) {
			continue
		}
		r := &wsv1.Radio{
			KernelName: name,
			Kind:       wsv1.RadioKind_RADIO_KIND_BLUETOOTH,
			KindReason: "bluetooth controller in /sys/class/bluetooth",
		}
		info := sysfsDeviceInfo(filepath.Join(classDir, name, "device"))
		info.fill(r)
		switch {
		case r.GetUsbSerial() != "":
			r.Id = "usb:" + r.GetUsbVendorId() + ":" + r.GetUsbProductId() + ":" + r.GetUsbSerial()
		case r.GetSysfsPath() != "":
			r.Id = "sysfs:" + r.GetSysfsPath()
		default:
			r.Id = "sysfs:" + "/sys/class/bluetooth/" + name
		}
		out = append(out, r)
	}
	return out
}

// deviceInfo is what sysfs knows about a device a class entry links to.
type deviceInfo struct {
	sysfsPath string // reported form, under /sys
	bus       string
	driver    string
	usb       *usbInfo
}

type usbInfo struct {
	vendor, product, manufacturer, productName, serial, port string
}

func (d deviceInfo) fill(r *wsv1.Radio) {
	r.SysfsPath, r.Bus, r.Driver = d.sysfsPath, d.bus, d.driver
	if d.usb != nil {
		r.UsbVendorId, r.UsbProductId = d.usb.vendor, d.usb.product
		r.UsbManufacturer, r.UsbProduct, r.UsbSerial = d.usb.manufacturer, d.usb.productName, d.usb.serial
		r.UsbPort = d.usb.port
	}
}

// sysfsDeviceInfo resolves a class entry's "device" link and walks up to
// the first USB device above it. Every read stays under radioSysRoot's
// devices tree; a link resolving anywhere else is ignored. Missing
// metadata leaves the corresponding fields empty rather than failing.
func sysfsDeviceInfo(deviceLink string) deviceInfo {
	var d deviceInfo
	resolved, err := filepath.EvalSymlinks(deviceLink)
	if err != nil {
		return d
	}
	devicesRoot, err := filepath.EvalSymlinks(filepath.Join(radioSysRoot, "devices"))
	if err != nil {
		return d
	}
	rel, err := filepath.Rel(devicesRoot, resolved)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return d
	}
	d.sysfsPath = "/sys/devices/" + filepath.ToSlash(rel)
	d.driver = linkBase(filepath.Join(resolved, "driver"))
	d.bus = linkBase(filepath.Join(resolved, "subsystem"))

	dir := resolved
	for i := 0; i < maxSysfsDepth && dir != devicesRoot && strings.HasPrefix(dir, devicesRoot+string(filepath.Separator)); i++ {
		if vendor := readSysAttr(dir, "idVendor"); vendor != "" {
			d.bus = "usb"
			d.usb = &usbInfo{
				vendor:       strings.ToLower(vendor),
				product:      strings.ToLower(readSysAttr(dir, "idProduct")),
				manufacturer: readSysAttr(dir, "manufacturer"),
				productName:  readSysAttr(dir, "product"),
				serial:       readSysAttr(dir, "serial"),
				port:         cleanAttr(filepath.Base(dir)),
			}
			break
		}
		dir = filepath.Dir(dir)
	}
	return d
}

// readSysAttr reads one small, fixed-name sysfs attribute of dir.
func readSysAttr(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	if len(b) > 4*maxRadioAttrLen {
		b = b[:4*maxRadioAttrLen]
	}
	return cleanAttr(string(b))
}

// linkBase is the base name of a symlink's target (a driver or subsystem
// name), or empty when the link is absent.
func linkBase(link string) string {
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	return cleanAttr(filepath.Base(target))
}
