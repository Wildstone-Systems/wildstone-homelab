package loop

import (
	"strings"
	"testing"
)

// F60: a plan applies in full or not at all, so a failed step halts it
// and every later step is never attempted. Correct transactionally, and
// with no exit: the failed resource is still desired, so the next tick
// rebuilds the same plan and halts identically — forever — while every
// other resource waits behind it.
//
// It happened. A package mounted a path the agent refuses, and a
// proposal to repair the ingress was swept into the same failing tick
// and reported failed having changed nothing. The way out was a proposal
// DELETING the poisoned resource, which nothing suggests.
//
// After enough identical failures the step is set aside and the rest of
// the plan proceeds.
func TestABrokenResourceStopsBlockingTheOthers(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &haltingApplier{failName: "broken"}

	// "broken" always fails; "fine" is behind it and has never had a
	// chance to converge.
	if err := state.Put(container("broken", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	if err := state.Put(container("fine", "n1", testImage)); err != nil {
		t.Fatal(err)
	}

	// Tick until the quarantine engages. Each tick halts on "broken".
	var lastBlocked int
	for i := 0; i < 6; i++ {
		out, err := l.Tick(t.Context())
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		lastBlocked = len(out.Blocked)
	}
	if lastBlocked == 0 {
		t.Fatal("a step that failed every tick was never set aside; the plan halts on it forever " +
			"and nothing else can converge")
	}

	// The whole point: the healthy resource got there in the end.
	if o := observedFor(t, db, "Container", "fine"); !o.Healthy {
		t.Errorf("a healthy resource never converged because a different one was broken: %+v", o)
	}
	// And the broken one still says so, with something to act on.
	o := observedFor(t, db, "Container", "broken")
	if o.Healthy {
		t.Errorf("the broken resource reported healthy: %+v", o)
	}
	if !strings.Contains(strings.ToLower(o.Detail), "skip") && !strings.Contains(strings.ToLower(o.Detail), "fail") {
		t.Errorf("detail = %q, want it to say the resource is stuck and being skipped", o.Detail)
	}
}

// A transient failure must not quarantine a resource that would have
// succeeded on the next tick — an image pull against a slow registry, a
// node that has just reconnected.
func TestOneFailureDoesNotBlockAResource(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &haltingApplier{failName: "flaky"}
	if err := state.Put(container("flaky", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Blocked) != 0 {
		t.Fatalf("one failure set a resource aside: %+v", out.Blocked)
	}
	// It recovers, and converges as though nothing happened.
	l.Rec.Applier = &containerApplier{}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o := observedFor(t, db, "Container", "flaky"); !o.Healthy {
		t.Errorf("a resource that failed once and then succeeded is not healthy: %+v", o)
	}
}

// Deleting a poisoned resource is how an operator gets it out of desired
// state — it was the only way out of the original incident. A create
// that has been set aside must not set aside the delete as well, or the
// escape hatch is quarantined along with the fault.
func TestABlockedResourceCanStillBeDeleted(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &haltingApplier{failName: "poisoned"}
	if err := state.Put(container("poisoned", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := l.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	// The operator takes it out of desired state.
	l.Rec.Applier = &containerApplier{}
	if err := state.Delete("Container", "poisoned"); err != nil {
		t.Fatal(err)
	}
	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range out.Blocked {
		if b.Name == "poisoned" {
			t.Fatalf("deleting a quarantined resource was itself quarantined: %s", b.Summary())
		}
	}
	obs, err := db.ListObserved()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Kind == "Container" && o.Name == "poisoned" {
			t.Errorf("a deleted resource is still observed: %+v", o)
		}
	}
}

// The other way out: fix the thing. A resource whose desired spec has
// changed since it failed has earned a fresh attempt — nobody should
// have to wait out a quarantine the OLD spec earned.
func TestFixingABlockedResourceRetriesIt(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &haltingApplier{failName: "broken"}
	if err := state.Put(container("broken", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := l.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	// Same name, different spec, and an applier that works now.
	l.Rec.Applier = &containerApplier{}
	fixed := container("broken", "n1", testImage)
	fixed.Container.Env = map[string]string{"FIXED": "1"}
	if err := state.Put(fixed); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o := observedFor(t, db, "Container", "broken"); !o.Healthy {
		t.Errorf("a resource that was fixed is still quarantined: %+v", o)
	}
}

// The status rows are the only durable record of what the reconciler
// manages on a node — the managed set is rebuilt from them on restart
// (F76). Pruning rows for a node the tick never reached would erase that
// record for an offline node, and a container removed from desired while
// its node was down would then never be planned for deletion: it runs
// forever, holding its published ports (F61).
func TestPruningSparesAnOfflineNode(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &containerApplier{}
	// Two resources, so the node still has agent work when one leaves —
	// which is what makes the tick SKIP the node rather than reconcile
	// an empty one.
	if err := state.Put(container("app", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	if err := state.Put(container("other", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	observedFor(t, db, "Container", "app") // the row the restart seeding depends on

	// The node goes offline, and the resource leaves desired state.
	l.Connected = func(string) bool { return false }
	if err := state.Delete("Container", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	obs, err := db.ListObserved()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Kind == "Container" && o.Name == "app" {
			return
		}
	}
	t.Error("the row for a resource on an offline node was pruned; nothing now records that the " +
		"reconciler manages it, so its deletion will never be planned")
}
