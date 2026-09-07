package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
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
	got := containerEnsureOp("caddy-private", spec, ContainerNetwork, "").GetEnsureContainer().GetPorts()
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
	if specHash(wide, ContainerNetwork, "") == specHash(narrow, ContainerNetwork, "") {
		t.Error("spec hash is blind to the bind address, so narrowing it would never redeploy")
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
	ec := containerEnsureOp("pihole", spec, ContainerNetwork, "").GetEnsureContainer()
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
		if specHash(base, ContainerNetwork, "") == specHash(changed, ContainerNetwork, "") {
			t.Errorf("spec hash is blind to %s, so adding one would never redeploy", name)
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
