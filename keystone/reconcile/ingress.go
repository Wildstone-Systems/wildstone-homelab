package reconcile

// Ingress applier: declaration only.
//
// Apply makes no external call. What the declaration FEEDS is the base
// Caddyfile and every Route fragment — both rendered from the node's
// ingress posture rather than from constants, which is what lets two
// profiles with opposite TLS stories share one renderer.

import (
	"context"
	"log"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func (r *Reconciler) applyIngressStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	_ = ctx // no external call; ctx accepted for signature symmetry
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.Ingress == nil {
			sr.Error = "internal: no desired Ingress spec for " + step.Name
			return sr
		}
		log.Printf("reconcile: Ingress %q declared (tls=%q); the base Caddyfile and Route fragments render from it",
			step.Name, want.Ingress.TLS)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, true)
		return sr

	case plan.ActionDelete:
		log.Printf("reconcile: Ingress %q removed from desired state; the node falls back to terminated-upstream posture", step.Name)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, false)
		return sr
	}
	sr.Error = "unexpected action " + step.Action.String()
	return sr
}
