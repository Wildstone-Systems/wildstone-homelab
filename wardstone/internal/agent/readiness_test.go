//go:build linux

package agent

// HomeAutomationReadiness (protocol minor 26, #436). Every input is a
// fixture: procfs/sysfs/run files under a temp root, a stubbed interface
// list and a fake systemd client. Nothing reads the networking, D-Bus or
// BlueZ of the machine running the tests.

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type readinessFixture struct {
	t    *testing.T
	root string
}

// newReadinessFixture roots the op at a short temp dir (a unix socket
// path must fit in 108 bytes, which t.TempDir() under a long test name
// can exceed) and stubs the interface list.
func newReadinessFixture(t *testing.T, ifaces []readinessInterface, ifErr error) *readinessFixture {
	t.Helper()
	root, err := os.MkdirTemp("", "rd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	origRoot, origIfaces := readinessRoot, readinessInterfaces
	t.Cleanup(func() { readinessRoot, readinessInterfaces = origRoot, origIfaces })
	readinessRoot = root
	readinessInterfaces = func() ([]readinessInterface, error) { return ifaces, ifErr }
	return &readinessFixture{t: t, root: root}
}

func (f *readinessFixture) write(p, content string) {
	f.t.Helper()
	full := filepath.Join(f.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// socket creates a real unix socket at p (the D-Bus socket's type).
func (f *readinessFixture) socket(p string) {
	f.t.Helper()
	full := filepath.Join(f.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	l, err := net.Listen("unix", full)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { l.Close() })
}

const routeHeader = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

// healthyNode is a node with everything present: eth0 holds the default
// route, docker0 and a veth are plumbing, IPv6 and forwarding are on,
// the bus is a socket, BlueZ runs and hci0 is unblocked.
func healthyNode(f *readinessFixture) {
	f.write("proc/sys/net/ipv6/conf/all/disable_ipv6", "0\n")
	f.write("proc/sys/net/ipv6/conf/all/forwarding", "1\n")
	f.write("proc/sys/net/ipv6/conf/eth0/disable_ipv6", "0\n")
	f.write("proc/net/route", routeHeader+
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"+
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n")
	f.write("proc/net/ipv6_route",
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n")
	f.socket("run/dbus/system_bus_socket")
	f.write("sys/class/bluetooth/hci0/rfkill0/soft", "0\n")
	f.write("sys/class/bluetooth/hci0/rfkill0/hard", "0\n")
}

var healthyIfaces = []readinessInterface{
	{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, Addrs: []string{"127.0.0.1/8", "::1/128"}},
	{Name: "eth0", Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast, Addrs: []string{"192.0.2.10/24", "fe80::1/64"}},
	{Name: "docker0", Flags: net.FlagUp | net.FlagMulticast, Addrs: []string{"172.17.0.1/16"}},
	{Name: "veth1234", Flags: net.FlagUp | net.FlagMulticast},
}

func stubBlueZ(t *testing.T, props map[string]any, dialErr error) *int {
	return stubSystemdConn(t, &fakeSystemdClient{props: props}, dialErr)
}

func runReadiness(t *testing.T) *wsv1.HomeAutomationReadinessResult {
	t.Helper()
	r, oerr := (&Agent{}).homeAutomationReadiness(context.Background())
	if oerr != nil {
		t.Fatalf("homeAutomationReadiness: %v", oerr)
	}
	return r.HomeAutomationReadiness
}

func findChecks(res *wsv1.HomeAutomationReadinessResult, name string) []*wsv1.ReadinessCheck {
	var out []*wsv1.ReadinessCheck
	for _, c := range res.GetChecks() {
		if c.GetCheck() == name {
			out = append(out, c)
		}
	}
	return out
}

// oneCheck returns the only check with this name, failing otherwise.
func oneCheck(t *testing.T, res *wsv1.HomeAutomationReadinessResult, name string) *wsv1.ReadinessCheck {
	t.Helper()
	cs := findChecks(res, name)
	if len(cs) != 1 {
		t.Fatalf("%s: got %d checks: %+v", name, len(cs), cs)
	}
	return cs[0]
}

func wantState(t *testing.T, c *wsv1.ReadinessCheck, state wsv1.ReadinessState, subject string) {
	t.Helper()
	if c.GetState() != state || c.GetSubject() != subject {
		t.Fatalf("%s = %v on %q (%s: %s), want %v on %q", c.GetCheck(), c.GetState(), c.GetSubject(), c.GetValue(), c.GetDetail(), state, subject)
	}
}

func TestReadinessAllReady(t *testing.T) {
	f := newReadinessFixture(t, healthyIfaces, nil)
	healthyNode(f)
	stubBlueZ(t, map[string]any{"LoadState": "loaded", "ActiveState": "active", "SubState": "running"}, nil)
	res := runReadiness(t)

	if !res.GetPlatformSupported() {
		t.Fatal("platform_supported = false")
	}
	if len(res.GetInterfaces()) != 1 || res.Interfaces[0].GetName() != "eth0" || !res.Interfaces[0].GetDefaultRoute() {
		t.Fatalf("interfaces = %+v (only the default-route interface is primary)", res.GetInterfaces())
	}
	if got := res.Interfaces[0].GetAddresses(); len(got) != 2 {
		t.Fatalf("eth0 addresses = %v", got)
	}
	wantState(t, oneCheck(t, res, checkIPv6), wsv1.ReadinessState_READINESS_STATE_READY, "/proc/sys/net/ipv6/conf/all/disable_ipv6")
	wantState(t, oneCheck(t, res, checkInterfaceIPv6), wsv1.ReadinessState_READINESS_STATE_READY, "eth0")
	fwd := oneCheck(t, res, checkIPv6Forwarding)
	wantState(t, fwd, wsv1.ReadinessState_READINESS_STATE_READY, "/proc/sys/net/ipv6/conf/all/forwarding")
	if fwd.GetValue() != "1" {
		t.Fatalf("forwarding value = %q", fwd.GetValue())
	}
	wantState(t, oneCheck(t, res, checkMulticast), wsv1.ReadinessState_READINESS_STATE_READY, "eth0")
	bus := oneCheck(t, res, checkDBusSystemBus)
	wantState(t, bus, wsv1.ReadinessState_READINESS_STATE_READY, "/run/dbus/system_bus_socket")
	if strings.Contains(bus.GetSubject()+bus.GetDetail(), f.root) {
		t.Fatalf("fixture root leaked: %+v", bus)
	}
	wantState(t, oneCheck(t, res, checkBlueZService), wsv1.ReadinessState_READINESS_STATE_READY, "bluetooth.service")
	wantState(t, oneCheck(t, res, checkBluetoothAdapter), wsv1.ReadinessState_READINESS_STATE_READY, "hci0")
	// /dev/net/tun is absent from the fixture.
	wantState(t, oneCheck(t, res, checkTun), wsv1.ReadinessState_READINESS_STATE_MISSING, "/dev/net/tun")

	// Every check in the closed set is present, in its declared order.
	var names []string
	for _, c := range res.GetChecks() {
		if len(names) == 0 || names[len(names)-1] != c.GetCheck() {
			names = append(names, c.GetCheck())
		}
	}
	if strings.Join(names, ",") != strings.Join(readinessChecks, ",") {
		t.Fatalf("check order = %v, want %v", names, readinessChecks)
	}
}

// Absence is a failed prerequisite with evidence, never an OpError.
func TestReadinessAllMissing(t *testing.T) {
	f := newReadinessFixture(t, []readinessInterface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, Addrs: []string{"127.0.0.1/8"}},
		{Name: "eth0", Flags: net.FlagUp | net.FlagBroadcast, Addrs: []string{"192.0.2.10/24"}},
	}, nil)
	f.write("proc/sys/net/ipv6/conf/all/disable_ipv6", "1\n")
	f.write("proc/sys/net/ipv6/conf/all/forwarding", "0\n")
	f.write("proc/sys/net/ipv6/conf/eth0/disable_ipv6", "1\n")
	f.write("run/dbus/system_bus_socket", "not a socket")
	dials := stubBlueZ(t, nil, errors.New("must not dial"))
	res := runReadiness(t)

	wantState(t, oneCheck(t, res, checkIPv6), wsv1.ReadinessState_READINESS_STATE_MISSING, "/proc/sys/net/ipv6/conf/all/disable_ipv6")
	wantState(t, oneCheck(t, res, checkIPv6Forwarding), wsv1.ReadinessState_READINESS_STATE_MISSING, "/proc/sys/net/ipv6/conf/all/forwarding")
	// No default route: eth0 is chosen as the up, addressed fallback.
	ifc := oneCheck(t, res, checkInterfaceIPv6)
	wantState(t, ifc, wsv1.ReadinessState_READINESS_STATE_MISSING, "eth0")
	if ifc.GetValue() != "disable_ipv6=1" {
		t.Fatalf("interface_ipv6 value = %q", ifc.GetValue())
	}
	mc := oneCheck(t, res, checkMulticast)
	wantState(t, mc, wsv1.ReadinessState_READINESS_STATE_MISSING, "eth0")
	if !strings.Contains(mc.GetDetail(), "MULTICAST") {
		t.Fatalf("multicast detail = %q", mc.GetDetail())
	}
	bus := oneCheck(t, res, checkDBusSystemBus)
	wantState(t, bus, wsv1.ReadinessState_READINESS_STATE_MISSING, "/run/dbus/system_bus_socket")
	if bus.GetValue() != "regular file" {
		t.Fatalf("dbus value = %q", bus.GetValue())
	}
	wantState(t, oneCheck(t, res, checkBlueZService), wsv1.ReadinessState_READINESS_STATE_MISSING, "bluetooth.service")
	if *dials != 0 {
		t.Fatal("dialed systemd with no system bus")
	}
	wantState(t, oneCheck(t, res, checkBluetoothAdapter), wsv1.ReadinessState_READINESS_STATE_MISSING, "/sys/class/bluetooth")
	wantState(t, oneCheck(t, res, checkTun), wsv1.ReadinessState_READINESS_STATE_MISSING, "/dev/net/tun")
}

// A kernel without IPv6 has no /proc/sys/net/ipv6 at all: UNSUPPORTED,
// distinct from disabled.
func TestReadinessIPv6Unsupported(t *testing.T) {
	newReadinessFixture(t, healthyIfaces, nil)
	stubBlueZ(t, nil, nil)
	res := runReadiness(t)
	wantState(t, oneCheck(t, res, checkIPv6), wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED, "/proc/sys/net/ipv6/conf/all/disable_ipv6")
	wantState(t, oneCheck(t, res, checkIPv6Forwarding), wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED, "/proc/sys/net/ipv6/conf/all/forwarding")
}

// UNKNOWN: an interface list that failed, a systemd that cannot be
// reached, a sysctl with a value the op does not understand.
func TestReadinessUnknown(t *testing.T) {
	f := newReadinessFixture(t, nil, errors.New("netlink: permission denied"))
	f.write("proc/sys/net/ipv6/conf/all/disable_ipv6", "banana\n")
	f.write("proc/sys/net/ipv6/conf/all/forwarding", "2\n")
	f.socket("run/dbus/system_bus_socket")
	stubBlueZ(t, nil, errors.New("connection refused"))
	res := runReadiness(t)
	wantState(t, oneCheck(t, res, checkIPv6), wsv1.ReadinessState_READINESS_STATE_UNKNOWN, "/proc/sys/net/ipv6/conf/all/disable_ipv6")
	wantState(t, oneCheck(t, res, checkIPv6Forwarding), wsv1.ReadinessState_READINESS_STATE_UNKNOWN, "/proc/sys/net/ipv6/conf/all/forwarding")
	wantState(t, oneCheck(t, res, checkMulticast), wsv1.ReadinessState_READINESS_STATE_UNKNOWN, "")
	wantState(t, oneCheck(t, res, checkInterfaceIPv6), wsv1.ReadinessState_READINESS_STATE_UNKNOWN, "")
	wantState(t, oneCheck(t, res, checkBlueZService), wsv1.ReadinessState_READINESS_STATE_UNKNOWN, "bluetooth.service")
}

func TestReadinessBlueZStates(t *testing.T) {
	cases := map[string]struct {
		props map[string]any
		want  wsv1.ReadinessState
	}{
		"not installed": {map[string]any{"LoadState": "not-found", "ActiveState": "inactive"}, wsv1.ReadinessState_READINESS_STATE_MISSING},
		"stopped":       {map[string]any{"LoadState": "loaded", "ActiveState": "failed", "SubState": "failed"}, wsv1.ReadinessState_READINESS_STATE_MISSING},
		"running":       {map[string]any{"LoadState": "loaded", "ActiveState": "active", "SubState": "running"}, wsv1.ReadinessState_READINESS_STATE_READY},
		"empty answer":  {map[string]any{}, wsv1.ReadinessState_READINESS_STATE_UNKNOWN},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReadinessFixture(t, healthyIfaces, nil)
			f.socket("run/dbus/system_bus_socket")
			stubBlueZ(t, c.props, nil)
			got := oneCheck(t, runReadiness(t), checkBlueZService)
			wantState(t, got, c.want, "bluetooth.service")
			if got.GetValue() == "" {
				t.Fatal("no evidence value")
			}
		})
	}
}

func TestReadinessBluetoothRfkill(t *testing.T) {
	f := newReadinessFixture(t, healthyIfaces, nil)
	stubBlueZ(t, nil, nil)
	f.write("sys/class/bluetooth/hci0/rfkill0/soft", "1\n")
	f.write("sys/class/bluetooth/hci1/rfkill1/hard", "1\n")
	f.write("sys/class/bluetooth/hci1:256/x", "") // a connection, not an adapter
	cs := findChecks(runReadiness(t), checkBluetoothAdapter)
	if len(cs) != 2 {
		t.Fatalf("adapters = %+v", cs)
	}
	wantState(t, cs[0], wsv1.ReadinessState_READINESS_STATE_MISSING, "hci0")
	wantState(t, cs[1], wsv1.ReadinessState_READINESS_STATE_MISSING, "hci1")
	if cs[0].GetValue() != "rfkill soft" || cs[1].GetValue() != "rfkill hard" {
		t.Fatalf("rfkill values = %q, %q", cs[0].GetValue(), cs[1].GetValue())
	}
}

// fakeFileInfo lets classifyTun see a character device without mknod.
type fakeFileInfo struct{ mode fs.FileMode }

func (f fakeFileInfo) Name() string       { return "tun" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

func TestClassifyTun(t *testing.T) {
	wantState(t, classifyTun(fakeFileInfo{mode: fs.ModeDevice | fs.ModeCharDevice | 0o666}, nil), wsv1.ReadinessState_READINESS_STATE_READY, "/dev/net/tun")
	wantState(t, classifyTun(fakeFileInfo{mode: 0o644}, nil), wsv1.ReadinessState_READINESS_STATE_MISSING, "/dev/net/tun")
	wantState(t, classifyTun(nil, fs.ErrNotExist), wsv1.ReadinessState_READINESS_STATE_MISSING, "/dev/net/tun")
	wantState(t, classifyTun(nil, fs.ErrPermission), wsv1.ReadinessState_READINESS_STATE_UNKNOWN, "/dev/net/tun")
}

// Interface names are only ever joined into a path after validation.
func TestReadinessRejectsHostileInterfaceName(t *testing.T) {
	newReadinessFixture(t, []readinessInterface{
		{Name: "../../etc", Flags: net.FlagUp | net.FlagMulticast, Addrs: []string{"192.0.2.1/24"}},
	}, nil)
	stubBlueZ(t, nil, nil)
	res := runReadiness(t)
	if len(res.GetInterfaces()) != 0 {
		t.Fatalf("hostile name reported: %+v", res.GetInterfaces())
	}
}

// The op is wired through dispatch like every other arm.
func TestReadinessDispatch(t *testing.T) {
	f := newReadinessFixture(t, healthyIfaces, nil)
	healthyNode(f)
	stubBlueZ(t, map[string]any{"LoadState": "loaded", "ActiveState": "active"}, nil)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	op := &wsv1.Operation{Op: &wsv1.Operation_HomeAutomationReadiness{HomeAutomationReadiness: &wsv1.HomeAutomationReadiness{}}}
	res := dispatch(t, h, "node-a", op)
	if r := res.GetHomeAutomationReadiness(); r == nil || len(r.GetChecks()) == 0 {
		t.Fatalf("readiness: %+v", res)
	}
	if arm, _ := auditFields(op); arm != "HomeAutomationReadiness" {
		t.Fatalf("audit arm = %q", arm)
	}
}
