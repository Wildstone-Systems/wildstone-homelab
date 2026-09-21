package runtimedb

import (
	"database/sql"
	"fmt"
)

type missingEffectReceipt struct{}

func (missingEffectReceipt) Error() string {
	return "every required effect needs a matching successful tool, scheduler or executor receipt"
}
func (missingEffectReceipt) MissingEffectReceipt() bool { return true }

var ErrEffectReceiptRequired error = missingEffectReceipt{}

// SetButlerObjectiveKind is called only by the runtime verifier, not a tool or API input.
func (db *DB) SetButlerObjectiveKind(taskID string, kind ObjectiveKind) error {
	return db.ClassifyButlerObjective(taskID, kind, false)
}

type RequiredEffect struct {
	ProposalOnly bool   `json:"proposalOnly"`
	Description  string `json:"description"`
	SourceKey    string `json:"sourceKey"`
}

func (db *DB) ClassifyButlerObjective(taskID string, kind ObjectiveKind, proposalOnly bool) error {
	return db.ClassifyButlerObjectiveEffects(taskID, kind, proposalOnly, nil)
}

// ClassifyButlerObjectiveEffects is a trusted runtime binding of each required
// effect to the exact verified operation that can satisfy it. Empty keys wait.
func (db *DB) ClassifyButlerObjectiveEffects(taskID string, kind ObjectiveKind, proposalOnly bool, effects []RequiredEffect) error {

	if kind != ObjectiveInformational {
		kind = ObjectiveEffectful
	}
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE butler_tasks SET objective_kind=?,proposal_only=? WHERE seq=? AND state='running'", kind, proposalOnly, task)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("objective classification requires a running task")
	}
	if _, err = tx.Exec("DELETE FROM butler_required_effects WHERE task=?", task); err != nil {
		return err
	}
	if len(effects) > 100 {
		return fmt.Errorf("too many required effects")
	}
	for i, e := range effects {
		if _, err = tx.Exec("INSERT INTO butler_required_effects(task,ordinal,description,source_key,proposal_only) VALUES(?,?,?,?,?)", task, i, e.Description, e.SourceKey, proposalOnly || e.ProposalOnly); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EffectReceipt is supplied by the trusted tool/scheduler/executor boundary,
// never parsed out of an answer or exposed as a model tool.
type EffectReceipt struct {
	ProposalOnly    bool
	Kind            string
	SourceID        string
	Step            int64
	Outcome         string
	PermissionLabel string
	Audience        ButlerTaskAudience
}

func (db *DB) RecordButlerEffectReceipt(taskID string, r EffectReceipt) (int64, error) {
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return 0, err
	}
	result, err := db.sql.Exec(`INSERT INTO butler_effect_receipts(task,recorded_at,kind,source_id,step,outcome,permission_label,audience,proposal_only) VALUES(?,?,?,?,NULLIF(?,0),?,?,?,?)`, task, now(), r.Kind, r.SourceID, r.Step, r.Outcome, r.PermissionLabel, r.Audience, r.ProposalOnly)
	if err != nil {
		// Retries may reuse an identical receipt, never rewrite one.
		var existing int64
		lookup := db.sql.QueryRow("SELECT seq FROM butler_effect_receipts WHERE task=? AND kind=? AND source_id=? AND COALESCE(step,0)=? AND outcome=? AND permission_label=? AND audience=? AND proposal_only=?", task, r.Kind, r.SourceID, r.Step, r.Outcome, r.PermissionLabel, r.Audience, r.ProposalOnly).Scan(&existing)
		if lookup == nil {
			return existing, nil
		}
		return 0, fmt.Errorf("recording effect receipt: %w", err)
	}
	return result.LastInsertId()
}
func checkObjectiveEvidence(tx *sql.Tx, task int64) error {
	var kind string
	var proposalOnly bool
	if err := tx.QueryRow("SELECT objective_kind,proposal_only FROM butler_tasks WHERE seq=?", task).Scan(&kind, &proposalOnly); err != nil {
		return err
	}
	if kind == string(ObjectiveInformational) {
		return nil
	}
	var count, missing int
	if err := tx.QueryRow("SELECT COUNT(*) FROM butler_required_effects WHERE task=?", task).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return ErrEffectReceiptRequired
	}
	err := tx.QueryRow(`SELECT COUNT(*) FROM butler_required_effects required WHERE required.task=? AND NOT EXISTS (
 SELECT 1 FROM butler_task_evidence e JOIN butler_effect_receipts r ON r.seq=e.receipt
 WHERE e.task=? AND r.task=? AND r.outcome='succeeded' AND r.proposal_only=required.proposal_only
 AND required.source_key=CASE WHEN r.step IS NOT NULL THEN 'step:'||r.step ELSE r.kind||':'||r.source_id END
 AND e.kind IN ('tool-result','external-verification'))`, task, task, task).Scan(&missing)
	if err != nil {
		return err
	}
	if missing != 0 {
		return ErrEffectReceiptRequired
	}
	return nil
}

// PersistedEffectEvidence recovers successful receipts after evidence links roll
// back in a failed completion transaction. Receipt purpose remains independently enforced.
func (db *DB) PersistedEffectEvidence(taskID string) ([]ButlerTaskEvidence, error) {
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`SELECT seq,source_id,permission_label,audience,proposal_only,kind FROM butler_effect_receipts WHERE task=? AND outcome='succeeded'`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evidence []ButlerTaskEvidence
	for rows.Next() {
		e := ButlerTaskEvidence{Kind: ButlerEvidenceKind("external-verification"), Description: "Plan persisted for Owner review; proposed changes have not been applied"}
		var proposal bool
		var kind string
		if err = rows.Scan(&e.ReceiptID, &e.SourceID, &e.PermissionLabel, &e.Audience, &proposal, &kind); err != nil {
			return nil, err
		}
		if !proposal {
			e.Description = "Requested effect confirmed by its " + kind
		}
		if kind == "tool" {
			e.Kind = ButlerEvidenceToolResult
		}

		evidence = append(evidence, e)
	}
	return evidence, rows.Err()
}

// ConfirmedObjectiveOperations exposes immutable executor receipt state to the
// verifier. Keys identify operations, values distinguish proposal persistence.
func (db *DB) ConfirmedObjectiveOperations(taskID string) (map[string]bool, error) {
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`SELECT CASE WHEN step IS NOT NULL THEN 'step:'||step ELSE kind||':'||source_id END,proposal_only FROM butler_effect_receipts WHERE task=? AND kind='executor' AND outcome='succeeded'`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		var proposal bool
		if err = rows.Scan(&key, &proposal); err != nil {
			return nil, err
		}
		out[key] = proposal
	}
	return out, rows.Err()
}

// ButlerRequiredEffectDescriptions supplies the verified objective's outcome
// labels. They describe what evidence must cover, not model claims of success.
func (db *DB) ButlerRequiredEffectDescriptions(taskID string) ([]string, error) {
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query("SELECT description FROM butler_required_effects WHERE task=? ORDER BY ordinal", task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}
