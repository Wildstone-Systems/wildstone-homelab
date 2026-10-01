package reconcile

import (
	"context"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// routeFileAgent is an in-memory managed-file store: enough to write a
// fragment and read it back, which is all a Route delete's gate does.
type routeFileAgent struct{ files map[string][]byte }

func (a *routeFileAgent) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_WriteManagedFile:
		a.files[o.WriteManagedFile.GetPath()] = o.WriteManagedFile.GetContent()
		return &wsv1.OpResult{}, nil
	case *wsv1.Operation_ReadManagedFile:
		return &wsv1.OpResult{Result: &wsv1.OpResult_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFileResult{Content: a.files[o.ReadManagedFile.GetPath()]},
		}}, nil
	}
	return &wsv1.OpResult{}, nil
}

// A Route delete writes a tombstone comment rather than empty bytes
// (F30: an empty import makes Caddy warn once a second forever). The
// gate asserted the file read back EMPTY — the opposite of what the same
// function had just written twenty lines earlier — so it could never
// pass.
//
// The cost was not cosmetic. A failed gate halts the plan, so every
// removal stopped at its Route step and the Container delete after it
// was never attempted: `wild capability disable` reported failure while
// leaving the container running with no route to it, which is where
// orphaned containers came from.
func TestRouteDeletePassesItsGateOnTheTombstoneItWrites(t *testing.T) {
	agent := &routeFileAgent{files: map[string][]byte{}}
	r := &Reconciler{Applier: agent, ManagedPrefix: "/etc/wildstone"}
	path := routeFragmentPath("/etc/wildstone", "pdf")
	agent.files[path] = []byte("https://pdf.example.com {\n\treverse_proxy stirling-pdf:8080\n}\n")

	sr := r.applyRouteStep(context.Background(), "n1",
		plan.PlanStep{Kind: resource.KindRoute, Name: "pdf", Action: plan.ActionDelete},
		map[resKey]resource.Resource{})

	if !sr.Applied {
		t.Fatalf("delete did not apply: %s", sr.Error)
	}
	if !sr.HealthOK {
		t.Fatal("a Route delete must pass its gate on the tombstone it writes, not on empty bytes — " +
			"a failed gate halts the plan and the Container delete after it never runs")
	}
	if len(agent.files[path]) == 0 {
		t.Fatal("the fragment must be left as a comment, not emptied (F30)")
	}
}

type routeFrontDoorAgent struct {
	probeURL string
	tcpCalls int
	httpsOK  bool
}

func (a *routeFrontDoorAgent) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_ProbeTcp:
		a.tcpCalls++
		return &wsv1.OpResult{Result: &wsv1.OpResult_ProbeTcp{
			ProbeTcp: &wsv1.ProbeTcpResult{Reachable: true},
		}}, nil
	case *wsv1.Operation_ProbeHttp:
		a.probeURL = o.ProbeHttp.GetUrl()
		return &wsv1.OpResult{Result: &wsv1.OpResult_ProbeHttp{
			ProbeHttp: &wsv1.ProbeHttpResult{Reachable: a.httpsOK},
		}}, nil
	}
	return &wsv1.OpResult{}, nil
}

func TestTailnetRouteGateRequiresHTTPSNotOnlyAnOpenPort(t *testing.T) {
	agent := &routeFrontDoorAgent{}
	r := &Reconciler{Applier: agent, HealthGateAttempts: 1}
	r.posture = ingressPosture{
		tls: "tailscale", tailnetName: "node.example.ts.net",
		tailnetPorts: map[string]int{"console.wildstone.internal": 47443},
	}
	spec := &resource.RouteSpec{Host: "console.wildstone.internal", Upstream: "100.64.0.1:9441"}
	if r.probeRoute(context.Background(), "n1", spec) {
		t.Fatal("an unrelated TCP listener must not pass the route health gate")
	}
	if agent.tcpCalls != 0 || agent.probeURL != "https://node.example.ts.net:47443/" {
		t.Fatalf("gate used tcp=%d url=%q", agent.tcpCalls, agent.probeURL)
	}
	agent.httpsOK = true
	if !r.probeRoute(context.Background(), "n1", spec) {
		t.Fatal("a reachable HTTPS front door should pass")
	}
}

// urlProbeAgent answers ProbeHttp per URL, so a test can make the front
// door answer while the service behind it does not.
type urlProbeAgent struct {
	up   map[string]bool
	urls []string
}

func (a *urlProbeAgent) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if o, ok := op.GetOp().(*wsv1.Operation_ProbeHttp); ok {
		u := o.ProbeHttp.GetUrl()
		a.urls = append(a.urls, u)
		return &wsv1.OpResult{Result: &wsv1.OpResult_ProbeHttp{
			ProbeHttp: &wsv1.ProbeHttpResult{Reachable: a.up[u], StatusCode: 302},
		}}, nil
	}
	return &wsv1.OpResult{}, nil
}

// TestTailnetHostRouteGateChecksTheHostServiceToo: under the tailnet
// posture the front door answers (302 to the sign-in page, or 502) with
// nothing behind it, so a host/<port> Route must also pass #433's probe
// of the service on the node. The live run (§1.7.6) applied a Route
// change against a stopped host service because this returned early.
func TestTailnetHostRouteGateChecksTheHostServiceToo(t *testing.T) {
	front := "https://node.example.ts.net:10443/"
	service := "http://127.0.0.1:18123/"
	agent := &urlProbeAgent{up: map[string]bool{front: true}}
	r := &Reconciler{Applier: agent, HealthGateAttempts: 1}
	r.posture = ingressPosture{
		tls: "tailscale", tailnetName: "node.example.ts.net",
		tailnetPorts: map[string]int{"hostroute.wildstone.internal": 10443},
	}
	spec := &resource.RouteSpec{Host: "hostroute.wildstone.internal", Upstream: "host/18123"}

	if r.probeRoute(context.Background(), "n1", spec) {
		t.Fatalf("passed with the host service down; probed %q", agent.urls)
	}
	agent.up[service] = true
	agent.urls = nil
	if !r.probeRoute(context.Background(), "n1", spec) {
		t.Fatalf("failed with the front door and the host service both up; probed %q", agent.urls)
	}
	if len(agent.urls) != 2 || agent.urls[0] != front || agent.urls[1] != service {
		t.Errorf("probed %q, want the front door then the host service", agent.urls)
	}

	// A container upstream still stops at the front door: the agent
	// cannot dial a container by name.
	agent.urls = nil
	if !r.probeRoute(context.Background(), "n1", &resource.RouteSpec{Host: "hostroute.wildstone.internal", Upstream: "container/app:80"}) {
		t.Fatal("container upstream with the front door up should pass")
	}
	if len(agent.urls) != 1 {
		t.Errorf("container upstream probed %q, want only the front door", agent.urls)
	}
}
