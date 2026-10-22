//go:build linux

package agent

// HomeAutomationReadiness (protocol minor 26, #436) reports whether a
// node can host LAN-native home automation: IPv6, multicast on the
// interfaces that face the LAN, the D-Bus system bus, BlueZ, a Bluetooth
// adapter and /dev/net/tun. Every check is a read — procfs, sysfs, a
// Lstat, the kernel's interface list and one systemd unit property query
// over D-Bus. Nothing here writes a sysctl, starts a unit or execs.
//
// A missing prerequisite is a result, never an OpError: the op itself
// only fails if it cannot run at all, which none of its reads can cause.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// readinessRoot prefixes every absolute path this op reads (/proc, /sys,
// /run, /dev). Empty in production; tests point it at a fixture tree.
// Subjects reported to Keystone never carry the prefix.
var readinessRoot = ""

// readinessInterface is what the op needs from one network interface.
type readinessInterface struct {
	Name  string
	Flags net.Flags
	Addrs []string // CIDR notation
}

// readinessInterfaces lists the node's interfaces. Go reads them over a
// netlink route socket — no exec. A var so tests never depend on the
// networking of the machine running them.
var readinessInterfaces = func() ([]readinessInterface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]readinessInterface, 0, len(ifs))
	for _, ifc := range ifs {
		ri := readinessInterface{Name: ifc.Name, Flags: ifc.Flags}
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				ri.Addrs = append(ri.Addrs, a.String())
			}
		}
		out = append(out, ri)
	}
	return out, nil
}

// Bounds on what one result may carry. A real node has a handful of
// interfaces; the caps only keep a container-dense host's answer small.
const (
	maxReadinessInterfaces = 16
	maxReadinessAddrs      = 16
	bluezQueryTimeout      = 3 * time.Second
	bluezUnit              = "bluetooth.service"
)

// virtualIfacePrefixes names interfaces that are never the LAN-facing
// one when choosing candidates without a default route: container and
// VM plumbing.
var virtualIfacePrefixes = []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "tun", "tap", "wg"}

func (a *Agent) homeAutomationReadiness(ctx context.Context) (*wsv1.OpResult_HomeAutomationReadiness, *wsv1.OpError) {
	res := &wsv1.HomeAutomationReadinessResult{PlatformSupported: true}

	ifaces, ifErr := readinessInterfaces()
	candidates := candidateInterfaces(ifaces, defaultRouteInterfaces())
	for _, c := range candidates {
		res.Interfaces = append(res.Interfaces, c.proto)
	}

	res.Checks = append(res.Checks, checkIPv6Enabled())
	res.Checks = append(res.Checks, checkInterfacesIPv6(candidates, ifErr)...)
	res.Checks = append(res.Checks, checkForwarding())
	res.Checks = append(res.Checks, checkInterfacesMulticast(candidates, ifErr)...)
	bus := checkDBus()
	res.Checks = append(res.Checks, bus)
	res.Checks = append(res.Checks, checkBlueZ(ctx, bus))
	res.Checks = append(res.Checks, checkBluetoothAdapters()...)
	res.Checks = append(res.Checks, classifyTun(os.Stat(readinessPath("/dev/net/tun"))))
	return &wsv1.OpResult_HomeAutomationReadiness{HomeAutomationReadiness: res}, nil
}

func readinessPath(p string) string {
	return filepath.Join(readinessRoot, p)
}

// readProcValue reads a one-line procfs/sysfs value. absent reports a
// not-exist error, which every caller treats differently from other
// failures.
func readProcValue(p string) (value string, absent bool, err error) {
	b, err := os.ReadFile(readinessPath(p))
	if err != nil {
		return "", errors.Is(err, fs.ErrNotExist), err
	}
	if len(b) > 64 {
		b = b[:64]
	}
	return strings.TrimSpace(string(b)), false, nil
}

// ipv6Supported reports whether the kernel exposes IPv6 at all: booted
// with ipv6.disable=1, or built without it, /proc/sys/net/ipv6 is absent.
func ipv6Supported() bool {
	_, err := os.Stat(readinessPath("/proc/sys/net/ipv6"))
	return err == nil
}

// checkIPv6Enabled reads net.ipv6.conf.all.disable_ipv6.
func checkIPv6Enabled() *wsv1.ReadinessCheck {
	const p = "/proc/sys/net/ipv6/conf/all/disable_ipv6"
	v, absent, err := readProcValue(p)
	switch {
	case absent && !ipv6Supported():
		return readinessCheck(checkIPv6, wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED, p, "", "kernel has no IPv6 (/proc/sys/net/ipv6 absent)")
	case err != nil:
		return readinessCheck(checkIPv6, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, p, "", "read: "+err.Error())
	case v == "0":
		return readinessCheck(checkIPv6, wsv1.ReadinessState_READINESS_STATE_READY, p, v, "IPv6 enabled")
	case v == "1":
		return readinessCheck(checkIPv6, wsv1.ReadinessState_READINESS_STATE_MISSING, p, v, "IPv6 disabled for all interfaces")
	default:
		return readinessCheck(checkIPv6, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, p, v, "unexpected value")
	}
}

// checkForwarding reads net.ipv6.conf.all.forwarding. Forwarding is a
// prerequisite only for a Thread border router (or anything else that
// routes IPv6 for another network); a hub that is not one does not need
// it, and the detail says so. The op reports, it never sets.
func checkForwarding() *wsv1.ReadinessCheck {
	const p = "/proc/sys/net/ipv6/conf/all/forwarding"
	const why = "; required only to route IPv6 for another network (Thread border router)"
	v, absent, err := readProcValue(p)
	switch {
	case absent && !ipv6Supported():
		return readinessCheck(checkIPv6Forwarding, wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED, p, "", "kernel has no IPv6")
	case err != nil:
		return readinessCheck(checkIPv6Forwarding, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, p, "", "read: "+err.Error())
	case v == "1":
		return readinessCheck(checkIPv6Forwarding, wsv1.ReadinessState_READINESS_STATE_READY, p, v, "IPv6 forwarding on"+why)
	case v == "0":
		return readinessCheck(checkIPv6Forwarding, wsv1.ReadinessState_READINESS_STATE_MISSING, p, v, "IPv6 forwarding off"+why)
	default:
		return readinessCheck(checkIPv6Forwarding, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, p, v, "unexpected value")
	}
}

// defaultRouteInterfaces names every interface carrying an IPv4 or IPv6
// default route, from /proc/net/route and /proc/net/ipv6_route.
func defaultRouteInterfaces() map[string]bool {
	out := map[string]bool{}
	scan := func(p string, match func([]string) (string, bool)) {
		f, err := os.Open(readinessPath(p))
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if name, ok := match(strings.Fields(sc.Text())); ok && name != "lo" {
				out[name] = true
			}
		}
	}
	// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
	scan("/proc/net/route", func(f []string) (string, bool) {
		return firstField(f), len(f) >= 8 && f[1] == "00000000" && f[7] == "00000000"
	})
	// dest dest_plen src src_plen nexthop metric refcnt use flags iface
	scan("/proc/net/ipv6_route", func(f []string) (string, bool) {
		return lastField(f), len(f) >= 10 && f[0] == strings.Repeat("0", 32) && f[1] == "00"
	})
	return out
}

func firstField(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func lastField(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

type candidateInterface struct {
	iface readinessInterface
	proto *wsv1.NodeInterface
}

// candidateInterfaces picks the LAN-facing interfaces: those holding a
// default route or, when there is none (an isolated LAN), every up,
// addressed, non-loopback interface that is not container or VM
// plumbing. Sorted by name.
func candidateInterfaces(ifaces []readinessInterface, defaults map[string]bool) []candidateInterface {
	var primary, fallback []readinessInterface
	for _, ifc := range ifaces {
		if !kernelDevName.MatchString(ifc.Name) {
			continue // never joined into a path, never reported
		}
		if defaults[ifc.Name] {
			primary = append(primary, ifc)
			continue
		}
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || len(ifc.Addrs) == 0 || isVirtualIface(ifc.Name) {
			continue
		}
		fallback = append(fallback, ifc)
	}
	chosen := primary
	if len(chosen) == 0 {
		chosen = fallback
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].Name < chosen[j].Name })
	if len(chosen) > maxReadinessInterfaces {
		chosen = chosen[:maxReadinessInterfaces]
	}
	out := make([]candidateInterface, 0, len(chosen))
	for _, ifc := range chosen {
		addrs := append([]string(nil), ifc.Addrs...)
		sort.Strings(addrs)
		if len(addrs) > maxReadinessAddrs {
			addrs = addrs[:maxReadinessAddrs]
		}
		out = append(out, candidateInterface{iface: ifc, proto: &wsv1.NodeInterface{
			Name:         ifc.Name,
			Addresses:    addrs,
			Up:           ifc.Flags&net.FlagUp != 0,
			Multicast:    ifc.Flags&net.FlagMulticast != 0,
			DefaultRoute: defaults[ifc.Name],
		}})
	}
	return out
}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// noCandidates is the single check reported in place of per-interface
// checks when there is no interface to check.
func noCandidates(name string, ifErr error) *wsv1.ReadinessCheck {
	if ifErr != nil {
		return readinessCheck(name, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, "", "", "list interfaces: "+ifErr.Error())
	}
	return readinessCheck(name, wsv1.ReadinessState_READINESS_STATE_MISSING, "", "", "no LAN-facing interface (no default route and no up, addressed interface)")
}

// checkInterfacesIPv6 reports, per candidate, whether IPv6 is enabled on
// it and it holds an IPv6 address.
func checkInterfacesIPv6(cands []candidateInterface, ifErr error) []*wsv1.ReadinessCheck {
	if len(cands) == 0 {
		return []*wsv1.ReadinessCheck{noCandidates(checkInterfaceIPv6, ifErr)}
	}
	var out []*wsv1.ReadinessCheck
	for _, c := range cands {
		name := c.iface.Name
		v, absent, err := readProcValue("/proc/sys/net/ipv6/conf/" + name + "/disable_ipv6")
		var v6 []string
		for _, a := range c.proto.GetAddresses() {
			if strings.Contains(a, ":") {
				v6 = append(v6, a)
			}
		}
		switch {
		case absent && !ipv6Supported():
			out = append(out, readinessCheck(checkInterfaceIPv6, wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED, name, "", "kernel has no IPv6"))
		case err != nil:
			out = append(out, readinessCheck(checkInterfaceIPv6, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, name, "", "read disable_ipv6: "+err.Error()))
		case v == "1":
			out = append(out, readinessCheck(checkInterfaceIPv6, wsv1.ReadinessState_READINESS_STATE_MISSING, name, "disable_ipv6="+v, "IPv6 disabled on "+name))
		case v != "0":
			out = append(out, readinessCheck(checkInterfaceIPv6, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, name, "disable_ipv6="+v, "unexpected value"))
		case len(v6) == 0:
			out = append(out, readinessCheck(checkInterfaceIPv6, wsv1.ReadinessState_READINESS_STATE_MISSING, name, "disable_ipv6=0", "IPv6 enabled but "+name+" holds no IPv6 address"))
		default:
			out = append(out, readinessCheck(checkInterfaceIPv6, wsv1.ReadinessState_READINESS_STATE_READY, name, strings.Join(v6, ","), "IPv6 address on "+name))
		}
	}
	return out
}

// checkInterfacesMulticast reports, per candidate, whether it is up and
// multicast-capable — what mDNS, SSDP and Matter discovery need.
func checkInterfacesMulticast(cands []candidateInterface, ifErr error) []*wsv1.ReadinessCheck {
	if len(cands) == 0 {
		return []*wsv1.ReadinessCheck{noCandidates(checkMulticast, ifErr)}
	}
	var out []*wsv1.ReadinessCheck
	for _, c := range cands {
		flags := c.iface.Flags.String()
		switch {
		case c.iface.Flags&net.FlagUp == 0:
			out = append(out, readinessCheck(checkMulticast, wsv1.ReadinessState_READINESS_STATE_MISSING, c.iface.Name, flags, c.iface.Name+" is down"))
		case c.iface.Flags&net.FlagMulticast == 0:
			out = append(out, readinessCheck(checkMulticast, wsv1.ReadinessState_READINESS_STATE_MISSING, c.iface.Name, flags, c.iface.Name+" lacks the MULTICAST flag"))
		default:
			out = append(out, readinessCheck(checkMulticast, wsv1.ReadinessState_READINESS_STATE_READY, c.iface.Name, flags, c.iface.Name+" is up and multicast-capable"))
		}
	}
	return out
}

// checkDBus Lstats the system bus socket: it must exist and be a socket.
func checkDBus() *wsv1.ReadinessCheck {
	const p = "/run/dbus/system_bus_socket"
	fi, err := os.Lstat(readinessPath(p))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return readinessCheck(checkDBusSystemBus, wsv1.ReadinessState_READINESS_STATE_MISSING, p, "", "no D-Bus system bus socket")
	case err != nil:
		return readinessCheck(checkDBusSystemBus, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, p, "", "lstat: "+err.Error())
	case fi.Mode().Type() == fs.ModeSocket:
		return readinessCheck(checkDBusSystemBus, wsv1.ReadinessState_READINESS_STATE_READY, p, "socket", "D-Bus system bus socket present")
	default:
		return readinessCheck(checkDBusSystemBus, wsv1.ReadinessState_READINESS_STATE_MISSING, p, fileTypeName(fi.Mode()), "exists but is not a socket")
	}
}

// checkBlueZ asks systemd, over the same D-Bus seam SystemdUnitStatus
// uses, for bluetooth.service's state — the unit BlueZ ships. The unit
// name is fixed here; nothing on the wire chooses it. BlueZ publishes no
// version over D-Bus, and reading it would mean running bluetoothd, so
// the version is not reported.
func checkBlueZ(ctx context.Context, bus *wsv1.ReadinessCheck) *wsv1.ReadinessCheck {
	switch bus.GetState() {
	case wsv1.ReadinessState_READINESS_STATE_READY:
	case wsv1.ReadinessState_READINESS_STATE_MISSING:
		return readinessCheck(checkBlueZService, wsv1.ReadinessState_READINESS_STATE_MISSING, bluezUnit, "", "BlueZ needs the D-Bus system bus, which is missing")
	default:
		return readinessCheck(checkBlueZService, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, bluezUnit, "", "D-Bus system bus state unknown")
	}
	ctx, cancel := context.WithTimeout(ctx, bluezQueryTimeout)
	defer cancel()
	conn, err := systemdConn(ctx)
	if err != nil {
		return readinessCheck(checkBlueZService, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, bluezUnit, "", "connect to systemd: "+err.Error())
	}
	defer conn.Close()
	props, err := conn.GetUnitPropertiesContext(ctx, bluezUnit)
	if err != nil {
		return readinessCheck(checkBlueZService, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, bluezUnit, "", "query unit: "+err.Error())
	}
	load, active, sub := stringProp(props, "LoadState"), stringProp(props, "ActiveState"), stringProp(props, "SubState")
	value := fmt.Sprintf("load=%s active=%s sub=%s", load, active, sub)
	switch {
	case load == "not-found":
		return readinessCheck(checkBlueZService, wsv1.ReadinessState_READINESS_STATE_MISSING, bluezUnit, value, "BlueZ is not installed")
	case active == "active":
		return readinessCheck(checkBlueZService, wsv1.ReadinessState_READINESS_STATE_READY, bluezUnit, value, "BlueZ is running (version not exposed without exec)")
	case active == "":
		return readinessCheck(checkBlueZService, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, bluezUnit, value, "systemd returned no state")
	default:
		return readinessCheck(checkBlueZService, wsv1.ReadinessState_READINESS_STATE_MISSING, bluezUnit, value, "BlueZ is installed but not running")
	}
}

// checkBluetoothAdapters reports each hciN controller in sysfs, MISSING
// when rfkill blocks it, and one MISSING check when there is none.
func checkBluetoothAdapters() []*wsv1.ReadinessCheck {
	const classDir = "/sys/class/bluetooth"
	entries, err := os.ReadDir(readinessPath(classDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return []*wsv1.ReadinessCheck{readinessCheck(checkBluetoothAdapter, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, classDir, "", "read: "+err.Error())}
	}
	var out []*wsv1.ReadinessCheck
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "hci") || strings.Contains(name, ":") || !kernelDevName.MatchString(name) {
			continue
		}
		dir := readinessPath(filepath.Join(classDir, name))
		soft, hard := rfkillState(dir)
		switch {
		case hard:
			out = append(out, readinessCheck(checkBluetoothAdapter, wsv1.ReadinessState_READINESS_STATE_MISSING, name, "rfkill hard", name+" is hard-blocked by rfkill"))
		case soft:
			out = append(out, readinessCheck(checkBluetoothAdapter, wsv1.ReadinessState_READINESS_STATE_MISSING, name, "rfkill soft", name+" is soft-blocked by rfkill"))
		default:
			out = append(out, readinessCheck(checkBluetoothAdapter, wsv1.ReadinessState_READINESS_STATE_READY, name, "unblocked", name+" present"))
		}
	}
	if len(out) == 0 {
		return []*wsv1.ReadinessCheck{readinessCheck(checkBluetoothAdapter, wsv1.ReadinessState_READINESS_STATE_MISSING, classDir, "", "no Bluetooth controller")}
	}
	return out
}

// rfkillState reads the soft/hard block flags of an hci's rfkillN child.
func rfkillState(hciDir string) (soft, hard bool) {
	entries, err := os.ReadDir(hciDir)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "rfkill") || !kernelDevName.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(hciDir, e.Name())
		if b, err := os.ReadFile(filepath.Join(dir, "soft")); err == nil && strings.TrimSpace(string(b)) == "1" {
			soft = true
		}
		if b, err := os.ReadFile(filepath.Join(dir, "hard")); err == nil && strings.TrimSpace(string(b)) == "1" {
			hard = true
		}
	}
	return soft, hard
}

// classifyTun turns a Stat of /dev/net/tun into a check. Split from the
// Stat so tests can hand it a character device without mknod.
func classifyTun(fi fs.FileInfo, err error) *wsv1.ReadinessCheck {
	const p = "/dev/net/tun"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return readinessCheck(checkTun, wsv1.ReadinessState_READINESS_STATE_MISSING, p, "", "no TUN device node (tun module not loaded?)")
	case err != nil:
		return readinessCheck(checkTun, wsv1.ReadinessState_READINESS_STATE_UNKNOWN, p, "", "stat: "+err.Error())
	case fi.Mode()&fs.ModeCharDevice != 0:
		value := "char"
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			value = fmt.Sprintf("char %d:%d", unix.Major(st.Rdev), unix.Minor(st.Rdev))
		}
		return readinessCheck(checkTun, wsv1.ReadinessState_READINESS_STATE_READY, p, value, "TUN device present")
	default:
		return readinessCheck(checkTun, wsv1.ReadinessState_READINESS_STATE_MISSING, p, fileTypeName(fi.Mode()), "exists but is not a character device")
	}
}

func fileTypeName(m fs.FileMode) string {
	switch t := m.Type(); {
	case t == 0:
		return "regular file"
	case t&fs.ModeDir != 0:
		return "directory"
	case t&fs.ModeSymlink != 0:
		return "symlink"
	case t&fs.ModeSocket != 0:
		return "socket"
	case t&fs.ModeNamedPipe != 0:
		return "fifo"
	case t&fs.ModeDevice != 0:
		return "block device"
	default:
		return t.String()
	}
}
