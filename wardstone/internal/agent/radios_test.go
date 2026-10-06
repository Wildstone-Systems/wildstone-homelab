//go:build linux

package agent

// ListRadios (protocol minor 25, #434). Every test builds its own sysfs
// and /dev/serial/by-id fixture: nothing may depend on what is plugged
// into the machine running the tests.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// radioFixture is a fake /sys + /dev tree.
type radioFixture struct {
	t        *testing.T
	sys, dev string
}

func newRadioFixture(t *testing.T) *radioFixture {
	t.Helper()
	root := t.TempDir()
	f := &radioFixture{t: t, sys: filepath.Join(root, "sys"), dev: filepath.Join(root, "dev")}
	f.mkdir(filepath.Join(f.sys, "devices"))
	origSys, origDev := radioSysRoot, radioDevRoot
	t.Cleanup(func() { radioSysRoot, radioDevRoot = origSys, origDev })
	radioSysRoot, radioDevRoot = f.sys, f.dev
	return f
}

func (f *radioFixture) mkdir(p string) {
	f.t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *radioFixture) write(p, content string) {
	f.t.Helper()
	f.mkdir(filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// link creates link -> target, as a relative symlink the way sysfs does.
func (f *radioFixture) link(target, link string) {
	f.t.Helper()
	f.mkdir(filepath.Dir(link))
	rel, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(rel, link); err != nil {
		f.t.Fatal(err)
	}
}

// usbDevice creates a USB device directory under a fake xHCI controller
// and returns its path.
func (f *radioFixture) usbDevice(port string, attrs map[string]string) string {
	dir := filepath.Join(f.sys, "devices", "pci0000:00", "0000:00:14.0", "usb1", port)
	f.mkdir(dir)
	for k, v := range attrs {
		f.write(filepath.Join(dir, k), v+"\n")
	}
	return dir
}

// driver binds dev to a fake driver and subsystem.
func (f *radioFixture) driver(dev, subsystem, driver string) {
	f.link(filepath.Join(f.sys, "bus", subsystem, "drivers", driver), filepath.Join(dev, "driver"))
	f.link(filepath.Join(f.sys, "bus", subsystem), filepath.Join(dev, "subsystem"))
}

// tty registers /sys/class/tty/<name> for a device directory (the tty's
// parent), mirroring the kernel's class/<name> -> devices/.../<name>
// link with a "device" link back to the parent.
func (f *radioFixture) tty(name, parent string) {
	ttyDir := filepath.Join(parent, "tty", name)
	f.mkdir(ttyDir)
	if parent != "" {
		f.link(parent, filepath.Join(ttyDir, "device"))
	}
	f.link(ttyDir, filepath.Join(f.sys, "class", "tty", name))
}

func (f *radioFixture) byID(alias, tty string) {
	f.link(filepath.Join(f.dev, tty), filepath.Join(f.dev, "serial", "by-id", alias))
}

func listRadiosFixture(t *testing.T) *wsv1.ListRadiosResult {
	t.Helper()
	r, oerr := (&Agent{}).listRadios()
	if oerr != nil {
		t.Fatalf("listRadios: %v", oerr)
	}
	return r.ListRadios
}

func radioByKernelName(res *wsv1.ListRadiosResult, name string) *wsv1.Radio {
	for _, r := range res.GetRadios() {
		if r.GetKernelName() == name {
			return r
		}
	}
	return nil
}

// populateRadios builds the fixture most tests share: a ConBee II
// (cdc_acm, exact USB id), a Sonoff-style CP210x stick behind a usb-serial
// port, a USB Bluetooth dongle with no serial number, a virtual console
// and an 8250 UART stub.
func populateRadios(f *radioFixture) {
	conbee := f.usbDevice("1-1.2", map[string]string{
		"idVendor": "1cf1", "idProduct": "0030",
		"manufacturer": "dresden elektronik ingenieurtechnik GmbH", "product": "ConBee II", "serial": "DE0000001",
	})
	conbeeIf := filepath.Join(conbee, "1-1.2:1.0")
	f.mkdir(conbeeIf)
	f.driver(conbeeIf, "usb", "cdc_acm")
	f.tty("ttyACM0", conbeeIf)
	f.byID("usb-dresden_elektronik_ingenieurtechnik_GmbH_ConBee_II_DE0000001-if00", "ttyACM0")

	cp := f.usbDevice("1-1.3", map[string]string{
		"idVendor": "10C4", "idProduct": "EA60",
		"manufacturer": "ITead", "product": "Sonoff Zigbee 3.0 USB Dongle Plus\x1b[31m", "serial": "0000cafe",
	})
	cpPort := filepath.Join(cp, "1-1.3:1.0", "ttyUSB0")
	f.mkdir(cpPort)
	f.driver(cpPort, "usb-serial", "cp210x")
	f.tty("ttyUSB0", cpPort)
	f.byID("usb-ITead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_0000cafe-if00-port0", "ttyUSB0")

	bt := f.usbDevice("1-1.4", map[string]string{"idVendor": "0a12", "idProduct": "0001", "product": "CSR8510 A10"})
	btIf := filepath.Join(bt, "1-1.4:1.0")
	f.mkdir(btIf)
	f.driver(btIf, "usb", "btusb")
	hci := filepath.Join(btIf, "bluetooth", "hci0")
	f.mkdir(hci)
	f.link(btIf, filepath.Join(hci, "device"))
	f.link(hci, filepath.Join(f.sys, "class", "bluetooth", "hci0"))
	// A connection handle is not an adapter.
	f.mkdir(filepath.Join(hci, "hci0:256"))
	f.link(filepath.Join(hci, "hci0:256"), filepath.Join(f.sys, "class", "bluetooth", "hci0:256"))

	// A virtual console has no device link; an 8250 stub has one but is
	// neither USB nor aliased. Neither is a radio candidate.
	vt := filepath.Join(f.sys, "devices", "virtual", "tty", "tty1")
	f.mkdir(vt)
	f.link(vt, filepath.Join(f.sys, "class", "tty", "tty1"))
	uart := filepath.Join(f.sys, "devices", "platform", "serial8250")
	f.mkdir(uart)
	f.driver(uart, "platform", "serial8250")
	f.tty("ttyS0", uart)
}

func TestListRadiosInventory(t *testing.T) {
	f := newRadioFixture(t)
	populateRadios(f)
	res := listRadiosFixture(t)

	if !res.GetPlatformSupported() {
		t.Fatal("platform_supported = false on linux")
	}
	if n := len(res.GetRadios()); n != 3 {
		for _, r := range res.GetRadios() {
			t.Logf("radio %s", r.GetId())
		}
		t.Fatalf("got %d radios, want 3 (ttyACM0, ttyUSB0, hci0)", n)
	}
	for i := 1; i < len(res.GetRadios()); i++ {
		if res.Radios[i-1].GetId() >= res.Radios[i].GetId() {
			t.Fatalf("radios not sorted by id: %q before %q", res.Radios[i-1].GetId(), res.Radios[i].GetId())
		}
	}

	acm := radioByKernelName(res, "ttyACM0")
	if acm == nil {
		t.Fatal("ttyACM0 missing")
	}
	if acm.GetKind() != wsv1.RadioKind_RADIO_KIND_ZIGBEE || acm.GetKindReason() != "usb id 1cf1:0030" {
		t.Fatalf("ConBee kind = %v (%s)", acm.GetKind(), acm.GetKindReason())
	}
	if acm.GetId() != "serial-by-id:usb-dresden_elektronik_ingenieurtechnik_GmbH_ConBee_II_DE0000001-if00" {
		t.Fatalf("ConBee id = %q", acm.GetId())
	}
	if acm.GetDevPath() != "/dev/ttyACM0" || acm.GetDriver() != "cdc_acm" || acm.GetBus() != "usb" ||
		acm.GetUsbPort() != "1-1.2" || acm.GetUsbSerial() != "DE0000001" || acm.GetUsbProduct() != "ConBee II" {
		t.Fatalf("ConBee = %+v", acm)
	}
	if acm.GetSysfsPath() != "/sys/devices/pci0000:00/0000:00:14.0/usb1/1-1.2/1-1.2:1.0" {
		t.Fatalf("ConBee sysfs path = %q (fixture root must not leak)", acm.GetSysfsPath())
	}

	usb := radioByKernelName(res, "ttyUSB0")
	if usb == nil {
		t.Fatal("ttyUSB0 missing")
	}
	// A CP210x is a generic bridge: the product string says Zigbee, but
	// the classifier does not guess from strings or tty names.
	if usb.GetKind() != wsv1.RadioKind_RADIO_KIND_UNKNOWN {
		t.Fatalf("CP210x kind = %v, want UNKNOWN", usb.GetKind())
	}
	if usb.GetUsbVendorId() != "10c4" || usb.GetUsbProductId() != "ea60" {
		t.Fatalf("usb id not lower-cased: %s:%s", usb.GetUsbVendorId(), usb.GetUsbProductId())
	}
	if strings.ContainsRune(usb.GetUsbProduct(), '\x1b') {
		t.Fatalf("control character relayed: %q", usb.GetUsbProduct())
	}
	if usb.GetDriver() != "cp210x" || len(usb.GetByIdPaths()) != 1 ||
		usb.GetByIdPaths()[0] != "/dev/serial/by-id/usb-ITead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_0000cafe-if00-port0" {
		t.Fatalf("ttyUSB0 = %+v", usb)
	}

	hci := radioByKernelName(res, "hci0")
	if hci == nil {
		t.Fatal("hci0 missing")
	}
	if hci.GetKind() != wsv1.RadioKind_RADIO_KIND_BLUETOOTH || hci.GetDevPath() != "" || hci.GetDriver() != "btusb" {
		t.Fatalf("hci0 = %+v", hci)
	}
	// No USB serial number: the id falls back to the port-bound path.
	if hci.GetId() != "sysfs:/sys/devices/pci0000:00/0000:00:14.0/usb1/1-1.4/1-1.4:1.0" {
		t.Fatalf("hci0 id = %q", hci.GetId())
	}

	// Deterministic: a second read is identical.
	again := listRadiosFixture(t)
	for i := range res.GetRadios() {
		if res.Radios[i].GetId() != again.Radios[i].GetId() {
			t.Fatalf("order changed between reads: %q vs %q", res.Radios[i].GetId(), again.Radios[i].GetId())
		}
	}
}

// No udev: the by-id directory is absent, so a serial adapter is still
// found through sysfs and keyed by its device path.
func TestListRadiosWithoutUdev(t *testing.T) {
	f := newRadioFixture(t)
	populateRadios(f)
	if err := os.RemoveAll(filepath.Join(f.dev, "serial")); err != nil {
		t.Fatal(err)
	}
	res := listRadiosFixture(t)
	usb := radioByKernelName(res, "ttyUSB0")
	if usb == nil || len(usb.GetByIdPaths()) != 0 {
		t.Fatalf("ttyUSB0 = %+v", usb)
	}
	if usb.GetId() != "sysfs:/sys/devices/pci0000:00/0000:00:14.0/usb1/1-1.3/1-1.3:1.0/ttyUSB0" {
		t.Fatalf("ttyUSB0 id = %q", usb.GetId())
	}
}

// No sysfs metadata: only a by-id alias exists (a container with /dev
// bound in but no /sys). The alias is still reported, identity-less.
func TestListRadiosWithoutSysfs(t *testing.T) {
	f := newRadioFixture(t)
	f.byID("usb-Some_Vendor_Some_Stick-if00-port0", "ttyUSB3")
	if err := os.RemoveAll(f.sys); err != nil {
		t.Fatal(err)
	}
	res := listRadiosFixture(t)
	if len(res.GetRadios()) != 1 {
		t.Fatalf("radios = %+v", res.GetRadios())
	}
	r := res.Radios[0]
	if r.GetId() != "serial-by-id:usb-Some_Vendor_Some_Stick-if00-port0" || r.GetDevPath() != "/dev/ttyUSB3" ||
		r.GetKind() != wsv1.RadioKind_RADIO_KIND_UNKNOWN || r.GetUsbVendorId() != "" || r.GetSysfsPath() != "" {
		t.Fatalf("alias-only radio = %+v", r)
	}
}

// Nothing at all: an empty, supported, successful answer.
func TestListRadiosEmpty(t *testing.T) {
	newRadioFixture(t)
	res := listRadiosFixture(t)
	if !res.GetPlatformSupported() || len(res.GetRadios()) != 0 {
		t.Fatalf("empty = %+v", res)
	}
}

// A device link that escapes the devices tree is not followed for
// metadata: the op reads only under /sys/devices.
func TestListRadiosIgnoresEscapingLink(t *testing.T) {
	f := newRadioFixture(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	f.write(filepath.Join(outside, "idVendor"), "dead\n")
	f.write(filepath.Join(outside, "idProduct"), "beef\n")
	ttyDir := filepath.Join(f.sys, "devices", "virtual", "tty", "ttyUSB9")
	f.mkdir(ttyDir)
	f.link(outside, filepath.Join(ttyDir, "device"))
	f.link(ttyDir, filepath.Join(f.sys, "class", "tty", "ttyUSB9"))
	f.byID("usb-escape-if00-port0", "ttyUSB9")

	res := listRadiosFixture(t)
	r := radioByKernelName(res, "ttyUSB9")
	if r == nil || r.GetUsbVendorId() != "" || r.GetSysfsPath() != "" {
		t.Fatalf("escaping link followed: %+v", r)
	}
}

func TestClassifySerial(t *testing.T) {
	cases := []struct {
		vendor, product string
		want            wsv1.RadioKind
	}{
		{"1cf1", "0030", wsv1.RadioKind_RADIO_KIND_ZIGBEE},
		{"0658", "0200", wsv1.RadioKind_RADIO_KIND_ZWAVE},
		{"10c4", "ea60", wsv1.RadioKind_RADIO_KIND_UNKNOWN},
		{"1a86", "55d4", wsv1.RadioKind_RADIO_KIND_UNKNOWN},
		{"", "", wsv1.RadioKind_RADIO_KIND_UNKNOWN},
	}
	for _, c := range cases {
		if got, _ := classifySerial(c.vendor, c.product); got != c.want {
			t.Errorf("classifySerial(%s:%s) = %v, want %v", c.vendor, c.product, got, c.want)
		}
	}
}

// The op is wired through dispatch and audit like every other arm.
func TestListRadiosDispatch(t *testing.T) {
	f := newRadioFixture(t)
	populateRadios(f)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_ListRadios{ListRadios: &wsv1.ListRadios{}}})
	if lr := res.GetListRadios(); lr == nil || len(lr.GetRadios()) != 3 {
		t.Fatalf("list radios: %+v", res)
	}
	if arm, _ := auditFields(&wsv1.Operation{Op: &wsv1.Operation_ListRadios{ListRadios: &wsv1.ListRadios{}}}); arm != "ListRadios" {
		t.Fatalf("audit arm = %q", arm)
	}
}
