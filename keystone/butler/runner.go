package butler

// The durable conversational runner (ADR-W030 A4/A11, work package P3).
//
// Session (session.go) is the in-memory loop: it drives a provider, runs
// tools, and returns a transcript. If the process dies halfway through a
// diagnosis, everything it learned dies with it, and the next attempt
// re-reads every node it had already read.
//
// Runner is the same loop with a ledger under it. Every model call and
// every tool call is checkpointed BEFORE dispatch and again when its
// result lands, so an interrupted run leaves a record of what was
// established and what was merely started. A resume then replays nothing
// — it reconstructs context by asking the model again, and serves any
// tool call whose exact result is already VERIFIED from the ledger
// instead of dispatching it a second time (A4: "resume from the
// validated step ledger, reconstructing context with current
// permissions, rather than replaying a saved model/tool transcript").
//
// Two boundaries are kept deliberately:
//
//   - This package still imports neither keystone/reconcile nor
//     keystone/runtimedb. TaskLedger is an interface defined here and
//     implemented in keystone/butlerstore, so the Butler holds a
//     checkpoint seam, not a database handle.
//   - Every dispatch is fenced. Fence() is called before each model and
//     tool attempt, so a coordinator that has been superseded stops at
//     its next checkpoint rather than completing work the promoted
//     coordinator now owns (A11).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// Ledger step kinds and states, mirrored from the runtime DB's closed
// sets so this package can name them without importing it.
const (
	LedgerStepModel = "model"
	LedgerStepTool  = "tool"

	LedgerStepVerified = "verified"
	LedgerStepFailed   = "failed"
)

// Work-trace event types this runner emits, mirrored from the runtime
// DB's vocabulary. They are strings on both sides rather than a shared
// type because the alternative is this package importing the store it
// deliberately does not depend on.
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

// Wait reasons a Runner records, mirrored from the same closed set.
const (
	LedgerWaitReply            = "reply"
	LedgerWaitBudgetExhausted  = "budget-exhausted"
	LedgerWaitModelUnavailable = "model-unavailable"
)

// LedgerStep is one entry of a task's validated step ledger as the
// runner reads it back. Result is the recorded result of the step's last
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
// permission label and audience are mandatory at the store, and the
// runner supplies its task's own rather than inventing one.
type LedgerEvidence struct {
	Kind            string
	Description     string
	SourceID        string
	PermissionLabel string
	Audience        string
}

// LedgerTraceEvent is one append-only work-trace row (A11). Payload is
// already-redacted, bounded content or "".
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

// TaskLedger is the durable checkpoint seam a Runner writes through.
// keystone/butlerstore implements it over the runtime DB.
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
	// Authorize reports why this task may not continue right now — it
	// was cancelled, its owner was offboarded or disabled, or its
	// deadline has passed. It is checked at the start of every slice and
	// again before each dispatch, because authority that was valid when
	// the work started is not evidence that it still is (ADR-W030 A2.1,
	// A4).
	Authorize(taskID string) error
	// FenceAttempts bumps the task's lease epoch and marks every
	// in-flight attempt fenced, returning the new epoch. A result from
	// the abandoned attempt is refused afterwards.
	FenceAttempts(taskID string) (int64, error)
}

// StallNotifier delivers the single deduplicated notice a stalled task
// sends its owner (ADR-W030 A4's P4 minimum: the owner's originating
// direct surface, the existing outbound dedupe key, held through the
// household's quiet hours). keystone/butlerstore implements it; a nil
// Notifier means the deployment has no outbound surface and the stall is
// recorded without a message rather than failing the task.
type StallNotifier interface {
	NotifyStalled(taskID, text string) error
}

// Runner drives a Toolset through a provider against a durable task.
//
// Budgets are two separate things and the distinction is the point. The
// SLICE budget bounds this turn's model rounds; reaching it checkpoints
// and waits for a person to say continue, discarding nothing. The
// AGGREGATE budget bounds the whole task's tool calls across every slice
// and every restart, and reaching it requires review — it does not reset
// on retry, because a budget that resets is not a budget (A4).
type Runner struct {
	Gateway Toolset
	Ledger  TaskLedger
	// ReadOnly is the QA-only verdict, applied at the dispatch boundary
	// exactly as Session applies it.
	ReadOnly bool
	// MaxSliceRounds bounds THIS turn's model rounds. <= 0 uses
	// defaultMaxIterations.
	MaxSliceRounds int
	// MaxToolCalls bounds the task's total tool calls across all slices
	// and restarts. <= 0 means only the slice budget applies.
	MaxToolCalls int
	// PermissionLabel and Audience are stamped on every trace and
	// evidence row this runner writes. They come from the task's own
	// admission, never from the model.
	PermissionLabel string
	Audience        string
	// Actor names who the work is attributed to in the trace.
	Actor string
	// StallTimeout bounds one model call. A provider that has stopped
	// answering is fenced and retried rather than hung on; <= 0 means no
	// deadline, which is only right in tests.
	StallTimeout time.Duration
	// RetryLimit is how many times one stalled or failed model call is
	// retried inside the remaining budget before a person is told. 0
	// means tell them the first time.
	RetryLimit int
	// ObservationTTL is how long a verified read may be reused on a
	// resume before it is taken again. 0 means never reuse — every
	// resume re-reads, which is safe and slow.
	ObservationTTL time.Duration
	// Notifier sends the one stalled-task notice. nil records the stall
	// without messaging anyone.
	Notifier StallNotifier
	// History is the conversation this slice continues (ADR-W027 A11):
	// the user and assistant TEXT of earlier turns, exactly what
	// Session.History carries and for the same reason.
	//
	// It is not in tension with A4's "replays nothing". What a resume
	// must not replay is a saved MODEL/TOOL transcript, because that
	// would reconstruct context from decisions taken under permissions
	// that may since have changed. Earlier turns of a conversation are
	// not that: they are what the person said, and dropping them makes
	// "why?" reach a model that never heard the question it answers.
	History []provider.Message
	// Now is the runner clock; nil means time.Now.
	Now func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) sliceRounds() int {
	if r.MaxSliceRounds > 0 {
		return r.MaxSliceRounds
	}
	return defaultMaxIterations
}

func (r *Runner) label() string {
	if r.PermissionLabel == "" {
		return "unlabelled"
	}
	return r.PermissionLabel
}

// strictestAudience is the audience a run gets when it names none.
//
// Declared here rather than imported: this package does not depend on
// keystone/runtimedb (see the note at the top — TaskLedger is an
// interface defined here precisely so it does not), and one default
// value is not worth inverting that.
//
// It MUST equal runtimedb.ButlerAudiencePrivate, and
// TestTheDefaultAudienceMatchesTheStore fails if it stops doing so. Two
// declarations of one value drift silently otherwise — and this one
// drifts into writing an audience the schema does not know, which the
// store refuses a long way from the line that chose it. Slice 7 turned
// these from words into rows in `audiences`, which is how the literal
// "private" that used to live here became wrong.
const strictestAudience = "aud_owner"

func (r *Runner) audience() string {
	if r.Audience == "" {
		return strictestAudience
	}
	return r.Audience
}

// trace records one event, ignoring a write failure for anything but the
// terminal outcome: a trace that cannot be written must not take down
// the work it is describing, and the failure is visible in the gap.
func (r *Runner) trace(taskID string, e LedgerTraceEvent) {
	e.PermissionLabel = r.label()
	e.Audience = r.audience()
	if e.Actor == "" {
		e.Actor = r.Actor
	}
	_ = r.Ledger.Trace(taskID, e)
}

// callFingerprint identifies a tool call by name and arguments, so a
// resume can tell "the model asked for this again" from "the model asked
// for something similar".
//
// Argument order must not change the answer, so the input is marshalled
// through encoding/json, which sorts map keys. A value that will not
// marshal produces a fingerprint that matches nothing, which fails
// toward dispatching again rather than toward serving a stale result at
// a different question.
func callFingerprint(name string, input map[string]any) string {
	body, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return name + "\x00" + string(body)
}

// verifiedResults reads the ledger and returns, per fingerprint, the
// recorded result of every tool step that reached verified.
func verifiedResults(steps []LedgerStep, now time.Time, ttl time.Duration) map[string]string {
	out := map[string]string{}
	for _, s := range steps {
		if s.Kind != LedgerStepTool || s.State != LedgerStepVerified {
			continue
		}
		// A reading taken long enough ago is not evidence about now. It
		// stays in the ledger — it is what the earlier part of the task
		// was built on — but the resume takes it again rather than
		// answering today's question from yesterday's observation.
		if ttl <= 0 || s.VerifiedAt.IsZero() || now.Sub(s.VerifiedAt) > ttl {
			continue
		}
		var rec toolStepResult
		if err := json.Unmarshal([]byte(s.Result), &rec); err != nil || rec.Fingerprint == "" {
			continue
		}
		out[rec.Fingerprint] = rec.Content
	}
	return out
}

// toolStepResult is the versioned envelope a tool step's result is
// stored in. Fingerprint travels WITH the result rather than being
// recomputed from the step's params, so a change to the fingerprint
// scheme cannot silently start matching old rows to new questions.
type toolStepResult struct {
	Version     int    `json:"v"`
	Fingerprint string `json:"fp"`
	Content     string `json:"content"`
	Rejected    bool   `json:"rejected,omitempty"`
}

func countToolSteps(steps []LedgerStep) int {
	n := 0
	for _, s := range steps {
		if s.Kind == LedgerStepTool {
			n++
		}
	}
	return n
}

// Run drives one slice of taskID's work for userMessage.
//
// The first call on a task and a resume after a restart are the same
// call: Run reads the ledger, serves verified tool results from it, and
// dispatches only what has not been established yet. The caller decides
// what userMessage is on a resume — normally the task's own objective.
func (r *Runner) Run(ctx context.Context, p provider.Provider, taskID, userMessage string) (*Transcript, error) {
	tr := &Transcript{Audit: Audit{Provider: p.Name()}}
	if r.Ledger == nil {
		return tr, fmt.Errorf("butler runner: no task ledger configured")
	}
	if err := r.Ledger.Fence(); err != nil {
		return tr, fmt.Errorf("butler runner: %w", err)
	}
	// Authority is rechecked before the task is even moved to running:
	// a task cancelled, offboarded or expired while it waited must not
	// take another step, and finding that out after the first model call
	// has already cost a provider round (ADR-W030 A2.1, A4).
	if err := r.Ledger.Authorize(taskID); err != nil {
		r.trace(taskID, LedgerTraceEvent{
			Type: TraceDenial, Payload: jsonPayload(map[string]any{"authority": "refused"}),
		})
		return tr, fmt.Errorf("butler runner: %w", err)
	}
	if err := r.Ledger.Running(taskID, "slice started"); err != nil {
		return tr, fmt.Errorf("butler runner: %w", err)
	}

	steps, err := r.Ledger.Steps(taskID)
	if err != nil {
		return tr, fmt.Errorf("butler runner: reading the step ledger: %w", err)
	}
	verified := verifiedResults(steps, r.now(), r.ObservationTTL)
	toolsUsed := countToolSteps(steps)
	resumed := len(steps) > 0

	manifest := r.Gateway.Capabilities(r.ReadOnly)
	r.trace(taskID, LedgerTraceEvent{
		Type:            TraceSliceStarted,
		ModelEndpoint:   p.Name(),
		ModelVerdict:    manifest.ModelVerdict,
		ManifestVersion: manifest.Fingerprint(),
		Payload:         jsonPayload(map[string]any{"resumed": resumed, "verifiedResults": len(verified)}),
	})

	messages := make([]provider.Message, 0, len(r.History)+1)
	messages = append(messages, r.History...)
	messages = append(messages, provider.Message{Role: "user", Text: userMessage})
	specs := r.Gateway.OfferedSpecs(r.ReadOnly)

	for round := 0; round < r.sliceRounds(); round++ {
		if err := r.Ledger.Fence(); err != nil {
			return tr, fmt.Errorf("butler runner: %w", err)
		}
		// modelRound checks before every attempt, including the first,
		// so the round's own check lives there now rather than here.
		answer, err := r.modelRound(ctx, p, taskID, manifest, messages, specs)
		if err != nil {
			return tr, err
		}
		if len(answer.ToolCalls) == 0 {
			// Accepting an answer is a boundary too: completing the
			// task writes evidence and a result summary the household
			// reads, and doing that for a person whose authority went
			// away mid-call would record work nobody was entitled to.
			if err := r.stillAuthorized(taskID, "final answer"); err != nil {
				return tr, err
			}
			tr.FinalText = answer.Text
			return tr, r.complete(taskID, tr, steps, answer.Text)
		}

		messages = append(messages, provider.Message{
			Role: "assistant", Text: answer.Text, ToolCalls: answer.ToolCalls,
		})
		results := make([]provider.ToolResult, 0, len(answer.ToolCalls))
		for _, tc := range answer.ToolCalls {
			if r.MaxToolCalls > 0 && toolsUsed >= r.MaxToolCalls {
				// The aggregate budget is exhausted. This is not a
				// failure and not a retry: the work stands, and a person
				// decides whether it gets more (A4).
				r.trace(taskID, LedgerTraceEvent{
					Type:    TraceDenial,
					Payload: jsonPayload(map[string]any{"reason": "task tool budget exhausted", "used": toolsUsed}),
				})
				tr.CappedIterations = true
				tr.FinalText = fmt.Sprintf(
					"(stopped after %d tool calls — this task's budget is used up and needs review before it continues)",
					toolsUsed)
				return tr, r.Ledger.Wait(taskID, LedgerWaitBudgetExhausted,
					fmt.Sprintf("%d tool calls used", toolsUsed))
			}
			// The dispatch boundary. Checked per call rather than per
			// round because the model's answer arrived some time ago
			// and may carry several calls, and because A2.1 names
			// dispatch specifically.
			if err := r.stillAuthorized(taskID, "tool dispatch"); err != nil {
				return tr, err
			}
			rec, content, dispatched := r.toolCall(ctx, taskID, tc, verified)
			// Only a call that actually went out spends budget. A
			// result served from the ledger appended no step, so
			// counting it here would disagree with countToolSteps on
			// the next resume as well as overcharging this one.
			if dispatched {
				toolsUsed++
			}
			tr.Audit.Calls = append(tr.Audit.Calls, rec)
			if rec.Plan != nil {
				tr.Audit.Plans = append(tr.Audit.Plans, rec.Plan)
			}
			results = append(results, provider.ToolResult{CallID: tc.ID, Content: content})
		}
		messages = append(messages, provider.Message{Role: "tool", ToolResults: results})

		if fresh, err := r.Ledger.Steps(taskID); err == nil {
			steps = fresh
			verified = verifiedResults(steps, r.now(), r.ObservationTTL)
		}
	}

	// The slice limit is a pause, not a failure: nothing is discarded and
	// the aggregate budget is untouched, so a continue picks up with
	// every verified read still verified (A4).
	tr.CappedIterations = true
	tr.FinalText = "(paused after this turn's tool-call limit — say continue and I will pick up where I left off)"
	r.trace(taskID, LedgerTraceEvent{
		Type:    TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": LedgerWaitReply, "rounds": r.sliceRounds()}),
	})
	return tr, r.Ledger.Wait(taskID, LedgerWaitReply, "turn tool-call limit reached; awaiting continue")
}

// stillAuthorized re-evaluates the task's authority at one boundary and
// traces the refusal.
//
// ADR-W030 A2.1 is "check current membership and link generation at
// every retrieval and dispatch", and design §10.1 says derived
// authority is "re-evaluated at every authorization boundary". A model
// call is the slowest thing this runner does: a session or Connection
// withdrawn while one is in flight would otherwise have its tool calls
// dispatched anyway, because the last check happened before the call
// went out. The gap is exactly as long as the model takes to answer.
//
// So the boundaries are: before the slice, before each model round,
// before each tool call, and before a final answer is accepted. The
// first two were already here; the last two were not, and were found by
// a review bot on #287.
func (r *Runner) stillAuthorized(taskID, at string) error {
	if err := r.Ledger.Authorize(taskID); err != nil {
		r.trace(taskID, LedgerTraceEvent{
			Type:    TraceDenial,
			Payload: jsonPayload(map[string]any{"authority": "withdrawn", "at": at}),
		})
		return fmt.Errorf("butler runner: %w", err)
	}
	return nil
}

// modelRound checkpoints one model call and retries a stalled or failed
// provider inside the remaining budget before involving a person.
//
// A stall is a deadline, not a hang. When StallTimeout expires the
// attempt is FENCED — the task's lease epoch moves — so if the abandoned
// provider ever answers, that result is refused rather than applied over
// whatever the retry established (ADR-W030 A4).
//
// Only when the retries are spent does anyone get told, and then once:
// a single deduplicated notice on the owner's originating surface,
// held through quiet hours, with the ledger untouched so a continue
// picks up every verified read (A4's P4 minimum).
func (r *Runner) modelRound(ctx context.Context, p provider.Provider, taskID string,
	manifest CapabilityManifest, messages []provider.Message, specs []provider.Tool,
) (*provider.ChatResponse, error) {
	var lastErr error
	for try := 0; try <= r.RetryLimit; try++ {
		// EVERY attempt, not only the first. A retry follows a stall or
		// a provider failure, so by definition time has passed since
		// the round's own check — often the whole StallTimeout — and
		// sending the conversation again after the Person, session,
		// Connection or grant behind it was withdrawn is the same
		// disclosure as sending it the first time. Found by a review
		// bot on #287, which is the second time this slice put a
		// boundary next to a gap rather than in it.
		if err := r.stillAuthorized(taskID, "model attempt"); err != nil {
			return nil, err
		}
		resp, err := r.modelAttempt(ctx, p, taskID, manifest, messages, specs)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) {
			break
		}
		// Fence before retrying so a late answer from the abandoned
		// attempt cannot be accepted.
		if epoch, ferr := r.Ledger.FenceAttempts(taskID); ferr == nil {
			r.trace(taskID, LedgerTraceEvent{
				Type:    TraceAttemptFenced,
				Payload: jsonPayload(map[string]any{"epoch": epoch, "try": try + 1}),
			})
		}
	}

	// The provider is unavailable, not wrong. The task waits with every
	// verified read intact, which is the whole reason the ledger exists.
	_ = r.Ledger.Wait(taskID, LedgerWaitModelUnavailable, "provider call failed")
	r.trace(taskID, LedgerTraceEvent{
		Type:    TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": LedgerWaitModelUnavailable, "tries": r.RetryLimit + 1}),
	})
	if r.Notifier != nil {
		// One notice, deduplicated on the task. A stalled task that is
		// retried and stalls again must not send a second message.
		_ = r.Notifier.NotifyStalled(taskID,
			"I could not reach the model to finish this. Nothing is lost — say continue and I will pick up where I left off.")
	}
	return nil, fmt.Errorf("butler runner: chat: %w", lastErr)
}

// modelAttempt is one checkpointed model call: a step and an attempt
// before the call, the attempt closed after it.
func (r *Runner) modelAttempt(ctx context.Context, p provider.Provider, taskID string,
	manifest CapabilityManifest, messages []provider.Message, specs []provider.Tool,
) (*provider.ChatResponse, error) {
	step, err := r.Ledger.AppendStep(taskID, LedgerStepModel, p.Name(),
		jsonPayload(map[string]any{"messages": len(messages), "tools": len(specs)}))
	if err != nil {
		return nil, fmt.Errorf("butler runner: checkpointing a model call: %w", err)
	}
	attempt, err := r.Ledger.StartAttempt(step)
	if err != nil {
		return nil, fmt.Errorf("butler runner: checkpointing a model call: %w", err)
	}
	r.trace(taskID, LedgerTraceEvent{
		Type: TraceModelCall, Step: step, Attempt: attempt.Seq,
		ModelEndpoint: p.Name(), ModelVerdict: manifest.ModelVerdict,
		ManifestVersion: manifest.Fingerprint(),
	})

	callCtx := ctx
	if r.StallTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, r.StallTimeout)
		defer cancel()
	}
	resp, err := p.Chat(callCtx, provider.ChatRequest{
		System: r.Gateway.SystemPrompt(), Messages: provider.SafeMessages(messages), Tools: specs,
	})
	if err != nil {
		stalled := errors.Is(callCtx.Err(), context.DeadlineExceeded)
		_ = r.Ledger.FinishAttempt(attempt.Seq, false, "", err.Error())
		_ = r.Ledger.SetStepState(step, LedgerStepFailed)
		r.trace(taskID, LedgerTraceEvent{
			Type: TraceOutcome, Step: step, Attempt: attempt.Seq,
			Payload: jsonPayload(map[string]any{"providerError": true, "stalled": stalled}),
		})
		if stalled {
			return nil, fmt.Errorf("provider stalled after %s: %w", r.StallTimeout, err)
		}
		return nil, err
	}
	// The coordinator is rechecked AFTER the call, before its result is
	// accepted.
	//
	// The fence before dispatch proves this coordinator held the
	// database when the call went out, and a model call takes seconds.
	// A promotion during one changes the COORDINATOR lease rather than
	// the task's attempt epoch, so FinishAttempt's own epoch check does
	// not see it. When the answer carries tool calls the next dispatch
	// fences anyway — but an answer with NO tool calls goes straight to
	// completion, and a superseded coordinator would close work its
	// replacement had already taken over.
	if err := r.Ledger.Fence(); err != nil {
		r.trace(taskID, LedgerTraceEvent{
			Type: TraceDenial, Step: step, Attempt: attempt.Seq,
			Payload: jsonPayload(map[string]any{"reason": "coordinator changed during the model call"}),
		})
		return nil, fmt.Errorf("butler runner: a model answer arrived after this coordinator was superseded: %w", err)
	}
	if err := r.Ledger.FinishAttempt(attempt.Seq, true,
		jsonPayload(map[string]any{"toolCalls": len(resp.ToolCalls)}), ""); err != nil {
		return nil, fmt.Errorf("butler runner: closing a model checkpoint: %w", err)
	}
	if err := r.Ledger.SetStepState(step, LedgerStepVerified); err != nil {
		return nil, fmt.Errorf("butler runner: closing a model checkpoint: %w", err)
	}
	// Only what the model DID is recorded — the tools it asked for. Its
	// text becomes an answer or a proposal when it is shown to someone;
	// there is no event here for reasoning, and none is requested (A11).
	r.trace(taskID, LedgerTraceEvent{
		Type: TraceModelAnswer, Step: step, Attempt: attempt.Seq,
		Payload: jsonPayload(map[string]any{"toolCalls": toolNames(resp.ToolCalls)}),
	})
	return resp, nil
}

// toolCall runs one tool call, serving it from the ledger when this
// exact call is already verified.
// toolCall runs one call and reports whether it actually DISPATCHED.
// A call served from the ledger dispatched nothing — it appends no
// step, spends no provider capacity, and must not spend budget either,
// or a task that re-reads one established fact talks itself out of a
// budget it never used.
func (r *Runner) toolCall(ctx context.Context, taskID string, tc provider.ToolCall,
	verified map[string]string,
) (rec ToolCallRecord, content string, dispatched bool) {
	rec = ToolCallRecord{Name: tc.Name, Input: tc.Input}
	fp := callFingerprint(tc.Name, tc.Input)

	// Recall is an authorization-sensitive resource query. Its tool-level
	// permission cannot validate cached records after an audience, scope or
	// Connection change, so always dispatch it through the current filter.
	if content, ok := verified[fp]; ok && fp != "" && tc.Name != "recall" {
		// A RETRIEVAL IS A BOUNDARY. ADR-W030 A2.1 is "at every
		// retrieval and dispatch", and this is the only path that
		// returns a tool's result without dispatching it — so it is the
		// only place where authority withdrawn since the result was
		// recorded would otherwise have no effect at all.
		//
		// It matters most for a grant: a revoked grant is refused per
		// tool by the resolver, which Execute reaches and this path
		// does not. Serving the cache would hand back exactly what the
		// revocation was about. Found by a review bot on #287.
		if allowed, why := r.Gateway.Permits(tc.Name, r.ReadOnly); !allowed {
			r.trace(taskID, LedgerTraceEvent{
				Type:      TraceDenial,
				SourceIDs: []string{tc.Name},
				Payload:   jsonPayload(map[string]any{"authority": "withdrawn", "at": "ledger retrieval"}),
			})
			rec.Rejected = true
			rec.Result = why
			return rec, why, false
		}
		// Already established. Re-reading it would not be harmless: a
		// second reading at a different moment can disagree with the one
		// the earlier part of this task was built on.
		rec.Result = content
		r.trace(taskID, LedgerTraceEvent{
			Type:      TraceToolResult,
			SourceIDs: []string{tc.Name},
			Payload:   jsonPayload(map[string]any{"servedFromLedger": true}),
		})
		return rec, content, false
	}

	if err := r.Ledger.Fence(); err != nil {
		rec.Error = err.Error()
		return rec, fmt.Sprintf("error: %v", err), false
	}
	step, err := r.Ledger.AppendStep(taskID, LedgerStepTool, tc.Name, jsonPayload(tc.Input))
	if err != nil {
		rec.Error = err.Error()
		return rec, fmt.Sprintf("error: %v", err), false
	}
	attempt, err := r.Ledger.StartAttempt(step)
	if err != nil {
		rec.Error = err.Error()
		return rec, fmt.Sprintf("error: %v", err), true
	}
	r.trace(taskID, LedgerTraceEvent{
		Type: TraceToolDispatched, Step: step, Attempt: attempt.Seq,
		SourceIDs: []string{tc.Name},
	})

	outcome, err := r.Gateway.Execute(ctx, tc.Name, tc.Input, r.ReadOnly)
	if err != nil {
		rec.Error = err.Error()
		_ = r.Ledger.FinishAttempt(attempt.Seq, false, "", err.Error())
		_ = r.Ledger.SetStepState(step, LedgerStepFailed)
		r.trace(taskID, LedgerTraceEvent{
			Type: TraceToolResult, Step: step, Attempt: attempt.Seq,
			Payload: jsonPayload(map[string]any{"error": true}),
		})
		return rec, fmt.Sprintf("error: %v", err), true
	}

	rec.Result = outcome.Content
	rec.Plan = outcome.Plan
	rec.ResourceYAML = outcome.ResourceYAML
	rec.Rejected = outcome.Rejected

	// A rejected tool call is a real, established outcome — the policy
	// said no — but it is not a verified READ, so it is not served from
	// the ledger on a resume: the answer could differ once the thing that
	// caused the rejection is fixed.
	result := toolStepResult{Version: 1, Fingerprint: fp, Content: outcome.Content, Rejected: outcome.Rejected}
	body, _ := json.Marshal(result)
	_ = r.Ledger.FinishAttempt(attempt.Seq, true, string(body), "")
	if outcome.Rejected {
		_ = r.Ledger.SetStepState(step, LedgerStepFailed)
	} else {
		_ = r.Ledger.SetStepState(step, LedgerStepVerified)
		verified[fp] = outcome.Content
	}
	r.trace(taskID, LedgerTraceEvent{
		Type: TraceToolResult, Step: step, Attempt: attempt.Seq,
		SourceIDs: []string{tc.Name},
		Payload:   jsonPayload(map[string]any{"rejected": outcome.Rejected, "bytes": len(outcome.Content)}),
	})
	return rec, outcome.Content, true
}

// complete finishes the task with evidence drawn from what the run
// actually established: one row per verified tool step, plus the
// delivered answer. A run that verified nothing completes on the
// delivery alone, which is honest for a question that needed no reads
// and refused for anything else by the store's own check.
func (r *Runner) complete(taskID string, tr *Transcript, steps []LedgerStep, finalText string) error {
	// Completion is the last thing a superseded coordinator could still
	// do, and the most consequential: a completed task is one nothing
	// resumes. This is the backstop for the answer that carried no tool
	// calls and so passed no other fence on its way here.
	if err := r.Ledger.Fence(); err != nil {
		return fmt.Errorf("butler runner: refusing to complete work after this coordinator was superseded: %w", err)
	}
	fresh, err := r.Ledger.Steps(taskID)
	if err == nil {
		steps = fresh
	}
	var evidence []LedgerEvidence
	for _, s := range steps {
		if s.Kind != LedgerStepTool || s.State != LedgerStepVerified {
			continue
		}
		evidence = append(evidence, LedgerEvidence{
			Kind:            "tool-result",
			Description:     fmt.Sprintf("%s returned a result that was used in the answer", s.Name),
			SourceID:        s.Name,
			PermissionLabel: r.label(),
			Audience:        r.audience(),
		})
	}
	evidence = append(evidence, LedgerEvidence{
		Kind:            "delivery",
		Description:     "an answer was produced and returned to the person who asked",
		PermissionLabel: r.label(),
		Audience:        r.audience(),
	})
	r.trace(taskID, LedgerTraceEvent{
		Type:    TraceOutcome,
		Payload: jsonPayload(map[string]any{"evidence": len(evidence)}),
	})
	return r.Ledger.Complete(taskID, evidence, summarize(finalText))
}

// summarize bounds a completion summary to a line's worth of the answer.
func summarize(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= 200 {
		return s
	}
	return truncate(s, 200)
}

func toolNames(calls []provider.ToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// jsonPayload renders a small structural payload. It carries counts and
// flags, not content: what a source SAID inherits that source's
// retention, and a trace row is not the place to duplicate it (A11).
func jsonPayload(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(body)
}
