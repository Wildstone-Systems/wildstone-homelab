package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestContainerEnsureOpCarriesHostIP is the end of the #41 gap: the wire
// protocol has carried host_ip since minor 1 and the agent has always
// rendered it into Docker's PortBindings, but the reconciler dropped it on
// the floor, so every published port bound every interface no matter what
// the spec said.
func TestContainerEnsureOpCarriesHostIP(t *testing.T) {
	spec := &resource.ContainerSpec{
		Image: "caddy@sha256:df7f1c2fb114453b951de51a98efc010db1655a92c2e86be6706714e2417a78d",
		Ports: []string{"100.64.0.1:8443:80", "127.0.0.1:9000:9000", "80:80"},
	}
	got := docker.ensureContainer("caddy-private", spec, ContainerNetwork, "", nil).GetEnsureContainer().GetPorts()
	want := []struct {
		ip        string
		host, ctr uint32
	}{
		{"100.64.0.1", 8443, 80},
		{"127.0.0.1", 9000, 9000},
		{"", 80, 80}, // no address named: Docker's every-interface default, unchanged
	}
	if len(got) != len(want) {
		t.Fatalf("got %d bindings, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].GetHostIp() != w.ip || got[i].GetHostPort() != w.host || got[i].GetContainerPort() != w.ctr {
			t.Errorf("binding %d = (%q, %d, %d), want (%q, %d, %d)", i,
				got[i].GetHostIp(), got[i].GetHostPort(), got[i].GetContainerPort(), w.ip, w.host, w.ctr)
		}
	}
}

// TestSpecHashCoversBindAddress: changing only the bind address must move
// the hash, or a fleet already running the 0.0.0.0 form would keep running
// it after the operator narrows the spec — converged on paper, publicly
// listening in fact. Same failure mode the network field had (#40).
func TestSpecHashCoversBindAddress(t *testing.T) {
	wide := &resource.ContainerSpec{Image: "x", Ports: []string{"8443:80"}}
	narrow := &resource.ContainerSpec{Image: "x", Ports: []string{"127.0.0.1:8443:80"}}
	if specHash(wide, ContainerNetwork, "", nil) == specHash(narrow, ContainerNetwork, "", nil) {
		t.Error("spec hash is blind to the bind address, so narrowing it would never redeploy")
	}
}

func TestPortPublishingUpgradeChangesPortedContainerHash(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       *resource.ContainerSpec
		wantChange bool
	}{
		{"published port", &resource.ContainerSpec{Image: "x", Ports: []string{"7443:7443"}}, true},
		{"no ports", &resource.ContainerSpec{Image: "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Match the pre-upgrade hash exactly. A changed label makes the
			// reconciler replace an already running Docker container.
			data, err := json.Marshal(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, []byte("\x00net="+ContainerNetwork)...)
			old := sha256.Sum256(data)
			legacy := hex.EncodeToString(old[:8])
			changed := specHash(tc.spec, ContainerNetwork, "", nil) != legacy
			if changed != tc.wantChange {
				t.Errorf("hash changed = %v, want %v", changed, tc.wantChange)
			}
		})
	}
}

// TestContainerEnsureOpCarriesHostAccess is the minor-12 half of the
// test above: a "/udp" suffix reaches the wire as the UDP protocol and
// a bare mapping as TCP (explicitly, never UNSPECIFIED), and devices and
// capabilities travel as declared. Without this the agent's new fields
// would be reachable from no package — the same gap host_ip sat in.
func TestContainerEnsureOpCarriesHostAccess(t *testing.T) {
	spec := &resource.ContainerSpec{
		Image:   "pihole/pihole@sha256:f7d1be836e3bc608b56d82fc9904f5a831cdfbc0dc9c6d58f94e4c985c70038b",
		Ports:   []string{"53:53", "53:53/udp", "192.168.1.10:8053:80/tcp"},
		Devices: []resource.DeviceMapping{{Source: "/dev/ttyUSB0"}, {Source: "/dev/dri", Target: "/dev/dri"}},
		CapAdd:  []string{"NET_ADMIN"},
	}
	ec := docker.ensureContainer("pihole", spec, ContainerNetwork, "", nil).GetEnsureContainer()
	ports := ec.GetPorts()
	if len(ports) != 3 {
		t.Fatalf("got %d bindings, want 3", len(ports))
	}
	if ports[0].GetProtocol() != wsv1.PortProtocol_PORT_PROTOCOL_TCP {
		t.Errorf("bare mapping protocol = %v, want explicit TCP", ports[0].GetProtocol())
	}
	if ports[1].GetProtocol() != wsv1.PortProtocol_PORT_PROTOCOL_UDP || ports[1].GetHostPort() != 53 {
		t.Errorf("/udp mapping = %+v", ports[1])
	}
	if ports[2].GetProtocol() != wsv1.PortProtocol_PORT_PROTOCOL_TCP || ports[2].GetHostIp() != "192.168.1.10" || ports[2].GetContainerPort() != 80 {
		t.Errorf("/tcp mapping with an address = %+v", ports[2])
	}
	devs := ec.GetDevices()
	if len(devs) != 2 || devs[0].GetHostPath() != "/dev/ttyUSB0" || devs[0].GetContainerPath() != "" || devs[1].GetContainerPath() != "/dev/dri" {
		t.Errorf("devices = %+v", devs)
	}
	if caps := ec.GetCapAdd(); len(caps) != 1 || caps[0] != "NET_ADMIN" {
		t.Errorf("cap_add = %+v", caps)
	}

	// The spec hash must move on each of the new fields, or a fleet
	// running without a device keeps running without it after the
	// operator adds one — converged on paper, missing in fact.
	base := &resource.ContainerSpec{Image: "x", Ports: []string{"53:53"}}
	for name, changed := range map[string]*resource.ContainerSpec{
		"protocol":   {Image: "x", Ports: []string{"53:53/udp"}},
		"device":     {Image: "x", Ports: []string{"53:53"}, Devices: []resource.DeviceMapping{{Source: "/dev/net/tun"}}},
		"capability": {Image: "x", Ports: []string{"53:53"}, CapAdd: []string{"NET_ADMIN"}},
	} {
		if specHash(base, ContainerNetwork, "", nil) == specHash(changed, ContainerNetwork, "", nil) {
			t.Errorf("spec hash is blind to %s, so adding one would never redeploy", name)
		}
	}
}

// TestContainerEnsureOpCarriesHardening is H1's wire-payload coverage,
// the same shape TestContainerEnsureOpCarriesHostAccess pins for
// devices/capAdd: every hardening field reaches the EnsureContainer op,
// and the spec hash moves for each one so a fleet already running
// without it is replaced once the field is added.
func TestContainerEnsureOpCarriesHardening(t *testing.T) {
	spec := &resource.ContainerSpec{
		Image:           testDigestImage,
		ReadOnlyRootfs:  true,
		CapDrop:         []string{"ALL"},
		NoNewPrivileges: true,
		Resources:       &resource.ContainerResources{Memory: "4g", Cpus: "2", PidsLimit: 512},
		ShmSize:         "1g",
	}
	ec := docker.ensureContainer("hermes", spec, ContainerNetwork, "", nil).GetEnsureContainer()
	if !ec.GetReadOnlyRootfs() {
		t.Error("read_only_rootfs did not reach the op")
	}
	if caps := ec.GetCapDrop(); len(caps) != 1 || caps[0] != "ALL" {
		t.Errorf("cap_drop = %+v", caps)
	}
	if !ec.GetNoNewPrivileges() {
		t.Error("no_new_privileges did not reach the op")
	}
	res := ec.GetResources()
	if res.GetMemory() != "4g" || res.GetCpus() != "2" || res.GetPidsLimit() != 512 {
		t.Errorf("resources = %+v", res)
	}
	if ec.GetShmSize() != "1g" {
		t.Errorf("shm_size = %q, want 1g", ec.GetShmSize())
	}

	base := &resource.ContainerSpec{Image: "x"}
	for name, changed := range map[string]*resource.ContainerSpec{
		"readOnlyRootfs":  {Image: "x", ReadOnlyRootfs: true},
		"capDrop":         {Image: "x", CapDrop: []string{"ALL"}},
		"noNewPrivileges": {Image: "x", NoNewPrivileges: true},
		"resources":       {Image: "x", Resources: &resource.ContainerResources{Memory: "4g"}},
		"shmSize":         {Image: "x", ShmSize: "1g"},
	} {
		if specHash(base, ContainerNetwork, "", nil) == specHash(changed, ContainerNetwork, "", nil) {
			t.Errorf("spec hash is blind to %s, so adding one would never redeploy", name)
		}
	}
}

// TestHardeningSpecIsRefusedOnAnOlderAgent is H1's cross-module refusal
// test, the sibling of TestMinor12SpecIsRefusedOnAnOlderAgent: a plan
// using a hardening field must be refused — not silently under-applied —
// against an agent below protocol minor 18, and must apply cleanly at or
// above it.
func TestHardeningSpecIsRefusedOnAnOlderAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		minor uint32
		known bool
		want  bool // applied
	}{
		{"minor 17 refuses", 17, true, false},
		{"minor 18 applies", 18, true, true},
		{"unknown minor applies", 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startGateway(t, []string{"tok"})
			agent := newTestAgent(t, h, "node-a", "tok")
			if !h.gw.WaitConnected("node-a", 10*time.Second) {
				t.Fatal("agent never connected")
			}
			hermes := containerResource("hermes", testDigestImage)
			hermes.Container.ReadOnlyRootfs = true
			hermes.Container.NoNewPrivileges = true
			r := newReconciler(&GatewayApplier{Gateway: h.gw})
			r.AgentProtocolMinor = func(string) (uint32, bool) { return tc.minor, tc.known }
			outcome, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{hermes})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			_, exists := agent.containerExists("hermes")
			if exists != tc.want {
				t.Fatalf("container exists = %v, want %v (outcome %+v)", exists, tc.want, outcome)
			}
			if tc.want {
				return
			}
			var msg string
			for _, sr := range outcome.Steps {
				if sr.Step.Name == "hermes" {
					msg = sr.Error
				}
			}
			for _, want := range []string{"protocol minor 18", "node-a", "minor 17", "upgrade wardstoned", "readOnlyRootfs"} {
				if !strings.Contains(msg, want) {
					t.Errorf("step error %q does not say %q", msg, want)
				}
			}
			if agent.opCount("EnsureContainer") != 0 || agent.opCount("PullImage") != 0 {
				t.Error("the node was touched before the refusal")
			}
		})
	}
}

func TestRuntimeIdentityIsRefusedOnAnOlderAgent(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	app := containerResource("app", testDigestImage)
	app.Container.RuntimeIdentity = testRuntimeIdentity(1000)
	app.Container.Mounts = append(app.Container.Mounts, resource.Mount{Source: "/run/dbus", Target: "/run/dbus", ReadOnly: true})
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = func(string) (uint32, bool) { return resource.RuntimeIdentityProtocolMinor - 1, true }
	out, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{app})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := agent.containerExists("app"); exists || agent.opCount("EnsureContainer") != 0 || agent.opCount("PullImage") != 0 {
		t.Fatalf("old agent was mutated: %+v", out)
	}
	msg := out.Steps[0].Error
	for _, want := range []string{"protocol minor 34", "runtimeIdentity", "minor 33", "upgrade wardstoned", "image-default user"} {
		if !strings.Contains(msg, want) {
			t.Errorf("step error %q does not say %q", msg, want)
		}
	}
}

// TestMinor12SpecIsRefusedOnAnOlderAgent: the one-minor-skew guarantee
// makes a minor-11 agent a legitimate peer, and it IGNORES the minor-12
// fields rather than refusing them — so "53:53/udp" reaches it as a
// second 53/tcp binding and Docker refuses the create. The reconciler
// must refuse the step first, naming the node, and must not touch the
// node at all; on a minor-12 agent the same spec applies.
func TestMinor12SpecIsRefusedOnAnOlderAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		minor uint32
		known bool
		want  bool // applied
	}{
		{"minor 11 refuses", 11, true, false},
		{"minor 12 applies", 12, true, true},
		{"unknown minor applies", 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startGateway(t, []string{"tok"})
			agent := newTestAgent(t, h, "node-a", "tok")
			if !h.gw.WaitConnected("node-a", 10*time.Second) {
				t.Fatal("agent never connected")
			}
			pihole := containerResource("pihole", testDigestImage)
			pihole.Container.Ports = []string{"53:53/udp", "53:53"}
			r := newReconciler(&GatewayApplier{Gateway: h.gw})
			r.AgentProtocolMinor = func(string) (uint32, bool) { return tc.minor, tc.known }
			outcome, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{pihole})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			_, exists := agent.containerExists("pihole")
			if exists != tc.want {
				t.Fatalf("container exists = %v, want %v (outcome %+v)", exists, tc.want, outcome)
			}
			if tc.want {
				return
			}
			var msg string
			for _, sr := range outcome.Steps {
				if sr.Step.Name == "pihole" {
					msg = sr.Error
				}
			}
			for _, want := range []string{"protocol minor 12", "node-a", "minor 11", "upgrade wardstoned"} {
				if !strings.Contains(msg, want) {
					t.Errorf("step error %q does not say %q", msg, want)
				}
			}
			if agent.opCount("EnsureContainer") != 0 || agent.opCount("PullImage") != 0 {
				t.Error("the node was touched before the refusal")
			}
		})
	}
}
