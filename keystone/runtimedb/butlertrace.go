package runtimedb

// The structured, append-only work trace (ADR-W030 A11, P1).
//
// The trace records what the system can actually OBSERVE about a run:
// which model answered and under what verdict, which prompt/policy/tool
// manifest versions were in force, which sources were retrieved and how
// fresh they were, what was dispatched and what came back, and how the
// work ended. P5 turns this into the Console task timeline and into
// sanitized regression fixtures; P1's job is to make it exist and make
// it honest.
//
// Two constraints are structural rather than advisory:
//
//   - Private chain-of-thought is never requested or stored. A rationale
//     that was SHOWN to a person may be recorded as model-authored
//     explanation — that is an answer or a proposal, and it is labelled
//     as one. There is no event type for reasoning.
//   - Every event carries a permission label and an audience. A trace
//     row that cannot be permission-filtered can never be shown to
//     anyone, so it is refused at write time instead of accumulating as
//     unshowable history.
//
// Payloads hold redacted, bounded material only — the same rule
// butler_sessions applies to its transcript, for the same reason. Source
// CONTENT is not duplicated here by default: source ids and freshness
// are, so a reader can go and read the source under their own
// permissions rather than through a copy that outlives it.

import (
	"fmt"
	"strings"
	"time"
)

// Trace event types. The set is open — a later package adds events
// without a migration — but these are the ones P1's callers emit, and
// naming them keeps the vocabulary from drifting per call site.
const (
	TraceTaskCreated      = "task.created"
	TraceTaskTransition   = "task.transition"
	TraceContextRetrieved = "context.retrieved"
	TraceStepProposed     = "step.proposed"
	TraceToolDispatched   = "tool.dispatched"
	TraceToolResult       = "tool.result"
	TraceModelCall        = "model.call"
	TraceModelAnswer      = "model.answer"
	TracePolicyDecision   = "policy.decision"
	TraceApproval         = "approval"
	TraceDenial           = "denial"
	TraceWaitOpened       = "wait.opened"
	TraceWaitResolved     = "wait.resolved"
	TraceAttemptFenced    = "attempt.fenced"
	TraceEvidence         = "evidence.recorded"
	TraceDelivery         = "delivery"
	TraceOutcome          = "outcome"
)

// ButlerTraceEvent is one append-only row of a task's work trace.
//
// The Version fields are what makes a before/after comparison in P5
// meaningful: without them a regression fixture cannot say what actually
// changed between two runs. TokensIn/TokensOut and LatencyMS are
// provider-REPORTED figures kept for capacity analysis; they establish
// neither cost nor remaining account allowance (A11).
type ButlerTraceEvent struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Type string    `json:"type"`

	// Step and Attempt are the rows this event belongs to, when it
	// belongs to one. A task-level event (created, outcome) has neither.
	Step    int64 `json:"step,omitempty"`
	Attempt int64 `json:"attempt,omitempty"`
	Epoch   int64 `json:"epoch,omitempty"`

	Actor string `json:"actor,omitempty"`

	ModelEndpoint   string `json:"modelEndpoint,omitempty"`
	ModelVerdict    string `json:"modelVerdict,omitempty"`
	PromptVersion   string `json:"promptVersion,omitempty"`
	PolicyVersion   string `json:"policyVersion,omitempty"`
	ManifestVersion string `json:"manifestVersion,omitempty"`

	// SourceIDs names the context sources this event used, without
	// copying their content. Freshness and scores belong in the payload.
	SourceIDs []string `json:"sourceIds,omitempty"`

	PermissionLabel string             `json:"permissionLabel"`
	Audience        ButlerTaskAudience `json:"audience"`
	// Redacted records that the payload went through the redaction
	// boundary. It defaults to true and a caller must say so explicitly
	// to write an unredacted payload, so the unsafe case is never the
	// one you get by forgetting.
	Redacted bool `json:"redacted"`

	PayloadJSON     string `json:"payloadJson,omitempty"`
	EnvelopeVersion int    `json:"envelopeVersion"`
}

// AppendButlerTraceEvent appends one event to a task's trace.
//
// It refuses an event with no type, no permission label, or an
// unrecognised audience: those are the three fields every later reader
// needs, and a row missing one is a row that can only be dropped.
func (db *DB) AppendButlerTraceEvent(taskID string, e ButlerTraceEvent) error {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	if e.Type == "" {
		return fmt.Errorf("runtimedb: butler trace event requires a type (ADR-W030 A11)")
	}
	if e.PermissionLabel == "" {
		return fmt.Errorf("runtimedb: butler trace event requires a permission label (ADR-W030 A11)")
	}
	if e.Audience != ButlerAudiencePrivate && e.Audience != ButlerAudienceShared {
		return fmt.Errorf("runtimedb: butler trace event requires audience private|shared, got %q (ADR-W030 A2)", e.Audience)
	}
	for _, id := range e.SourceIDs {
		if strings.Contains(id, ",") {
			return fmt.Errorf("runtimedb: butler trace source id %q may not contain a comma", id)
		}
	}
	payload, err := jsonObjectOrEmpty(e.PayloadJSON, "trace payload")
	if err != nil {
		return err
	}
	var step, attempt any
	if e.Step != 0 {
		step = e.Step
	}
	if e.Attempt != 0 {
		attempt = e.Attempt
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_work_trace (task, at, type, step, attempt, epoch, actor,
		     model_endpoint, model_verdict, prompt_version, policy_version, manifest_version,
		     source_ids, permission_label, audience, redacted, payload_json, envelope_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seq, now(), e.Type, step, attempt, e.Epoch, e.Actor,
		e.ModelEndpoint, e.ModelVerdict, e.PromptVersion, e.PolicyVersion, e.ManifestVersion,
		strings.Join(e.SourceIDs, ","), e.PermissionLabel, string(e.Audience),
		boolInt(e.Redacted), payload, TaskEnvelopeVersion); err != nil {
		return fmt.Errorf("runtimedb: appending butler trace event: %w", err)
	}
	return nil
}

// ListButlerTrace returns a task's trace, oldest first.
//
// It returns everything stored. Permission FILTERING for a particular
// reader is the API layer's job (P5): this package's contract is that
// every row carries the label that filtering needs.
func (db *DB) ListButlerTrace(taskID string) ([]ButlerTraceEvent, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, at, type, COALESCE(step, 0), COALESCE(attempt, 0), epoch, actor,
		        model_endpoint, model_verdict, prompt_version, policy_version, manifest_version,
		        source_ids, permission_label, audience, redacted, payload_json, envelope_version
		 FROM butler_work_trace WHERE task = ? ORDER BY seq`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler trace: %w", err)
	}
	defer rows.Close()
	var out []ButlerTraceEvent
	for rows.Next() {
		var e ButlerTraceEvent
		var at, sources, audience string
		var redacted int
		if err := rows.Scan(&e.Seq, &at, &e.Type, &e.Step, &e.Attempt, &e.Epoch, &e.Actor,
			&e.ModelEndpoint, &e.ModelVerdict, &e.PromptVersion, &e.PolicyVersion,
			&e.ManifestVersion, &sources, &e.PermissionLabel, &audience, &redacted,
			&e.PayloadJSON, &e.EnvelopeVersion); err != nil {
			return nil, err
		}
		e.Audience = ButlerTaskAudience(audience)
		e.Redacted = redacted != 0
		if sources != "" {
			e.SourceIDs = strings.Split(sources, ",")
		}
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("runtimedb: bad butler trace timestamp %q: %w", at, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
