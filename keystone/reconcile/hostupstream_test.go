package reconcile

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Route upstreams on the node host (#433): "host/<port>" rendered to the
// host-gateway alias, which only the proxy and the dashboard are given.

func minor(n uint32) func(string) (uint32, bool) {
	return func(string) (uint32, bool) { return n, true }
}

// hostRouteDesired is a node with an ingress proxy, a dashboard, an
// unrelated app, a host-networked hub and, when upstream is non-empty, a
// Route to it.
func hostRouteDesired(upstream string) []resource.Resource {
	owned := func(r resource.Resource, owner string) resource.Resource {
		r.Metadata.Owner = owner
		return r
	}
	hub := containerResource("hub", testDigestImage)
	hub.Container.NetworkMode = resource.NetworkModeHost
	out := []resource.Resource{
		{
			ApiVersion: resource.ApiVersion, Kind: resource.KindIngress,
			Metadata: resource.Metadata{Name: "ingress", Owner: "capability/ingress", Node: "node-a"},
			Ingress:  &resource.IngressSpec{},
		},
		owned(containerResource("caddy", testDigestImage), "capability/ingress"),
		owned(containerResource("homepage", testDigestImage), "capability/dashboard"),
		owned(containerResource("app", testDigestImage), "capability/photos"),
		owned(hub, "capability/home-automation"),
	}
	if upstream != "" {
		out = append(out, routeResource("home", "home.example.internal", upstream))
	}
	return out
}

func desiredMap(desired []resource.Resource) map[resKey]resource.Resource {
	m := map[resKey]resource.Resource{}
	for _, r := range desired {
		m[resKey{r.Kind, r.Metadata.Name}] = r
	}
	return m
}

// TestHostGatewayAliasGoesOnlyToTheProxyAndTheDashboard: the two
// containers that dial Route upstreams get it; an app does not, and nor
// does the host-networked container itself.
func TestHostGatewayAliasGoesOnlyToTheProxyAndTheDashboard(t *testing.T) {
	r := &Reconciler{AgentProtocolMinor: minor(resource.HostGatewayProtocolMinor)}
	desired := desiredMap(hostRouteDesired("host/8123"))
	node := []string{"node.tail0.ts.net"}
	for name, want := range map[string][]string{
		"caddy":    {"node.tail0.ts.net", resource.HostGatewayAlias},
		"homepage": {"node.tail0.ts.net", resource.HostGatewayAlias},
		"app":      node,
		"hub":      node,
	} {
		if got := r.containerHostAliases("node-a", name, node, desired); !slices.Equal(got, want) {
			t.Errorf("%s: aliases %v, want %v", name, got, want)
		}
	}
	if !slices.Equal(node, []string{"node.tail0.ts.net"}) {
		t.Errorf("the node's alias slice was modified: %v", node)
	}
}

// TestHostGatewayAliasIsWithheldWhenItCannotHelp: with no host Route, or
// an agent that cannot map the name (known-old or unknown), the proxy's
// aliases — and so its hash — are exactly what they were before #433.
func TestHostGatewayAliasIsWithheldWhenItCannotHelp(t *testing.T) {
	cases := map[string]struct {
		r        *Reconciler
		upstream string
	}{
		"no host route":   {&Reconciler{AgentProtocolMinor: minor(resource.HostGatewayProtocolMinor)}, "container/app:80"},
		"minor 23 agent":  {&Reconciler{AgentProtocolMinor: minor(resource.HostGatewayProtocolMinor - 1)}, "host/8123"},
		"unknown agent":   {&Reconciler{AgentProtocolMinor: func(string) (uint32, bool) { return 0, false }}, "host/8123"},
		"no reporter":     {&Reconciler{}, "host/8123"},
		"malformed route": {&Reconciler{AgentProtocolMinor: minor(resource.HostGatewayProtocolMinor)}, "host/0"},
	}
	for name, c := range cases {
		if got := c.r.containerHostAliases("node-a", "caddy", nil, desiredMap(hostRouteDesired(c.upstream))); len(got) != 0 {
			t.Errorf("%s: want no aliases, got %v", name, got)
		}
	}
	// A host Route on ANOTHER node does not concern this node's proxy.
	desired := hostRouteDesired("host/8123")
	desired[len(desired)-1].Metadata.Node = "node-b"
	r := &Reconciler{AgentProtocolMinor: minor(resource.HostGatewayProtocolMinor)}
	if got := r.containerHostAliases("node-a", "caddy", nil, desiredMap(desired)); len(got) != 0 {
		t.Errorf("another node's host Route gave this proxy the alias: %v", got)
	}
}

// startLoopbackService is a "service on the node host": the test agent
// runs in this process, so its loopback is the host's, exactly as the
// real agent's is. Returns the port.
func startLoopbackService(t *testing.T) string {
	t.Helper()
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(svc.Close)
	_, port, err := net.SplitHostPort(svc.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestHostRouteConvergesEndToEnd: the proxy is created with the alias,
// the fragment dials the alias, the gate probes the host service where
// it actually listens, and a second tick has nothing to do.
func TestHostRouteConvergesEndToEnd(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	port := startLoopbackService(t)
	desired := hostRouteDesired("host/" + port)

	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = minor(resource.HostGatewayProtocolMinor)
	r.Approve = func(*plan.Plan) bool { return true }

	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, sr := range outcome.Steps {
		if sr.Error != "" || !sr.HealthOK {
			t.Fatalf("%s/%s: error=%q healthy=%v", sr.Step.Kind, sr.Step.Name, sr.Error, sr.HealthOK)
		}
	}
	if aliases, _ := agent.containerHostAliases("caddy"); !slices.Contains(aliases, resource.HostGatewayAlias) {
		t.Errorf("proxy created without the host-gateway alias: %v", aliases)
	}
	if aliases, _ := agent.containerHostAliases("app"); slices.Contains(aliases, resource.HostGatewayAlias) {
		t.Errorf("an app that dials no Route was given the alias: %v", aliases)
	}
	frag := string(agent.readFragmentForTest(t, "home"))
	if !strings.Contains(frag, "\treverse_proxy "+resource.HostGatewayAlias+":"+port+"\n") {
		t.Errorf("fragment does not dial the alias:\n%s", frag)
	}

	ensures, writes := agent.opCount("EnsureContainer"), agent.opCount("WriteManagedFile")
	second, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(second.Plan.Steps) != 0 {
		t.Errorf("did not converge: second tick planned %+v", second.Plan.Steps)
	}
	if agent.opCount("EnsureContainer") != ensures || agent.opCount("WriteManagedFile") != writes {
		t.Error("second tick touched the node")
	}
}

// TestHostRouteGateFailsWhenNothingListens: the probe is a real check of
// the host service, not a pass-by-construction.
func TestHostRouteGateFailsWhenNothingListens(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	addr, err := unreachableAddr()
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(addr)
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = minor(resource.HostGatewayProtocolMinor)
	if r.probeRoute(context.Background(), "node-a", &resource.RouteSpec{Upstream: "host/" + port}) {
		t.Fatal("a host Route passed its gate with nothing listening on the host")
	}
}

// TestHostRouteIsRefusedOnAnOlderAgent: an agent that cannot map the
// alias would leave the proxy with no route to the host, so the Route is
// refused with an upgrade message and no fragment is written.
func TestHostRouteIsRefusedOnAnOlderAgent(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	port := startLoopbackService(t)
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = minor(resource.HostGatewayProtocolMinor - 1)
	r.Approve = func(*plan.Plan) bool { return true }

	outcome, err := r.Reconcile(context.Background(), "node-a", hostRouteDesired("host/"+port))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var msg string
	for _, sr := range outcome.Steps {
		if sr.Step.Kind == resource.KindRoute && sr.Step.Name == "home" {
			msg = sr.Error
		}
	}
	for _, want := range []string{"protocol minor 24", "node-a", "minor 23", "upgrade wardstoned"} {
		if !strings.Contains(msg, want) {
			t.Errorf("route error %q does not say %q", msg, want)
		}
	}
	if frag := agent.readFragmentForTest(t, "home"); len(frag) != 0 {
		t.Errorf("fragment written despite the refusal:\n%s", frag)
	}
	if aliases, _ := agent.containerHostAliases("caddy"); slices.Contains(aliases, resource.HostGatewayAlias) {
		t.Errorf("an older agent was sent the alias: %v", aliases)
	}
}

// TestNoHostRouteLeavesTheProxyHashUnchanged is the no-churn property:
// on an agent that CAN map the alias, a household with no host Route
// dispatches the proxy exactly as before #433.
func TestNoHostRouteLeavesTheProxyHashUnchanged(t *testing.T) {
	desired := desiredMap(hostRouteDesired(""))
	spec := desired[resKey{resource.KindContainer, "caddy"}].Container
	r := &Reconciler{AgentProtocolMinor: minor(resource.HostGatewayProtocolMinor)}
	before := specHash(spec, ContainerNetwork, "", nil)
	if got := specHash(spec, ContainerNetwork, "", r.containerHostAliases("node-a", "caddy", nil, desired)); got != before {
		t.Errorf("proxy hash moved with no host Route: %s -> %s", before, got)
	}
}
