package api

import (
	"context"
	"net/http"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"

	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile/loop"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

type driftApplier struct{}

func (driftApplier) Apply(_ context.Context, _ string, _ *wsv1.Operation) (*wsv1.OpResult, error) {
	return &wsv1.OpResult{}, nil
}

// seedDrift establishes a managed volume, then removes it from desired so
// the next tick sees High-risk drift. Each change is committed: Put and
// Delete only stage, and a proposal computed against an uncommitted tree
// would record a head that never described it.
func seedDrift(t *testing.T, h *harness, l *loop.Loop) {
	t.Helper()
	if err := h.s.State.Put(driftVolume("data")); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "seed", "create the volume"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Delete(resource.KindVolume, "data"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "drift", "remove it from desired"); err != nil {
		t.Fatal(err)
	}
}

func driftVolume(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindVolume,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: "n1"},
		Volume:     &resource.VolumeSpec{Name: name},
	}
}

// TestDriftPlanIsApprovableThroughTheAPI is the F28 regression, and it
// lives here rather than in the loop package on purpose: the loop's own
// drift test approves with db.TransitionPlan, which walks straight past
// the staleness gate in approvePlan. That gate is where the bug was, so a
// test that never issues POST /plans/{id}/approve cannot see it.
//
// The loop wrote its drift proposal as an untyped map with no
// basedOnHead. It unmarshalled to "", the gate compares
// head != stored.BasedOnHead, and no Statefile head is ever "", so every
// approval 409'd — for as long as the drift lasted, on a Statefile that
// had not moved. A drifted Route stayed down with no supported way back.
func TestDriftPlanIsApprovableThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	l := &loop.Loop{
		State: h.s.State,
		DB:    h.s.DB,
		Rec:   &reconcile.Reconciler{Applier: driftApplier{}, ManagedPrefix: "/etc/wildstone"},
	}

	// Establish the volume so the engine manages it, then remove it from
	// desired: the diff becomes a High-risk delete a drift tick refuses to
	// auto-apply, which is the surfaced-for-approval path.
	seedDrift(t, h, l)
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatalf("drift tick: %v", err)
	}
	if out.DriftProposed == "" {
		t.Fatalf("no drift plan proposed: %+v", out)
	}

	// The head has not moved since the proposal, so approval must be
	// accepted. Before the fix this was 409 every time, forever.
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+out.DriftProposed+"/approve", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve drift plan = %d, want 200 (the F28 conflict is back)", resp.StatusCode)
	}
	rec, err := h.s.DB.GetPlan(out.DriftProposed)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State == runtimedb.PlanSuperseded {
		t.Fatalf("drift plan superseded on an unmoved statefile: %+v", rec)
	}
}

// TestDriftPlanSupersededWhenStatefileActuallyMoves keeps the fix honest.
// Recording the head is only correct if a head that really moved still
// supersedes — otherwise F28 would have been "fixed" by defeating the
// gate, which exists so a stale diff and its risk grades cannot be
// approved after the desired state changed underneath them.
func TestDriftPlanSupersededWhenStatefileActuallyMoves(t *testing.T) {
	h := newHarness(t)
	l := &loop.Loop{
		State: h.s.State,
		DB:    h.s.DB,
		Rec:   &reconcile.Reconciler{Applier: driftApplier{}, ManagedPrefix: "/etc/wildstone"},
	}
	seedDrift(t, h, l)
	out, err := l.Tick(t.Context())
	if err != nil || out.DriftProposed == "" {
		t.Fatalf("drift tick: %+v, %v", out, err)
	}
	// Move the Statefile after the proposal was computed. Put only
	// stages; the head does not move until Commit, so both are needed
	// for this to be a real move rather than a no-op.
	if err := h.s.State.Put(driftVolume("unrelated")); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "unrelated", "move the head"); err != nil {
		t.Fatal(err)
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+out.DriftProposed+"/approve", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("approve after the statefile moved = %d, want 409", resp.StatusCode)
	}
	rec, _ := h.s.DB.GetPlan(out.DriftProposed)
	if rec.State != runtimedb.PlanSuperseded {
		t.Fatalf("plan state = %s, want superseded", rec.State)
	}
}
