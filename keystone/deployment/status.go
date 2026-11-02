package deployment

import (
	"errors"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/localgit"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/publish"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func (m *Manager) Status(id string) runtimedb.DeploymentStatus {
	out := runtimedb.DeploymentStatus{Phase: "unknown", Detail: "Local publication cannot be observed.", Delivery: "unobserved", Reconciliation: "unobserved", Application: "unobserved", ObservedAt: time.Now().UTC()}
	rec, err := m.DB.GetPlan(id)
	if err != nil {
		return out
	}
	doc, err := runtimedb.DecodeDeploymentPlan(rec.PlanJSON)
	if err != nil || doc.Deployment.Review == nil {
		return out
	}
	r := doc.Deployment.Review
	out.ProductRevision = r.ProductRevision
	attemptPhase, attemptCode, _ := m.DB.DeploymentAttempt(id)
	out.AttemptCode = attemptCode

	switch rec.State {
	case runtimedb.PlanProposed:
		out.Phase = "awaiting_approval"
		out.Detail = "Waiting for exact Owner approval."
	case runtimedb.PlanRejected, runtimedb.PlanSuperseded, runtimedb.PlanFailed:
		out.Phase = string(rec.State)
		out.Detail = "This Plan authorizes no new promotion."
	default:
		out.Phase = "accepted"
		out.Detail = "Accepted product input; local promotion is pending."
		if rec.State == runtimedb.PlanApplying {
			out.Phase = "promoting"
			out.Detail = "Local promotion is pending; no controller outcome is implied."
		}
		if attemptPhase != "" && attemptPhase != "promoted" {
			out.Phase = attemptPhase
			out.Detail = "Local publication is held; retry/inspection is required."
		}

	}
	expectedBinding, err := publish.DeploymentBinding(m.repo.Identity(), request(id, doc))
	if err != nil || expectedBinding != doc.Deployment.Binding {
		out.Phase = "blocked"
		out.Detail = "Installation binding changed; fresh review is required."
		return out
	}
	row, err := m.DB.GetDeploymentPromotion(r.OperationID)
	if errors.Is(err, runtimedb.ErrNotFound) {
		if rec.State == runtimedb.PlanApproved || rec.State == runtimedb.PlanApplying {
			_, inputErr := m.checkPlan(rec, false)
			if authorityErr := m.DB.ObserveDeploymentAuthority(rec.ID); inputErr != nil || authorityErr != nil {
				out.Phase = "blocked"
				out.Detail = "Reviewed input is stale or unavailable; promotion is held."
			}
		}
		return out
	}
	if err != nil || row.Binding != expectedBinding {
		return out
	}
	out.PreparedRevision = row.CandidateRevision
	head, err := m.repo.Head()
	if err != nil {
		out.Phase = "unknown"
		return out
	}
	if head == row.CandidateRevision {
		out.Phase = "promoted"
		out.PromotedRevision = head
		out.Detail = "Local ref promotion observed; controller/application outcomes remain unobserved."
		if row.State != "promoted" {
			out.Detail = "Local ref advanced; durable acknowledgment is pending."
		}
	} else if head == row.ExpectedRevision && row.State != "promoted" {
		out.Phase = "prepared"
		out.Detail = "Candidate prepared; approved ref has not advanced."
		if attemptPhase == "failed" || attemptPhase == "unknown" {
			out.Phase = attemptPhase
			out.Detail = "Local promotion did not complete; the dispatcher will re-observe before retrying."
		}

		if _, err := m.checkPlan(rec, false); err != nil {
			out.Phase = "blocked"
			out.Detail = "Reviewed input cannot be verified; promotion is held."
		} else if err := m.DB.ObserveDeploymentAuthority(rec.ID); err != nil {
			out.Phase = "blocked"
			out.Detail = "Current approval authority cannot be verified; promotion is held."
		}

	} else {
		out.Phase = "diverged"
		out.Detail = "Actual ref differs from this operation; automatic overwrite is refused."
	}
	out.Delivery = "unavailable"
	s, err := load(m.configPath)
	if err != nil {
		return out
	}
	mirror, err := localgit.MirrorHead(s.config.MirrorRepository)
	if err != nil {
		return out
	}
	out.MirrorRevision = mirror
	out.Delivery = "pending"
	if out.PromotedRevision != "" && mirror == out.PromotedRevision {
		out.Delivery = "mirrored"
	}
	return out
}

// Slot exposes only harmless supported configuration choices, never bootstrap
// paths, filesystem credentials or an implied runtime/readiness result.
func (m *Manager) Slot() (map[string]any, error) {
	s, err := load(m.configPath)
	if err != nil {
		return nil, err
	}
	timezone := "UTC"
	if rs, err := m.State.Load(); err == nil {
		for _, r := range rs {
			if r.Kind == resource.KindHelmDeployment && r.Metadata.Name == s.config.Instance {
				timezone = r.HelmDeployment.Timezone
			}
		}
	} else {
		return nil, err
	}
	return map[string]any{"configured": true, "instance": s.config.Instance, "package": s.pkg.Metadata.Name, "version": s.pkg.Metadata.Version, "timezone": timezone, "reconciliation": "unobserved", "application": "unobserved"}, nil
}
