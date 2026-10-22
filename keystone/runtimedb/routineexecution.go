package runtimedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// RoutineRun is one household-owned occurrence. PlanID is its sole
// authorization principal; no proposer or approver is copied onto the run.
type RoutineRun struct {
	ID            int64  `json:"id"`
	PlanID        string `json:"planId"`
	TriggerEvent  int64  `json:"triggerEvent"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	EffectState   string `json:"effectState"`
	EffectSummary string `json:"effectSummary,omitempty"`
}

type ReviewedRoutineRun struct {
	RoutineRun
	OccurredAt string `json:"occurredAt"`
	Subject    string `json:"subject,omitempty"`
}

func (db *DB) ListReviewedRoutineRuns(name string, limit int) ([]ReviewedRoutineRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := db.sql.Query(`SELECT x.seq,x.plan,x.trigger_event,x.state,x.reason,x.effect_state,x.effect_summary,e.occurred_at,e.subject
 FROM routine_runs x JOIN routine_plan_versions v ON v.plan=x.plan
 JOIN household_routines r ON r.seq=v.routine JOIN household_events e ON e.seq=x.trigger_event
 WHERE r.name=? ORDER BY x.seq DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewedRoutineRun{}
	for rows.Next() {
		var item ReviewedRoutineRun
		var plan int64
		if err := rows.Scan(&item.ID, &plan, &item.TriggerEvent, &item.State, &item.Reason, &item.EffectState, &item.EffectSummary, &item.OccurredAt, &item.Subject); err != nil {
			return nil, err
		}
		item.PlanID = planID(plan)
		out = append(out, item)
	}
	return out, rows.Err()
}

// RoutineExecutionVersion is a reviewed snapshot, not a grant derived from
// the routine name. The executor still rechecks it at every run boundary.
type RoutineExecutionVersion struct {
	PlanID         string
	Name           string
	Document       string
	OutputAudience string
	Delivery       string
	AssignedAgent  string
	UpdatedAt      string
}

func (db *DB) ApprovedRoutineVersions() ([]RoutineExecutionVersion, error) {
	rows, err := db.sql.Query(`SELECT v.plan,r.name,v.document,v.output_audience,v.delivery,
 v.assigned_butler,r.updated_at FROM routine_plan_versions v
 JOIN household_routines r ON r.seq=v.routine JOIN plans p ON p.seq=v.plan
 WHERE r.ownership='wildstone' AND r.state='enabled' AND r.disabled_at=''
 AND r.revoked_at='' AND r.deleted_at='' AND r.paused_reason=''
 AND r.revision=v.revision AND r.approved_revision=v.revision
 AND r.document=v.document AND r.output_audience=v.output_audience
 AND r.butler=v.assigned_butler AND v.approved_by IS NOT NULL AND p.state='applied'
 ORDER BY r.seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineExecutionVersion
	for rows.Next() {
		var v RoutineExecutionVersion
		var plan int64
		if err := rows.Scan(&plan, &v.Name, &v.Document, &v.OutputAudience,
			&v.Delivery, &v.AssignedAgent, &v.UpdatedAt); err != nil {
			return nil, err
		}
		v.PlanID = planID(plan)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (db *DB) RoutineExecutionCursor() (int64, bool, error) {
	var cursor int64
	var initialized bool
	err := db.sql.QueryRow("SELECT cursor,initialized FROM routine_execution_state WHERE singleton=1").Scan(&cursor, &initialized)
	return cursor, initialized, err
}

func (db *DB) SetRoutineExecutionCursor(cursor int64) error {
	if cursor < 0 {
		return fmt.Errorf("negative routine execution cursor")
	}
	res, err := db.sql.Exec(`UPDATE routine_execution_state SET cursor=?,initialized=1
 WHERE singleton=1 AND cursor<=?`, cursor, cursor)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("routine execution cursor cannot rewind")
	}
	return nil
}

// approvedRoutineSnapshot checks the reviewed identity without requiring a
// live model. Deterministic work may continue when the assigned Agent's model
// is unavailable; inference and Agent-scoped resources add their own checks.
func approvedRoutineSnapshot(tx *sql.Tx, plan int64) error {
	var document, liveDocument, audience, liveAudience, assigned, liveAssigned, signature, secrets, memory, state, planState, ownership, disabled, revoked, deleted, paused string
	var revision, liveRevision, approvedRevision int
	var approved sql.NullString
	err := tx.QueryRow(`SELECT v.document,r.document,v.output_audience,r.output_audience,
 v.assigned_butler,r.butler,r.resource_signature,v.secret_ids,v.memory_ids,
 r.state,p.state,r.ownership,r.disabled_at,r.revoked_at,r.deleted_at,r.paused_reason,v.revision,r.revision,r.approved_revision,v.approved_by
 FROM routine_plan_versions v JOIN household_routines r ON r.seq=v.routine
 JOIN plans p ON p.seq=v.plan WHERE v.plan=?`, plan).
		Scan(&document, &liveDocument, &audience, &liveAudience, &assigned, &liveAssigned,
			&signature, &secrets, &memory, &state, &planState, &ownership, &disabled,
			&revoked, &deleted, &paused, &revision, &liveRevision, &approvedRevision, &approved)
	if err != nil || document != liveDocument || audience != liveAudience || assigned != liveAssigned ||
		revision != liveRevision || revision != approvedRevision || state != RoutineEnabled ||
		planState != string(PlanApplied) || ownership != RoutineWildstone || disabled != "" || revoked != "" || deleted != "" || paused != "" ||
		!approved.Valid || approved.String == "" {
		return ErrRoutineDenied
	}
	var resources RoutineResources
	if json.Unmarshal([]byte(secrets), &resources.SecretIDs) != nil ||
		json.Unmarshal([]byte(memory), &resources.MemoryIDs) != nil {
		return ErrRoutineDenied
	}
	_, expected, err := normalizeRoutineResources(resources)
	if err != nil || expected != signature {
		return ErrRoutineDenied
	}
	return nil
}

// ClaimRoutineRun creates at most one occurrence for the exact Plan and
// trigger. A retry returns the existing state, including an uncertain effect.
func (db *DB) ClaimRoutineRun(planID string, trigger int64) (RoutineRun, bool, error) {
	if trigger <= 0 {
		return RoutineRun{}, false, ErrRoutineDenied
	}
	plan, err := parsePlanID(planID)
	if err != nil {
		return RoutineRun{}, false, ErrRoutineDenied
	}
	if err := db.CheckHouseholdExecution(); err != nil {
		return RoutineRun{}, false, err
	}
	tx, err := db.sql.BeginTx(context.Background(), nil)
	if err != nil {
		return RoutineRun{}, false, err
	}
	defer tx.Rollback()
	if err := approvedRoutineSnapshot(tx, plan); err != nil {
		return RoutineRun{}, false, err
	}
	var present int
	if err := tx.QueryRow("SELECT count(*) FROM household_events WHERE seq=?", trigger).Scan(&present); err != nil || present != 1 {
		return RoutineRun{}, false, ErrRoutineDenied
	}
	stamp := now()
	res, err := tx.Exec(`INSERT OR IGNORE INTO routine_runs(plan,trigger_event,state,created_at,updated_at)
 VALUES(?,?,'pending',?,?)`, plan, trigger, stamp, stamp)
	if err != nil {
		return RoutineRun{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return RoutineRun{}, false, err
	}
	run, err := scanRoutineRun(tx.QueryRow(`SELECT seq,plan,trigger_event,state,reason,effect_state,effect_summary
 FROM routine_runs WHERE plan=? AND trigger_event=?`, plan, trigger))
	if err != nil {
		return RoutineRun{}, false, err
	}
	if n == 1 {
		if _, err = tx.Exec(`INSERT INTO routine_run_transitions(run,at,from_state,to_state,reason)
 VALUES(?,?,'','pending','trigger matched')`, run.ID, stamp); err != nil {
			return RoutineRun{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RoutineRun{}, false, err
	}
	return run, n == 1, nil
}

func scanRoutineRun(row *sql.Row) (RoutineRun, error) {
	var r RoutineRun
	var plan int64
	if err := row.Scan(&r.ID, &plan, &r.TriggerEvent, &r.State, &r.Reason, &r.EffectState, &r.EffectSummary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	r.PlanID = planID(plan)
	return r, nil
}

func (db *DB) GetRoutineRun(id int64) (RoutineRun, error) {
	return scanRoutineRun(db.sql.QueryRow(`SELECT seq,plan,trigger_event,state,reason,effect_state,effect_summary
 FROM routine_runs WHERE seq=?`, id))
}

func (db *DB) CapabilityBlockedRoutineRuns(limit int) ([]RoutineRun, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.sql.Query(`SELECT seq,plan,trigger_event,state,reason,effect_state,effect_summary
 FROM routine_runs WHERE state='blocked' AND effect_state='none'
 AND reason LIKE 'required capability is unavailable:%' ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineRun
	for rows.Next() {
		var r RoutineRun
		var plan int64
		if err := rows.Scan(&r.ID, &plan, &r.TriggerEvent, &r.State, &r.Reason, &r.EffectState, &r.EffectSummary); err != nil {
			return nil, err
		}
		r.PlanID = planID(plan)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) RoutineTriggerEvent(seq int64) (HouseholdEvent, error) {
	rows, err := db.sql.Query(`SELECT seq,event_id,occurred_at,recorded_at,subject,source,type,entity,payload,correlation_id,schema_version
 FROM household_events WHERE seq=?`, seq)
	if err != nil {
		return HouseholdEvent{}, err
	}
	defer rows.Close()
	events, err := scanHouseholdEvents(rows)
	if err != nil {
		return HouseholdEvent{}, err
	}
	if len(events) != 1 {
		return HouseholdEvent{}, ErrNotFound
	}
	return events[0], nil
}

// CheckRoutineRunBoundary is called before every model round, prepared
// context handoff, retry, and domestic effect. It deliberately ignores the
// proposer and approving Owner's current Person state.
func (db *DB) CheckRoutineRunBoundary(id int64) error {
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var plan int64
	var state string
	if err := tx.QueryRow("SELECT plan,state FROM routine_runs WHERE seq=?", id).Scan(&plan, &state); err != nil {
		return fmt.Errorf("%w: run %d", ErrRoutineDenied, id)
	}
	if state == "completed" || state == "failed" || state == "uncertain" || state == "stopped" {
		return ErrRoutineDenied
	}
	return approvedRoutineSnapshot(tx, plan)
}

// ClaimRoutineEffect is the durable at-most-once point before external
// dispatch. A retry after a crash must inspect the recorded outcome; it may
// never claim the same effect twice merely because no response was saved.
func (db *DB) ClaimRoutineEffect(id int64) (bool, error) {
	if err := db.CheckRoutineRunBoundary(id); err != nil {
		return false, err
	}
	tx, err := db.sql.BeginTx(context.Background(), nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var plan int64
	var from string
	if err := tx.QueryRow("SELECT plan,state FROM routine_runs WHERE seq=?", id).Scan(&plan, &from); err != nil {
		return false, ErrRoutineDenied
	}
	if err := approvedRoutineSnapshot(tx, plan); err != nil {
		return false, err
	}
	res, err := tx.Exec(`UPDATE routine_runs SET effect_state='claimed',state='running',updated_at=?
 WHERE seq=? AND effect_state='none' AND state IN ('pending','blocked')`, now(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		if _, err = tx.Exec(`INSERT INTO routine_run_transitions(run,at,from_state,to_state,reason)
 VALUES(?,?,?,'running','effect claimed')`, id, now(), from); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetRoutineRunState records why work stopped or completed. It does not
// require a live execution grant: recording a stop after revocation is still
// essential, and a committed effect remains in the row.
func (db *DB) SetRoutineRunState(id int64, state, reason string) error {
	allowed := map[string]map[string]bool{
		"pending": {"blocked": true, "failed": true, "stopped": true},
		"running": {"blocked": true, "completed": true, "failed": true, "uncertain": true, "stopped": true},
		"blocked": {"blocked": true, "failed": true, "stopped": true},
	}
	tx, err := db.sql.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var from string
	if err := tx.QueryRow("SELECT state FROM routine_runs WHERE seq=?", id).Scan(&from); err != nil {
		return ErrNotFound
	}
	if !allowed[from][state] {
		return fmt.Errorf("routine run cannot move from %s to %s", from, state)
	}
	if _, err := tx.Exec("UPDATE routine_runs SET state=?,reason=?,updated_at=? WHERE seq=?", state, reason, now(), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO routine_run_transitions(run,at,from_state,to_state,reason)
 VALUES(?,?,?,?,?)`, id, now(), from, state, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordRoutineEffect preserves what an executor reported even after a stop
// committed. An ambiguous result stays uncertain and is never auto-retried.
func (db *DB) RecordRoutineEffect(id int64, outcome, summary string) error {
	switch outcome {
	case "pending", "confirmed", "failed", "uncertain":
	default:
		return fmt.Errorf("invalid routine effect outcome %q", outcome)
	}
	if summary == "" {
		return fmt.Errorf("routine effect outcome needs a summary")
	}
	tx, err := db.sql.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE routine_runs SET effect_state=?,effect_summary=?,updated_at=?
 WHERE seq=? AND effect_state='claimed'`, outcome, summary, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRoutineDenied
	}
	if _, err := tx.Exec(`INSERT INTO routine_effect_receipts(run,recorded_at,outcome,summary)
 VALUES(?,?,?,?)`, id, now(), outcome, summary); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverClaimedRoutineEffects runs once when the coordinator starts. An
// effect claimed by the previous process may have happened; absence of its
// result is not permission to retry it. A fresh process holds the exclusive
// coordinator lock before it can call this method.
func (db *DB) RecoverClaimedRoutineEffects() error {
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	rows, err := db.sql.Query(`SELECT seq FROM routine_runs
 WHERE state='running' AND effect_state='claimed' ORDER BY seq`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := db.RecordRoutineEffect(id, "uncertain", "coordinator restarted after dispatch claim"); err != nil {
			return err
		}
		if err := db.SetRoutineRunState(id, "uncertain", "effect outcome needs Owner resolution"); err != nil {
			return err
		}
	}
	return nil
}
