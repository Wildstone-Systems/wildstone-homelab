package loop

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// TestStaleDriftProposalIsSuperseded is defect B. A drift proposal speaks
// only for the desired state it was computed from, and approval refuses
// one whose base has moved (F28) — correctly. But the loop's "at most one
// open drift plan" dedupe did not check the base: once a stale proposal
// existed, every later tick returned early and no fresh one was ever
// raised.
//
// The operator was left with exactly one action available, and it was the
// one that could not work: approving returns 409 and supersedes, and only
// then can the next tick propose again. Nothing said so in between. The
// loop now supersedes the stale proposal itself, on the tick that finds
// the drift still there.
func TestStaleDriftProposalIsSuperseded(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &containerApplier{}
	if err := state.Put(container("app", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Drift the container: Medium Update, so a proposal is raised.
	if err := state.Put(container("app", "n1", testImage2)); err != nil {
		t.Fatal(err)
	}
	first, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.DriftProposed == "" {
		t.Fatalf("no drift proposal raised: %+v", first)
	}

	// While it sits unapproved, desired state moves — an unrelated
	// resource is added, which is all it takes for the head to change.
	if err := state.Put(volume("data", "n1")); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("mika", "add a volume", "unrelated change"); err != nil {
		t.Fatal(err)
	}
	second, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.DriftProposed == "" {
		t.Fatalf("stale proposal blocked a fresh one: %+v", second)
	}
	if second.DriftProposed == first.DriftProposed {
		t.Fatalf("same plan id returned twice: %s", first.DriftProposed)
	}

	stale, err := db.GetPlan(first.DriftProposed)
	if err != nil {
		t.Fatal(err)
	}
	if stale.State != runtimedb.PlanSuperseded {
		t.Fatalf("stale drift plan is %s, want superseded", stale.State)
	}
	fresh, err := db.GetPlan(second.DriftProposed)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != runtimedb.PlanProposed {
		t.Fatalf("fresh drift plan is %s, want proposed", fresh.State)
	}
	// Superseding a plan the operator can see is not something to do
	// quietly.
	if types := strings.Join(eventTypes(t, db), ","); !strings.Contains(types, "drift.superseded") {
		t.Fatalf("no drift.superseded event: %v", types)
	}
}

// The dedupe it replaces must still hold: an unchanged Statefile does not
// get a second proposal every tick.
func TestCurrentDriftProposalIsNotDuplicated(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &containerApplier{}
	if err := state.Put(container("app", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := state.Put(container("app", "n1", testImage2)); err != nil {
		t.Fatal(err)
	}
	first, err := l.Tick(t.Context())
	if err != nil || first.DriftProposed == "" {
		t.Fatalf("first drift tick = %+v, %v", first, err)
	}
	for i := 0; i < 3; i++ {
		out, err := l.Tick(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if out.DriftProposed != "" {
			t.Fatalf("tick %d proposed again: %s", i, out.DriftProposed)
		}
	}
	open, err := db.PlansInState(runtimedb.PlanProposed)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("%d open plans, want 1: %+v", len(open), open)
	}
}
