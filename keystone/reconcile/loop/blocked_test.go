package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
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

// undeletableApplier lets a container be created but refuses to remove
// it — an agent that will not, or cannot, take it away.
type undeletableApplier struct {
	containerApplier
	name string
}

func (a *undeletableApplier) Apply(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if rc := op.GetRemoveContainer(); rc != nil && rc.GetName() == a.name {
		return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
			Code: wsv1.ErrorCode_ERROR_CODE_RUNTIME, Message: "will not remove",
		}}}, nil
	}
	return a.containerApplier.Apply(ctx, node, op)
}

// A DELETE can be quarantined too, and that resource is desired by
// nobody — so the row-pruning that keeps the Console honest would take
// away the only record that the container is still running. An orphan
// nothing reports is worse than an orphan.
func TestAQuarantinedDeleteStaysVisible(t *testing.T) {
	l, state, db, _ := harness(t)
	l.Rec.Applier = &undeletableApplier{name: "stuck"}
	if err := state.Put(container("stuck", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	if err := state.Put(container("other", "n1", testImage)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}

	// It leaves desired state, and the delete fails every time.
	if err := state.Delete("Container", "stuck"); err != nil {
		t.Fatal(err)
	}
	// A delete is High risk and waits for a human, so each pass runs on an
	// approved plan — which is how a real removal reaches the agent.
	for i := 0; i < 6; i++ {
		id, err := db.InsertPlan("op", "", "remove stuck", "high", `{"x":1}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.TransitionPlan(id, runtimedb.PlanApproved, "op"); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Tick(t.Context()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}

	o := observedFor(t, db, "Container", "stuck")
	if o.Healthy {
		t.Errorf("a container that could not be removed reports healthy: %+v", o)
	}
	if !strings.Contains(o.Detail, "skipped") {
		t.Errorf("the row still shows the raw failure rather than saying the resource has been set "+
			"aside and what to do about it: %q", o.Detail)
	}
	// And the resource it was holding up got there.
	if o := observedFor(t, db, "Container", "other"); !o.Healthy {
		t.Errorf("an unrelated resource never converged behind a stuck delete: %+v", o)
	}
}
