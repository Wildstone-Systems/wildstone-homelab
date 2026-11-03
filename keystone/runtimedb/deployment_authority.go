package runtimedb

import (
	"fmt"
)

// DeploymentApproval is exact dispatcher authority inside a standalone Plan.
// Binding fingerprints repository, operation, instance-specific output, accepted
// revision, artifacts and encrypted-secret references. A digest is not approval.
type DeploymentApproval struct {
	Binding           string            `json:"binding"`
	SourceRevision    string            `json:"sourceRevision"`
	OwnerPersonID     string            `json:"ownerPersonId"`
	OwnerGeneration   int64             `json:"ownerGeneration"`
	OwnerRoleRevision int64             `json:"ownerRoleRevision"`
	Review            *DeploymentReview `json:"review,omitempty"`
}

// DeploymentPlanDocument is deliberately standalone: it cannot also approve
// resource, routine, setting, subscription or audit-only work. Production
// creation, approval and restart dispatch use this same immutable document.
type DeploymentPlanDocument struct {
	BasedOnHead string              `json:"basedOnHead"`
	Summary     string              `json:"summary"`
	Deployment  *DeploymentApproval `json:"deployment"`
}

// WithDeploymentApproval linearizes a bounded local Git ref effect against
// SQLite-backed revocation, coordinator promotion, Plan changes and product
// publication. The existing immediate write transaction is held across effect.
// effect must perform only bounded local Git work, without DB calls or network.
// Git cannot be rolled back if SQLite commit fails: the promotion journal must
// recognize its exact ref on retry. An earlier callback check is not this fence.
func (db *DB) WithDeploymentApproval(planID, expectedPlanJSON, binding, source string, effect func() error) error {
	if effect == nil || expectedPlanJSON == "" || binding == "" || source == "" {
		return fmt.Errorf("deployment: exact approval material required")
	}
	lease := db.executionLease.Load()
	if lease == nil {
		return ErrCoordinatorFenced
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.gateTx(tx, GateDispatch); err != nil {
		return err
	}
	currentLease, err := readCoordinator(tx)
	if err != nil {
		return err
	}
	if currentLease.Holder != lease.Holder || currentLease.Epoch != lease.Epoch {
		return ErrCoordinatorFenced
	}
	seq, err := parsePlanID(planID)
	if err != nil {
		return err
	}
	var state, document, approvedBy string
	if err := tx.QueryRow(`SELECT state,plan_json,resolved_by FROM plans WHERE seq=?`, seq).Scan(&state, &document, &approvedBy); err != nil {
		return err
	}
	if document != expectedPlanJSON || (PlanState(state) != PlanApproved && PlanState(state) != PlanApplying) {
		return fmt.Errorf("deployment: Plan is not the exact approved version")
	}
	envelope, err := DecodeDeploymentPlan(document)
	if err != nil {
		return err
	}
	a := envelope.Deployment
	if a.Binding != binding || a.SourceRevision != source || a.OwnerPersonID == "" || a.OwnerGeneration <= 0 || a.OwnerRoleRevision < 0 {
		return fmt.Errorf("deployment: approval binding mismatch")
	}
	if a.Review == nil {
		if envelope.BasedOnHead != source {
			return fmt.Errorf("deployment: approval source mismatch")
		}
	} else {
		if a.Review.ProductRevision != source || a.Review.PreviousProductRevision != envelope.BasedOnHead {
			return fmt.Errorf("deployment: product review mismatch")
		}
		var currentPlan int64
		if err := tx.QueryRow(`SELECT plan_seq FROM deployment_plan_heads WHERE instance_id=?`, a.Review.Instance).Scan(&currentPlan); err != nil {
			return err
		}
		if currentPlan != seq {
			return fmt.Errorf("deployment: approval was superseded")
		}
	}
	var accepted string
	if err := tx.QueryRow(`SELECT head FROM statefile_head WHERE id=1`).Scan(&accepted); err != nil {
		return err
	}
	if accepted != source {
		return ErrRevisionMoved
	}
	if err := checkDeploymentOwnerTx(tx, a, approvedBy); err != nil {
		return err
	}
	if err := effect(); err != nil {
		return err
	}
	return tx.Commit()
}
