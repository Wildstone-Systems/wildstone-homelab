package agent

import (
	"os"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// deviceExamples is the fixture keystone/resource's twin test uses
// verbatim (TestDeviceAllowlistMatchesAgent). The two allowlists live in
// different modules and cannot share code; sharing the examples is what
// keeps lint and enforcement answering the same question.
var deviceExamples = map[string]bool{
	"/dev/dri":            true,
	"/dev/dri/renderD128": true,
	"/dev/dri/card0":      true,
	"/dev/net/tun":        true,
	"/dev/ttyUSB0":        true,
	"/dev/ttyACM1":        true,
	"/dev/ttyAMA0":        true,
	"/dev/serial/by-id/usb-ITead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_abc-if00-port0": true,
	"/dev/bus/usb":                 true,
	"/dev/bus/usb/001/004":         true,
	"/dev/apex_0":                  true,
	"/dev/nvidia0":                 true,
	"/dev/nvidiactl":               true,
	"/dev/nvidia-uvm":              true,
	"/dev/kfd":                     true,
	"/dev/video0":                  true,
	"/dev/vchiq":                   true,
	"/dev/sda":                     false,
	"/dev/sda1":                    false,
	"/dev/nvme0n1":                 false,
	"/dev/mapper/root":             false,
	"/dev/mem":                     false,
	"/dev/kmsg":                    false,
	"/dev/null":                    false,
	"/dev/tty":                     false,
	"/dev/tty1":                    false,
	"/dev/dri/../sda":              false,
	"/dev/serial/by-id/../sda":     false,
	"/dev/serial/by-id":            false,
	"/etc/wildstone":               false,
	"dev/ttyUSB0":                  false,
	"":                             false,
	"/dev/ttyUSB0/":                false,
	"/dev/nvidia-caps/nvidia-cap1": false,
}

func TestDeviceAllowlist(t *testing.T) {
	for path, want := range deviceExamples {
		if got := deviceAllowed(path); got != want {
			t.Errorf("deviceAllowed(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestCheckDeviceRefusesWhatTheAllowlistDoesNotName: a block device is the
// case the list exists for, and it must be refused before existence is
// even consulted — the message names the policy, not a missing file.
func TestCheckDeviceRefusesWhatTheAllowlistDoesNotName(t *testing.T) {
	a := &Agent{}
	for _, host := range []string{"/dev/sda", "/dev/mem", "/dev/mapper/root", "/dev/dri/../sda"} {
		_, oe := a.checkDevice(&wsv1.DeviceMapping{HostPath: host})
		if oe == nil {
			t.Fatalf("device %q was allowed", host)
		}
		if oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			t.Errorf("device %q: refused as missing rather than as forbidden; the policy must answer first", host)
		}
	}
	if _, oe := a.checkDevice(&wsv1.DeviceMapping{HostPath: "ttyUSB0"}); oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Errorf("a relative device path was not refused as invalid: %v", oe)
	}
}

// TestCheckDeviceRequiresTheDeviceToExist: a typo in a device path
// should be a NOT_FOUND from the agent, naming the path, not a Docker
// error after the plan was approved.
func TestCheckDeviceRequiresTheDeviceToExist(t *testing.T) {
	a := &Agent{}
	_, oe := a.checkDevice(&wsv1.DeviceMapping{HostPath: "/dev/ttyUSB99"})
	if oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("missing device: got %v, want NOT_FOUND", oe)
	}
	if !strings.Contains(oe.GetMessage(), "/dev/ttyUSB99") {
		t.Errorf("message does not name the device: %q", oe.GetMessage())
	}
}

// TestCheckDeviceResolvesSymlinks: /dev/serial/by-id/... is the path
// every guide tells people to use because it is a stable alias — and an
// alias can point at anything. The literal name is on the allowlist; a
// link that lands on a block device must still be refused, and one that
// lands on a real serial device is handed to Docker RESOLVED, so the
// container gets the node the check approved.
func TestCheckDeviceResolvesSymlinks(t *testing.T) {
	links := map[string]string{
		"/dev/serial/by-id/usb-good-if00-port0": "/dev/ttyUSB0",
		"/dev/serial/by-id/usb-evil-if00-port0": "/dev/sda",
		"/dev/ttyUSB0":                          "/dev/ttyUSB0",
		"/dev/sda":                              "/dev/sda",
	}
	prevEval, prevStat := evalSymlinks, statDevice
	evalSymlinks = func(p string) (string, error) {
		if r, ok := links[p]; ok {
			return r, nil
		}
		return "", os.ErrNotExist
	}
	statDevice = func(string) (os.FileInfo, error) { return os.Stat(os.DevNull) }
	t.Cleanup(func() { evalSymlinks, statDevice = prevEval, prevStat })

	a := &Agent{}
	good, oe := a.checkDevice(&wsv1.DeviceMapping{HostPath: "/dev/serial/by-id/usb-good-if00-port0"})
	if oe != nil {
		t.Fatalf("an alias to a serial device was refused: %s", oe.GetMessage())
	}
	if good.PathOnHost != "/dev/ttyUSB0" {
		t.Errorf("PathOnHost = %q, want the resolved node", good.PathOnHost)
	}
	if good.PathInContainer != "/dev/serial/by-id/usb-good-if00-port0" {
		t.Errorf("PathInContainer = %q, want the name the package asked for", good.PathInContainer)
	}
	if good.CgroupPermissions != "rwm" {
		t.Errorf("CgroupPermissions = %q", good.CgroupPermissions)
	}

	_, oe = a.checkDevice(&wsv1.DeviceMapping{HostPath: "/dev/serial/by-id/usb-evil-if00-port0"})
	if oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("an alias resolving to /dev/sda: got %v, want FORBIDDEN", oe)
	}
	if !strings.Contains(oe.GetMessage(), "/dev/sda") {
		t.Errorf("message does not say where the alias led: %q", oe.GetMessage())
	}

	// An explicit container path is honoured, and must itself be clean.
	m, oe := a.checkDevice(&wsv1.DeviceMapping{HostPath: "/dev/ttyUSB0", ContainerPath: "/dev/zigbee"})
	if oe != nil || m.PathInContainer != "/dev/zigbee" {
		t.Errorf("explicit container path: %+v, %v", m, oe)
	}
	if _, oe := a.checkDevice(&wsv1.DeviceMapping{HostPath: "/dev/ttyUSB0", ContainerPath: "dev/zigbee"}); oe == nil {
		t.Error("a relative container path was accepted")
	}
}

func TestCapabilityAllowlist(t *testing.T) {
	for _, ok := range []string{"NET_ADMIN", "NET_RAW", "SYS_NICE"} {
		if oe := checkCapability(ok); oe != nil {
			t.Errorf("%s refused: %v", ok, oe.GetMessage())
		}
	}
	for _, bad := range []string{"SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "DAC_READ_SEARCH", "ALL", "", "net_admin"} {
		oe := checkCapability(bad)
		if oe == nil {
			t.Errorf("%q was allowed", bad)
			continue
		}
		if oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
			t.Errorf("%q: code %v, want FORBIDDEN", bad, oe.GetCode())
		}
	}
	// The CAP_ spelling is refused as invalid rather than silently
	// accepted or silently refused: one canonical form.
	if oe := checkCapability("CAP_NET_ADMIN"); oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Errorf("CAP_NET_ADMIN: got %v, want INVALID with the fix in the message", oe)
	}
}

// TestEnsureContainerCarriesHostAccessToDocker runs the whole path over
// the real session and the fake Docker: a UDP port lands under a /udp
// key, a device lands in HostConfig.Devices resolved and rwm, and a
// capability lands in CapAdd — while a forbidden capability fails the
// op before anything is created.
func TestEnsureContainerCarriesHostAccessToDocker(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// A device node that exists on every Linux box the tests run on is
	// not on the allowlist (that is the point of the list), so stand in
	// for the existence check.
	prev := statDevice
	statDevice = func(string) (os.FileInfo, error) { return os.Stat(os.DevNull) }
	t.Cleanup(func() { statDevice = prev })

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "pihole", Image: testDigestImage,
			Ports: []*wsv1.PortBinding{
				{HostPort: 53, ContainerPort: 53},
				{HostPort: 53, ContainerPort: 53, Protocol: wsv1.PortProtocol_PORT_PROTOCOL_UDP},
				{HostPort: 8080, ContainerPort: 80, Protocol: wsv1.PortProtocol_PORT_PROTOCOL_TCP},
			},
			Devices: []*wsv1.DeviceMapping{{HostPath: "/dev/net/tun"}},
			CapAdd:  []string{"NET_ADMIN"},
		},
	}})
	if oe := res.GetError(); oe != nil {
		// /dev/net/tun may genuinely not exist where the tests run; the
		// existence check is stubbed above, but EvalSymlinks is not.
		if oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			t.Skip("no /dev/net/tun on this machine: " + oe.GetMessage())
		}
		t.Fatalf("ensure: %v", oe)
	}
	fake.mu.Lock()
	hc := fake.containers["pihole"].host
	fake.mu.Unlock()
	if got := hc.PortBindings["53/tcp"]; len(got) != 1 || got[0].HostPort != "53" {
		t.Errorf("53/tcp = %+v", got)
	}
	if got := hc.PortBindings["53/udp"]; len(got) != 1 || got[0].HostPort != "53" {
		t.Errorf("53/udp = %+v; a UDP binding must be keyed by protocol or Docker publishes TCP", got)
	}
	if got := hc.PortBindings["80/tcp"]; len(got) != 1 || got[0].HostPort != "8080" {
		t.Errorf("80/tcp = %+v", got)
	}
	if len(hc.Devices) != 1 || hc.Devices[0].PathOnHost != "/dev/net/tun" || hc.Devices[0].PathInContainer != "/dev/net/tun" || hc.Devices[0].CgroupPermissions != "rwm" {
		t.Errorf("Devices = %+v", hc.Devices)
	}
	if len(hc.CapAdd) != 1 || hc.CapAdd[0] != "NET_ADMIN" {
		t.Errorf("CapAdd = %+v", hc.CapAdd)
	}

	// A forbidden capability fails the op, and nothing is created.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "rooted", Image: testDigestImage, CapAdd: []string{"SYS_ADMIN"},
		},
	}})
	if oe := res.GetError(); oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("SYS_ADMIN: got %v, want FORBIDDEN", res.GetError())
	}
	fake.mu.Lock()
	_, created := fake.containers["rooted"]
	fake.mu.Unlock()
	if created {
		t.Error("a container was created despite the refused capability")
	}
}
