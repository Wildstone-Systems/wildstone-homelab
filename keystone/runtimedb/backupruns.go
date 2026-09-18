package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BackupRunKind distinguishes the three things that can write a row to
// the backup_runs ledger (ADR-W011 A4): an ordinary backup, the scheduled
// restore drill, and an operator-initiated restore-alongside. The last
// two share one mechanism — RestoreAlongside IS the drill — but the
// ledger is the record of what happened, and "drill" is the wrong word
// for a restore a human asked for during an incident.
type BackupRunKind string

const (
	BackupRunBackup  BackupRunKind = "backup"
	BackupRunDrill   BackupRunKind = "drill"
	BackupRunRestore BackupRunKind = "restore"
)

// BackupRunStatus is the lifecycle of one ledger row.
type BackupRunStatus string

const (
	BackupRunRunning   BackupRunStatus = "running"
	BackupRunSucceeded BackupRunStatus = "succeeded"
	BackupRunFailed    BackupRunStatus = "failed"
)

// BackupRun is one row of the backup run ledger: the durable "when did
// this plan last run, and how did it go" record the backup engine's
// scheduler is driven by. plan/event records carry the richer provenance;
// this table exists so scheduling survives restarts without parsing them.
type BackupRun struct {
	Seq        int64           `json:"seq"`
	PlanName   string          `json:"planName"`
	RunKind    BackupRunKind   `json:"runKind"`
	PlanID     string          `json:"planId,omitempty"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt,omitzero"`
	Status     BackupRunStatus `json:"status"`
	Detail     string          `json:"detail,omitempty"`
}

// StartBackupRun opens a ledger row in state 'running' and returns its seq.
func (db *DB) StartBackupRun(planName string, kind BackupRunKind, planID string) (int64, error) {
	if planName == "" || kind == "" {
		return 0, fmt.Errorf("runtimedb: start backup run: plan name and kind required")
	}
	res, err := db.sql.Exec(
		`INSERT INTO backup_runs (plan_name, run_kind, plan_id, started_at, status)
		 VALUES (?, ?, ?, ?, ?)`,
		planName, string(kind), planID, now(), string(BackupRunRunning))
	if err != nil {
		return 0, fmt.Errorf("runtimedb: starting backup run: %w", err)
	}
	return res.LastInsertId()
}

// FinishBackupRun closes a ledger row with its outcome.
func (db *DB) FinishBackupRun(seq int64, status BackupRunStatus, detail string) error {
	if status != BackupRunSucceeded && status != BackupRunFailed {
		return fmt.Errorf("runtimedb: finish backup run: status must be succeeded or failed, got %q", status)
	}
	res, err := db.sql.Exec(
		`UPDATE backup_runs SET status = ?, detail = ?, finished_at = ? WHERE seq = ?`,
		string(status), detail, now(), seq)
	if err != nil {
		return fmt.Errorf("runtimedb: finishing backup run %d: %w", seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LastBackupRun returns the most recent ledger row for (planName, kind)
// with the given status, or ErrNotFound. The scheduler asks for
// BackupRunSucceeded: a failed run never counts as coverage, so a failing
// plan is retried on the next engine tick rather than waiting a full
// interval.
func (db *DB) LastBackupRun(planName string, kind BackupRunKind, status BackupRunStatus) (BackupRun, error) {
	row := db.sql.QueryRow(
		`SELECT seq, plan_name, run_kind, plan_id, started_at, finished_at, status, detail
		 FROM backup_runs WHERE plan_name = ? AND run_kind = ? AND status = ?
		 ORDER BY seq DESC LIMIT 1`,
		planName, string(kind), string(status))
	return scanBackupRun(row)
}

// ListBackupRuns returns the newest limit ledger rows across all plans,
// newest first — the Console/CLI history view.
func (db *DB) ListBackupRuns(limit int) ([]BackupRun, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.sql.Query(
		`SELECT seq, plan_name, run_kind, plan_id, started_at, finished_at, status, detail
		 FROM backup_runs ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing backup runs: %w", err)
	}
	defer rows.Close()
	var out []BackupRun
	for rows.Next() {
		r, err := scanBackupRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanBackupRun(row rowScanner) (BackupRun, error) {
	var r BackupRun
	var kind, status, started, finished string
	err := row.Scan(&r.Seq, &r.PlanName, &kind, &r.PlanID, &started, &finished, &status, &r.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupRun{}, ErrNotFound
	}
	if err != nil {
		return BackupRun{}, fmt.Errorf("runtimedb: scanning backup run: %w", err)
	}
	r.RunKind = BackupRunKind(kind)
	r.Status = BackupRunStatus(status)
	if r.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return BackupRun{}, fmt.Errorf("runtimedb: parsing backup run started_at: %w", err)
	}
	if finished != "" {
		if r.FinishedAt, err = time.Parse(time.RFC3339Nano, finished); err != nil {
			return BackupRun{}, fmt.Errorf("runtimedb: parsing backup run finished_at: %w", err)
		}
	}
	return r, nil
}

// LatestBackupRun returns the newest finished run of one kind, and
// false when there has never been one.
//
// A QUESTION OF ITS OWN RATHER THAN A FILTER OVER THE CONSOLE'S WINDOW.
// The household's reassurance card wants two facts — when the last
// backup succeeded, and how the last restore drill went — and it used
// to look for them among the newest hundred runs. Four daily backup
// plans put four hundred rows between monthly drills, so the drill was
// outside the window and the card lost both its date and its proof
// while a drill sat in the table; a hundred failures would have hidden
// the last good backup the same way. A card about whether the household
// could recover must not go quiet because the household backs up often.
//
// succeededOnly separates the two: the last SUCCESSFUL backup is the
// honest answer to "when was the last backup", while the last drill
// decides pass or fail whichever way it went — a household told "a
// restore was proved" by an older success while last night's drill
// failed has been reassured by the one thing this card exists for.
func (db *DB) LatestBackupRun(kind BackupRunKind, succeededOnly bool) (BackupRun, bool, error) {
	where := "run_kind = ? AND finished_at != ''"
	args := []any{string(kind)}
	if succeededOnly {
		where += " AND status = ?"
		args = append(args, string(BackupRunSucceeded))
	}
	row := db.sql.QueryRow(
		`SELECT seq, plan_name, run_kind, plan_id, started_at, finished_at, status, detail
		   FROM backup_runs WHERE `+where+` ORDER BY seq DESC LIMIT 1`, args...)
	r, err := scanBackupRun(row)
	// ErrNotFound rather than sql.ErrNoRows: scanBackupRun has already
	// translated it, and "there has never been one" is an answer here
	// rather than a failure — a household that has not run a drill yet
	// gets a card that says so, not one that says it cannot tell.
	if errors.Is(err, ErrNotFound) {
		return BackupRun{}, false, nil
	}
	if err != nil {
		return BackupRun{}, false, fmt.Errorf("runtimedb: reading the latest %s run: %w", kind, err)
	}
	return r, true, nil
}
