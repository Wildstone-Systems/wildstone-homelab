package resource

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

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
	caps := containerWith(func(s *ContainerSpec) { s.CapAdd = []string{"IPC_LOCK", "NET_ADMIN", "NET_RAW", "SYS_NICE"} })
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

func TestRequiredProtocolMinor(t *testing.T) {
	for name, tc := range map[string]struct {
		spec ContainerSpec
		want uint32
	}{
		"plain":            {ContainerSpec{Image: "x", Ports: []string{"53:53", "127.0.0.1:8080:80/tcp"}}, 0},
		"udp":              {ContainerSpec{Image: "x", Ports: []string{"53:53/udp"}}, HostAccessProtocolMinor},
		"device":           {ContainerSpec{Image: "x", Devices: []DeviceMapping{{Source: "/dev/net/tun"}}}, HostAccessProtocolMinor},
		"cap":              {ContainerSpec{Image: "x", CapAdd: []string{"NET_ADMIN"}}, HostAccessProtocolMinor},
		"ipcLock":          {ContainerSpec{Image: "x", CapAdd: []string{"IPC_LOCK"}}, IPCLockProtocolMinor},
		"ipcLock+harden":   {ContainerSpec{Image: "x", CapAdd: []string{"IPC_LOCK"}, ReadOnlyRootfs: true}, IPCLockProtocolMinor},
		"nil spec":         {ContainerSpec{}, 0},
		"readOnlyRootfs":   {ContainerSpec{Image: "x", ReadOnlyRootfs: true}, HardeningProtocolMinor},
		"capDrop":          {ContainerSpec{Image: "x", CapDrop: []string{"ALL"}}, HardeningProtocolMinor},
		"noNewPrivileges":  {ContainerSpec{Image: "x", NoNewPrivileges: true}, HardeningProtocolMinor},
		"resources":        {ContainerSpec{Image: "x", Resources: &ContainerResources{Memory: "4g"}}, HardeningProtocolMinor},
		"shmSize":          {ContainerSpec{Image: "x", ShmSize: "1g"}, HardeningProtocolMinor},
		"hardening+device": {ContainerSpec{Image: "x", ReadOnlyRootfs: true, Devices: []DeviceMapping{{Source: "/dev/net/tun"}}}, HardeningProtocolMinor},
		"hostNetwork":      {ContainerSpec{Image: "x", NetworkMode: NetworkModeHost}, HostNetworkProtocolMinor},
		"host+hardening":   {ContainerSpec{Image: "x", NetworkMode: NetworkModeHost, ReadOnlyRootfs: true, CapAdd: []string{"NET_RAW"}}, HostNetworkProtocolMinor},
		"runtimeIdentity":  {ContainerSpec{Image: "x", RuntimeIdentity: testRuntimeIdentity(1000)}, RuntimeIdentityProtocolMinor},
		"runtime+dbus": {ContainerSpec{Image: "x", RuntimeIdentity: testRuntimeIdentity(1000),
			Mounts: []Mount{{Source: "/run/dbus", Target: "/run/dbus", ReadOnly: true}}}, RuntimeIdentityProtocolMinor},
		"host+dbus": {ContainerSpec{Image: "x", NetworkMode: NetworkModeHost, CapAdd: []string{"NET_ADMIN", "NET_RAW"},
			Mounts: []Mount{{Source: "cfg", Target: "/config"}, {Source: "/run/dbus", Target: "/run/dbus", ReadOnly: true}}}, HostSystemMountProtocolMinor},
		"volume only": {ContainerSpec{Image: "x", Mounts: []Mount{{Source: "cfg", Target: "/config"}}}, 0},
	} {
		if got := tc.spec.RequiredProtocolMinor(); got != tc.want {
			t.Errorf("%s: RequiredProtocolMinor = %d, want %d", name, got, tc.want)
		}
	}
	var nilSpec *ContainerSpec
	if nilSpec.RequiredProtocolMinor() != 0 {
		t.Error("nil spec must need nothing")
	}
}

// TestContainerHardeningFieldsAreValidated is H1's lint coverage: a
// container using the hardening fields correctly passes, and each
// malformed form (bad capDrop syntax, a capability in both capAdd and
// capDrop, a non-Docker-size memory/shmSize string, a non-positive cpus
// value, a negative pidsLimit) is refused.
func TestContainerHardeningFieldsAreValidated(t *testing.T) {
	ok := containerWith(func(s *ContainerSpec) {
		s.ReadOnlyRootfs = true
		s.CapDrop = []string{"ALL"}
		s.NoNewPrivileges = true
		s.Resources = &ContainerResources{Memory: "4g", Cpus: "2", PidsLimit: 512}
		s.ShmSize = "1g"
	})
	if err := ok.Validate(); err != nil {
		t.Fatalf("well-formed hardening fields refused: %v", err)
	}
	for name, mutate := range map[string]func(*ContainerSpec){
		"capDrop CAP_ prefixed":     func(s *ContainerSpec) { s.CapDrop = []string{"CAP_SYS_ADMIN"} },
		"capDrop lowercase":         func(s *ContainerSpec) { s.CapDrop = []string{"sys_admin"} },
		"capDrop empty entry":       func(s *ContainerSpec) { s.CapDrop = []string{""} },
		"capDrop overlaps capAdd":   func(s *ContainerSpec) { s.CapAdd = []string{"NET_ADMIN"}; s.CapDrop = []string{"NET_ADMIN"} },
		"memory not a size string":  func(s *ContainerSpec) { s.Resources = &ContainerResources{Memory: "lots"} },
		"memory zero":               func(s *ContainerSpec) { s.Resources = &ContainerResources{Memory: "0"} },
		"cpus not a number":         func(s *ContainerSpec) { s.Resources = &ContainerResources{Cpus: "two"} },
		"cpus zero":                 func(s *ContainerSpec) { s.Resources = &ContainerResources{Cpus: "0"} },
		"cpus negative":             func(s *ContainerSpec) { s.Resources = &ContainerResources{Cpus: "-1"} },
		"pidsLimit negative":        func(s *ContainerSpec) { s.Resources = &ContainerResources{PidsLimit: -1} },
		"shmSize not a size string": func(s *ContainerSpec) { s.ShmSize = "huge" },
		"runtime uid missing":       func(s *ContainerSpec) { s.RuntimeIdentity = &ContainerRuntimeIdentity{} },
		"runtime gid only":          func(s *ContainerSpec) { gid := int64(1000); s.RuntimeIdentity = &ContainerRuntimeIdentity{GID: &gid} },
		"runtime uid negative":      func(s *ContainerSpec) { s.RuntimeIdentity = testRuntimeIdentity(-1) },
		"runtime uid too large":     func(s *ContainerSpec) { s.RuntimeIdentity = testRuntimeIdentity(1 << 32) },
		"runtime identity on k3s": func(s *ContainerSpec) {
			s.Backend = "k3s"
			s.RuntimeIdentity = testRuntimeIdentity(1000)
		},
		"runtime gid negative": func(s *ContainerSpec) {
			s.RuntimeIdentity = testRuntimeIdentity(1000, -1)
		},
	} {
		if err := containerWith(mutate).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestContainerRuntimeIdentityYAMLAndHashShape(t *testing.T) {
	r, err := Unmarshal([]byte(`
apiVersion: wildstone.dev/v1
kind: Container
metadata:
  name: app
  owner: capability/example
  node: node-a
spec:
  image: ghcr.io/example/app@sha256:` + strings.Repeat("a", 64) + `
  runtimeIdentity:
    uid: 1000
    gid: 1001
`))
	if err != nil || r.Container.RuntimeIdentity == nil || r.Container.RuntimeIdentity.GID == nil {
		t.Fatalf("numeric runtime identity did not decode: resource=%+v err=%v", r, err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	plain, _ := json.Marshal(&ContainerSpec{Image: "x"})
	if strings.Contains(string(plain), "runtimeIdentity") {
		t.Fatalf("omitted runtime identity changed hashed JSON: %s", plain)
	}
	withIdentity, _ := json.Marshal(r.Container)
	if !strings.Contains(string(withIdentity), `"runtimeIdentity":{"uid":1000,"gid":1001}`) {
		t.Fatalf("runtime identity missing from hashed JSON: %s", withIdentity)
	}
	for _, value := range []string{"-1", "service-account"} {
		doc := strings.Replace(string(mustMarshalResource(t, r)), "uid: 1000", "uid: "+value, 1)
		if parsed, err := Unmarshal([]byte(doc)); err == nil && parsed.Validate() == nil {
			t.Errorf("uid %q was accepted", value)
		}
	}
	explicitRoot := strings.Replace(string(mustMarshalResource(t, r)), "uid: 1000", "uid: 0", 1)
	parsedRoot, err := Unmarshal([]byte(explicitRoot))
	if err != nil || parsedRoot.Validate() != nil || parsedRoot.Container.RuntimeIdentity.UID == nil || *parsedRoot.Container.RuntimeIdentity.UID != 0 {
		t.Fatalf("explicit uid 0 must remain distinguishable from an omitted uid: resource=%+v err=%v", parsedRoot, err)
	}
	for name, identity := range map[string]string{
		"empty":    "{}",
		"gid only": `{"gid":1001}`,
	} {
		encoded, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		doc := strings.Replace(string(encoded), `"runtimeIdentity":{"uid":1000,"gid":1001}`, `"runtimeIdentity":`+identity, 1)
		if parsed, err := Unmarshal([]byte(doc)); err == nil && parsed.Validate() == nil {
			t.Errorf("%s runtime identity was accepted", name)
		}
	}
}

func testRuntimeIdentity(uid int64, gid ...int64) *ContainerRuntimeIdentity {
	id := &ContainerRuntimeIdentity{UID: &uid}
	if len(gid) > 0 {
		id.GID = &gid[0]
	}
	return id
}

func mustMarshalResource(t *testing.T, r Resource) []byte {
	t.Helper()
	b, err := Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestContainerHardeningFieldsAbsentDoNotChangeValidation: the H1 fields
// are additive — a container that never sets them validates exactly as
// it did before this change existed.
func TestContainerHardeningFieldsAbsentDoNotChangeValidation(t *testing.T) {
	plain := containerWith(func(s *ContainerSpec) {})
	if err := plain.Validate(); err != nil {
		t.Fatalf("a container with no hardening fields was refused: %v", err)
	}
}

// TestContainerCapabilityAllowlistIsPinned fixes the exact set. The agent
// keeps a mirror (wardstone/internal/agent TestCapabilityAllowlistIsPinned
// pins the same list), so a change to one side fails a test until the
// other is changed to match.
func TestContainerCapabilityAllowlistIsPinned(t *testing.T) {
	want := []string{"IPC_LOCK", "NET_ADMIN", "NET_RAW", "SYS_NICE"}
	if got := ContainerCapabilityNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("allowlist = %v, want %v", got, want)
	}
	for _, bad := range []string{"SYS_ADMIN", "ALL", "CAP_IPC_LOCK", "ipc_lock"} {
		spec := containerWith(func(s *ContainerSpec) { s.CapAdd = []string{bad} })
		if err := spec.Validate(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestContainerNetworkModeIsValidated: host is the one non-default mode,
// it cannot be combined with published ports, and a Job may not use it.
func TestContainerNetworkModeIsValidated(t *testing.T) {
	host := containerWith(func(s *ContainerSpec) {
		s.NetworkMode = NetworkModeHost
		s.Devices = []DeviceMapping{{Source: "/dev/ttyUSB0"}}
	})
	if err := host.Validate(); err != nil {
		t.Fatalf("networkMode: host refused: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*ContainerSpec)
		field  string
	}{
		"bridge spelled out":  {func(s *ContainerSpec) { s.NetworkMode = "bridge" }, "spec.networkMode"},
		"none":                {func(s *ContainerSpec) { s.NetworkMode = "none" }, "spec.networkMode"},
		"container namespace": {func(s *ContainerSpec) { s.NetworkMode = "container:other" }, "spec.networkMode"},
		"wrong case":          {func(s *ContainerSpec) { s.NetworkMode = "Host" }, "spec.networkMode"},
		"host with ports": {func(s *ContainerSpec) {
			s.NetworkMode = NetworkModeHost
			s.Ports = []string{"8123:8123"}
		}, "spec.ports"},
	} {
		err := containerWith(tc.mutate).Validate()
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: got %v, want a ValidationError", name, err)
			continue
		}
		if ve.Field != tc.field {
			t.Errorf("%s: field = %q, want %q", name, ve.Field, tc.field)
		}
	}

	job := &Resource{ApiVersion: ApiVersion, Kind: KindJob, Metadata: Metadata{Name: "j", Owner: "capability/x", Node: "n1"},
		Job: &JobSpec{Run: ContainerSpec{Image: host.Container.Image, NetworkMode: NetworkModeHost}}}
	var ve *ValidationError
	if err := job.Validate(); !errors.As(err, &ve) || ve.Field != "spec.run.networkMode" {
		t.Errorf("job with networkMode: host: got %v, want a spec.run.networkMode refusal", err)
	}
}

// TestContainerNetworkModeYAMLAndHashShape: the field is spelled
// networkMode in YAML, and an omitted mode adds nothing to the JSON the
// reconciler hashes, so no existing container's hash moves.
func TestContainerNetworkModeYAMLAndHashShape(t *testing.T) {
	r, err := Unmarshal([]byte(`
apiVersion: wildstone.dev/v1
kind: Container
metadata:
  name: hub
  owner: capability/home
  node: node-a
spec:
  image: ghcr.io/example/hub@sha256:` + strings.Repeat("a", 64) + `
  networkMode: host
`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !r.Container.HostNetwork() {
		t.Fatalf("networkMode = %q, want host", r.Container.NetworkMode)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := Marshal(r)
	if err != nil || !strings.Contains(string(out), "networkMode: host") {
		t.Errorf("Marshal = %q, %v; want networkMode: host", out, err)
	}

	plain, _ := json.Marshal(&ContainerSpec{Image: "x"})
	if strings.Contains(string(plain), "networkMode") {
		t.Errorf("omitted networkMode reached the hashed JSON: %s", plain)
	}
	withHost, _ := json.Marshal(&ContainerSpec{Image: "x", NetworkMode: NetworkModeHost})
	if !strings.Contains(string(withHost), `"networkMode":"host"`) {
		t.Errorf("host networkMode missing from the hashed JSON: %s", withHost)
	}
}

// hostSystemMountExamples is the SAME fixture wardstone/internal/agent's
// TestHostSystemMountAllowlist runs (#445). Exact paths only: nothing is
// cleaned into a match, and nothing under or beside /run/dbus counts.
var hostSystemMountExamples = map[string]bool{
	"/run/dbus":                     true,
	"/run/dbus/":                    false,
	"/run/dbus/system_bus_socket":   false,
	"/var/run/dbus":                 false,
	"/run/../run/dbus":              false,
	"/run":                          false,
	"/run/docker.sock":              false,
	"/var/run/docker.sock":          false,
	"/run/systemd":                  false,
	"/etc":                          false,
	"/":                             false,
	"run/dbus":                      false,
	"":                              false,
	"/run/dbus-other":               false,
	"/sys/class/bluetooth":          false,
	"/dev/bus/usb":                  false,
	"/run/dbus/../docker.sock":      false,
	"/run/dbus/containers":          false,
	"/run/user/1000/bus":            false,
	"/var/lib/bluetooth":            false,
	"/etc/dbus-1/system.d":          false,
	"/run/dbus/system_bus_socket/x": false,
}

func TestHostSystemMountAllowlist(t *testing.T) {
	for path, want := range hostSystemMountExamples {
		if got := HostSystemMountAllowed(path); got != want {
			t.Errorf("HostSystemMountAllowed(%q) = %v, want %v", path, got, want)
		}
	}
	if len(HostSystemMounts) != 1 {
		t.Errorf("HostSystemMounts = %v: a new entry is a new way into the host and needs the agent's twin, a fixture row and a review", HostSystemMounts)
	}
}

// TestHostSystemMountIsReadOnlyAtItsOwnPath: /run/dbus lints only as a
// read-only bind at /run/dbus; anything else is the lint error, not the
// agent's FORBIDDEN after approval.
func TestHostSystemMountIsReadOnlyAtItsOwnPath(t *testing.T) {
	ok := containerWith(func(s *ContainerSpec) {
		s.Mounts = []Mount{{Source: "cfg", Target: "/config"}, {Source: "/run/dbus", Target: "/run/dbus", ReadOnly: true}}
	})
	if err := ok.Validate(); err != nil {
		t.Fatalf("read-only /run/dbus refused: %v", err)
	}
	for name, m := range map[string]Mount{
		"writable":         {Source: "/run/dbus", Target: "/run/dbus"},
		"elsewhere inside": {Source: "/run/dbus", Target: "/host/dbus", ReadOnly: true},
	} {
		spec := containerWith(func(s *ContainerSpec) { s.Mounts = []Mount{m} })
		err := spec.Validate()
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "readOnly: true at the same path") {
			t.Errorf("%s: got %v, want the read-only/same-path refusal", name, err)
		}
	}
}
