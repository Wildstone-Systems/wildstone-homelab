package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PlanState is the lifecycle state of a plan record (ADR-W007 A4).
type PlanState string

const (
	PlanProposed   PlanState = "proposed"
	PlanApproved   PlanState = "approved"
	PlanApplying   PlanState = "applying"
	PlanApplied    PlanState = "applied"
	PlanFailed     PlanState = "failed"
	PlanRejected   PlanState = "rejected"
	PlanSuperseded PlanState = "superseded"
)

// planTransitions is the closed state machine: a transition absent here is
// a bug in the caller, not a policy question.
var planTransitions = map[PlanState][]PlanState{
	PlanProposed: {PlanApproved, PlanRejected, PlanSuperseded},
	PlanApproved: {PlanApplying, PlanSuperseded},
	PlanApplying: {PlanApplied, PlanFailed},
}

// ErrNotFound is returned by lookups for absent rows across the package.
var ErrNotFound = errors.New("runtimedb: not found")

// PlanRecord is the runtime-DB row for one plan. PlanJSON carries the full
// plan.Plan serialization; the projected columns exist so lists and audits
// don't parse JSON.
type PlanRecord struct {
	Seq        int64     `json:"seq"`
	ID         string    `json:"id"` // "plan-<seq>" (ADR-W004 A3's "DB seq later")
	CreatedAt  time.Time `json:"createdAt"`
	Principal  string    `json:"principal"`
	ActingFor  string    `json:"actingFor,omitempty"`
	Summary    string    `json:"summary"`
	Risk       string    `json:"risk"`
	State      PlanState `json:"state"`
	PlanJSON   string    `json:"planJson,omitempty"`
	ResolvedBy string    `json:"resolvedBy,omitempty"`
	ResolvedAt time.Time `json:"resolvedAt,omitzero"`
}

// InsertPlan records a newly proposed plan and returns its "plan-<seq>" ID.
func (db *DB) InsertPlan(principal, actingFor, summary, risk, planJSON string, privateOrigin ...bool) (string, error) {
	if principal == "" || summary == "" || planJSON == "" {
		return "", fmt.Errorf("runtimedb: insert plan: principal, summary, and plan JSON required")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`INSERT INTO plans (created_at, principal, acting_for, summary, risk, state, plan_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		now(), principal, actingFor, summary, risk, string(PlanProposed), planJSON)
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting plan: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	if len(privateOrigin) > 0 && privateOrigin[0] {
		if _, err = tx.Exec("INSERT INTO plan_private_origins(plan) VALUES(?)", seq); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return planID(seq), nil
}

// BeginHouseholdAudit records an immediate, non-executing tracker edit. It
// never enters proposed/approved, so it cannot authorize infrastructure repair.
func (db *DB) BeginHouseholdAudit(principal, summary, risk, planJSON string) (string, error) {
	if principal == "" || summary == "" || planJSON == "" {
		return "", fmt.Errorf("household audit needs an actor and document")
	}
	res, err := db.sql.Exec(`INSERT INTO plans(created_at,principal,acting_for,summary,risk,state,plan_json,resolved_by,resolved_at) VALUES(?,?,'',?,?,'applying',?,?,?)`, now(), principal, summary, risk, planJSON, principal, now())
	if err != nil {
		return "", err
	}
	seq, err := res.LastInsertId()
	return planID(seq), err
}

func planID(seq int64) string { return fmt.Sprintf("plan-%d", seq) }

// parsePlanID inverts planID; it rejects anything not "plan-<n>".
func parsePlanID(id string) (int64, error) {
	var seq int64
	if _, err := fmt.Sscanf(id, "plan-%d", &seq); err != nil || planID(seq) != id {
		return 0, fmt.Errorf("runtimedb: malformed plan id %q", id)
	}
	return seq, nil
}

// GetPlan returns one plan record, including its full JSON.
func (db *DB) GetPlan(id string) (PlanRecord, error) {
	seq, err := parsePlanID(id)
	if err != nil {
		return PlanRecord{}, err
	}
	row := db.sql.QueryRow(
		`SELECT seq, created_at, principal, acting_for, summary, risk, state, plan_json, resolved_by, resolved_at
		 FROM plans WHERE seq = ?`, seq)
	return scanPlan(row)
}

// AppliedPlanJSON returns the stored JSON of the newest applied plans,
// newest first.
//
// It exists because ListPlans deliberately blanks plan_json — a listing
// of every plan carrying every plan's full document would be enormous —
// and a caller that needs the ORIGIN of an install had no way to get it
// short of one GetPlan per row.
//
// That gap was not theoretical. installedVersions() read ListPlans and
// unmarshalled the JSON it does not return, so every unmarshal failed,
// the version map was always empty, and the Console's "this capability
// is behind the catalog" (F91) reported nothing on any deployment. The
// feature had been shipped, tested against a fake, and was inert.
// It returns every plan whose approval COMMITTED the change, which is a
// wider set than state = applied and deliberately so. The reconcile loop
// says it plainly: "approval already committed the change", and on a
// node that does not converge, "the Statefile is not reverted on
// failure: desired stays desired and the next tick retries". So `failed`
// means the node has not caught up yet, not that the install never
// happened.
//
// Reading only `applied` made that difference visible in the worst way.
// A Route health gate wrongly failed a books-calibre-web install that
// had already written timezone America/Chicago into desired state; the
// plan was recorded failed, so the newest APPLIED origin stayed the
// older one, and the install form went on offering the household a
// timezone — CST — that the running container no longer had and that
// validation would now refuse.
//
// proposed/rejected/superseded stay out: those never reached the
// Statefile, and they are the ones that genuinely never happened.
func (db *DB) AppliedPlanJSON(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.Query(
		`SELECT plan_json FROM plans WHERE state IN (?, ?, ?, ?) ORDER BY seq DESC LIMIT ?`,
		string(PlanApproved), string(PlanApplying), string(PlanApplied), string(PlanFailed), limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing applied plans: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, fmt.Errorf("runtimedb: scanning applied plan: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListPlans returns the newest limit plans, newest first, without their
// full JSON (fetch one with GetPlan, or AppliedPlanJSON for origins).
func (db *DB) ListPlans(limit int) ([]PlanRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.Query(
		`SELECT seq, created_at, principal, acting_for, summary, risk, state, '', resolved_by, resolved_at
		 FROM plans ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing plans: %w", err)
	}
	defer rows.Close()
	var out []PlanRecord
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlansInState returns every plan currently in state, oldest first,
// including full JSON — the reconcile service's work queue (ADR-W008 A2).
func (db *DB) PlansInState(state PlanState) ([]PlanRecord, error) {
	rows, err := db.sql.Query(
		`SELECT seq, created_at, principal, acting_for, summary, risk, state, plan_json, resolved_by, resolved_at
		 FROM plans WHERE state = ? ORDER BY seq`, string(state))
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing plans in %s: %w", state, err)
	}
	defer rows.Close()
	var out []PlanRecord
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanPlan(row rowScanner) (PlanRecord, error) {
	var p PlanRecord
	var created, resolvedAt, state string
	err := row.Scan(&p.Seq, &created, &p.Principal, &p.ActingFor, &p.Summary,
		&p.Risk, &state, &p.PlanJSON, &p.ResolvedBy, &resolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanRecord{}, ErrNotFound
	}
	if err != nil {
		return PlanRecord{}, fmt.Errorf("runtimedb: scanning plan: %w", err)
	}
	p.ID = planID(p.Seq)
	p.State = PlanState(state)
	if p.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return PlanRecord{}, fmt.Errorf("runtimedb: bad plan timestamp %q: %w", created, err)
	}
	if resolvedAt != "" {
		if p.ResolvedAt, err = time.Parse(time.RFC3339Nano, resolvedAt); err != nil {
			return PlanRecord{}, fmt.Errorf("runtimedb: bad plan resolved_at %q: %w", resolvedAt, err)
		}
	}
	return p, nil
}

// TransitionPlan moves a plan to a new state, enforcing the closed
// transition table. resolvedBy names the acting principal for terminal
// decisions (approve/reject); pass "" for machine transitions
// (applying/applied/failed/superseded by the reconciler).
func (db *DB) TransitionPlan(id string, to PlanState, resolvedBy string) error {
	seq, err := parsePlanID(id)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: transition plan: %w", err)
	}
	defer tx.Rollback()
	var cur string
	err = tx.QueryRow(`SELECT state FROM plans WHERE seq = ?`, seq).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: transition plan: %w", err)
	}
	allowed := false
	for _, next := range planTransitions[PlanState(cur)] {
		if next == to {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("runtimedb: plan %s: illegal transition %s → %s", id, cur, to)
	}
	if resolvedBy != "" {
		_, err = tx.Exec(`UPDATE plans SET state = ?, resolved_by = ?, resolved_at = ? WHERE seq = ?`,
			string(to), resolvedBy, now(), seq)
	} else {
		_, err = tx.Exec(`UPDATE plans SET state = ? WHERE seq = ?`, string(to), seq)
	}
	if err != nil {
		return fmt.Errorf("runtimedb: transition plan: %w", err)
	}
	return tx.Commit()
}
