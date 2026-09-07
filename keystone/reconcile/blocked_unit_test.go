package reconcile

import (
	"fmt"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func step(a plan.Action, name string) plan.PlanStep {
	return plan.PlanStep{Action: a, Kind: resource.KindContainer, Name: name}
}

// Removing a resource from desired state is how an operator gets a
// poisoned one out — it was the only way out of the F60 incident. So a
// quarantine earned by a create or an update must never set aside the
// DELETE of the same resource, or the escape hatch is quarantined along
// with the fault.
func TestQuarantineIsScopedToTheActionThatFailed(t *testing.T) {
	r := &Reconciler{}
	poisoned := containerResource("poisoned", testDigestImage)
	failing := step(plan.ActionCreate, "poisoned")
	for i := 0; i < blockAfter; i++ {
		r.noteStepFailure("node-a", failing, fingerprintOf(poisoned), "mount outside managed prefix")
	}

	// The resource has left desired state, so the plan now carries its
	// delete.
	blocking := r.blockedSteps("node-a", map[resKey]resource.Resource{})
	p := &plan.Plan{Steps: []plan.PlanStep{step(plan.ActionDelete, "poisoned")}}
	out, skipped := withoutBlocked(p, blocking)
	if len(skipped) != 0 {
		t.Errorf("deleting a quarantined resource was itself quarantined: %+v", skipped)
	}
	if len(out.Steps) != 1 {
		t.Errorf("the delete was dropped from the plan: %+v", out.Steps)
	}

	// The action that actually failed is still set aside.
	back := map[resKey]resource.Resource{
		{resource.KindContainer, "poisoned"}: poisoned,
	}
	blocking = r.blockedSteps("node-a", back)
	if _, skipped = withoutBlocked(&plan.Plan{Steps: []plan.PlanStep{failing}}, blocking); len(skipped) != 1 {
		t.Errorf("the failing create was not set aside: %+v", skipped)
	}
}

// A quarantine record belongs to one node. Two nodes running the same
// package hit the same error for their own reasons, and one being stuck
// must not stop the other from trying.
func TestQuarantineDoesNotCrossNodes(t *testing.T) {
	r := &Reconciler{}
	s := step(plan.ActionCreate, "app")
	for i := 0; i < blockAfter; i++ {
		r.noteStepFailure("node-a", s, "spec-v1", "boom")
	}
	if got := r.blockedSteps("node-b", map[resKey]resource.Resource{}); len(got) != 0 {
		t.Errorf("a failure on node-a quarantined node-b: %+v", got)
	}
}

// A step that succeeds is not a failing step any more, whatever it did
// before — otherwise a resource that recovered on its own would be
// quarantined by the next unrelated failure sooner than it should be.
func TestSuccessForgetsEarlierFailures(t *testing.T) {
	r := &Reconciler{}
	s := step(plan.ActionCreate, "app")
	r.noteStepFailure("node-a", s, "spec-v1", "boom")
	r.noteStepFailure("node-a", s, "spec-v1", "boom")
	r.clearStepFailure("node-a", s)
	if r.noteStepFailure("node-a", s, "spec-v1", "boom") {
		t.Error("one failure after a success was enough to quarantine; the count was never reset")
	}
}

// A different error is a different situation. Quarantine is for a
// failure that repeats identically, not for a resource that is failing
// in a new way each time — that one is still telling you something.
func TestADifferentErrorStartsTheCountAgain(t *testing.T) {
	r := &Reconciler{}
	s := step(plan.ActionCreate, "app")
	r.noteStepFailure("node-a", s, "spec-v1", "no such image")
	r.noteStepFailure("node-a", s, "spec-v1", "no such image")
	if r.noteStepFailure("node-a", s, "spec-v1", "port already allocated") {
		t.Error("a run of different errors was quarantined as though it were the same failure repeating")
	}
}

// The way OUT of a capability awaiting a secret is to remove it, and a
// removal is a delete. Blocking deletes would make the trap inescapable
// — which is the corner ADR-W028 exists to open up, so it has a test of
// its own rather than resting on a comment.
func TestAwaitingSecretsNeverBlocksADelete(t *testing.T) {
	owner := "capability/chat-telegram-family"
	desired := map[resKey]resource.Resource{
		{resource.KindContainer, "keep-me"}: {
			Kind: resource.KindContainer, Metadata: resource.Metadata{Name: "keep-me", Owner: owner},
		},
	}
	p := &plan.Plan{Steps: []plan.PlanStep{
		// A create for the blocked capability: set aside.
		{Kind: resource.KindContainer, Name: "keep-me", Action: plan.ActionCreate},
		// A delete of something it owns, no longer in desired: must run.
		{Kind: resource.KindContainer, Name: "chat-telegram-family", Action: plan.ActionDelete},
		// A create for a capability that is fine: must run.
		{Kind: resource.KindContainer, Name: "open-webui", Action: plan.ActionCreate},
	}}
	awaiting := map[string]string{owner: "1 secret(s) must be supplied"}

	out, skipped := withoutAwaitingSecrets(p, desired, awaiting)
	var kept []string
	for _, st := range out.Steps {
		kept = append(kept, fmt.Sprintf("%v %s", st.Action, st.Name))
	}
	if len(out.Steps) != 2 {
		t.Fatalf("kept %v, want the delete and the unrelated create", kept)
	}
	for _, want := range []string{"chat-telegram-family", "open-webui"} {
		var found bool
		for _, st := range out.Steps {
			if st.Name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was removed from the plan; kept %v", want, kept)
		}
	}
	if len(skipped) != 1 || skipped[0].Name != "keep-me" {
		t.Fatalf("skipped = %+v, want only the blocked capability's create", skipped)
	}
}
