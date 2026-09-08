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
