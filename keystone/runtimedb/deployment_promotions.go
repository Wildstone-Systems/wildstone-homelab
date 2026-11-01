package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
)

// DeploymentPromotion is a journal, not an approval. Binding fingerprints the
// repository, Plan, reviewed projection, accepted revision and complete files.
type DeploymentPromotion struct {
	OperationID       string
	Binding           string
	ExpectedRevision  string
	CandidateRevision string
	TreeDigest        string
	State             string
}

// PrepareDeploymentPromotion is idempotent only for the exact same operation.
// A concurrent process's immutable journal entry always wins over a retry's
// newly built candidate commit (whose timestamp may differ).
func (db *DB) PrepareDeploymentPromotion(p DeploymentPromotion) (DeploymentPromotion, error) {
	if p.OperationID == "" || p.Binding == "" || p.CandidateRevision == "" || p.TreeDigest == "" {
		return DeploymentPromotion{}, fmt.Errorf("deployment: incomplete journal entry")
	}
	_, err := db.sql.Exec(`INSERT INTO deployment_promotions(operation_id,binding,expected_revision,candidate_revision,tree_digest,state,created_at,updated_at) VALUES(?,?,?,?,?,'prepared',?,?) ON CONFLICT(operation_id) DO NOTHING`, p.OperationID, p.Binding, p.ExpectedRevision, p.CandidateRevision, p.TreeDigest, now(), now())
	if err != nil {
		return DeploymentPromotion{}, err
	}
	got, err := db.GetDeploymentPromotion(p.OperationID)
	if err != nil {
		return got, err
	}
	if got.Binding != p.Binding || got.ExpectedRevision != p.ExpectedRevision {
		return DeploymentPromotion{}, fmt.Errorf("deployment: operation id already binds different review material")
	}
	return got, nil
}

func (db *DB) GetDeploymentPromotion(id string) (DeploymentPromotion, error) {
	var p DeploymentPromotion
	err := db.sql.QueryRow(`SELECT operation_id,binding,expected_revision,candidate_revision,tree_digest,state FROM deployment_promotions WHERE operation_id=?`, id).Scan(&p.OperationID, &p.Binding, &p.ExpectedRevision, &p.CandidateRevision, &p.TreeDigest, &p.State)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func (db *DB) MarkDeploymentPromoted(id, binding string) error {
	res, err := db.sql.Exec(`UPDATE deployment_promotions SET state='promoted',updated_at=? WHERE operation_id=? AND binding=? AND state IN ('prepared','promoted')`, now(), id, binding)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("deployment: promotion journal mismatch")
	}
	return nil
}
