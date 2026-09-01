package reconcile

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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
