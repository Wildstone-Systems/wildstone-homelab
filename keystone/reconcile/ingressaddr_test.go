package reconcile

// The identity proxy's address is observed, and the failures all keep
// the previous belief rather than revoking it.
//
// Every test here is about a lockout. The value gates the household
// review page, the household portal and the Console's own pages, so a
// wrong answer does not degrade a feature — it takes the control plane
// away, with the proxy still telling people they are signed in and
// nothing anywhere reporting a fault.

import (
	"context"
	"errors"
	"testing"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// inspectAgent answers InspectContainer from a table, and can fail.
type inspectAgent struct {
	ips  map[string][]string
	fail map[string]bool
	// asked records the containers inspected, so a test can prove the
	// proxy was identified rather than everything being swept up.
	asked []string
}

func (a *inspectAgent) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	ic := op.GetInspectContainer()
	if ic == nil {
		return &wsv1.OpResult{}, nil
	}
	a.asked = append(a.asked, ic.GetName())
	if a.fail[ic.GetName()] {
		return nil, errors.New("node not connected")
	}
	return &wsv1.OpResult{Result: &wsv1.OpResult_InspectContainer{
		InspectContainer: &wsv1.InspectContainerResult{IpAddresses: a.ips[ic.GetName()]},
	}}, nil
}

// ingressDesired is a node running the ingress capability plus an
// unrelated one, which is the shape that matters: the proxy has to be
// picked out from among other containers.
func ingressDesired() []resource.Resource {
	return []resource.Resource{
		{Kind: resource.KindIngress, Metadata: resource.Metadata{Name: "ingress", Owner: "capability/ingress"}},
		{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "caddy-tailnet-ts", Owner: "capability/ingress"}},
		{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "jellyfin", Owner: "capability/media"}},
	}
}

// The proxy is found by owner, and nothing else is.
//
// By owner rather than by name because four packages provide this
// capability under four container names. A name list here would be a
// fifth place to update when a profile is added, and forgetting would
// fail silently — the address would simply never refresh, which is the
// stale-value lockout arriving by a different road.
func TestTheProxyContainerIsFoundByOwnerNotByName(t *testing.T) {
	got := ingressContainerNames(ingressDesired())
	if len(got) != 1 || got[0] != "caddy-tailnet-ts" {
		t.Fatalf("ingress containers = %v, want just the ingress capability's", got)
	}
	// A node with no ingress has no proxy, and must be indistinguishable
	// from one — this is the ordinary state of a household without a
	// tailnet, not a degraded one.
	if got := ingressContainerNames([]resource.Resource{
		{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "jellyfin", Owner: "capability/media"}},
	}); len(got) != 0 {
		t.Errorf("a node with no Ingress resource reported proxy containers: %v", got)
	}
}

func TestTheProxyAddressIsObservedAndReported(t *testing.T) {
	agent := &inspectAgent{ips: map[string][]string{"caddy-tailnet-ts": {"172.18.0.4"}}}
	var got []string
	var gotNode string
	r := &Reconciler{Applier: agent, NoteIngressAddrs: func(node string, a []string) {
		gotNode, got = node, a
	}}

	r.refreshIngressAddrs(context.Background(), "n1", ingressDesired())

	if gotNode != "n1" || len(got) != 1 || got[0] != "172.18.0.4" {
		t.Fatalf("observed %q %v, want n1 [172.18.0.4]", gotNode, got)
	}
	if len(agent.asked) != 1 || agent.asked[0] != "caddy-tailnet-ts" {
		t.Errorf("inspected %v, want only the proxy — inspecting every container "+
			"would trust whatever address an unrelated one happens to have", agent.asked)
	}
}

// Every failure keeps the previous belief. This is the whole safety
// argument, and each case is one a live node reaches normally.
func TestAFailedObservationNeverRevokesTheProxy(t *testing.T) {
	for _, c := range []struct {
		what    string
		agent   *inspectAgent
		desired []resource.Resource
	}{
		{
			// The node is not connected, or the container has not been
			// created yet — the state of every node during its first
			// install, and of any node that restarts.
			what:    "the inspect fails",
			agent:   &inspectAgent{fail: map[string]bool{"caddy-tailnet-ts": true}},
			desired: ingressDesired(),
		},
		{
			// An agent older than protocol minor 17 leaves the field
			// empty. Empty is "unknown", never "the proxy has no
			// address": reading it the other way would revoke a working
			// trust boundary on contact with an older node.
			what:    "the agent reports no addresses",
			agent:   &inspectAgent{ips: map[string][]string{"caddy-tailnet-ts": nil}},
			desired: ingressDesired(),
		},
		{
			// No ingress at all. A household with a pinned address and
			// no proxy on this node must not have it cleared by the
			// tick that notices.
			what:    "the node has no ingress",
			agent:   &inspectAgent{},
			desired: []resource.Resource{{Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "jellyfin", Owner: "capability/media"}}},
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			called := false
			r := &Reconciler{Applier: c.agent, NoteIngressAddrs: func(string, []string) { called = true }}
			r.refreshIngressAddrs(context.Background(), "n1", c.desired)
			if called {
				t.Error("reported an address when " + c.what +
					"; the previous value must stand, or the household loses " +
					"the review page, the portal and the Console at once")
			}
		})
	}
}

// A nil seam observes nothing at all, so every existing caller is
// untouched and no inspect is dispatched on their behalf.
func TestNoSeamMeansNoObservation(t *testing.T) {
	agent := &inspectAgent{ips: map[string][]string{"caddy-tailnet-ts": {"172.18.0.4"}}}
	r := &Reconciler{Applier: agent}
	r.refreshIngressAddrs(context.Background(), "n1", ingressDesired())
	if len(agent.asked) != 0 {
		t.Errorf("dispatched %v with no seam configured", agent.asked)
	}
}
