package runtimedb

// Work inspection and the improvement loop (ADR-W030 A11, P5).
//
// Three things a person does with a finished task: say what was wrong
// with it, turn one of those judgements into something that can be run
// again, and — when the structural trace genuinely is not enough — turn
// on extra capture for a bounded time.
//
// All three record a HUMAN's decision. Butler may recommend an
// improvement or file a capability request; it cannot label its own
// work, write its own evaluations, or enable its own debug capture, and
// there is no function here that would let it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ButlerFeedbackLabel is A11's closed vocabulary. Closed because the
// value of a label is that it can be counted: "this happens twice a
// week" is the sentence that starts an improvement, and free text
// cannot produce it.
type ButlerFeedbackLabel string

const (
	FeedbackWrongAnswer           ButlerFeedbackLabel = "wrong-answer"
	FeedbackStaleRead             ButlerFeedbackLabel = "stale-read"
	FeedbackRepeatedWork          ButlerFeedbackLabel = "repeated-work"
	FeedbackInappropriateTool     ButlerFeedbackLabel = "inappropriate-tool"
	FeedbackPermissionRefusal     ButlerFeedbackLabel = "permission-refusal"
	FeedbackExcessiveInterruption ButlerFeedbackLabel = "excessive-interruption"
	FeedbackUsefulCompletion      ButlerFeedbackLabel = "useful-completion"
)

func validFeedbackLabel(l ButlerFeedbackLabel) bool {
	switch l {
	case FeedbackWrongAnswer, FeedbackStaleRead, FeedbackRepeatedWork,
		FeedbackInappropriateTool, FeedbackPermissionRefusal,
		FeedbackExcessiveInterruption, FeedbackUsefulCompletion:
		return true
	}
	return false
}

// ButlerFeedback is one person's label on a task.
type ButlerFeedback struct {
	Seq       int64               `json:"seq"`
	CreatedAt time.Time           `json:"createdAt"`
	Author    string              `json:"author"`
	Label     ButlerFeedbackLabel `json:"label"`
	Note      string              `json:"note,omitempty"`
	// TraceSeq pins the label to the trace event that prompted it, when
	// the person picked one — "this tool call was the wrong one" is a
	// more useful record than "this task went wrong".
	TraceSeq int64 `json:"traceSeq,omitempty"`
}

// AddButlerFeedback records one label. author is mandatory and is the
// person: this row is a human judgement or it is nothing.
func (db *DB) AddButlerFeedback(taskID string, f ButlerFeedback) error {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	if f.Author == "" {
		return fmt.Errorf("runtimedb: butler feedback requires an author (ADR-W030 A11)")
	}
	if !validFeedbackLabel(f.Label) {
		return fmt.Errorf("runtimedb: %q is not a feedback label (ADR-W030 A11)", f.Label)
	}
	var trace any
	if f.TraceSeq != 0 {
		trace = f.TraceSeq
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_task_feedback (task, trace, created_at, author, label, note)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		seq, trace, now(), f.Author, string(f.Label), f.Note); err != nil {
		return fmt.Errorf("runtimedb: recording butler feedback: %w", err)
	}
	return nil
}

// ListButlerFeedback returns a task's labels, oldest first.
func (db *DB) ListButlerFeedback(taskID string) ([]ButlerFeedback, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, COALESCE(trace, 0), created_at, author, label, note
		 FROM butler_task_feedback WHERE task = ? ORDER BY seq`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler feedback: %w", err)
	}
	defer rows.Close()
	var out []ButlerFeedback
	for rows.Next() {
		var f ButlerFeedback
		var created, label string
		if err := rows.Scan(&f.Seq, &f.TraceSeq, &created, &f.Author, &label, &f.Note); err != nil {
			return nil, err
		}
		f.Label = ButlerFeedbackLabel(label)
		if f.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("runtimedb: bad feedback timestamp %q: %w", created, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ButlerEvaluation is a reviewed failure turned into a repeatable check.
type ButlerEvaluation struct {
	Seq              int64     `json:"seq"`
	CreatedAt        time.Time `json:"createdAt"`
	Author           string    `json:"author"`
	Name             string    `json:"name"`
	SourceTaskID     string    `json:"sourceTaskId,omitempty"`
	Summary          string    `json:"summary"`
	Expected         string    `json:"expected"`
	PromptVersion    string    `json:"promptVersion,omitempty"`
	PolicyVersion    string    `json:"policyVersion,omitempty"`
	ManifestVersion  string    `json:"manifestVersion,omitempty"`
	ModelEndpoint    string    `json:"modelEndpoint,omitempty"`
	EvaluatorVersion string    `json:"evaluatorVersion,omitempty"`
	FixtureJSON      string    `json:"fixtureJson,omitempty"`
	EnvelopeVersion  int       `json:"envelopeVersion"`
}

// ErrDuplicateEvaluation reports that this name is already taken. Names
// are unique because an evaluation is referred to by name in a report,
// and two different checks answering to one name make every such report
// ambiguous.
var ErrDuplicateEvaluation = errors.New("runtimedb: an evaluation with that name already exists")

// SaveButlerEvaluation records a version-pinned regression fixture.
func (db *DB) SaveButlerEvaluation(e ButlerEvaluation) (int64, error) {
	if e.Author == "" || e.Name == "" || e.Expected == "" {
		return 0, fmt.Errorf("runtimedb: an evaluation requires an author, a name, and the behaviour it expects")
	}
	fixture := e.FixtureJSON
	if fixture == "" {
		fixture = "{}"
	}
	if !json.Valid([]byte(fixture)) {
		return 0, fmt.Errorf("runtimedb: evaluation fixture is not valid JSON")
	}
	var source any
	if e.SourceTaskID != "" {
		seq, err := parseButlerTaskID(e.SourceTaskID)
		if err != nil {
			return 0, err
		}
		source = seq
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_evaluations (created_at, author, name, source_task, summary, expected,
		     prompt_version, policy_version, manifest_version, model_endpoint, evaluator_version,
		     fixture_json, envelope_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now(), e.Author, e.Name, source, e.Summary, e.Expected,
		e.PromptVersion, e.PolicyVersion, e.ManifestVersion, e.ModelEndpoint,
		e.EvaluatorVersion, fixture, TaskEnvelopeVersion)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDuplicateEvaluation
		}
		return 0, fmt.Errorf("runtimedb: saving evaluation: %w", err)
	}
	return res.LastInsertId()
}

// isUniqueViolation reports whether err is SQLite's unique-constraint
// failure. Matching on the message is what the driver gives us; the
// alternative — a SELECT before the INSERT — has a race the constraint
// exists to close.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ListButlerEvaluations returns the stored evaluations, newest first.
func (db *DB) ListButlerEvaluations(limit int) ([]ButlerEvaluation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.Query(
		`SELECT seq, created_at, author, name, COALESCE(source_task, 0), summary, expected,
		        prompt_version, policy_version, manifest_version, model_endpoint,
		        evaluator_version, fixture_json, envelope_version
		 FROM butler_evaluations ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing evaluations: %w", err)
	}
	defer rows.Close()
	var out []ButlerEvaluation
	for rows.Next() {
		var e ButlerEvaluation
		var created string
		var source int64
		if err := rows.Scan(&e.Seq, &created, &e.Author, &e.Name, &source, &e.Summary, &e.Expected,
			&e.PromptVersion, &e.PolicyVersion, &e.ManifestVersion, &e.ModelEndpoint,
			&e.EvaluatorVersion, &e.FixtureJSON, &e.EnvelopeVersion); err != nil {
			return nil, err
		}
		if source != 0 {
			e.SourceTaskID = butlerTaskID(source)
		}
		if e.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("runtimedb: bad evaluation timestamp %q: %w", created, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxDebugCaptureWindow bounds how long extra capture may stay on. A
// week is long enough to reproduce an intermittent failure and short
// enough that nobody forgets (ADR-W030 A11: "explicit, scoped, visibly
// enabled, and automatically expires").
const MaxDebugCaptureWindow = 7 * 24 * time.Hour

// ButlerDebugCapture is one bounded window of extra capture.
type ButlerDebugCapture struct {
	Seq       int64     `json:"seq"`
	CreatedAt time.Time `json:"createdAt"`
	EnabledBy string    `json:"enabledBy"`
	TaskID    string    `json:"taskId"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expiresAt"`
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

// EnableButlerDebugCapture turns on extra capture for one task for a
// bounded window. There is deliberately no "until further notice": that
// is the state this exists to prevent.
func (db *DB) EnableButlerDebugCapture(taskID, enabledBy, reason string, window time.Duration) (int64, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return 0, err
	}
	if enabledBy == "" || reason == "" {
		return 0, fmt.Errorf("runtimedb: debug capture requires who enabled it and why (ADR-W030 A11)")
	}
	if window <= 0 || window > MaxDebugCaptureWindow {
		return 0, fmt.Errorf("runtimedb: debug capture window must be between 0 and %s, got %s",
			MaxDebugCaptureWindow, window)
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_debug_capture (created_at, enabled_by, task, reason, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		now(), enabledBy, seq, reason,
		time.Now().UTC().Add(window).Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("runtimedb: enabling debug capture: %w", err)
	}
	return res.LastInsertId()
}

// DebugCaptureActive reports whether extra capture is currently on for a
// task. An expired or revoked window is not active, and nothing has to
// remember to turn it off.
func (db *DB) DebugCaptureActive(taskID string) (bool, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return false, err
	}
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_debug_capture
		 WHERE task = ? AND revoked_at = '' AND expires_at > ?`, seq, now()).Scan(&n); err != nil {
		return false, fmt.Errorf("runtimedb: checking debug capture: %w", err)
	}
	return n > 0, nil
}

// ListActiveDebugCaptures returns every capture window currently on,
// across all tasks — the "visibly enabled" half of A11's requirement.
// Console shows this; a window nobody can see is a window nobody turns
// off.
func (db *DB) ListActiveDebugCaptures() ([]ButlerDebugCapture, error) {
	rows, err := db.sql.Query(
		`SELECT seq, created_at, enabled_by, task, reason, expires_at
		 FROM butler_debug_capture WHERE revoked_at = '' AND expires_at > ? ORDER BY seq DESC`, now())
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing debug captures: %w", err)
	}
	defer rows.Close()
	var out []ButlerDebugCapture
	for rows.Next() {
		var c ButlerDebugCapture
		var created, expires string
		var task int64
		if err := rows.Scan(&c.Seq, &created, &c.EnabledBy, &task, &c.Reason, &expires); err != nil {
			return nil, err
		}
		c.TaskID = butlerTaskID(task)
		if c.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		if c.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeButlerDebugCapture turns a window off before it expires.
func (db *DB) RevokeButlerDebugCapture(seq int64) error {
	res, err := db.sql.Exec(
		`UPDATE butler_debug_capture SET revoked_at = ? WHERE seq = ? AND revoked_at = ''`, now(), seq)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking debug capture: %w", err)
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
