package runtimedb

// The step ledger a Butler task resumes from (ADR-W030 A4, P1).
//
// Steps are what the task decided to do; attempts are the individual
// tries. A resume reads verified steps and does NOT repeat them, and
// reconstructs context from these rows with current permissions rather
// than replaying a saved model transcript — which is why a step records
// what it was and what came back, not how the model felt about it.
//
// Every attempt carries the task's lease epoch at dispatch. Bumping the
// lease (a stall deadline firing, a coordinator promotion) fences every
// attempt still in flight, and a result that arrives afterwards is
// refused instead of overwriting state the fencing already settled.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ButlerStepKind is what a step does. The set is closed so a Console
// timeline can render each one without guessing.
type ButlerStepKind string

const (
	ButlerStepModel    ButlerStepKind = "model"
	ButlerStepTool     ButlerStepKind = "tool"
	ButlerStepWait     ButlerStepKind = "wait"
	ButlerStepDelivery ButlerStepKind = "delivery"
)

// ButlerStepState is a step's position in the ledger. "verified" is the
// load-bearing one: it means the effect was established, and a resume
// may skip it.
type ButlerStepState string

const (
	ButlerStepProposed ButlerStepState = "proposed"
	ButlerStepRunning  ButlerStepState = "running"
	ButlerStepVerified ButlerStepState = "verified"
	ButlerStepFailed   ButlerStepState = "failed"
	ButlerStepSkipped  ButlerStepState = "skipped"
)

// ButlerAttemptState is one try's outcome. "stalled" and "fenced" are
// distinct on purpose: a stall is a provider that stopped answering, a
// fence is a result this coordinator will no longer accept.
type ButlerAttemptState string

const (
	ButlerAttemptDispatched ButlerAttemptState = "dispatched"
	ButlerAttemptSucceeded  ButlerAttemptState = "succeeded"
	ButlerAttemptFailed     ButlerAttemptState = "failed"
	ButlerAttemptStalled    ButlerAttemptState = "stalled"
	ButlerAttemptFenced     ButlerAttemptState = "fenced"
	ButlerAttemptCancelled  ButlerAttemptState = "cancelled"
)

// ErrAttemptFenced is returned when a result arrives for an attempt the
// task has already superseded. Callers must drop the result: it belongs
// to a lease that no longer holds (ADR-W030 A4, A5).
var ErrAttemptFenced = errors.New("runtimedb: butler attempt fenced by a newer lease epoch")

// ButlerTaskStep is one entry in a task's step ledger.
type ButlerTaskStep struct {
	Seq             int64           `json:"seq"`
	Ordinal         int             `json:"ordinal"`
	Kind            ButlerStepKind  `json:"kind"`
	Name            string          `json:"name"`
	State           ButlerStepState `json:"state"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
	ParamsJSON      string          `json:"paramsJson,omitempty"`
	EnvelopeVersion int             `json:"envelopeVersion"`
}

// ButlerTaskAttempt is one try at a step, checkpointed before dispatch
// and again when its result lands.
type ButlerTaskAttempt struct {
	Seq             int64              `json:"seq"`
	TaskID          string             `json:"taskId"`
	Step            int64              `json:"step"`
	Attempt         int                `json:"attempt"`
	Epoch           int64              `json:"epoch"`
	State           ButlerAttemptState `json:"state"`
	StartedAt       time.Time          `json:"startedAt"`
	EndedAt         time.Time          `json:"endedAt,omitzero"`
	ResultJSON      string             `json:"resultJson,omitempty"`
	ErrorText       string             `json:"errorText,omitempty"`
	EnvelopeVersion int                `json:"envelopeVersion"`
}

// AppendButlerTaskStep adds a step at the next ordinal and returns its
// row id. Steps are appended, never renumbered: an ordinal is a stable
// reference a trace event and a Console timeline both point at.
func (db *DB) AppendButlerTaskStep(taskID string, s ButlerTaskStep) (int64, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return 0, err
	}
	switch s.Kind {
	case ButlerStepModel, ButlerStepTool, ButlerStepWait, ButlerStepDelivery:
	default:
		return 0, fmt.Errorf("runtimedb: butler step kind must be model|tool|wait|delivery, got %q", s.Kind)
	}
	if s.Name == "" {
		return 0, fmt.Errorf("runtimedb: butler step requires a name")
	}
	params, err := jsonObjectOrEmpty(s.ParamsJSON, "step params")
	if err != nil {
		return 0, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: appending butler step: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM butler_tasks WHERE seq = ?`, seq).Scan(&exists); err != nil {
		return 0, fmt.Errorf("runtimedb: appending butler step: %w", err)
	}
	if exists == 0 {
		return 0, ErrNotFound
	}
	var ordinal int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(ordinal) + 1, 0) FROM butler_task_steps WHERE task = ?`, seq,
	).Scan(&ordinal); err != nil {
		return 0, fmt.Errorf("runtimedb: appending butler step: %w", err)
	}
	stamp := now()
	res, err := tx.Exec(
		`INSERT INTO butler_task_steps (task, ordinal, kind, name, state, created_at, updated_at, params_json, envelope_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seq, ordinal, string(s.Kind), s.Name, string(ButlerStepProposed), stamp, stamp,
		params, TaskEnvelopeVersion)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: appending butler step: %w", err)
	}
	stepSeq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: appending butler step: %w", err)
	}
	return stepSeq, nil
}

// SetButlerStepState records a step's outcome.
func (db *DB) SetButlerStepState(stepSeq int64, state ButlerStepState) error {
	switch state {
	case ButlerStepProposed, ButlerStepRunning, ButlerStepVerified, ButlerStepFailed, ButlerStepSkipped:
	default:
		return fmt.Errorf("runtimedb: unknown butler step state %q", state)
	}
	res, err := db.sql.Exec(
		`UPDATE butler_task_steps SET state = ?, updated_at = ? WHERE seq = ?`,
		string(state), now(), stepSeq)
	if err != nil {
		return fmt.Errorf("runtimedb: setting butler step state: %w", err)
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

// ListButlerTaskSteps returns a task's ledger, oldest first.
func (db *DB) ListButlerTaskSteps(taskID string) ([]ButlerTaskStep, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, ordinal, kind, name, state, created_at, updated_at, params_json, envelope_version
		 FROM butler_task_steps WHERE task = ? ORDER BY ordinal`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler steps: %w", err)
	}
	defer rows.Close()
	var out []ButlerTaskStep
	for rows.Next() {
		var s ButlerTaskStep
		var kind, state, created, updated string
		if err := rows.Scan(&s.Seq, &s.Ordinal, &kind, &s.Name, &state, &created, &updated,
			&s.ParamsJSON, &s.EnvelopeVersion); err != nil {
			return nil, err
		}
		s.Kind = ButlerStepKind(kind)
		s.State = ButlerStepState(state)
		if s.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("runtimedb: bad butler step created_at %q: %w", created, err)
		}
		if s.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
			return nil, fmt.Errorf("runtimedb: bad butler step updated_at %q: %w", updated, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// StartButlerAttempt is the checkpoint BEFORE a dispatch: it records
// that a call is about to go out, stamped with the task's current lease
// epoch, and returns the attempt to finish afterwards.
//
// Writing this row first is what makes an interrupted dispatch legible.
// A process killed between here and the result leaves an attempt in
// "dispatched" — an outcome that is unknown, which the resume path must
// treat as uncertain rather than retry (ADR-W030 A4).
func (db *DB) StartButlerAttempt(stepSeq int64) (ButlerTaskAttempt, error) {
	// This is the dispatch: a call is about to go out. Gating here
	// rather than at the task transition catches every model and tool
	// call, whichever caller drove it.
	if err := db.gate(GateDispatch); err != nil {
		return ButlerTaskAttempt{}, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return ButlerTaskAttempt{}, fmt.Errorf("runtimedb: starting butler attempt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var task, epoch int64
	err = tx.QueryRow(
		`SELECT t.seq, t.lease_epoch FROM butler_task_steps s
		 JOIN butler_tasks t ON t.seq = s.task WHERE s.seq = ?`, stepSeq).Scan(&task, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerTaskAttempt{}, ErrNotFound
	}
	if err != nil {
		return ButlerTaskAttempt{}, fmt.Errorf("runtimedb: starting butler attempt: %w", err)
	}
	var attempt int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(attempt) + 1, 1) FROM butler_task_attempts WHERE step = ?`, stepSeq,
	).Scan(&attempt); err != nil {
		return ButlerTaskAttempt{}, fmt.Errorf("runtimedb: starting butler attempt: %w", err)
	}
	stamp := now()
	res, err := tx.Exec(
		`INSERT INTO butler_task_attempts (task, step, attempt, epoch, state, started_at, envelope_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		task, stepSeq, attempt, epoch, string(ButlerAttemptDispatched), stamp, TaskEnvelopeVersion)
	if err != nil {
		return ButlerTaskAttempt{}, fmt.Errorf("runtimedb: starting butler attempt: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE butler_task_steps SET state = ?, updated_at = ? WHERE seq = ?`,
		string(ButlerStepRunning), stamp, stepSeq); err != nil {
		return ButlerTaskAttempt{}, fmt.Errorf("runtimedb: starting butler attempt: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return ButlerTaskAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return ButlerTaskAttempt{}, fmt.Errorf("runtimedb: starting butler attempt: %w", err)
	}
	started, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return ButlerTaskAttempt{}, err
	}
	return ButlerTaskAttempt{
		Seq: seq, TaskID: butlerTaskID(task), Step: stepSeq, Attempt: attempt,
		Epoch: epoch, State: ButlerAttemptDispatched, StartedAt: started,
		EnvelopeVersion: TaskEnvelopeVersion,
	}, nil
}

// FinishButlerAttempt is the checkpoint AFTER a result. It refuses two
// things a naive UPDATE would accept: a result for an attempt whose
// epoch the task has already moved past (ErrAttemptFenced), and a second
// result for an attempt that is no longer dispatched — a duplicate
// delivery, which must not reopen settled state.
func (db *DB) FinishButlerAttempt(attemptSeq int64, state ButlerAttemptState, resultJSON, errText string) error {
	switch state {
	case ButlerAttemptSucceeded, ButlerAttemptFailed, ButlerAttemptStalled, ButlerAttemptCancelled:
	default:
		return fmt.Errorf("runtimedb: butler attempt cannot finish as %q", state)
	}
	result, err := jsonObjectOrEmpty(resultJSON, "attempt result")
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: finishing butler attempt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var cur string
	var attemptEpoch, taskEpoch int64
	err = tx.QueryRow(
		`SELECT a.state, a.epoch, t.lease_epoch FROM butler_task_attempts a
		 JOIN butler_tasks t ON t.seq = a.task WHERE a.seq = ?`, attemptSeq,
	).Scan(&cur, &attemptEpoch, &taskEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: finishing butler attempt: %w", err)
	}
	if attemptEpoch < taskEpoch {
		return fmt.Errorf("%w: attempt epoch %d, task epoch %d", ErrAttemptFenced, attemptEpoch, taskEpoch)
	}
	if ButlerAttemptState(cur) != ButlerAttemptDispatched {
		return fmt.Errorf("runtimedb: butler attempt %d already finished as %s", attemptSeq, cur)
	}
	if _, err := tx.Exec(
		`UPDATE butler_task_attempts SET state = ?, ended_at = ?, result_json = ?, error_text = ? WHERE seq = ?`,
		string(state), now(), result, errText, attemptSeq); err != nil {
		return fmt.Errorf("runtimedb: finishing butler attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: finishing butler attempt: %w", err)
	}
	return nil
}

// FenceButlerTaskAttempts bumps the task's lease epoch and marks every
// still-dispatched attempt fenced, returning the new epoch.
//
// This is the one operation that makes a stall recoverable: the
// abandoned attempt is cancelled here, a fresh one starts at the new
// epoch, and if the old provider ever answers, FinishButlerAttempt
// refuses it (ADR-W030 A4).
func (db *DB) FenceButlerTaskAttempts(taskID string) (int64, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return 0, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: fencing butler attempts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var epoch int64
	err = tx.QueryRow(`SELECT lease_epoch FROM butler_tasks WHERE seq = ?`, seq).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("runtimedb: fencing butler attempts: %w", err)
	}
	epoch++
	stamp := now()
	if _, err := tx.Exec(`UPDATE butler_tasks SET lease_epoch = ?, updated_at = ? WHERE seq = ?`,
		epoch, stamp, seq); err != nil {
		return 0, fmt.Errorf("runtimedb: fencing butler attempts: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE butler_task_attempts SET state = ?, ended_at = ? WHERE task = ? AND state = ?`,
		string(ButlerAttemptFenced), stamp, seq, string(ButlerAttemptDispatched)); err != nil {
		return 0, fmt.Errorf("runtimedb: fencing butler attempts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: fencing butler attempts: %w", err)
	}
	return epoch, nil
}

// ListButlerTaskAttempts returns every attempt on a task, oldest first.
func (db *DB) ListButlerTaskAttempts(taskID string) ([]ButlerTaskAttempt, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, step, attempt, epoch, state, started_at, ended_at, result_json, error_text, envelope_version
		 FROM butler_task_attempts WHERE task = ? ORDER BY seq`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler attempts: %w", err)
	}
	defer rows.Close()
	var out []ButlerTaskAttempt
	for rows.Next() {
		a := ButlerTaskAttempt{TaskID: taskID}
		var state, started, ended string
		if err := rows.Scan(&a.Seq, &a.Step, &a.Attempt, &a.Epoch, &state, &started, &ended,
			&a.ResultJSON, &a.ErrorText, &a.EnvelopeVersion); err != nil {
			return nil, err
		}
		a.State = ButlerAttemptState(state)
		if a.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return nil, fmt.Errorf("runtimedb: bad butler attempt started_at %q: %w", started, err)
		}
		if ended != "" {
			if a.EndedAt, err = time.Parse(time.RFC3339Nano, ended); err != nil {
				return nil, fmt.Errorf("runtimedb: bad butler attempt ended_at %q: %w", ended, err)
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ButlerWaitReason is the closed set of reasons a task waits. Console
// has to tell a stalled provider apart from a person who has not
// answered yet, so the reason is a column, not prose (ADR-W030 A4).
type ButlerWaitReason string

const (
	ButlerWaitReply             ButlerWaitReason = "reply"
	ButlerWaitApproval          ButlerWaitReason = "approval"
	ButlerWaitSchedule          ButlerWaitReason = "schedule"
	ButlerWaitModelUnavailable  ButlerWaitReason = "model-unavailable"
	ButlerWaitBudgetExhausted   ButlerWaitReason = "budget-exhausted"
	ButlerWaitClarification     ButlerWaitReason = "clarification"
	ButlerWaitSourceUnavailable ButlerWaitReason = "source-unavailable"
	ButlerWaitUncertainOutcome  ButlerWaitReason = "uncertain-outcome"
)

// ButlerTaskWait is one recorded waiting reason.
type ButlerTaskWait struct {
	Seq        int64            `json:"seq"`
	CreatedAt  time.Time        `json:"createdAt"`
	Reason     ButlerWaitReason `json:"reason"`
	Detail     string           `json:"detail,omitempty"`
	ResumeAt   time.Time        `json:"resumeAt,omitzero"`
	ResolvedAt time.Time        `json:"resolvedAt,omitzero"`
	ResolvedBy string           `json:"resolvedBy,omitempty"`
}

// OpenButlerTaskWait moves a task to waiting and records why, in one
// transaction. The schema allows only one open wait per task: a task
// waiting on two things has no defined resume.
func (db *DB) OpenButlerTaskWait(taskID string, w ButlerTaskWait) error {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	switch w.Reason {
	case ButlerWaitReply, ButlerWaitApproval, ButlerWaitSchedule, ButlerWaitModelUnavailable,
		ButlerWaitBudgetExhausted, ButlerWaitClarification, ButlerWaitSourceUnavailable,
		ButlerWaitUncertainOutcome:
	default:
		return fmt.Errorf("runtimedb: unknown butler wait reason %q (ADR-W030 A4)", w.Reason)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: opening butler task wait: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := transitionButlerTaskTx(tx, seq, ButlerTaskWaiting, string(w.Reason), ""); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO butler_task_waits (task, created_at, reason, detail, resume_at)
		 VALUES (?, ?, ?, ?, ?)`,
		seq, now(), string(w.Reason), w.Detail, timeText(w.ResumeAt)); err != nil {
		return fmt.Errorf("runtimedb: opening butler task wait: %w", err)
	}
	// next_wakeup_at is the scheduler's index; keeping it on the task
	// means "what is due" is one indexed read, not a join.
	if _, err := tx.Exec(`UPDATE butler_tasks SET next_wakeup_at = ? WHERE seq = ?`,
		timeText(w.ResumeAt), seq); err != nil {
		return fmt.Errorf("runtimedb: opening butler task wait: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: opening butler task wait: %w", err)
	}
	return nil
}

// ResolveButlerTaskWait closes the open wait and returns the task to
// running. resolvedBy names who or what unblocked it — a person's reply,
// or "" when the schedule simply came due.
func (db *DB) ResolveButlerTaskWait(taskID, resolvedBy, reason string) error {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: resolving butler task wait: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`UPDATE butler_task_waits SET resolved_at = ?, resolved_by = ?
		 WHERE task = ? AND resolved_at = ''`, now(), resolvedBy, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: resolving butler task wait: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := transitionButlerTaskTx(tx, seq, ButlerTaskRunning, reason, resolvedBy); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE butler_tasks SET next_wakeup_at = '' WHERE seq = ?`, seq); err != nil {
		return fmt.Errorf("runtimedb: resolving butler task wait: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: resolving butler task wait: %w", err)
	}
	return nil
}

// ListButlerTaskWaits returns a task's waits, oldest first.
func (db *DB) ListButlerTaskWaits(taskID string) ([]ButlerTaskWait, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, created_at, reason, detail, resume_at, resolved_at, resolved_by
		 FROM butler_task_waits WHERE task = ? ORDER BY seq`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler task waits: %w", err)
	}
	defer rows.Close()
	var out []ButlerTaskWait
	for rows.Next() {
		var w ButlerTaskWait
		var reason, created, resume, resolved string
		if err := rows.Scan(&w.Seq, &created, &reason, &w.Detail, &resume, &resolved, &w.ResolvedBy); err != nil {
			return nil, err
		}
		w.Reason = ButlerWaitReason(reason)
		for _, f := range []struct {
			text string
			into *time.Time
			name string
		}{
			{created, &w.CreatedAt, "created_at"},
			{resume, &w.ResumeAt, "resume_at"},
			{resolved, &w.ResolvedAt, "resolved_at"},
		} {
			if f.text == "" {
				continue
			}
			parsed, err := time.Parse(time.RFC3339Nano, f.text)
			if err != nil {
				return nil, fmt.Errorf("runtimedb: bad butler wait %s %q: %w", f.name, f.text, err)
			}
			*f.into = parsed
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ButlerEvidenceKind is what sort of proof a completion rests on.
type ButlerEvidenceKind string

const (
	ButlerEvidenceObservation          ButlerEvidenceKind = "observation"
	ButlerEvidenceToolResult           ButlerEvidenceKind = "tool-result"
	ButlerEvidenceApproval             ButlerEvidenceKind = "approval"
	ButlerEvidenceDelivery             ButlerEvidenceKind = "delivery"
	ButlerEvidenceExternalVerification ButlerEvidenceKind = "external-verification"
)

// ButlerTaskEvidence is one observation that justifies calling work
// done. PermissionLabel and Audience are mandatory: evidence that cannot
// be permission-filtered cannot be shown to anyone, so it is refused
// rather than written (ADR-W030 A11).
type ButlerTaskEvidence struct {
	Seq             int64              `json:"seq"`
	RecordedAt      time.Time          `json:"recordedAt"`
	Kind            ButlerEvidenceKind `json:"kind"`
	Description     string             `json:"description"`
	SourceID        string             `json:"sourceId,omitempty"`
	ObservedAt      time.Time          `json:"observedAt,omitzero"`
	PermissionLabel string             `json:"permissionLabel"`
	Audience        ButlerTaskAudience `json:"audience"`
	PayloadJSON     string             `json:"payloadJson,omitempty"`
	EnvelopeVersion int                `json:"envelopeVersion"`
}

func validateEvidence(e ButlerTaskEvidence) (string, error) {
	switch e.Kind {
	case ButlerEvidenceObservation, ButlerEvidenceToolResult, ButlerEvidenceApproval,
		ButlerEvidenceDelivery, ButlerEvidenceExternalVerification:
	default:
		return "", fmt.Errorf("runtimedb: unknown butler evidence kind %q", e.Kind)
	}
	if e.Description == "" {
		return "", fmt.Errorf("runtimedb: butler evidence requires a description (ADR-W030 A4)")
	}
	if e.PermissionLabel == "" {
		return "", fmt.Errorf("runtimedb: butler evidence requires a permission label (ADR-W030 A11)")
	}
	// Non-empty only, for the reason validateMemory gives: the closed
	// set of audiences lives in the `audiences` table now, and the
	// foreign key on this column is the backstop.
	if e.Audience == "" {
		return "", fmt.Errorf("runtimedb: butler evidence requires an audience — who it may be disclosed to (ADR-W030 A2, design §10 layer 3)")
	}
	return jsonObjectOrEmpty(e.PayloadJSON, "evidence payload")
}

func insertEvidenceTx(tx *sql.Tx, task int64, e ButlerTaskEvidence) error {
	payload, err := validateEvidence(e)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO butler_task_evidence (task, recorded_at, kind, description, source_id,
		     observed_at, permission_label, audience, payload_json, envelope_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task, now(), string(e.Kind), e.Description, e.SourceID, timeText(e.ObservedAt),
		e.PermissionLabel, string(e.Audience), payload, TaskEnvelopeVersion); err != nil {
		return fmt.Errorf("runtimedb: inserting butler evidence: %w", err)
	}
	return nil
}

// AppendButlerTaskEvidence records one piece of completion evidence
// without changing the task's state.
func (db *DB) AppendButlerTaskEvidence(taskID string, e ButlerTaskEvidence) error {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: appending butler evidence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM butler_tasks WHERE seq = ?`, seq).Scan(&exists); err != nil {
		return fmt.Errorf("runtimedb: appending butler evidence: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if err := insertEvidenceTx(tx, seq, e); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: appending butler evidence: %w", err)
	}
	return nil
}

// ListButlerTaskEvidence returns a task's evidence, oldest first.
func (db *DB) ListButlerTaskEvidence(taskID string) ([]ButlerTaskEvidence, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, recorded_at, kind, description, source_id, observed_at,
		        permission_label, audience, payload_json, envelope_version
		 FROM butler_task_evidence WHERE task = ? ORDER BY seq`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler evidence: %w", err)
	}
	defer rows.Close()
	var out []ButlerTaskEvidence
	for rows.Next() {
		var e ButlerTaskEvidence
		var kind, recorded, observed, audience string
		if err := rows.Scan(&e.Seq, &recorded, &kind, &e.Description, &e.SourceID, &observed,
			&e.PermissionLabel, &audience, &e.PayloadJSON, &e.EnvelopeVersion); err != nil {
			return nil, err
		}
		e.Kind = ButlerEvidenceKind(kind)
		e.Audience = ButlerTaskAudience(audience)
		if e.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded); err != nil {
			return nil, fmt.Errorf("runtimedb: bad butler evidence recorded_at %q: %w", recorded, err)
		}
		if observed != "" {
			if e.ObservedAt, err = time.Parse(time.RFC3339Nano, observed); err != nil {
				return nil, fmt.Errorf("runtimedb: bad butler evidence observed_at %q: %w", observed, err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CompleteButlerTask is the only way a task becomes completed: the
// evidence and the state change go in together, or neither does.
//
// It refuses an empty evidence set, and the schema refuses the
// transition independently — belt and braces on purpose. "A model said
// it was done" is not a completion (ADR-W030 A4), and a process that
// dies between writing evidence and flipping the state leaves a running
// task with evidence attached, which is recoverable, rather than a
// completed task that never delivered, which is not.
func (db *DB) CompleteButlerTask(taskID string, evidence []ButlerTaskEvidence, summary, actor string) error {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	if len(evidence) == 0 {
		return fmt.Errorf("runtimedb: butler task %s cannot complete without evidence (ADR-W030 A4)", taskID)
	}
	for _, e := range evidence {
		if _, err := validateEvidence(e); err != nil {
			return err
		}
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: completing butler task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, e := range evidence {
		if err := insertEvidenceTx(tx, seq, e); err != nil {
			return err
		}
	}
	if err := transitionButlerTaskTx(tx, seq, ButlerTaskCompleted, "completed", actor); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE butler_tasks SET result_summary = ?, next_wakeup_at = '' WHERE seq = ?`,
		summary, seq); err != nil {
		return fmt.Errorf("runtimedb: completing butler task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: completing butler task: %w", err)
	}
	return nil
}
