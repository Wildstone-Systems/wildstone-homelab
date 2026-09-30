package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestContainerEnsureOpCarriesHostNetwork: networkMode: host reaches the
// wire as CONTAINER_NETWORK_MODE_HOST with NO managed network, since a
// container in the host's namespace cannot also join the bridge; a spec
// that omits it is exactly what it was before (UNSPECIFIED, attached to
// ContainerNetwork).
func TestContainerEnsureOpCarriesHostNetwork(t *testing.T) {
	host := &resource.ContainerSpec{Image: testDigestImage, NetworkMode: resource.NetworkModeHost}
	ec := containerEnsureOp("hub", host, containerNetworkFor(host), "", nil).GetEnsureContainer()
	if ec.GetNetworkMode() != wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST {
		t.Errorf("network_mode = %v, want HOST", ec.GetNetworkMode())
	}
	if ec.GetNetwork() != "" {
		t.Errorf("network = %q, want empty: host mode cannot join the managed bridge", ec.GetNetwork())
	}

	bridge := &resource.ContainerSpec{Image: testDigestImage}
	ec = containerEnsureOp("hub", bridge, containerNetworkFor(bridge), "", nil).GetEnsureContainer()
	if ec.GetNetworkMode() != wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_UNSPECIFIED {
		t.Errorf("network_mode = %v, want UNSPECIFIED for a spec that never asked", ec.GetNetworkMode())
	}
	if ec.GetNetwork() != ContainerNetwork {
		t.Errorf("network = %q, want %q", ec.GetNetwork(), ContainerNetwork)
	}
}

// TestHostNetworkLeavesOmittedHashUnchanged is the no-churn property for
// this field, pinned against the same pre-change value
// TestContainerWithNoManagedConfigIsUnaffected uses: routing the network
// through containerNetworkFor must not move a bridge container's hash,
// while switching to host must.
func TestHostNetworkLeavesOmittedHashUnchanged(t *testing.T) {
	spec := &resource.ContainerSpec{Image: "nginx@sha256:abc", Restart: "unless-stopped"}
	const beforeThisChange = "e2d3456039e9a85a"
	if got := specHash(spec, containerNetworkFor(spec), "", nil); got != beforeThisChange {
		t.Errorf("spec hash for a container without networkMode changed to %q (was %q): every container "+
			"in the fleet would be replaced once for no reason", got, beforeThisChange)
	}
	host := *spec
	host.NetworkMode = resource.NetworkModeHost
	if specHash(&host, containerNetworkFor(&host), "", nil) == beforeThisChange {
		t.Error("spec hash is blind to networkMode, so moving to host would never redeploy")
	}
}

// TestContainerMovesBetweenBridgeAndHost is the declarative round trip:
// the same named container goes bridge -> host -> bridge purely by
// editing its spec, and each move is a replacement the node really sees.
func TestContainerMovesBetweenBridgeAndHost(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = func(string) (uint32, bool) { return resource.HostNetworkProtocolMinor, true }
	r.Approve = func(*plan.Plan) bool { return true } // a network move is a Medium-risk update

	for i, step := range []struct {
		mode        string
		wantNetwork string
		wantMode    wsv1.ContainerNetworkMode
	}{
		{"", ContainerNetwork, wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_UNSPECIFIED},
		{resource.NetworkModeHost, "", wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST},
		{"", ContainerNetwork, wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_UNSPECIFIED},
	} {
		hub := containerResource("hub", testDigestImage)
		hub.Container.NetworkMode = step.mode
		outcome, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{hub})
		if err != nil {
			t.Fatalf("step %d: reconcile: %v", i, err)
		}
		for _, sr := range outcome.Steps {
			if sr.Error != "" {
				t.Fatalf("step %d: %s failed: %s", i, sr.Step.Name, sr.Error)
			}
		}
		network, mode, ok := agent.containerNetworking("hub")
		if !ok {
			t.Fatalf("step %d: container missing", i)
		}
		if network != step.wantNetwork || mode != step.wantMode {
			t.Errorf("step %d: container on (%q, %v), want (%q, %v)", i, network, mode, step.wantNetwork, step.wantMode)
		}
		if want := i + 1; agent.opCount("EnsureContainer") != want {
			t.Errorf("step %d: %d EnsureContainer ops, want %d", i, agent.opCount("EnsureContainer"), want)
		}
	}
}

// TestHostNetworkSpecIsRefusedOnAnOlderAgent: a minor-21 agent ignores
// network_mode and would start the container on the bridge with nothing
// published. The reconciler must refuse the step with an upgrade message
// before touching the node, leaving the running bridge container alone.
func TestHostNetworkSpecIsRefusedOnAnOlderAgent(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = func(string) (uint32, bool) { return 21, true }
	r.Approve = func(*plan.Plan) bool { return true }

	hub := containerResource("hub", testDigestImage)
	if _, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{hub}); err != nil {
		t.Fatalf("reconcile bridge: %v", err)
	}
	ensures := agent.opCount("EnsureContainer")

	hub.Container.NetworkMode = resource.NetworkModeHost
	outcome, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{hub})
	if err != nil {
		t.Fatalf("reconcile host: %v", err)
	}
	var msg string
	for _, sr := range outcome.Steps {
		if sr.Step.Name == "hub" {
			msg = sr.Error
		}
	}
	for _, want := range []string{"protocol minor 22", "networkMode: host", "node-a", "minor 21", "upgrade wardstoned"} {
		if !strings.Contains(msg, want) {
			t.Errorf("step error %q does not say %q", msg, want)
		}
	}
	if agent.opCount("EnsureContainer") != ensures || agent.opCount("RemoveContainer") != 0 {
		t.Error("the node was touched before the refusal")
	}
	network, mode, ok := agent.containerNetworking("hub")
	if !ok || network != ContainerNetwork || mode != wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_UNSPECIFIED {
		t.Errorf("old bridge container disturbed: (%q, %v, %v)", network, mode, ok)
	}
}
