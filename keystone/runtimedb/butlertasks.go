package runtimedb

// Durable Butler work (ADR-W030 A4, work package P1).
//
// A task is one unit of work the Butler owes someone: a conversational
// request, or one occurrence of a Wildstone-owned routine. Both run on
// the same machinery, and both survive a restart, because the ledger is
// here rather than in the process.
//
// The rules this file exists to enforce, all of them from A4:
//
//   - A tool call is checkpointed BEFORE dispatch and again after its
//     result. StartButlerAttempt / FinishButlerAttempt are that pair.
//   - A result from a superseded attempt is rejected, not applied. Every
//     attempt carries the task's lease epoch at dispatch; finishing with
//     a stale one fails.
//   - Completion requires evidence. CompleteButlerTask writes the
//     evidence and the state change in one transaction, and the schema
//     refuses the transition if there is none — so a crash between the
//     two leaves work to finish, never a lie about finished work.
//   - Authority metadata is mandatory. A task with no owner, surface,
//     profile or audience cannot be permission-filtered later, so it is
//     refused at write time.
//
// This file stores work; it does not run it. The runner refactor is P3.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TaskEnvelopeVersion is the schema version stamped on every serialized
// payload written by this package. It is stored per row rather than
// inferred from the migration, so a later package can change a payload
// shape and still read what an older Keystone wrote (ADR-W030 A5.1).
const TaskEnvelopeVersion = 1

// ButlerTaskKind separates a conversational task from a routine run.
// They share execution machinery; the routine's DEFINITION lives
// elsewhere and each occurrence is its own task (ADR-W030 A4).
type ButlerTaskKind string

const (
	ButlerTaskConversation ButlerTaskKind = "conversation"
	ButlerTaskRoutine      ButlerTaskKind = "routine"
)

// ButlerTaskState is the lifecycle of one task.
type ButlerTaskState string

const (
	ButlerTaskPending   ButlerTaskState = "pending"
	ButlerTaskRunning   ButlerTaskState = "running"
	ButlerTaskWaiting   ButlerTaskState = "waiting"
	ButlerTaskCompleted ButlerTaskState = "completed"
	ButlerTaskFailed    ButlerTaskState = "failed"
	ButlerTaskCancelled ButlerTaskState = "cancelled"
	// ButlerTaskPausedOffboarded is where an offboarded member's
	// unfinished personal work goes: no retries, no notices, no model
	// calls, and no deletion of what it already recorded (A4).
	ButlerTaskPausedOffboarded ButlerTaskState = "paused-offboarded"
)

// butlerTaskTransitions is the closed state machine. A transition absent
// here is a bug in the caller, refused rather than recorded — the same
// treatment plans get.
//
// paused-offboarded can resume because a SHARED task transfers to the
// administrator and resumes after grant review; a personal one simply is
// never resumed by policy above this layer (A4).
var butlerTaskTransitions = map[ButlerTaskState][]ButlerTaskState{
	ButlerTaskPending: {ButlerTaskRunning, ButlerTaskCancelled, ButlerTaskPausedOffboarded},
	ButlerTaskRunning: {ButlerTaskWaiting, ButlerTaskCompleted, ButlerTaskFailed,
		ButlerTaskCancelled, ButlerTaskPausedOffboarded},
	ButlerTaskWaiting: {ButlerTaskRunning, ButlerTaskFailed, ButlerTaskCancelled,
		ButlerTaskPausedOffboarded},
	ButlerTaskPausedOffboarded: {ButlerTaskRunning, ButlerTaskCancelled},
}

// ButlerTaskAudience is the disclosure label every task, evidence row
// and trace event carries. Shared history does not mean shared
// disclosure (ADR-W030 A2), and a row whose audience is unknown can
// never be safely shown, so it is never written.
type ButlerTaskAudience string

const (
	ButlerAudiencePrivate ButlerTaskAudience = "private"
	ButlerAudienceShared  ButlerTaskAudience = "shared"
)

// ButlerTaskRetention is the configured lifetime of the task record.
// The default across the product is manual deletion (ADR-W030 A10);
// "conversation" follows the originating conversation's policy.
type ButlerTaskRetention string

const (
	ButlerRetentionManual       ButlerTaskRetention = "manual"
	ButlerRetentionConversation ButlerTaskRetention = "conversation"
	ButlerRetentionEphemeral    ButlerTaskRetention = "ephemeral"
)

// ButlerTask is one durable unit of work.
//
// Author never changes; Owner does, when offboarding transfers a shared
// task to the administrator. Surface and Profile are snapshotted at
// admission: a resume rechecks authority against what the task claimed,
// which is only meaningful if the claim was written down.
type ButlerTask struct {
	Seq       int64           `json:"seq"`
	ID        string          `json:"id"` // "bt-<seq>"
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Kind      ButlerTaskKind  `json:"kind"`
	State     ButlerTaskState `json:"state"`

	Author string `json:"author"`
	Owner  string `json:"owner"`

	Surface  ButlerSurface      `json:"surface"`
	Profile  string             `json:"profile"`
	Audience ButlerTaskAudience `json:"audience"`

	// Session and OriginTurn point at the existing conversation store.
	// Nothing in butler_sessions is rewritten to carry a task id: the
	// link is one-way, so a conversation's attribution stays exactly what
	// it was when it was written (ADR-W030 P1).
	SessionID  string `json:"sessionId,omitempty"` // "bs-<seq>", empty for routines
	OriginTurn *int   `json:"originTurn,omitempty"`

	// CorrelationID ties a routine run to the W024 household timeline.
	// The timeline stays authoritative for deterministic outcomes;
	// anything mirrored onto the task is a projection (A4).
	CorrelationID string `json:"correlationId,omitempty"`

	Objective          string `json:"objective"`
	CompletionCriteria string `json:"completionCriteria"`

	GrantsJSON   string    `json:"grantsJson,omitempty"`
	BudgetJSON   string    `json:"budgetJson,omitempty"`
	DeadlineAt   time.Time `json:"deadlineAt,omitzero"`
	NextWakeupAt time.Time `json:"nextWakeupAt,omitzero"`

	LeaseEpoch int64 `json:"leaseEpoch"`

	// Uncertainty is set when an action's outcome could not be
	// established. A resume reads it and pauses rather than blindly
	// retrying an effect that may already have happened (A4).
	Uncertainty   string `json:"uncertainty,omitempty"`
	ResultSummary string `json:"resultSummary,omitempty"`

	Retention       ButlerTaskRetention `json:"retention"`
	EnvelopeVersion int                 `json:"envelopeVersion"`
}

func butlerTaskID(seq int64) string { return fmt.Sprintf("bt-%d", seq) }

// parseButlerTaskID inverts butlerTaskID; it rejects anything else.
func parseButlerTaskID(id string) (int64, error) {
	var seq int64
	if _, err := fmt.Sscanf(id, "bt-%d", &seq); err != nil || butlerTaskID(seq) != id {
		return 0, fmt.Errorf("runtimedb: malformed butler task id %q", id)
	}
	return seq, nil
}

// InsertButlerTask records a new task and returns its "bt-<seq>" id.
//
// It refuses a task missing any of the fields a later permission check
// needs — owner, author, surface, profile, audience, objective — because
// the alternative is a row that cannot be filtered, shown, or resumed.
// State is always pending: a task that begins life finished is the
// partial-write failure this schema exists to prevent.
func (db *DB) InsertButlerTask(t ButlerTask) (string, error) {
	if t.Kind != ButlerTaskConversation && t.Kind != ButlerTaskRoutine {
		return "", fmt.Errorf("runtimedb: butler task kind must be conversation or routine, got %q", t.Kind)
	}
	if t.Author == "" || t.Owner == "" {
		return "", fmt.Errorf("runtimedb: butler task requires author and owner (ADR-W030 A4)")
	}
	if !ValidButlerSurface(t.Surface) {
		return "", fmt.Errorf("runtimedb: butler task requires a valid surface (admin|family), got %q (ADR-W026 A1)", t.Surface)
	}
	if t.Profile == "" {
		return "", fmt.Errorf("runtimedb: butler task requires a profile (ADR-W030 A1)")
	}
	if t.Audience != ButlerAudiencePrivate && t.Audience != ButlerAudienceShared {
		return "", fmt.Errorf("runtimedb: butler task requires audience private|shared, got %q (ADR-W030 A2)", t.Audience)
	}
	if t.Objective == "" {
		return "", fmt.Errorf("runtimedb: butler task requires an objective (ADR-W030 A4)")
	}
	if t.Retention == "" {
		t.Retention = ButlerRetentionManual
	}
	switch t.Retention {
	case ButlerRetentionManual, ButlerRetentionConversation, ButlerRetentionEphemeral:
	default:
		return "", fmt.Errorf("runtimedb: butler task retention must be manual|conversation|ephemeral, got %q (ADR-W030 A10)", t.Retention)
	}
	grants, err := jsonObjectOrEmpty(t.GrantsJSON, "grants")
	if err != nil {
		return "", err
	}
	budget, err := jsonObjectOrEmpty(t.BudgetJSON, "budget")
	if err != nil {
		return "", err
	}
	var session any
	if t.SessionID != "" {
		var seq int64
		if _, err := fmt.Sscanf(t.SessionID, "bs-%d", &seq); err != nil {
			return "", fmt.Errorf("runtimedb: malformed butler session id %q", t.SessionID)
		}
		session = seq
	}
	var originTurn any
	if t.OriginTurn != nil {
		originTurn = *t.OriginTurn
	}

	stamp := now()
	tx, err := db.sql.Begin()
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`INSERT INTO butler_tasks (created_at, updated_at, kind, state, author, owner,
		     surface, profile, audience, session, origin_turn, correlation_id, objective,
		     completion_criteria, grants_json, budget_json, deadline_at, next_wakeup_at,
		     lease_epoch, uncertainty, result_summary, retention, envelope_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', ?, ?)`,
		stamp, stamp, string(t.Kind), string(ButlerTaskPending), t.Author, t.Owner,
		string(t.Surface), t.Profile, string(t.Audience), session, originTurn,
		t.CorrelationID, t.Objective, t.CompletionCriteria, grants, budget,
		timeText(t.DeadlineAt), timeText(t.NextWakeupAt),
		string(t.Retention), TaskEnvelopeVersion)
	if err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	// The opening transition is written with the row so the history is
	// complete from the first state, not from the first change.
	if _, err := tx.Exec(
		`INSERT INTO butler_task_transitions (task, at, from_state, to_state, reason, actor)
		 VALUES (?, ?, '', ?, 'created', ?)`,
		seq, stamp, string(ButlerTaskPending), t.Author); err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("runtimedb: inserting butler task: %w", err)
	}
	return butlerTaskID(seq), nil
}

// GetButlerTask returns one task by id.
func (db *DB) GetButlerTask(id string) (ButlerTask, error) {
	seq, err := parseButlerTaskID(id)
	if err != nil {
		return ButlerTask{}, err
	}
	return scanButlerTask(db.sql.QueryRow(butlerTaskColumns+` WHERE seq = ?`, seq))
}

const butlerTaskColumns = `SELECT seq, created_at, updated_at, kind, state, author, owner,
	 surface, profile, audience, session, origin_turn, correlation_id, objective,
	 completion_criteria, grants_json, budget_json, deadline_at, next_wakeup_at,
	 lease_epoch, uncertainty, result_summary, retention, envelope_version
	 FROM butler_tasks`

func scanButlerTask(row rowScanner) (ButlerTask, error) {
	var t ButlerTask
	var created, updated, kind, state, surface, audience, retention string
	var deadline, wakeup string
	var session sql.NullInt64
	var originTurn sql.NullInt64
	err := row.Scan(&t.Seq, &created, &updated, &kind, &state, &t.Author, &t.Owner,
		&surface, &t.Profile, &audience, &session, &originTurn, &t.CorrelationID,
		&t.Objective, &t.CompletionCriteria, &t.GrantsJSON, &t.BudgetJSON,
		&deadline, &wakeup, &t.LeaseEpoch, &t.Uncertainty, &t.ResultSummary,
		&retention, &t.EnvelopeVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerTask{}, ErrNotFound
	}
	if err != nil {
		return ButlerTask{}, fmt.Errorf("runtimedb: scanning butler task: %w", err)
	}
	t.ID = butlerTaskID(t.Seq)
	t.Kind = ButlerTaskKind(kind)
	t.State = ButlerTaskState(state)
	t.Surface = ButlerSurface(surface)
	t.Audience = ButlerTaskAudience(audience)
	t.Retention = ButlerTaskRetention(retention)
	if session.Valid {
		t.SessionID = butlerSessionID(session.Int64)
	}
	if originTurn.Valid {
		n := int(originTurn.Int64)
		t.OriginTurn = &n
	}
	for _, f := range []struct {
		text string
		into *time.Time
		name string
	}{
		{created, &t.CreatedAt, "created_at"},
		{updated, &t.UpdatedAt, "updated_at"},
		{deadline, &t.DeadlineAt, "deadline_at"},
		{wakeup, &t.NextWakeupAt, "next_wakeup_at"},
	} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return ButlerTask{}, fmt.Errorf("runtimedb: bad butler task %s %q: %w", f.name, f.text, err)
		}
		*f.into = parsed
	}
	return t, nil
}

// ListButlerTasks returns the newest limit tasks, newest first. An empty
// owner lists every owner's; naming one lists that member's work.
func (db *DB) ListButlerTasks(limit int, owner string) ([]ButlerTask, error) {
	if limit <= 0 {
		limit = 50
	}
	where, args := "", []any{}
	if owner != "" {
		where = " WHERE owner = ?"
		args = append(args, owner)
	}
	args = append(args, limit)
	rows, err := db.sql.Query(butlerTaskColumns+where+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler tasks: %w", err)
	}
	defer rows.Close()
	return collectButlerTasks(rows)
}

// ButlerTasksInState returns every task in state, oldest first — the
// resume queue after a restart.
func (db *DB) ButlerTasksInState(state ButlerTaskState) ([]ButlerTask, error) {
	rows, err := db.sql.Query(butlerTaskColumns+` WHERE state = ? ORDER BY seq`, string(state))
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler tasks in %s: %w", state, err)
	}
	defer rows.Close()
	return collectButlerTasks(rows)
}

func collectButlerTasks(rows *sql.Rows) ([]ButlerTask, error) {
	var out []ButlerTask
	for rows.Next() {
		t, err := scanButlerTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TransitionButlerTask moves a task to a new state, enforcing the closed
// table and appending to the transition history in the same transaction.
//
// It deliberately cannot reach ButlerTaskCompleted: completion needs
// evidence, so it goes through CompleteButlerTask. The schema trigger
// enforces the same thing a layer down, for anything that reaches SQL by
// another route.
func (db *DB) TransitionButlerTask(id string, to ButlerTaskState, reason, actor string) error {
	if to == ButlerTaskCompleted {
		return fmt.Errorf("runtimedb: butler task %s: completion requires evidence — use CompleteButlerTask (ADR-W030 A4)", id)
	}
	seq, err := parseButlerTaskID(id)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: transition butler task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := transitionButlerTaskTx(tx, seq, to, reason, actor); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: transition butler task: %w", err)
	}
	return nil
}

func transitionButlerTaskTx(tx *sql.Tx, seq int64, to ButlerTaskState, reason, actor string) error {
	var cur string
	err := tx.QueryRow(`SELECT state FROM butler_tasks WHERE seq = ?`, seq).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: transition butler task: %w", err)
	}
	allowed := false
	for _, next := range butlerTaskTransitions[ButlerTaskState(cur)] {
		if next == to {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("runtimedb: butler task %s: illegal transition %s → %s",
			butlerTaskID(seq), cur, to)
	}
	stamp := now()
	if _, err := tx.Exec(`UPDATE butler_tasks SET state = ?, updated_at = ? WHERE seq = ?`,
		string(to), stamp, seq); err != nil {
		return fmt.Errorf("runtimedb: transition butler task: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO butler_task_transitions (task, at, from_state, to_state, reason, actor)
		 VALUES (?, ?, ?, ?, ?, ?)`, seq, stamp, cur, string(to), reason, actor); err != nil {
		return fmt.Errorf("runtimedb: recording butler task transition: %w", err)
	}
	return nil
}

// ButlerTaskTransition is one row of a task's append-only history.
type ButlerTaskTransition struct {
	At        time.Time `json:"at"`
	FromState string    `json:"fromState"`
	ToState   string    `json:"toState"`
	Reason    string    `json:"reason,omitempty"`
	Actor     string    `json:"actor,omitempty"`
}

// ListButlerTaskTransitions returns a task's history, oldest first.
func (db *DB) ListButlerTaskTransitions(id string) ([]ButlerTaskTransition, error) {
	seq, err := parseButlerTaskID(id)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT at, from_state, to_state, reason, actor
		 FROM butler_task_transitions WHERE task = ? ORDER BY seq`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler task transitions: %w", err)
	}
	defer rows.Close()
	var out []ButlerTaskTransition
	for rows.Next() {
		var t ButlerTaskTransition
		var at string
		if err := rows.Scan(&at, &t.FromState, &t.ToState, &t.Reason, &t.Actor); err != nil {
			return nil, err
		}
		if t.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("runtimedb: bad butler transition timestamp %q: %w", at, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// jsonObjectOrEmpty validates an optional JSON payload, defaulting to
// "{}". A payload that does not parse is refused here rather than
// becoming a row nothing downstream can read.
func jsonObjectOrEmpty(raw, field string) (string, error) {
	if raw == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(raw)) {
		return "", fmt.Errorf("runtimedb: butler task %s is not valid JSON", field)
	}
	return raw, nil
}

// timeText renders an optional timestamp in the storage format; the zero
// time becomes "", which is how the schema spells "unset".
func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
