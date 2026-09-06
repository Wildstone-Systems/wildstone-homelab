package reconcile

// Quarantining a step that cannot succeed (ADR-W008 A5, F60).
//
// A plan applies in full or not at all (ADR-W004 A3), so a failed step
// halts the plan and every later step is never attempted. That is the
// right transactional behaviour and it has a failure mode with no exit:
// the failed resource is still in desired state, so the next tick
// rebuilds the same plan, halts on the same step, and does so forever —
// while every OTHER resource in that plan waits behind it.
//
// It happened. A package mounted a path the agent refuses; the create
// was rejected; and a proposal to repair the ingress was swept into the
// same failing tick and reported failed having changed nothing. The only
// way out was a proposal DELETING the poisoned resource, which nothing
// in the system suggests and which took reading the statefile on disk to
// work out.
//
// So a step that has failed the same way repeatedly is set aside. The
// rest of the plan proceeds, the operator is told which resource is
// stuck and why, and the control plane keeps working on everything else
// — which is the difference between one broken resource and a homelab
// that cannot repair itself.

import (
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// blockAfter is how many identical consecutive failures set a step
// aside.
//
// Three rather than one: a transient failure — an image pull against a
// slow registry, a node that has just reconnected — must not quarantine
// a resource that would have succeeded on the next tick. Three identical
// errors is not a blip.
const blockAfter = 3

type blockInfo struct {
	failures int
	lastErr  string
	// spec is the desired resource that failed, so a CHANGED spec gets a
	// fresh attempt. An operator who fixes the package and re-installs
	// should not have to wait out a quarantine earned by the old one.
	spec string
	// action is what failed. Only that action is set aside: a create
	// that cannot succeed must NEVER block a delete of the same
	// resource, because deleting it is how an operator gets a poisoned
	// resource out of desired state — the one escape hatch the original
	// incident had.
	action plan.Action
}

// blockedStep is a quarantined action and the error that earned it.
type blockedStep struct {
	action plan.Action
	reason string
}

// noteStepFailure records a step that did not apply, and reports whether
// it has now failed enough times to be set aside.
func (r *Reconciler) noteStepFailure(nodeID string, step plan.PlanStep, spec, errText string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.blocked == nil {
		r.blocked = map[string]map[resKey]*blockInfo{}
	}
	m := r.blocked[nodeID]
	if m == nil {
		m = map[resKey]*blockInfo{}
		r.blocked[nodeID] = m
	}
	k := resKey{step.Kind, step.Name}
	b := m[k]
	// A different error, or a different spec, starts the count again:
	// this is not the same failure repeating, it is a new situation.
	if b == nil || b.lastErr != errText || b.spec != spec {
		b = &blockInfo{}
		m[k] = b
	}
	b.failures++
	b.lastErr, b.spec, b.action = errText, spec, step.Action
	return b.failures >= blockAfter
}

// clearStepFailure forgets a resource's failures once it applies.
func (r *Reconciler) clearStepFailure(nodeID string, step plan.PlanStep) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.blocked[nodeID]; m != nil {
		delete(m, resKey{step.Kind, step.Name})
	}
}

// forgetUndesired drops quarantine records for resources that have left
// desired state. A resource the operator removes and later re-adds must
// get a real attempt, not a sentence carried over from its last life.
// Records for a failing DELETE are kept: see below.
func (r *Reconciler) forgetUndesired(nodeID string, desiredByKey map[resKey]resource.Resource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, b := range r.blocked[nodeID] {
		if _, ok := desiredByKey[k]; ok {
			continue
		}
		// A quarantined DELETE is about a resource that is SUPPOSED to be
		// undesired — that is the whole situation. Forgetting it here
		// reset the count on every tick, so a delete the agent refuses
		// could never be set aside and went on halting the pass forever,
		// which is F60 wearing a different hat.
		if b.action == plan.ActionDelete {
			continue
		}
		delete(r.blocked[nodeID], k)
	}
}

// blockedSteps returns the keys currently set aside on a node, with the
// error that got them there.
func (r *Reconciler) blockedSteps(nodeID string, desiredByKey map[resKey]resource.Resource) map[resKey]blockedStep {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[resKey]blockedStep{}
	for k, b := range r.blocked[nodeID] {
		if b.failures < blockAfter || b.spec == "" {
			continue
		}
		// A resource whose desired spec has changed since it failed is
		// no longer blocked: the operator has done something, and the
		// new spec deserves an attempt.
		if want, ok := desiredByKey[k]; ok && fingerprintOf(want) != b.spec {
			continue
		}
		out[k] = blockedStep{action: b.action, reason: b.lastErr}
	}
	return out
}

// withoutBlocked removes quarantined steps from a plan, and reports what
// was removed so the operator can be told rather than left wondering why
// a resource never converges.
func withoutBlocked(p *plan.Plan, blocked map[resKey]blockedStep) (*plan.Plan, []BlockedResource) {
	if len(blocked) == 0 {
		return p, nil
	}
	var kept []plan.PlanStep
	var skipped []BlockedResource
	for _, st := range p.Steps {
		if b, isBlocked := blocked[resKey{st.Kind, st.Name}]; isBlocked && b.action == st.Action {
			skipped = append(skipped, BlockedResource{
				Kind: st.Kind, Name: st.Name, Action: st.Action, Reason: b.reason,
			})
			continue
		}
		kept = append(kept, st)
	}
	if len(skipped) == 0 {
		return p, nil
	}
	// Rebuilt through plan.Diff's own constructor rather than mutated in
	// place, so OverallRisk is recomputed by the code that owns that
	// rule instead of a second copy of it here.
	out := plan.WithSteps(*p, kept)
	return &out, skipped
}

// BlockedResource is a step set aside after repeated identical failure.
type BlockedResource struct {
	Kind   resource.Kind
	Name   string
	Action plan.Action
	Reason string
}

// Summary is what an operator needs: which resource, and what to do.
func (b BlockedResource) Summary() string {
	return fmt.Sprintf("%s %s/%s has failed %d times and is being skipped so other resources can converge: %s "+
		"— fix the cause, or remove it from desired state",
		b.Action, b.Kind, b.Name, blockAfter, b.Reason)
}

// absentSpec is the fingerprint of a step whose resource is not in
// desired state — a delete. It is stable, which is what the quarantine
// needs: a delete the agent refuses halts the pass exactly like a create
// does, so it has to be capable of being set aside too, and the "has the
// spec changed" test has no spec to look at.
const absentSpec = "<absent from desired state>"

// specFingerprint identifies what a failing step was trying to apply.
func specFingerprint(desiredByKey map[resKey]resource.Resource, step plan.PlanStep) string {
	want, ok := desiredByKey[resKey{step.Kind, step.Name}]
	if !ok {
		return absentSpec
	}
	return fingerprintOf(want)
}

// fingerprintOf identifies the exact desired spec that failed, so a
// changed one is recognised as a new attempt rather than a repeat.
func fingerprintOf(r resource.Resource) string {
	b, err := resource.Marshal(r)
	if err != nil {
		// Unmarshalable desired state is somebody else's problem. An
		// empty fingerprint is treated as "no fingerprint" and never
		// quarantines (see blockedSteps): erring toward another attempt
		// beats setting a resource aside on evidence we do not have.
		return ""
	}
	return string(b)
}
