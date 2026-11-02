package runtimedb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// DeploymentApproval is exact dispatcher authority inside a standalone Plan.
// Binding fingerprints repository, operation, instance-specific output, accepted
// revision, artifacts and encrypted-secret references. A digest is not approval.
type DeploymentApproval struct {
	Binding           string `json:"binding"`
	SourceRevision    string `json:"sourceRevision"`
	OwnerPersonID     string `json:"ownerPersonId"`
	OwnerGeneration   int64  `json:"ownerGeneration"`
	OwnerRoleRevision int64  `json:"ownerRoleRevision"`
}

// DeploymentPlanDocument is deliberately standalone: it cannot also approve
// resource, routine, setting, subscription or audit-only work. Production Plan
// creation/approval and dispatcher integration remain separate work under #257.
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
	if err := gateTx(tx, GateDispatch); err != nil {
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
	var envelope DeploymentPlanDocument
	decoder := json.NewDecoder(bytes.NewBufferString(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("deployment: standalone deployment Plan required")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("deployment: invalid Plan document")
	}
	a := envelope.Deployment
	if a == nil || a.Binding != binding || a.SourceRevision != source || envelope.BasedOnHead != source || a.OwnerPersonID == "" || a.OwnerGeneration <= 0 || a.OwnerRoleRevision < 0 {
		return fmt.Errorf("deployment: approval binding mismatch")
	}
	var accepted string
	if err := tx.QueryRow(`SELECT head FROM statefile_head WHERE id=1`).Scan(&accepted); err != nil {
		return err
	}
	if accepted != source {
		return ErrRevisionMoved
	}
	var member, role, disabled, offboarded string
	var admitted int
	var generation, roleRevision int64
	if err := tx.QueryRow(`SELECT member,role,admitted,generation,disabled_at,offboarded_at FROM butler_members WHERE person_id=?`, a.OwnerPersonID).Scan(&member, &role, &admitted, &generation, &disabled, &offboarded); err != nil {
		return err
	}
	// Role changes do not advance admission generation. Bind their audit sequence
	// too, so demotion followed by re-promotion cannot revive an old approval.
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM butler_role_changes WHERE person_id=?`, a.OwnerPersonID).Scan(&roleRevision); err != nil {
		return err
	}
	if role != RoleOwner || admitted != 1 || generation != a.OwnerGeneration || roleRevision != a.OwnerRoleRevision || disabled != "" || offboarded != "" || approvedBy != member {
		return fmt.Errorf("deployment: approving Owner authority changed")
	}
	if err := effect(); err != nil {
		return err
	}
	return tx.Commit()
}
