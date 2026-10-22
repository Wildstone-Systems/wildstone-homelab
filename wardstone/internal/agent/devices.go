package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// Devices and capabilities (protocol minor 12) are the two places where
// a container spec asks for a piece of the HOST rather than a piece of
// Docker, and both are closed allowlists here for the reason the
// operation surface is a closed oneof (ADR-W003): "no arbitrary host
// access" has to be a property of what the agent will do, not of what a
// package author remembered not to ask for.
//
// The lists are deliberately narrow and each entry says why it exists.
// keystone/resource carries the same lists so a package fails LINT on a
// device outside them rather than failing here, in an approved plan;
// this side is the one that is enforced. TestDeviceAllowlistMatchesKeystone
// in each module pins the two to one set of examples.

// devicePatterns are the host device paths a container may be handed.
// Anchored regexps over the cleaned path, one per class of device a
// household service has a reason to want. What is NOT here is the
// point: no block devices, no /dev/mem, no /dev/kmsg, nothing under
// /dev/mapper — a container with any of those has the host.
var devicePatterns = []*regexp.Regexp{
	// GPU render nodes for hardware transcode and inference (Jellyfin,
	// Plex, Frigate, Ollama on Intel/AMD).
	regexp.MustCompile(`^/dev/dri(/(card|renderD)[0-9]+)?$`),
	// NVIDIA's device nodes, for the same jobs on that hardware.
	regexp.MustCompile(`^/dev/nvidia(ctl|-uvm|-uvm-tools|-modeset|[0-9]+)$`),
	// AMD ROCm compute.
	regexp.MustCompile(`^/dev/kfd$`),
	// A VPN client's tunnel interface (Gluetun, WireGuard).
	regexp.MustCompile(`^/dev/net/tun$`),
	// USB and UART radios — Zigbee and Z-Wave sticks — by kernel name
	// or by the stable udev alias every guide tells people to use.
	regexp.MustCompile(`^/dev/tty(USB|ACM|AMA)[0-9]+$`),
	regexp.MustCompile(`^/dev/serial/by-(id|path)/[^/]+$`),
	// Whole-USB passthrough, for the radios that need it and for
	// Home Assistant's USB discovery.
	regexp.MustCompile(`^/dev/bus/usb(/[0-9]{3}(/[0-9]{3})?)?$`),
	// Coral Edge TPU (PCIe and USB present differently; the USB one
	// arrives through /dev/bus/usb above).
	regexp.MustCompile(`^/dev/apex_[0-9]+$`),
	// Cameras plugged into the node, for Frigate.
	regexp.MustCompile(`^/dev/video[0-9]+$`),
	// Raspberry Pi's VideoCore, for hardware decode there.
	regexp.MustCompile(`^/dev/vchiq$`),
}

// capabilities is the closed set of Linux capabilities a container may
// add over Docker's default. Named without the CAP_ prefix, which is
// how Docker's API spells them.
//
// SYS_ADMIN, SYS_MODULE, SYS_PTRACE, DAC_READ_SEARCH and their friends
// are absent on purpose: each is a different spelling of "root on the
// host", and the answer to a package that needs one is a schema
// discussion, not an entry here.
var capabilities = map[string]string{
	// Locking memory with mlock, which the OpenThread Border Router
	// uses to keep its keys and state out of swap. Grants no access to
	// the host beyond the container's own memory accounting limits.
	"IPC_LOCK": "memory locking (mlock), e.g. OpenThread Border Router",
	// Creating a tunnel interface and routing through it (Gluetun,
	// WireGuard). Grants control of the container's OWN network
	// namespace, not the host's.
	"NET_ADMIN": "VPN clients: create and route through a tunnel interface",
	// Raw sockets, for ping and packet capture inside the container
	// (Uptime Kuma's ICMP monitors, some VPN clients).
	"NET_RAW": "ICMP and raw sockets, e.g. ping-based monitors",
	// Raising process priority, which media servers ask for to keep a
	// transcode ahead of a scan.
	"SYS_NICE": "scheduling priority for transcodes",
}

// hostSystemMounts is the closed set of host paths OUTSIDE the managed
// prefixes a container may be bind-mounted (protocol minor 27, #445).
// Everywhere else ADR-W003 A1 holds: a host path is mounted only from a
// managed prefix. Each entry is an exact path, mounted read-only at the
// same path inside; keystone/resource.HostSystemMounts is the lint twin
// and TestHostSystemMountAllowlist on each side runs one fixture.
//
// Not here, and not to be: /var/run/docker.sock or anything else that
// is the agent's own job in a socket, /, /etc, /sys, /proc.
var hostSystemMounts = map[string]string{
	// The D-Bus system bus, for Home Assistant's Bluetooth over the
	// host's BlueZ. The directory rather than the socket, because
	// dbus-daemon recreates the socket on restart. Read-only: connecting
	// to a socket is not a write, and what the bus lets the client do is
	// the host's D-Bus policy.
	"/run/dbus": "the D-Bus system bus, for host BlueZ (Bluetooth)",
}

// hostSystemMountAllowed reports whether a host path, exactly as written
// (no cleaning), is on the host system mount allowlist.
func hostSystemMountAllowed(path string) bool {
	_, ok := hostSystemMounts[path]
	return ok
}

// checkHostSystemMount holds an allowlisted host system mount to its
// rules: read-only, the same path inside, and the path itself rather than
// a symlink to somewhere else. A missing path is NOT_FOUND — the host has
// no D-Bus system bus — rather than letting Docker create an empty
// directory there.
func checkHostSystemMount(m *wsv1.Mount) *wsv1.OpError {
	src := m.GetSource()
	if !m.GetReadOnly() || m.GetTarget() != src {
		return opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"host path %q (%s) may only be mounted read-only at the same path inside", src, hostSystemMounts[src])
	}
	resolved, err := evalSymlinks(src)
	if err != nil {
		if os.IsNotExist(err) {
			return opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "host path %q does not exist on this node", src)
		}
		return opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "resolve %q: %v", src, err)
	}
	if resolved != src {
		return opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"host path %q resolves to %q: an allowlisted host path is mounted only as itself", src, resolved)
	}
	return nil
}

// statDevice is os.Stat, replaceable in tests: the allowlist can be
// exercised on a machine with no such devices, and the existence check
// is what makes a typo in a device path a NOT_FOUND now rather than a
// Docker error after the plan was approved.
var statDevice = os.Stat

// evalSymlinks is filepath.EvalSymlinks, replaceable in tests for the
// same reason: the alias-to-block-device case cannot be built under
// /dev by a test, and it is the case the resolution rule exists for.
var evalSymlinks = filepath.EvalSymlinks

// deviceAllowed reports whether a cleaned host path is on the device
// allowlist. Pure, so keystone/resource's twin can be held to the same
// examples.
func deviceAllowed(path string) bool {
	for _, re := range devicePatterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// checkDevice validates one DeviceMapping and returns Docker's form of it.
//
// The path is checked as written AND after resolving symlinks. The
// second check is the one that matters: /dev/serial/by-id/... is the
// path people are told to use precisely because it is a stable alias,
// and an alias can point anywhere. A link that resolves outside the
// allowlist is refused even though its name is on it.
func (a *Agent) checkDevice(d *wsv1.DeviceMapping) (deviceMapping, *wsv1.OpError) {
	host := d.GetHostPath()
	if host == "" || !filepath.IsAbs(host) || filepath.Clean(host) != host {
		return deviceMapping{}, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"device %q: must be an absolute, clean host path under /dev", host)
	}
	if !deviceAllowed(host) {
		return deviceMapping{}, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"device %q is not on the agent's device allowlist (%s): a container is never handed "+
				"a block device or anything else that amounts to the host", host, deviceClasses())
	}
	resolved, err := evalSymlinks(host)
	if err != nil {
		if os.IsNotExist(err) {
			return deviceMapping{}, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND,
				"device %q does not exist on this node", host)
		}
		return deviceMapping{}, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "resolve device %q: %v", host, err)
	}
	if resolved != host && !deviceAllowed(resolved) {
		return deviceMapping{}, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"device %q resolves to %q, which is not on the device allowlist", host, resolved)
	}
	if _, err := statDevice(resolved); err != nil {
		if os.IsNotExist(err) {
			return deviceMapping{}, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND,
				"device %q does not exist on this node", host)
		}
		return deviceMapping{}, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stat device %q: %v", host, err)
	}
	inContainer := d.GetContainerPath()
	if inContainer == "" {
		inContainer = host
	}
	if !filepath.IsAbs(inContainer) || filepath.Clean(inContainer) != inContainer {
		return deviceMapping{}, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"device %q: container path %q must be absolute and clean", host, inContainer)
	}
	// Docker resolves the symlink itself, but handing it the resolved
	// node means the container sees the same device the check approved,
	// not whatever the alias points at by the time Docker looks.
	return deviceMapping{PathOnHost: resolved, PathInContainer: inContainer, CgroupPermissions: "rwm"}, nil
}

// checkCapability refuses anything outside the closed set, including
// the CAP_-prefixed spelling: one canonical form, so a package cannot
// carry a capability the allowlist would have refused under its other
// name.
func checkCapability(name string) *wsv1.OpError {
	if _, ok := capabilities[name]; ok {
		return nil
	}
	if strings.HasPrefix(name, "CAP_") {
		return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"capability %q: name it without the CAP_ prefix, as Docker does", name)
	}
	return opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
		"capability %q is not on the agent's allowlist (%s)", name, capabilityNames())
}

func capabilityNames() string {
	names := make([]string, 0, len(capabilities))
	for n := range capabilities {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func deviceClasses() string {
	return "GPU render nodes, /dev/net/tun, USB and serial radios, /dev/bus/usb, Coral TPUs, video devices"
}

// portProtocol maps the wire enum to Docker's port-key suffix.
// UNSPECIFIED is TCP, so an older Keystone publishes what it always has.
func portProtocol(p wsv1.PortProtocol) string {
	if p == wsv1.PortProtocol_PORT_PROTOCOL_UDP {
		return "udp"
	}
	return "tcp"
}

// String form for logs and audit rows.
func describeDevices(ds []*wsv1.DeviceMapping) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		parts = append(parts, fmt.Sprintf("%s->%s", d.GetHostPath(), d.GetContainerPath()))
	}
	return strings.Join(parts, ",")
}

// DeviceSourcesLabel records, on a container the agent creates, each
// device's host path AS REQUESTED — the /dev/serial/by-id alias, not
// the node it resolved to — keyed by the path inside the container.
//
// checkDevice hands Docker the resolved node (/dev/ttyUSB1), and Docker
// keeps that in HostConfig for every later start: its own restart
// policy after a crash or a reboot, and ensureContainer's docker start
// of a stopped container whose spec has not changed. The by-id alias
// is the stable thing; the ttyUSB number is not. Unplug two sticks and
// plug them back in the other order and the stored node is now the
// OTHER stick: Zigbee2MQTT restarted against a Thread RCP, a second
// owner for one radio (#435) with nothing in the spec to show it. The
// label is what lets the agent notice: re-resolve the alias, compare
// with what Docker will actually hand over.
const DeviceSourcesLabel = "dev.wildstone.device-sources"

// deviceSourcesLabel encodes requested[i] (the op's host paths) against
// the container paths checkDevice settled on, in the same order. Empty
// when there are no devices, so no label is written.
func deviceSourcesLabel(requested []*wsv1.DeviceMapping, checked []deviceMapping) string {
	if len(checked) == 0 {
		return ""
	}
	m := make(map[string]string, len(checked))
	for i, d := range checked {
		m[d.PathInContainer] = requested[i].GetHostPath()
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// currentDeviceMappings re-resolves a container's recorded device
// sources now. A source that no longer resolves (the stick is
// unplugged) is left out: there is nothing to compare it with, and
// re-creating the container would only fail checkDevice's NOT_FOUND.
// A container without the label (created before it existed, or with no
// devices) yields nothing, and so is never reported stale.
func currentDeviceMappings(ci *containerInspect) []deviceMapping {
	raw := ci.Config.Labels[DeviceSourcesLabel]
	if raw == "" {
		return nil
	}
	var sources map[string]string
	if json.Unmarshal([]byte(raw), &sources) != nil {
		return nil
	}
	var out []deviceMapping
	for inContainer, host := range sources {
		resolved, err := evalSymlinks(host)
		if err != nil {
			continue
		}
		out = append(out, deviceMapping{PathOnHost: resolved, PathInContainer: inContainer})
	}
	return out
}

// staleDevice reports the first wanted mapping whose host node differs
// from what Docker has stored for the same container path, or "" when
// every one matches.
func staleDevice(ci *containerInspect, want []deviceMapping) string {
	have := make(map[string]string, len(ci.HostConfig.Devices))
	for _, d := range ci.HostConfig.Devices {
		have[d.PathInContainer] = d.PathOnHost
	}
	for _, d := range want {
		if got := have[d.PathInContainer]; got != d.PathOnHost {
			return fmt.Sprintf("%s is %s now, but the container still maps %q", d.PathInContainer, d.PathOnHost, got)
		}
	}
	return ""
}
