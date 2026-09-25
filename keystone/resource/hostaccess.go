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

// HostAccessProtocolMinor is the wardstone protocol minor that first
// carried UDP port publication, devices and capabilities.
const HostAccessProtocolMinor = 12

// HardeningProtocolMinor is the wardstone protocol minor that first
// carried the H1 hardening fields: readOnlyRootfs, capDrop,
// noNewPrivileges, resources and shmSize (ADR-W039 Phase 6/H1). Unlike
// HostAccessProtocolMinor's fields, which WIDEN what a container may
// touch, these NARROW it — so an agent that silently ignores one does
// not fail loud, it gives the container more reach than the spec asked
// for. That is the wrong direction to degrade in, which is why a spec
// using any of them is refused rather than dispatched to an agent that
// cannot honour it, exactly as HostAccessProtocolMinor already is.
const HardeningProtocolMinor = 18

// RequiredProtocolMinor reports the lowest agent protocol minor that can
// honour this spec in full, or 0 when any agent can. An agent below it
// does not fail: it IGNORES the fields it cannot read, which is the
// wrong kind of quiet — a "/udp" port published as TCP is a second
// binding on the same TCP port, which Docker refuses, a device or
// capability simply absent leaves the app to discover it, and a
// hardening field silently dropped leaves the container LESS contained
// than the spec asked for. The reconciler asks this before dispatching,
// so the answer is a refusal naming the node rather than a container
// that failed to start, or one that started with a shell it should not
// have had.
func (s *ContainerSpec) RequiredProtocolMinor() uint32 {
	if s == nil {
		return 0
	}
	if s.usesHardeningFields() {
		return HardeningProtocolMinor
	}
	if len(s.Devices) > 0 || len(s.CapAdd) > 0 {
		return HostAccessProtocolMinor
	}
	for _, p := range s.Ports {
		if m, ok := ParsePortMapping(p); ok && m.Protocol == "udp" {
			return HostAccessProtocolMinor
		}
	}
	return 0
}

// usesHardeningFields reports whether the spec sets any H1 field, so
// RequiredProtocolMinor can gate on the HIGHEST minor a spec actually
// needs: a plan that uses hardening AND a device still needs only minor
// 18, since 18 is defined to also carry everything minor 12 did (v1
// evolves additively, ADR-W003) and an agent new enough for hardening is
// new enough for devices too.
func (s *ContainerSpec) usesHardeningFields() bool {
	return s.ReadOnlyRootfs || len(s.CapDrop) > 0 || s.NoNewPrivileges || s.Resources != nil || s.ShmSize != ""
}
