package resource

import (
	"regexp"
	"sort"
)

// Devices and capabilities are the two places a ContainerSpec asks for
// a piece of the HOST rather than a piece of Docker (protocol minor 12).
// Both are closed allowlists, and both are enforced by the agent
// (wardstone/internal/agent/devices.go) — the lists here exist so a
// package fails LINT on /dev/sda or SYS_ADMIN rather than failing on
// the node inside an approved plan.
//
// The two modules cannot share code (the agent is internal to its
// module and Keystone must not depend on the agent), so they share a
// FIXTURE instead: TestDeviceAllowlistMatchesAgent here and
// TestDeviceAllowlist there run the same examples, and a pattern added
// to one list without the other fails whichever side was forgotten.

// devicePatterns mirror the agent's, entry for entry. Each is a class
// of device a household service has a reason to want; what is absent
// is the point — no block devices, no /dev/mem, nothing under
// /dev/mapper.
var devicePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^/dev/dri(/(card|renderD)[0-9]+)?$`),
	regexp.MustCompile(`^/dev/nvidia(ctl|-uvm|-uvm-tools|-modeset|[0-9]+)$`),
	regexp.MustCompile(`^/dev/kfd$`),
	regexp.MustCompile(`^/dev/net/tun$`),
	regexp.MustCompile(`^/dev/tty(USB|ACM|AMA)[0-9]+$`),
	regexp.MustCompile(`^/dev/serial/by-(id|path)/[^/]+$`),
	regexp.MustCompile(`^/dev/bus/usb(/[0-9]{3}(/[0-9]{3})?)?$`),
	regexp.MustCompile(`^/dev/apex_[0-9]+$`),
	regexp.MustCompile(`^/dev/video[0-9]+$`),
	regexp.MustCompile(`^/dev/vchiq$`),
}

// deviceClasses is the human list the validation message names.
const deviceClasses = "GPU render nodes, /dev/net/tun, USB and serial radios, /dev/bus/usb, Coral TPUs, video devices"

// DeviceAllowed reports whether a cleaned host path is on the device
// allowlist.
func DeviceAllowed(path string) bool {
	for _, re := range devicePatterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// ContainerCapabilities is the closed set of Linux capabilities a
// container may add, named without the CAP_ prefix. Mirrors the agent's.
var ContainerCapabilities = map[string]bool{
	"NET_ADMIN": true, // VPN clients: create and route through a tunnel interface
	"NET_RAW":   true, // ICMP and raw sockets, e.g. ping-based monitors
	"SYS_NICE":  true, // scheduling priority for transcodes
}

// ContainerCapabilityNames is the allowlist, sorted, for messages.
func ContainerCapabilityNames() []string {
	out := make([]string, 0, len(ContainerCapabilities))
	for n := range ContainerCapabilities {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
