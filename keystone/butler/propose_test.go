package butler

import (
	"context"
	"testing"
)

func TestProposeChange_ProducesPlan_NeverApplies(t *testing.T) {
	store := newTestStore(t)
	gw := newTestToolGateway(store, nil)

	out, err := gw.Dispatch(context.Background(), "propose_change", map[string]any{
		"kind":  "Route",
		"node":  "node-a",
		"name":  "grafana",
		"owner": "capability/grafana",
		"spec": map[string]any{
			"host":           "grafana.example.com",
			"upstream":       "container/grafana:3000",
			"auth":           "authelia_oidc",
			"classification": "family_public",
		},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if out.Rejected {
		t.Fatalf("valid, policy-compliant proposal was rejected: %q", out.Content)
	}
	if out.Plan == nil {
		t.Fatalf("propose_change did not produce a Plan")
	}
	if len(out.Plan.Steps) != 1 {
		t.Fatalf("propose_change plan has %d steps, want 1: %+v", len(out.Plan.Steps), out.Plan.Steps)
	}
	if !contains(out.Content, "NOT been applied") {
		t.Fatalf("propose_change result did not disclaim application: %q", out.Content)
	}

	// The structural assertion: propose_change must not have written
	// anything to the Statefile — its only output is the Plan value.
	refs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("propose_change wrote to the Statefile (it must never apply): %+v", refs)
	}
}

func TestProposeChange_InvalidSpec_RejectedNotError(t *testing.T) {
	store := newTestStore(t)
	gw := newTestToolGateway(store, nil)

	// A Route with no classification fails resource.Validate — propose_change
	// must surface this as a textual rejection the model can read, not a Go
	// error that aborts the tool gateway.
	out, err := gw.Dispatch(context.Background(), "propose_change", map[string]any{
		"kind":  "Route",
		"node":  "node-a",
		"name":  "bad",
		"owner": "capability/x",
		"spec": map[string]any{
			"host":     "bad.example.com",
			"upstream": "container/bad:80",
			"auth":     "none",
		},
	})
	if err != nil {
		t.Fatalf("Dispatch returned a Go error for a model-supplied invalid spec: %v", err)
	}
	if !contains(out.Content, "invalid spec") {
		t.Fatalf("expected an invalid-spec rejection, got: %q", out.Content)
	}
	if out.Plan != nil {
		t.Fatalf("invalid spec should not produce a Plan")
	}
}
