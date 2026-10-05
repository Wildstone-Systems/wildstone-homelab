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

// The host-port preflight (#440): a host-networked container whose package
// routes host/<port> is refused, before the node is touched, when that
// port is already listening on the node.

// hubPackage is a Home-Assistant-shaped package: one container, bridge or
// host, and in host mode a host/18123 Route from the same package.
func hubPackage(hostMode bool, image string) []resource.Resource {
	hub := containerResource("hub", image)
	hub.Metadata.Owner, hub.Metadata.Package = "capability/home-assistant", "home-assistant"
	route := routeResource("hass", "hass.example.internal", "container/hub:18123")
	route.Metadata.Owner, route.Metadata.Package = "capability/home-assistant", "home-assistant"
	if hostMode {
		hub.Container.NetworkMode = resource.NetworkModeHost
		route.Route.Upstream = "host/18123"
	}
	return []resource.Resource{hub, route}
}

func hubStepError(t *testing.T, outcome *Outcome) string {
	t.Helper()
	for _, sr := range outcome.Steps {
		if sr.Step.Kind == resource.KindContainer && sr.Step.Name == "hub" {
			return sr.Error
		}
	}
	t.Fatal("no step for Container/hub")
	return ""
}

func TestHostPortInUseIsRefusedBeforeTheOldContainerIsReplaced(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = minor(resource.HostGatewayProtocolMinor)
	r.Approve = func(*plan.Plan) bool { return true }
	ctx := context.Background()

	// 1.x: on the bridge, nothing on the host's port.
	if _, err := r.Reconcile(ctx, "node-a", hubPackage(false, testDigestImage)); err != nil {
		t.Fatal(err)
	}
	if agent.opCount("ListPorts") != 0 {
		t.Error("a bridge container's step read the node's ports")
	}
	ensures := agent.opCount("EnsureContainer")

	// Something else owns 18123 on the node; the upgrade to host mode is
	// refused and the bridge container keeps running.
	agent.setListening(&wsv1.ListeningPort{Proto: "tcp", Port: 18123, LocalIp: "0.0.0.0"})
	outcome, err := r.Reconcile(ctx, "node-a", hubPackage(true, testDigestImage))
	if err != nil {
		t.Fatal(err)
	}
	msg := hubStepError(t, outcome)
	for _, want := range []string{"networkMode: host", "18123", "Route hass", "0.0.0.0", "node-a", "before the existing container is touched"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not say %q", msg, want)
		}
	}
	if agent.opCount("EnsureContainer") != ensures || agent.opCount("RemoveContainer") != 0 || agent.opCount("StopContainer") != 0 {
		t.Error("the node's container was touched before the refusal")
	}
	if network, mode, ok := agent.containerNetworking("hub"); !ok || network != ContainerNetwork ||
		mode != wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_UNSPECIFIED {
		t.Errorf("old bridge container disturbed: (%q, %v, %v)", network, mode, ok)
	}

	// The port is freed: the same desired state now converges.
	agent.setListening()
	outcome, err = r.Reconcile(ctx, "node-a", hubPackage(true, testDigestImage))
	if err != nil {
		t.Fatal(err)
	}
	if msg := hubStepError(t, outcome); msg != "" {
		t.Fatalf("hub with the port free: %s", msg)
	}
	if _, mode, _ := agent.containerNetworking("hub"); mode != wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST {
		t.Fatalf("hub not moved to host networking: %v", mode)
	}

	// Now the listener on 18123 is the hub itself. Replacing it (a new
	// image) must not be refused over its own port.
	agent.setListening(&wsv1.ListeningPort{Proto: "tcp6", Port: 18123, LocalIp: "::"})
	ensures = agent.opCount("EnsureContainer")
	outcome, err = r.Reconcile(ctx, "node-a", hubPackage(true, strings.ReplaceAll(testDigestImage, "a", "b")))
	if err != nil {
		t.Fatal(err)
	}
	if msg := hubStepError(t, outcome); msg != "" {
		t.Errorf("a host container was refused over its own port: %s", msg)
	}
	if agent.opCount("EnsureContainer") != ensures+1 {
		t.Error("the host container was not replaced")
	}
}

// A stopped host-networked container cannot be the listener, so a port
// something else took while it was down is still refused.
func TestHostPortTakenWhileHostContainerStoppedIsRefused(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = minor(resource.HostGatewayProtocolMinor)
	r.Approve = func(*plan.Plan) bool { return true }
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, "node-a", hubPackage(true, testDigestImage)); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	agent.containers["hub"].running = false
	agent.mu.Unlock()
	agent.setListening(&wsv1.ListeningPort{Proto: "tcp", Port: 18123, LocalIp: "0.0.0.0"})

	outcome, err := r.Reconcile(ctx, "node-a", hubPackage(true, strings.ReplaceAll(testDigestImage, "a", "b")))
	if err != nil {
		t.Fatal(err)
	}
	if msg := hubStepError(t, outcome); !strings.Contains(msg, "18123") {
		t.Errorf("stopped host container over a taken port was not refused: %q", msg)
	}
}

// Ports of host Routes from OTHER packages, or on other nodes, are not
// this container's to claim.
func TestHostRoutePortsAreThePackagesOwn(t *testing.T) {
	desired := hubPackage(true, testDigestImage)
	other := routeResource("other", "other.example.internal", "host/9000")
	other.Metadata.Package = "someone-else"
	elsewhere := routeResource("elsewhere", "elsewhere.example.internal", "host/9001")
	elsewhere.Metadata.Package, elsewhere.Metadata.Node = "home-assistant", "node-b"
	desired = append(desired, other, elsewhere)

	ports, routeFor := hostRoutePorts(desired[0], desiredMap(desired))
	if len(ports) != 1 || ports[0] != 18123 || routeFor[18123] != "hass" {
		t.Errorf("ports = %v (%v), want [18123] from hass", ports, routeFor)
	}

	// A container with no package (hand-written desired state) has no
	// Route pairing to go on, and is not checked.
	loose := desired[0]
	loose.Metadata.Package = ""
	if ports, _ := hostRoutePorts(loose, desiredMap(desired)); len(ports) != 0 {
		t.Errorf("an unpackaged container claimed %v", ports)
	}
}
