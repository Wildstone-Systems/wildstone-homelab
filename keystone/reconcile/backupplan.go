package reconcile

// BackupPlan applier (ADR-W011 A4): declared-only, the LdapGroup pattern.
// A BackupPlan is a standing declaration the BACKUP ENGINE
// (keystone/backup) acts on — the reconciler's job is only to admit it
// into desired state through the normal plan/approve path and track it in
// the managed cache. Apply therefore never dispatches a Wardstone op: the
// restic runs themselves are the engine's, each with its own plan/event
// provenance.
//
// Observe: managed-cache "observe-by-apply", shared with LdapGroup/Volume
// in reconcile.go's observe().
//
// Health gate: trivially passes — nothing external was changed here.

import (
	"context"
	"log"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// applyBackupPlanStep records step's BackupPlan as managed (Create/Update)
// or unmanaged (Delete) and always reports success. nodeID keys the
// managed-cache lookup consistently with the other kinds.
func (r *Reconciler) applyBackupPlanStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	_ = ctx // no external call is made; accepted for signature symmetry
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.BackupPlan == nil {
			sr.Error = "internal: no desired BackupPlan spec for " + step.Name
			return sr
		}
		log.Printf("reconcile: BackupPlan %q declared: volumes=%v schedule=%s (runs are the backup engine's)",
			step.Name, want.BackupPlan.Volumes, want.BackupPlan.Schedule)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, true)
		return sr

	case plan.ActionDelete:
		log.Printf("reconcile: BackupPlan %q removed from desired state (no data touched; its runs simply stop)", step.Name)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, false)
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}

// applyBackupTargetStep records step's BackupTarget as managed or
// unmanaged (ADR-W045 A2) — the same declared-only pattern as a
// BackupPlan. No op is dispatched: a target is only a name for a bucket,
// and the backup engine's off-site runs are what reach it, each with its
// own provenance. Its credentials stay secret references throughout.
func (r *Reconciler) applyBackupTargetStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	_ = ctx // no external call is made; accepted for signature symmetry
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.BackupTarget == nil {
			sr.Error = "internal: no desired BackupTarget spec for " + step.Name
			return sr
		}
		log.Printf("reconcile: BackupTarget %q declared: %s bucket=%s (off-site runs are the backup engine's)",
			step.Name, want.BackupTarget.Endpoint, want.BackupTarget.Bucket)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, true)
		return sr

	case plan.ActionDelete:
		log.Printf("reconcile: BackupTarget %q removed from desired state (nothing in the bucket is touched)", step.Name)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, false)
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}
