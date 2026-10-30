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
	"IPC_LOCK":  true, // memory locking (mlock) for OpenThread Border Router
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

// Host system mounts are the third place a ContainerSpec reaches into
// the host (#445). Wardstone bind-mounts host paths only from its managed
// prefixes (ADR-W003 A1); this is the closed list of host paths OUTSIDE
// them a container may still be given, each read-only and at the same
// path inside. It mirrors the agent's hostSystemMounts entry for entry,
// and TestHostSystemMountAllowlist on both sides runs one fixture.
//
// /run/dbus is the one entry: the directory holding the D-Bus system bus
// socket, which is how a containerised Home Assistant talks to the host's
// BlueZ (bluetooth.service). The directory, not the socket file, because
// dbus-daemon recreates the socket on restart and a file bind would keep
// pointing at the deleted one. Read-only because a client only connects
// to the socket, and connecting is not a write; what the bus then allows
// is the host's D-Bus policy, which Wildstone does not change.
var HostSystemMounts = map[string]string{
	"/run/dbus": "the D-Bus system bus, for host BlueZ (Bluetooth)",
}

// HostSystemMountAllowed reports whether a host path, exactly as written,
// is on the host system mount allowlist. No cleaning: "/run/dbus/" and
// "/run/../run/dbus" are refused rather than normalised.
func HostSystemMountAllowed(path string) bool {
	_, ok := HostSystemMounts[path]
	return ok
}

// HostSystemMountProtocolMinor is the wardstone protocol minor whose agent
// first accepts a HostSystemMounts path. An older agent refuses it as
// outside its managed prefixes, so Keystone refuses to dispatch the spec
// there with the upgrade named, rather than the node answering FORBIDDEN
// after approval.
const HostSystemMountProtocolMinor = 27

// UsesHostSystemMount reports whether the spec mounts any
// HostSystemMounts path.
func (s *ContainerSpec) UsesHostSystemMount() bool {
	if s == nil {
		return false
	}
	for _, m := range s.Mounts {
		if HostSystemMountAllowed(m.Source) {
			return true
		}
	}
	return false
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

// RuntimeIdentityProtocolMinor is the first minor that carries a numeric
// runtime identity and authoritative Docker hardening readback. An older
// Wardstone would silently start the image-default user, so Keystone must
// refuse before mutation whenever runtimeIdentity is present.
const RuntimeIdentityProtocolMinor = 34

// MountAccessPreflightProtocolMinor is the first minor that checks declared
// read-write mounts under the requested effective identity before replacing a
// container. An older Wardstone would ignore the opt-in and perform the
// destructive create/update without the approved safety check.
const MountAccessPreflightProtocolMinor = 37

// IPCLockProtocolMinor is the wardstone protocol minor whose closed
// capability allowlist first admits IPC_LOCK. An older agent still
// refuses it as FORBIDDEN, so a spec asking for it is gated on this
// minor rather than on HostAccessProtocolMinor.
const IPCLockProtocolMinor = 21

// HostNetworkProtocolMinor is the wardstone protocol minor that first
// carried EnsureContainer.network_mode. An older agent ignores the field
// and starts the container on the bridge with nothing published — up,
// green, and deaf to every discovery packet it was moved to hear — so a
// host-mode spec is refused rather than dispatched to one.
const HostNetworkProtocolMinor = 23

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
	var required uint32
	require := func(minor uint32) {
		if minor > required {
			required = minor
		}
	}
	if s.UsesHostSystemMount() {
		require(HostSystemMountProtocolMinor)
	}
	if s.RuntimeIdentity != nil {
		require(RuntimeIdentityProtocolMinor)
	}
	if s.PreflightMountAccess {
		require(MountAccessPreflightProtocolMinor)
	}
	if s.HostNetwork() {
		require(HostNetworkProtocolMinor)
	}
	for _, c := range s.CapAdd {
		if c == "IPC_LOCK" {
			require(IPCLockProtocolMinor)
		}
	}
	if s.usesHardeningFields() {
		require(HardeningProtocolMinor)
	}
	if len(s.Devices) > 0 || len(s.CapAdd) > 0 {
		require(HostAccessProtocolMinor)
	}
	for _, p := range s.Ports {
		if m, ok := ParsePortMapping(p); ok && m.Protocol == "udp" {
			require(HostAccessProtocolMinor)
		}
	}
	return required
}

// usesHardeningFields reports whether the spec sets any H1 field, so
// RequiredProtocolMinor can gate on the HIGHEST minor a spec actually
// needs: a plan that uses hardening AND a device still needs only minor
// 18 (and host networking plus either needs only 23), since each minor is
// defined to also carry everything earlier ones did (v1
// evolves additively, ADR-W003) and an agent new enough for hardening is
// new enough for devices too.
func (s *ContainerSpec) usesHardeningFields() bool {
	return s.ReadOnlyRootfs || len(s.CapDrop) > 0 || s.NoNewPrivileges || s.Resources != nil || s.ShmSize != ""
}
