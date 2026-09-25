package agentruntime

import (
	"encoding/json"
	"time"
)

// Ledger step kinds and states, mirrored from the runtime DB's closed
// sets so this package can name them without importing it (the same
// reason keystone/butler/runner.go used to give for keeping its own
// copies — see runtimedb/butlertrace.go and runtimedb's step/attempt
// state vocabularies for the source of truth).
const (
	LedgerStepModel = "model"
	LedgerStepTool  = "tool"

	LedgerStepVerified = "verified"
	LedgerStepFailed   = "failed"
)

// Work-trace event types the runner and the Agent Gateway emit,
// mirrored from the runtime DB's vocabulary (runtimedb/butlertrace.go).
const (
	TraceSliceStarted   = "slice.started"
	TraceModelCall      = "model.call"
	TraceModelAnswer    = "model.answer"
	TraceToolDispatched = "tool.dispatched"
	TraceToolResult     = "tool.result"
	TraceDenial         = "denial"
	TraceWaitOpened     = "wait.opened"
	TraceAttemptFenced  = "attempt.fenced"
	TraceOutcome        = "outcome"
)

// Wait reasons the runner records on the ledger directly (not through
// agentapi.WorkWaitReason, which mirrors the same closed set for a
// runtime crossing the wire), mirrored from the same closed set.
const (
	LedgerWaitReply            = "reply"
	LedgerWaitBudgetExhausted  = "budget-exhausted"
	LedgerWaitModelUnavailable = "model-unavailable"
	LedgerWaitUncertainOutcome = "uncertain-outcome"
)

// LedgerStep is one entry of a task's validated step ledger as it is
// read back. Result is the recorded result of the step's last
// successful attempt — the thing a resume serves instead of dispatching
// again.
type LedgerStep struct {
	Seq     int64
	Ordinal int
	Kind    string
	Name    string
	State   string
	Params  string
	Result  string
	// VerifiedAt is when the step's last successful attempt closed. A
	// resume reuses a verified result only while it is still fresh; past
	// that the reading is taken again, because "do not repeat verified
	// effects" and "refresh stale observations" are both A4 and this is
	// where they meet.
	VerifiedAt time.Time
}

// LedgerAttempt is one checkpointed try, carrying the lease epoch it was
// dispatched under.
type LedgerAttempt struct {
	Seq     int64
	Attempt int
	Epoch   int64
}

// LedgerEvidence is one observation justifying completion. The
// permission label and audience are mandatory at the store, and a
// caller supplies its task's own rather than inventing one.
type LedgerEvidence struct {
	ReceiptID       int64
	ProposalOnly    bool
	ReceiptStep     int64
	Kind            string
	Description     string
	SourceID        string
	PermissionLabel string
	Audience        string
}

// LedgerTraceEvent is one append-only work-trace row (ADR-W030 A11).
// Payload is already-redacted, bounded content or "".
type LedgerTraceEvent struct {
	Type            string
	Step            int64
	Attempt         int64
	Actor           string
	ModelEndpoint   string
	ModelVerdict    string
	ManifestVersion string
	SourceIDs       []string
	PermissionLabel string
	Audience        string
	Payload         string
}

// TaskLedger is the durable checkpoint seam keystone/butlerstore
// implements over the runtime database. It predates the Agent Gateway
// (ADR-W039 Phase 2a/2b): the gateway now drives most of it on a
// runner's behalf, and keystone/agentruntime/legacy's Runner reaches
// only the narrower SliceLedger seam directly. The full interface is
// kept here, aliased from keystone/butler, because keystone/butlerstore
// documents its Ledger against it.
type TaskLedger interface {
	// Fence reports whether this coordinator still holds the runtime
	// database. A non-nil error stops the run at its next checkpoint.
	Fence() error
	// Steps returns the task's ledger, oldest first, each verified step
	// carrying its recorded result.
	Steps(taskID string) ([]LedgerStep, error)
	// AppendStep records a proposed step and returns its row id.
	AppendStep(taskID, kind, name, paramsJSON string) (int64, error)
	// StartAttempt is the checkpoint before a dispatch.
	StartAttempt(stepSeq int64) (LedgerAttempt, error)
	// FinishAttempt is the checkpoint after a result.
	FinishAttempt(attemptSeq int64, ok bool, resultJSON, errText string) error
	// SetStepState records a step's outcome (verified or failed).
	SetStepState(stepSeq int64, state string) error
	// Trace appends one work-trace event.
	Trace(taskID string, e LedgerTraceEvent) error
	// Wait moves the task to waiting with a closed reason.
	Wait(taskID, reason, detail string) error
	// Complete writes evidence and completes the task, atomically.
	Complete(taskID string, evidence []LedgerEvidence, summary string) error
	// Fail moves the task to failed.
	Fail(taskID, reason string) error
	// Running moves a pending or waiting task to running.
	Running(taskID, reason string) error
	// Authorize reports why this task may not continue right now.
	Authorize(taskID string) error
	// FenceAttempts bumps the task's lease epoch and marks every
	// in-flight attempt fenced, returning the new epoch. A result from
	// the abandoned attempt is refused afterwards.
	FenceAttempts(taskID string) (int64, error)
}

// StallNotifier delivers the single deduplicated notice a stalled task
// sends its owner (ADR-W030 A4's P4 minimum). keystone/butlerstore
// implements it; a nil Notifier means the deployment has no outbound
// surface and the stall is recorded without a message rather than
// failing the task.
type StallNotifier interface {
	NotifyStalled(taskID, text string) error
}

// ToolStepResult is the versioned envelope a tool step's result is
// stored in. Fingerprint travels WITH the result rather than being
// recomputed from the step's params, so a change to the fingerprint
// scheme cannot silently start matching old rows to new questions.
//
// Two writers share this definition: the Agent Gateway
// (keystone/agentgateway), which now writes it on the runner's behalf
// for every tool call (ADR-W039 §3.4 step 7), and
// keystone/agentruntime/legacy's Runner, which still writes it directly
// for the closure path and reads it back to serve a verified read on a
// resume (see the objective-verification helper below).
type ToolStepResult struct {
	EffectPending     bool   `json:"effectPending,omitempty"`
	EffectConfirmed   bool   `json:"effectConfirmed,omitempty"`
	ProposalConfirmed bool   `json:"proposalConfirmed,omitempty"`
	Version           int    `json:"v"`
	Fingerprint       string `json:"fp"`
	Content           string `json:"content"`
	Rejected          bool   `json:"rejected,omitempty"`
}

// CallFingerprint identifies a tool call by name and arguments, so a
// resume can tell "the model asked for this again" from "the model
// asked for something similar". Argument order does not change it,
// because the input is marshalled through encoding/json, which sorts
// map keys.
func CallFingerprint(name string, input map[string]any) string {
	body, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return name + "\x00" + string(body)
}

// VerifiedResults reads a task's ledger and returns, per fingerprint,
// the recorded result of every tool step that reached verified within
// ttl of now. A ttl <= 0 reuses nothing.
func VerifiedResults(steps []LedgerStep, now time.Time, ttl time.Duration) map[string]string {
	out := map[string]string{}
	for _, s := range steps {
		if s.Kind != LedgerStepTool || s.State != LedgerStepVerified {
			continue
		}
		if ttl <= 0 || s.VerifiedAt.IsZero() || now.Sub(s.VerifiedAt) > ttl {
			continue
		}
		var rec ToolStepResult
		if err := json.Unmarshal([]byte(s.Result), &rec); err != nil || rec.Fingerprint == "" {
			continue
		}
		out[rec.Fingerprint] = rec.Content
	}
	return out
}

// ObjectiveEffect is one required or claimed effect the objective
// classifier reports for a task's turn.
type ObjectiveEffect struct {
	ProposalOnly bool   `json:"proposalOnly"`
	Description  string `json:"description"`
	SourceKey    string `json:"sourceKey"`
}

// ObjectiveLedger is the runtime-only seam a Runner's objective
// verification writes through. No model tool can classify its own
// task: the seam is deliberately separate from the invoke path.
type ObjectiveLedger interface {
	Objective(taskID string) (string, error)
	ConfirmedObjectiveOperations(taskID string) (map[string]bool, error)
	ClassifyObjective(taskID, kind string, proposalOnly bool, effects []ObjectiveEffect) error
}
