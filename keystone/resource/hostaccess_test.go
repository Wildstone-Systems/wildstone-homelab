package resource

import "testing"

// deviceExamples is the SAME fixture wardstone/internal/agent's
// TestDeviceAllowlist runs. Keep them identical: the two allowlists live
// in modules that cannot share code, and a shared fixture is what keeps
// lint (here) and enforcement (there) answering the same question.
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

func TestDeviceAllowlistMatchesAgent(t *testing.T) {
	for path, want := range deviceExamples {
		if got := DeviceAllowed(path); got != want {
			t.Errorf("DeviceAllowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func containerWith(mutate func(*ContainerSpec)) *Resource {
	spec := &ContainerSpec{Image: "nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	mutate(spec)
	return &Resource{ApiVersion: ApiVersion, Kind: KindContainer, Metadata: Metadata{Name: "c", Owner: "capability/x", Node: "n1"}, Container: spec}
}

// TestContainerDevicesAreValidated: the allowlist at the door, so a
// package naming a block device fails lint with the reason, not the node
// with FORBIDDEN after approval.
func TestContainerDevicesAreValidated(t *testing.T) {
	ok := containerWith(func(s *ContainerSpec) {
		s.Devices = []DeviceMapping{{Source: "/dev/ttyUSB0"}, {Source: "/dev/dri", Target: "/dev/dri"}}
	})
	if err := ok.Validate(); err != nil {
		t.Fatalf("allowlisted devices refused: %v", err)
	}
	for name, mutate := range map[string]func(*ContainerSpec){
		"block device":            func(s *ContainerSpec) { s.Devices = []DeviceMapping{{Source: "/dev/sda"}} },
		"traversal":               func(s *ContainerSpec) { s.Devices = []DeviceMapping{{Source: "/dev/dri/../sda"}} },
		"relative source":         func(s *ContainerSpec) { s.Devices = []DeviceMapping{{Source: "dev/ttyUSB0"}} },
		"empty source":            func(s *ContainerSpec) { s.Devices = []DeviceMapping{{Source: ""}} },
		"relative target":         func(s *ContainerSpec) { s.Devices = []DeviceMapping{{Source: "/dev/ttyUSB0", Target: "zigbee"}} },
		"unclean target":          func(s *ContainerSpec) { s.Devices = []DeviceMapping{{Source: "/dev/ttyUSB0", Target: "/dev//zigbee"}} },
		"forbidden capability":    func(s *ContainerSpec) { s.CapAdd = []string{"SYS_ADMIN"} },
		"CAP_ prefixed":           func(s *ContainerSpec) { s.CapAdd = []string{"CAP_NET_ADMIN"} },
		"lowercase capability":    func(s *ContainerSpec) { s.CapAdd = []string{"net_admin"} },
		"udp on a bad port":       func(s *ContainerSpec) { s.Ports = []string{"0:53/udp"} },
		"unknown protocol suffix": func(s *ContainerSpec) { s.Ports = []string{"53:53/sctp"} },
	} {
		if err := containerWith(mutate).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	caps := containerWith(func(s *ContainerSpec) { s.CapAdd = []string{"NET_ADMIN", "NET_RAW", "SYS_NICE"} })
	if err := caps.Validate(); err != nil {
		t.Errorf("allowlisted capabilities refused: %v", err)
	}
}

// TestParsePortMappingProtocol pins the suffix: absent means tcp, /udp
// and /tcp are accepted, anything else is refused, and the address and
// port rules are unchanged underneath.
func TestParsePortMappingProtocol(t *testing.T) {
	for in, want := range map[string]PortMapping{
		"53:53":                  {HostPort: 53, ContainerPort: 53, Protocol: "tcp"},
		"53:53/udp":              {HostPort: 53, ContainerPort: 53, Protocol: "udp"},
		"53:53/tcp":              {HostPort: 53, ContainerPort: 53, Protocol: "tcp"},
		"192.168.1.10:53:53/udp": {HostIP: "192.168.1.10", HostPort: 53, ContainerPort: 53, Protocol: "udp"},
		"[::1]:5353:53/udp":      {HostIP: "::1", HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
	} {
		got, ok := ParsePortMapping(in)
		if !ok || got != want {
			t.Errorf("ParsePortMapping(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{"53:53/", "53:53/UDP", "53:53/udp/udp", "/udp", "53/udp:53", "localhost:53:53/udp"} {
		if m, ok := ParsePortMapping(in); ok {
			t.Errorf("ParsePortMapping(%q) = %+v, want refusal", in, m)
		}
	}
	// The four-value form still answers, protocol dropped, for the
	// callers that only care where a port is bound.
	if ip, hp, cp, ok := SplitPortMapping("100.64.0.1:8443:80/udp"); !ok || ip != "100.64.0.1" || hp != 8443 || cp != 80 {
		t.Errorf("SplitPortMapping with a suffix = (%q, %d, %d, %v)", ip, hp, cp, ok)
	}
}
