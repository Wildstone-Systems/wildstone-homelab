package runtimedb

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// DeploymentReview carries bounded non-secret exact review material. It holds
// ordinary generated YAML and references, never bootstrap paths or credentials.
type DeploymentSecretTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	Type      string `json:"type"`
}

type DeploymentSecretKustomization struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// DeploymentSecretReview is safe identity/version metadata only. It never
// contains ciphertext, SOPS metadata, raw recipients, local paths or keys.
type DeploymentSecretReview struct {
	Validation            string                        `json:"validation"`
	Target                DeploymentSecretTarget        `json:"target"`
	SecretGeneration      uint64                        `json:"secretGeneration"`
	CiphertextRevision    string                        `json:"ciphertextRevision"`
	RecipientFingerprints []string                      `json:"recipientFingerprints"`
	OutputPath            string                        `json:"outputPath"`
	OuterKustomization    DeploymentSecretKustomization `json:"outerKustomization"`
	DecryptionSecretName  string                        `json:"decryptionSecretName"`
	RestartPolicy         string                        `json:"restartPolicy"`
	RestartEffect         string                        `json:"restartEffect"`
	PrivateFileCount      int                           `json:"privateFileCount"`
}

type DeploymentReview struct {
	Instance                   string                  `json:"instance"`
	OperationID                string                  `json:"operationId"`
	PreviousProductRevision    string                  `json:"previousProductRevision"`
	ProductRevision            string                  `json:"productRevision"`
	ExpectedDeploymentRevision string                  `json:"expectedDeploymentRevision"`
	ProjectionDigest           string                  `json:"projectionDigest"`
	ProfileDigest              string                  `json:"profileDigest"`
	InventoryDigest            string                  `json:"inventoryDigest"`
	EncryptedSecretRevision    string                  `json:"encryptedSecretRevision"`
	Package                    string                  `json:"package"`
	PackageVersion             string                  `json:"packageVersion"`
	ChartSource                string                  `json:"chartSource"`
	ChartCommit                string                  `json:"chartCommit"`
	ChartPath                  string                  `json:"chartPath"`
	ChartVersion               string                  `json:"chartVersion"`
	Image                      string                  `json:"image"`
	Namespace                  string                  `json:"namespace"`
	Release                    string                  `json:"release"`
	Timezone                   string                  `json:"timezone"`
	DesiredResource            string                  `json:"desiredResource"`
	Files                      map[string]string       `json:"files"`
	Secret                     *DeploymentSecretReview `json:"secret,omitempty"`
}

func DecodeDeploymentPlan(raw string) (DeploymentPlanDocument, error) {
	var doc DeploymentPlanDocument
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&doc); err != nil {
		return doc, fmt.Errorf("deployment: standalone deployment Plan required")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return doc, fmt.Errorf("deployment: invalid Plan document")
	}
	if doc.Deployment == nil {
		return doc, fmt.Errorf("deployment: approval document missing")
	}
	return doc, nil
}

func (db *DB) deploymentExecutionTx(tx *sql.Tx) error {
	lease := db.executionLease.Load()
	if lease == nil {
		return ErrCoordinatorFenced
	}
	if err := db.gateTx(tx, GateDispatch); err != nil {
		return err
	}
	current, err := readCoordinator(tx)
	if err != nil {
		return err
	}
	if current.Holder != lease.Holder || current.Epoch != lease.Epoch {
		return ErrCoordinatorFenced
	}
	return nil
}

func deploymentOwnerTx(tx queryRower, personID string) (DeploymentApproval, string, error) {
	a := DeploymentApproval{OwnerPersonID: personID}
	var member, role, disabled, offboarded string
	var admitted int
	err := tx.QueryRow(`SELECT member,role,admitted,generation,disabled_at,offboarded_at FROM butler_members WHERE person_id=?`, personID).Scan(&member, &role, &admitted, &a.OwnerGeneration, &disabled, &offboarded)
	if err != nil || role != RoleOwner || admitted != 1 || disabled != "" || offboarded != "" {
		return a, "", fmt.Errorf("deployment: active Owner required")
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM butler_role_changes WHERE person_id=?`, personID).Scan(&a.OwnerRoleRevision); err != nil {
		return a, "", err
	}
	return a, member, nil
}

func checkDeploymentOwnerTx(tx queryRower, a *DeploymentApproval, actor string) error {
	current, member, err := deploymentOwnerTx(tx, a.OwnerPersonID)
	if err != nil {
		return err
	}
	if actor != member || a.OwnerGeneration != current.OwnerGeneration || a.OwnerRoleRevision != current.OwnerRoleRevision {
		return fmt.Errorf("deployment: approving Owner authority changed")
	}
	return nil
}

// InsertDeploymentPlan allocates its ID and freezes exact review material in
// one transaction. build is pure and bounded: no DB/Git/files/network calls.
// Explicit sequence allocation avoids updating immutable Plan JSON afterward.
func (db *DB) InsertDeploymentPlan(owner, expected string, build func(string, DeploymentApproval) (DeploymentPlanDocument, error)) (string, error) {
	if build == nil {
		return "", fmt.Errorf("deployment: review builder required")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := db.deploymentExecutionTx(tx); err != nil {
		return "", err
	}
	person, err := resolvePersonID(tx, owner)
	if err != nil {
		return "", err
	}
	authority, member, err := deploymentOwnerTx(tx, person)
	if err != nil {
		return "", err
	}
	var current string
	err = tx.QueryRow(`SELECT head FROM statefile_head WHERE id=1`).Scan(&current)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if current != expected {
		return "", ErrRevisionMoved
	}
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='plans'),0)+1`).Scan(&seq); err != nil {
		return "", err
	}
	id := planID(seq)
	doc, err := build(id, authority)
	if err != nil {
		return "", err
	}
	a := doc.Deployment
	if doc.BasedOnHead != expected || doc.Summary == "" || a == nil || a.Review == nil || a.Binding == "" || a.SourceRevision != a.Review.ProductRevision || a.Review.PreviousProductRevision != expected {
		return "", fmt.Errorf("deployment: incomplete exact review")
	}
	if err := checkDeploymentOwnerTx(tx, a, member); err != nil {
		return "", err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO plans(seq,created_at,principal,acting_for,summary,risk,state,plan_json) VALUES(?,?,?,'',?,'high','proposed',?)`, seq, now(), member, doc.Summary, string(raw)); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// checkDeploymentPublicationTx extends the existing accepted-Statefile writer,
// so product acceptance and Owner approval cannot split across transactions.
func (db *DB) checkDeploymentPublicationTx(tx *sql.Tx, p Publication, to PlanState, actor string) error {
	if p.PlanID == "" {
		return nil
	}
	seq, err := parsePlanID(p.PlanID)
	if err != nil {
		return err
	}
	var raw string
	if err := tx.QueryRow(`SELECT plan_json FROM plans WHERE seq=?`, seq).Scan(&raw); err != nil {
		return err
	}
	var probe struct {
		Deployment json.RawMessage `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return err
	}
	if len(probe.Deployment) == 0 || string(probe.Deployment) == "null" {
		return nil
	}
	doc, err := DecodeDeploymentPlan(raw)
	if err != nil {
		return err
	}
	a := doc.Deployment
	r := a.Review
	if to != PlanApproved || r == nil || doc.BasedOnHead != p.ExpectedRevision || r.PreviousProductRevision != p.ExpectedRevision || r.ProductRevision != p.CandidateRevision || a.SourceRevision != p.CandidateRevision {
		return fmt.Errorf("deployment: exact product publication required")
	}
	if err := db.deploymentExecutionTx(tx); err != nil {
		return err
	}
	if err := checkDeploymentOwnerTx(tx, a, actor); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO deployment_plan_heads(instance_id,plan_seq) VALUES(?,?) ON CONFLICT(instance_id) DO UPDATE SET plan_seq=excluded.plan_seq`, r.Instance, seq)
	return err
}

// AcknowledgeObservedDeployment records an already-observed exact ref, not a
// new effect or an authority grant. Callers verify journal binding/tree/ref.
// Current-ref observation remains separate: an owner may edit it afterward.
func (db *DB) AcknowledgeObservedDeployment(id, operation, binding, revision string) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.deploymentExecutionTx(tx); err != nil {
		return err
	}
	seq, err := parsePlanID(id)
	if err != nil {
		return err
	}
	var raw, state string
	if err := tx.QueryRow(`SELECT plan_json,state FROM plans WHERE seq=?`, seq).Scan(&raw, &state); err != nil {
		return err
	}
	doc, err := DecodeDeploymentPlan(raw)
	if err != nil {
		return err
	}
	if doc.Deployment.Binding != binding || doc.Deployment.Review == nil || doc.Deployment.Review.OperationID != operation {
		return fmt.Errorf("deployment: observation binding mismatch")
	}
	result, err := tx.Exec(`UPDATE deployment_promotions SET state='promoted',updated_at=? WHERE operation_id=? AND binding=? AND candidate_revision=?`, now(), operation, binding, revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return fmt.Errorf("deployment: exact observed journal candidate required")
	}
	if PlanState(state) == PlanApproved {
		if err := transitionPlanTx(tx, id, PlanApplying, ""); err != nil {
			return err
		}
		state = string(PlanApplying)
	}
	if PlanState(state) == PlanApplying {
		if err := transitionPlanTx(tx, id, PlanApplied, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeploymentPlanIDs returns only a type flag for list summaries, not review
// files or private inventory. Full review remains behind the Owner detail route.
func (db *DB) DeploymentPlanIDs(ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 10000 {
		return nil, fmt.Errorf("deployment: bounded Plan list required")
	}
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		seq, err := parsePlanID(id)
		if err != nil {
			return nil, err
		}
		marks[i] = "?"
		args[i] = seq
	}
	rows, err := db.sql.Query(`SELECT seq FROM plans WHERE seq IN (`+strings.Join(marks, ",")+`) AND CASE WHEN json_valid(plan_json) THEN json_type(plan_json,'$.deployment.review')='object' ELSE 0 END`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		out[planID(seq)] = true
	}
	return out, rows.Err()
}

// ObserveDeploymentAuthority is a read-only status check, never an effect grant.
// Actual promotion always repeats these conditions under WithDeploymentApproval.
func (db *DB) ObserveDeploymentAuthority(id string) error {
	rec, err := db.GetPlan(id)
	if err != nil {
		return err
	}
	doc, err := DecodeDeploymentPlan(rec.PlanJSON)
	if err != nil || doc.Deployment.Review == nil {
		return fmt.Errorf("deployment: exact review unavailable")
	}
	if rec.State != PlanApproved && rec.State != PlanApplying {
		return fmt.Errorf("deployment: Plan is not pending execution")
	}
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	if err := checkDeploymentOwnerTx(db.sql, doc.Deployment, rec.ResolvedBy); err != nil {
		return err
	}
	current, err := db.StatefileHead()
	if err != nil {
		return err
	}
	if current != doc.Deployment.SourceRevision {
		return ErrRevisionMoved
	}
	seq, err := parsePlanID(id)
	if err != nil {
		return err
	}
	var selected int64
	if err := db.sql.QueryRow(`SELECT plan_seq FROM deployment_plan_heads WHERE instance_id=?`, doc.Deployment.Review.Instance).Scan(&selected); err != nil {
		return err
	}
	if selected != seq {
		return fmt.Errorf("deployment: approval superseded")
	}
	return nil
}

func (db *DB) RecordDeploymentAttempt(id, phase, code string) error {
	seq, err := parsePlanID(id)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.deploymentExecutionTx(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO deployment_publication_attempts(plan_seq,phase,detail_code,observed_at) VALUES(?,?,?,?) ON CONFLICT(plan_seq) DO UPDATE SET phase=excluded.phase,detail_code=excluded.detail_code,observed_at=excluded.observed_at`, seq, phase, code, now()); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) DeploymentAttempt(id string) (phase, code string, err error) {
	seq, err := parsePlanID(id)
	if err != nil {
		return "", "", err
	}
	err = db.sql.QueryRow(`SELECT phase,detail_code FROM deployment_publication_attempts WHERE plan_seq=?`, seq).Scan(&phase, &code)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return
}

// RevokeDeploymentPlan removes future authority; it never rolls back a ref or
// rewrites accepted product intent. SQLite orders revocation with in-flight CAS.
// It remains a restriction even while household execution is stopped.
func (db *DB) RevokeDeploymentPlan(id, owner string) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	person, err := resolvePersonID(tx, owner)
	if err != nil {
		return err
	}
	if _, _, err := deploymentOwnerTx(tx, person); err != nil {
		return err
	}
	seq, err := parsePlanID(id)
	if err != nil {
		return err
	}
	var raw, state string
	if err := tx.QueryRow(`SELECT plan_json,state FROM plans WHERE seq=?`, seq).Scan(&raw, &state); err != nil {
		return err
	}
	doc, err := DecodeDeploymentPlan(raw)
	if err != nil || doc.Deployment.Review == nil {
		return fmt.Errorf("deployment: exact deployment Plan required")
	}
	var to PlanState
	switch PlanState(state) {
	case PlanProposed:
		to = PlanRejected
	case PlanApproved:
		to = PlanSuperseded
	case PlanApplying:
		to = PlanFailed
	default:
		return fmt.Errorf("deployment: completed/closed Plan cannot authorize another effect")
	}
	if _, err := tx.Exec(`DELETE FROM deployment_plan_heads WHERE instance_id=? AND plan_seq=?`, doc.Deployment.Review.Instance, seq); err != nil {
		return err
	}
	if err := transitionPlanTx(tx, id, to, owner); err != nil {
		return err
	}
	return tx.Commit()
}

// DeploymentPlansToResume also includes closed Plans with an unacknowledged
// journal. They may acknowledge an already advanced ref, never start an effect.
func (db *DB) DeploymentPlansToResume() ([]PlanRecord, error) {
	rows, err := db.sql.Query(`SELECT seq,created_at,principal,acting_for,summary,risk,state,plan_json,resolved_by,resolved_at FROM plans p
 WHERE CASE WHEN json_valid(plan_json) THEN json_type(plan_json,'$.deployment.review')='object' AND
 (state IN ('approved','applying') OR EXISTS(SELECT 1 FROM deployment_promotions d WHERE d.state='prepared' AND d.operation_id=json_extract(p.plan_json,'$.deployment.review.operationId'))) ELSE 0 END ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanRecord
	for rows.Next() {
		rec, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
