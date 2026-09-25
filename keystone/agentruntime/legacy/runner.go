package legacy

// The durable conversational runner (ADR-W030 A4/A11, work package P3;
// re-plumbed onto keystone/agentapi in ADR-W039 Phase 2b, #337).
//
// Session (keystone/butler) is the in-memory loop: it drives a provider,
// runs tools through a Toolset directly, and returns a transcript. If
// the process dies halfway through a diagnosis, everything it learned
// dies with it. ADR-W039 §7.3 keeps that legacy, closure-driven path in
// keystone/butler on purpose ("the legacy ButlerHost and butler.Session
// paths ... are not migrated; they remain until no deployment lacks a
// Butler"), so Session stays put.
//
// Runner is the loop that gained a durable ledger under it (Phase 2 of
// ADR-W030), and it is what this package now holds. Through Phase 2a it
// checkpointed every model and tool call directly against a
// butlerstore.Ledger and dispatched every tool directly against a
// butler.Toolset. Phase 2b removes both: Runner now calls Wildstone
// exclusively through an agentapi.Client bound to one opaque task
// Handle (ADR-W039 §7.1's "target" column) —
//
//   - Capabilities/OfferedSpecs/SystemPrompt -> one Client.Manifest per
//     slice;
//   - a tool call -> Client.Invoke, which folds in the ledger
//     retrieval, the aggregate budget and the dispatch-boundary
//     authority check the runner used to do itself (see
//     keystone/agentgateway/invoke.go's own "duplicated-until-2b"
//     comment — this is the "until" landing);
//   - a model call's checkpoints -> Client.Checkpoint;
//   - Running/Wait/Complete/Fail -> Client.Start/Wait/Complete/Fail.
//
// What is deleted as an exact duplicate of what the gateway now owns:
// the `Gateway Toolset` and `Ledger TaskLedger` fields, the `verified`
// map a resume built from the ledger (keystone/agentgateway/invoke.go
// step 3 does this per call now), the per-call `Permits` recheck for a
// ledger-served read (same place), and the `MaxToolCalls`/
// `UseParentBudget` aggregate-budget bookkeeping
// (keystone/agentgateway/invoke.go step 2 and
// keystone/agentgateway/gateway.go's ConversationToolCalls/
// ObservationTTL, which is where the observation-freshness cache lives
// now too).
//
// What is NOT yet part of the Agent API surface (ADR-W039 §8 names this
// Phase 4: "task lifecycle and checkpoints over the wire") still
// reaches the ledger directly, through the narrow SliceLedger seam
// below rather than the old full TaskLedger: the coordinator lease
// (Fence), the task's live authority (Authorize, checked at the same
// four boundaries #287 put it at — slice start, before each model
// attempt, before a final answer, and before completion), the step
// ledger read to assemble completion evidence and to feed objective
// verification, the model-retry fence (FenceAttempts — never
// FenceTask: a stalled model call is retried under the SAME authority
// a moment later, so only the lease moves, and the task's handle stays
// live), the trace of the runner's own model-round decisions
// (Trace — the gateway already writes the tool-call side of the trace;
// this is the model-call side, and Work has no wire call for it yet),
// and the objective-classification bookkeeping
// (Objective/ConfirmedObjectiveOperations/ClassifyObjective) that
// verifyObjective in objective.go still drives. All eight are exactly
// the "Phase 4 leftover" the split calls for: reachable through one
// small interface, never the whole Ledger.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// defaultSliceRounds bounds this turn's model rounds when MaxSliceRounds
// names none (ADR-W006 A5.3/B3: "no infinite tool loops"). It mirrors
// keystone/butler's defaultMaxIterations, which this package cannot
// import; both values need to agree and neither owns the other, the
// same shape as the audience literal runner.go used to carry.
const defaultSliceRounds = 8

// SliceLedger is the narrow, Phase-4-leftover ledger seam described
// above. keystone/butlerstore.Ledger implements it structurally — every
// method here already exists on it with this exact signature — so
// wiring the runner needs no change there.
type SliceLedger interface {
	// Fence reports whether this coordinator still holds the runtime
	// database. A non-nil error stops the run at its next checkpoint.
	Fence() error
	// Authorize reports why this task may not continue right now — it
	// was cancelled, its owner was offboarded or disabled, or its
	// deadline has passed (ADR-W030 A2.1, A4).
	Authorize(taskID string) error
	// Steps returns the task's ledger, oldest first, for completion
	// evidence and objective verification.
	Steps(taskID string) ([]agentruntime.LedgerStep, error)
	// FenceAttempts bumps the task's lease epoch and marks every
	// in-flight attempt fenced, returning the new epoch, without
	// touching the task's handle or state (contrast Ledger.FenceTask,
	// which revocation uses).
	FenceAttempts(taskID string) (int64, error)
	// Trace appends one work-trace event: the model-round side of the
	// trace the runner still owns directly.
	Trace(taskID string, e agentruntime.LedgerTraceEvent) error

	// Objective, ConfirmedObjectiveOperations and ClassifyObjective are
	// the objective-classification seam verifyObjective (objective.go)
	// drives. No model tool can classify its own task.
	Objective(taskID string) (string, error)
	ConfirmedObjectiveOperations(taskID string) (map[string]bool, error)
	ClassifyObjective(taskID, kind string, proposalOnly bool, effects []agentruntime.ObjectiveEffect) error
}

// StallNotifier delivers the single deduplicated notice a stalled task
// sends its owner. Alias of the shared shape so a caller wiring
// keystone/butlerstore.Notifier here needs no conversion.
type StallNotifier = agentruntime.StallNotifier

// Runner drives a provider through an agentapi.Client against one
// durable task, named by a Handle Wildstone issued at admission.
//
// Budgets are Wildstone's now. The SLICE budget (MaxSliceRounds) is the
// one thing left for the runner to bound on its own: it is a property
// of THIS turn's loop, not of anything the gateway checkpoints. The
// AGGREGATE tool-call budget across the whole task lives in the
// gateway's Invoke (ADR-W039 §3.4 step 2) and is reported back as an
// agentapi.OutcomeWaiting/WaitBudgetExhausted the runner turns into a
// Work.Wait transition — the gateway refuses the call but does not
// itself move the task, exactly as keystone/agentgateway/invoke.go's
// own comment says ("the slice's state transition is the runner's
// until Phase 4 makes waiting a Work call").
type Runner struct {
	// Client is how this runner reaches Wildstone: in-process
	// (keystone/agentgateway.InProcess) today, over a transport later,
	// with no change to this package (ADR-W039 §7.1).
	Client agentapi.Client
	// Handle names the task every call acts within. It is opaque; the
	// only thing this package reads from it is Handle.TaskID(), the
	// public half, for the SliceLedger calls above, which are not yet
	// Handle-shaped themselves.
	Handle agentapi.Handle
	// Ledger is the Phase-4-leftover seam. nil disables objective
	// verification and the runner's own model-round trace (both
	// degrade safely: Wildstone's own authority and ledger checks,
	// reached through Client, still run in full).
	Ledger SliceLedger
	// Provider is the model this slice talks to.
	Provider provider.Provider
	// VerifyObjective uses a separate, tool-free call on the assigned
	// route to classify what this turn actually did.
	VerifyObjective bool
	// MaxSliceRounds bounds THIS turn's model rounds. <= 0 uses
	// defaultSliceRounds.
	MaxSliceRounds int
	// PermissionLabel and Audience are stamped on the model-round trace
	// rows this runner still writes directly (SliceLedger.Trace).
	// They are audit labels, not an authority claim: every call that
	// carries real authority derives its own label and audience from
	// the task on the Handle, inside the gateway, regardless of what is
	// set here (ADR-W039 §3.1 principle 4).
	PermissionLabel string
	Audience        string
	// Actor names who the work is attributed to in the trace.
	Actor string
	// StallTimeout bounds one model call; <= 0 means no deadline, which
	// is only right in tests.
	StallTimeout time.Duration
	// RetryLimit is how many times one stalled or failed model call is
	// retried before a person is told. 0 means tell them the first time.
	RetryLimit int
	// Notifier sends the one stalled-task notice. nil records the stall
	// without messaging anyone.
	Notifier StallNotifier
	// History is the conversation this slice continues (ADR-W027 A11):
	// the user and assistant TEXT of earlier turns.
	History []provider.Message
	// Now is the runner clock; nil means time.Now.
	Now func() time.Time
	// Log receives operational detail this runner can no longer put on
	// the ledger's own trace once it goes through the gateway (a
	// Checkpoint/Trace write failure, for instance): diagnostic only,
	// never a substitute for the ledger's audit trail.
	Log *slog.Logger
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
	return defaultSliceRounds
}

func (r *Runner) label() string {
	if r.PermissionLabel == "" {
		return "unlabelled"
	}
	return r.PermissionLabel
}

func (r *Runner) providerName() string {
	if r.Provider == nil {
		return ""
	}
	return r.Provider.Name()
}

// trace records one model-round event on the Phase-4-leftover seam,
// logging rather than failing the run if it cannot be written — a trace
// that cannot be written must not take down the work it is describing.
func (r *Runner) trace(taskID string, e agentruntime.LedgerTraceEvent) {
	if r.Ledger == nil {
		return
	}
	e.PermissionLabel = r.label()
	e.Audience = r.Audience
	if e.Actor == "" {
		e.Actor = r.Actor
	}
	if err := r.Ledger.Trace(taskID, e); err != nil && r.Log != nil {
		r.Log.Warn("agentruntime/legacy: trace write failed", "type", e.Type, "err", err)
	}
}

// checkAuthority re-evaluates the task's authority at one boundary
// (ADR-W030 A2.1, A4; #287's four boundaries — slice start is Client.
// Start's own check, so this covers the other three: each model
// attempt, the final answer, and completion).
func (r *Runner) checkAuthority(taskID, at string) error {
	if r.Ledger == nil {
		return nil
	}
	if err := r.Ledger.Authorize(taskID); err != nil {
		r.trace(taskID, agentruntime.LedgerTraceEvent{
			Type: agentruntime.TraceDenial, Payload: jsonPayload(map[string]any{"authority": "withdrawn", "at": at}),
		})
		return fmt.Errorf("agentruntime/legacy: %w", err)
	}
	return nil
}

// Run drives one slice of the task on r.Handle for userMessage. The
// first call on a task and a resume after a restart are the same call:
// Wildstone's own ledger (reached through Client) serves any verified
// tool result and dispatches only what has not been established yet.
func (r *Runner) Run(ctx context.Context, userMessage string) (*agentruntime.Transcript, error) {
	tr := &agentruntime.Transcript{Audit: agentruntime.Audit{Provider: r.providerName()}}
	if r.Client == nil {
		return tr, fmt.Errorf("agentruntime/legacy: no agent API client configured")
	}
	if r.Handle.IsZero() {
		return tr, fmt.Errorf("agentruntime/legacy: no task handle configured")
	}
	if r.Provider == nil {
		return tr, fmt.Errorf("agentruntime/legacy: no provider configured")
	}
	taskID := r.Handle.TaskID()

	manifest, err := r.start(ctx, taskID)
	if err != nil {
		return tr, err
	}

	messages := make([]provider.Message, 0, len(r.History)+1)
	messages = append(messages, r.History...)
	messages = append(messages, provider.Message{Role: "user", Text: userMessage})
	specs := toolsFromManifest(manifest.Tools)

	for round := 0; round < r.sliceRounds(); round++ {
		answer, err := r.modelRound(ctx, taskID, manifest.SystemPrompt, manifest.Fingerprint, messages, specs)
		if err != nil {
			return tr, err
		}
		if len(answer.ToolCalls) == 0 {
			return tr, r.finish(ctx, taskID, userMessage, answer.Text, manifest, tr)
		}

		messages = append(messages, provider.Message{
			Role: "assistant", Text: answer.Text, ToolCalls: answer.ToolCalls,
		})
		results := make([]provider.ToolResult, 0, len(answer.ToolCalls))
		for _, tc := range answer.ToolCalls {
			rec, content, wait := r.toolCall(ctx, taskID, tc)
			tr.Audit.Calls = append(tr.Audit.Calls, rec)
			if wait != nil {
				tr.CappedIterations = true
				tr.FinalText = fmt.Sprintf(
					"(stopped after this task's tool-call budget — it needs review before it continues: %s)", wait.Reason)
				return tr, wait.err(r, ctx)
			}
			results = append(results, provider.ToolResult{CallID: tc.ID, Content: content})
		}
		messages = append(messages, provider.Message{Role: "tool", ToolResults: results})
	}

	return tr, r.pause(ctx, taskID, tr)
}

// start opens a slice: Client.Start fences, re-checks authority and
// moves the task to running, in that order, exactly as Ledger.Fence +
// Ledger.Authorize + Ledger.Running used to here; then the manifest the
// slice's model is offered. Shared by Run and BeginSlice.
func (r *Runner) start(ctx context.Context, taskID string) (agentapi.Manifest, error) {
	if err := r.Client.Start(ctx, agentapi.WorkStart{Handle: r.Handle, Reason: "slice started"}); err != nil {
		r.trace(taskID, agentruntime.LedgerTraceEvent{
			Type: agentruntime.TraceDenial, Payload: jsonPayload(map[string]any{"authority": "refused"}),
		})
		return agentapi.Manifest{}, fmt.Errorf("agentruntime/legacy: %w", err)
	}

	var resumed bool
	var priorSteps int
	if r.Ledger != nil {
		if steps, err := r.Ledger.Steps(taskID); err == nil {
			resumed = len(steps) > 0
			priorSteps = len(steps)
		}
	}

	manifest, err := r.Client.Manifest(ctx, r.Handle)
	if err != nil {
		return agentapi.Manifest{}, fmt.Errorf("agentruntime/legacy: fetching the manifest: %w", err)
	}
	r.trace(taskID, agentruntime.LedgerTraceEvent{
		Type:            agentruntime.TraceSliceStarted,
		ModelEndpoint:   r.providerName(),
		ManifestVersion: manifest.Fingerprint,
		Payload:         jsonPayload(map[string]any{"resumed": resumed, "priorSteps": priorSteps}),
	})
	return manifest, nil
}

// finish accepts a final answer: the authority boundary, the optional
// objective check, and completion with evidence. Shared by Run and by
// Slice, whose loop runs in another runtime.
//
// An effectful objective completes only with a matching receipt (the
// effectful_completion_requires_receipt trigger), and that stays so. What
// finish decides is what a refused completion MEANS. When a tool that can
// change something ran and no receipt proves the change, the outcome
// really is uncertain: the task waits and the conversation is held until
// a person reviews it. When nothing that could change anything ran at
// all, there is no uncertainty to review — the request simply was not
// done — so the task ends failed, with an answer that says so, and the
// conversation carries on. See nothingAttempted for what "could change
// something" means.
func (r *Runner) finish(ctx context.Context, taskID, userMessage, answer string, manifest agentapi.Manifest, tr *agentruntime.Transcript) error {
	if err := r.checkAuthority(taskID, "final answer"); err != nil {
		return err
	}
	claims := true
	if r.VerifyObjective {
		var err error
		if claims, err = r.verifyObjective(ctx, taskID, userMessage, answer, manifest.Fingerprint); err != nil {
			return err
		}
	}
	tr.FinalText = answer
	err := r.complete(ctx, taskID, answer)
	var missing interface{ MissingEffectReceipt() bool }
	if !errors.As(err, &missing) || !missing.MissingEffectReceipt() {
		return err
	}
	if r.nothingAttempted(taskID, manifest) {
		return r.notDone(ctx, taskID, answer, claims, tr)
	}
	tr.FinalText = "I couldn't verify every requested effect. The task is still open; any confirmed work is preserved for review."
	return r.Client.Wait(ctx, agentapi.WorkWait{
		Handle: r.Handle, Reason: agentapi.WorkWaitUncertainOutcome,
		Detail: "one or more required effects lack matching successful receipts; review before continuing",
	})
}

// notDoneAnswer replaces an answer that claims an effect nothing
// performed. It is true by construction: nothing that could change
// anything ran.
const notDoneAnswer = "I wasn't able to do that — nothing was changed."

// notDone ends a task whose requested effect was never attempted. The
// answer is shown as the model wrote it when it does not claim the
// effect (a decline, a question, an explanation), and replaced when it
// does, or when that cannot be judged: a claim with nothing behind it is
// exactly what the receipt rule exists to keep from reaching a person.
func (r *Runner) notDone(ctx context.Context, taskID, answer string, claims bool, tr *agentruntime.Transcript) error {
	tr.FinalText = answer
	if claims || strings.TrimSpace(answer) == "" {
		tr.FinalText = notDoneAnswer
	}
	r.trace(taskID, agentruntime.LedgerTraceEvent{
		Type:    agentruntime.TraceOutcome,
		Payload: jsonPayload(map[string]any{"notDone": true, "answerClaimedEffect": claims}),
	})
	return r.Client.Fail(ctx, agentapi.WorkFail{
		Handle: r.Handle,
		Reason: "not done: the requested effect was never attempted (no tool that can change anything ran)",
	})
}

// nothingAttempted reports whether this task provably changed nothing:
// every tool step it recorded names a tool the manifest marks read-only,
// and none of them reported an effect. The read-only mark is the
// platform's own declaration (a capability's Mutating flag), never the
// model's. A tool the manifest does not list, a step whose result cannot
// be read, or an unreadable ledger all count as "something may have
// happened", so the uncertain path stays the default.
func (r *Runner) nothingAttempted(taskID string, manifest agentapi.Manifest) bool {
	if r.Ledger == nil {
		return false
	}
	steps, err := r.Ledger.Steps(taskID)
	if err != nil {
		return false
	}
	readOnly := make(map[string]bool, len(manifest.Tools))
	for _, t := range manifest.Tools {
		readOnly[t.Name] = t.ReadOnlyHint
	}
	for _, step := range steps {
		if step.Kind != agentruntime.LedgerStepTool {
			continue
		}
		if !readOnly[step.Name] {
			return false
		}
		if step.Result == "" {
			continue // a read that never finished changed nothing either
		}
		var result agentruntime.ToolStepResult
		if json.Unmarshal([]byte(step.Result), &result) != nil {
			return false
		}
		if result.EffectConfirmed || result.EffectPending || result.ProposalConfirmed {
			return false
		}
	}
	return true
}

// pause parks the task at the slice limit. It is a pause, not a failure:
// nothing is discarded and the aggregate budget is untouched, so a
// continue picks up with every verified read still verified.
func (r *Runner) pause(ctx context.Context, taskID string, tr *agentruntime.Transcript) error {
	tr.CappedIterations = true
	tr.FinalText = "(paused after this turn's tool-call limit — say continue and I will pick up where I left off)"
	r.trace(taskID, agentruntime.LedgerTraceEvent{
		Type:    agentruntime.TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": string(agentapi.WorkWaitReply), "rounds": r.sliceRounds()}),
	})
	return r.Client.Wait(ctx, agentapi.WorkWait{
		Handle: r.Handle, Reason: agentapi.WorkWaitReply, Detail: "turn tool-call limit reached; awaiting continue",
	})
}

// waitTransition is a task-level Wait the tool-call loop must perform
// itself, because the gateway's Invoke refuses a budget-exhausted call
// without moving the task (ADR-W039 §3.4 step 2's own note: "no Wait
// transition, no trace" — that is the runner's to do, same as always).
type waitTransition struct {
	Reason agentapi.WorkWaitReason
	Detail string
}

func (w waitTransition) err(r *Runner, ctx context.Context) error {
	return r.Client.Wait(ctx, agentapi.WorkWait{Handle: r.Handle, Reason: w.Reason, Detail: w.Detail})
}

// toolCall runs one call through Client.Invoke. Wildstone's Invoke has
// already done everything the runner's own toolCall used to do inline —
// the authority recheck, the aggregate budget, the ledger-served read,
// the dispatch and its checkpoints — so this just maps the outcome onto
// a ToolCallRecord and, for an exhausted budget only, the task-level
// Wait the gateway deliberately leaves to the caller.
func (r *Runner) toolCall(ctx context.Context, taskID string, tc provider.ToolCall) (rec agentruntime.ToolCallRecord, content string, wait *waitTransition) {
	rec = agentruntime.ToolCallRecord{Name: tc.Name, Input: tc.Input}
	args, err := json.Marshal(tc.Input)
	if err != nil {
		args = nil
	}
	res, err := r.Client.Invoke(ctx, agentapi.InvokeRequest{Handle: r.Handle, Tool: tc.Name, Args: args})
	if err != nil {
		rec.Error = err.Error()
		return rec, fmt.Sprintf("error: %v", err), nil
	}
	switch {
	case res.Outcome == agentapi.OutcomeWaiting && res.WaitReason == agentapi.WaitBudgetExhausted:
		return rec, res.Reason, &waitTransition{Reason: agentapi.WorkWaitBudgetExhausted, Detail: res.Reason}
	case res.Outcome == agentapi.OutcomeDenied || res.Outcome == agentapi.OutcomeRejected:
		rec.Rejected = true
		rec.Result = res.Content
		content = res.Content
		if content == "" {
			content = res.Reason
			rec.Result = res.Reason
		}
	case res.Outcome == agentapi.OutcomeError:
		rec.Error = res.Reason
		content = res.Content
		if content == "" {
			content = fmt.Sprintf("error: %s", res.Reason)
		}
	default: // OutcomeOK, or OutcomeWaiting for anything but a budget.
		rec.Result = res.Content
		content = res.Content
	}
	return rec, content, nil
}

// modelRound checkpoints one model call and retries a stalled or failed
// provider inside the remaining budget before involving a person.
func (r *Runner) modelRound(ctx context.Context, taskID, system, manifestVersion string, messages []provider.Message, specs []provider.Tool) (*provider.ChatResponse, error) {
	var lastErr error
	for try := 0; try <= r.RetryLimit; try++ {
		if err := r.checkAuthority(taskID, "model attempt"); err != nil {
			return nil, err
		}
		resp, err := r.modelAttempt(ctx, taskID, system, manifestVersion, messages, specs)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) {
			break
		}
		if r.Ledger != nil {
			if epoch, ferr := r.Ledger.FenceAttempts(taskID); ferr == nil {
				r.trace(taskID, agentruntime.LedgerTraceEvent{
					Type: agentruntime.TraceAttemptFenced, Payload: jsonPayload(map[string]any{"epoch": epoch, "try": try + 1}),
				})
			}
		}
	}

	_ = r.Client.Wait(ctx, agentapi.WorkWait{
		Handle: r.Handle, Reason: agentapi.WorkWaitModelUnavailable, Detail: "provider call failed",
	})
	r.trace(taskID, agentruntime.LedgerTraceEvent{
		Type:    agentruntime.TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": string(agentapi.WorkWaitModelUnavailable), "tries": r.RetryLimit + 1}),
	})
	if r.Notifier != nil {
		_ = r.Notifier.NotifyStalled(taskID,
			"I could not reach the model to finish this. Nothing is lost — say continue and I will pick up where I left off.")
	}
	return nil, fmt.Errorf("agentruntime/legacy: chat: %w", lastErr)
}

// modelAttempt is one checkpointed model call.
func (r *Runner) modelAttempt(ctx context.Context, taskID, system, manifestVersion string, messages []provider.Message, specs []provider.Tool) (*provider.ChatResponse, error) {
	opened, err := r.Client.Checkpoint(ctx, agentapi.Checkpoint{
		Handle: r.Handle, Kind: agentapi.StepModel, Name: r.providerName(), State: agentapi.StepProposed,
		Params: json.RawMessage(jsonPayload(map[string]any{"messages": len(messages), "tools": len(specs)})),
	})
	if err != nil {
		return nil, fmt.Errorf("agentruntime/legacy: checkpointing a model call: %w", err)
	}
	started, err := r.Client.Checkpoint(ctx, agentapi.Checkpoint{Handle: r.Handle, Step: opened.Step, State: agentapi.StepRunning})
	if err != nil {
		return nil, fmt.Errorf("agentruntime/legacy: checkpointing a model call: %w", err)
	}
	r.trace(taskID, agentruntime.LedgerTraceEvent{
		Type: agentruntime.TraceModelCall, Step: opened.Step, Attempt: started.Attempt,
		ModelEndpoint: r.providerName(), ManifestVersion: manifestVersion,
	})

	callCtx := ctx
	if r.StallTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, r.StallTimeout)
		defer cancel()
	}
	resp, err := r.Provider.Chat(callCtx, provider.ChatRequest{
		System: system, Messages: provider.SafeMessages(messages), Tools: specs,
	})
	if err != nil {
		stalled := errors.Is(callCtx.Err(), context.DeadlineExceeded)
		_, _ = r.Client.Checkpoint(ctx, agentapi.Checkpoint{
			Handle: r.Handle, Step: opened.Step, Attempt: started.Attempt, State: agentapi.StepFailed, Error: err.Error(),
		})
		r.trace(taskID, agentruntime.LedgerTraceEvent{
			Type: agentruntime.TraceOutcome, Step: opened.Step, Attempt: started.Attempt,
			Payload: jsonPayload(map[string]any{"providerError": true, "stalled": stalled}),
		})
		if stalled {
			return nil, fmt.Errorf("provider stalled after %s: %w", r.StallTimeout, err)
		}
		return nil, err
	}
	// The coordinator is rechecked AFTER the call, before its result is
	// accepted — a model call takes seconds, and a promotion during one
	// changes the coordinator lease, which Checkpoint's own epoch check
	// on the NEXT write would not catch for an answer with no tool
	// calls (it goes straight to completion).
	if r.Ledger != nil {
		if err := r.Ledger.Fence(); err != nil {
			r.trace(taskID, agentruntime.LedgerTraceEvent{
				Type: agentruntime.TraceDenial, Step: opened.Step, Attempt: started.Attempt,
				Payload: jsonPayload(map[string]any{"reason": "coordinator changed during the model call"}),
			})
			return nil, fmt.Errorf("agentruntime/legacy: a model answer arrived after this coordinator was superseded: %w", err)
		}
	}
	if _, err := r.Client.Checkpoint(ctx, agentapi.Checkpoint{
		Handle: r.Handle, Step: opened.Step, Attempt: started.Attempt, State: agentapi.StepVerified,
		Result: json.RawMessage(jsonPayload(map[string]any{"toolCalls": len(resp.ToolCalls)})),
	}); err != nil {
		return nil, fmt.Errorf("agentruntime/legacy: closing a model checkpoint: %w", err)
	}
	r.trace(taskID, agentruntime.LedgerTraceEvent{
		Type: agentruntime.TraceModelAnswer, Step: opened.Step, Attempt: started.Attempt,
		Payload: jsonPayload(map[string]any{"toolCalls": toolNames(resp.ToolCalls)}),
	})
	return resp, nil
}

// complete finishes the task with evidence drawn from the step ledger:
// one row per verified tool step, plus the delivered answer.
func (r *Runner) complete(ctx context.Context, taskID, finalText string) error {
	var evidence []agentapi.Evidence
	if r.Ledger != nil {
		if steps, err := r.Ledger.Steps(taskID); err == nil {
			for _, s := range steps {
				if s.Kind != agentruntime.LedgerStepTool || s.State != agentruntime.LedgerStepVerified {
					continue
				}
				var result agentruntime.ToolStepResult
				var receiptStep int64
				if json.Unmarshal([]byte(s.Result), &result) == nil && (result.EffectConfirmed || result.ProposalConfirmed) && !result.Rejected {
					receiptStep = s.Seq
				}
				evidence = append(evidence, agentapi.Evidence{
					ReceiptStep:  receiptStep,
					ProposalOnly: result.ProposalConfirmed,
					Kind:         "tool-result",
					Description:  fmt.Sprintf("%s returned a result that was used in the answer", s.Name),
					SourceID:     s.Name,
				})
			}
		}
	}
	evidence = append(evidence, agentapi.Evidence{
		Kind:        "delivery",
		Description: "an answer was produced and returned to the person who asked",
	})
	r.trace(taskID, agentruntime.LedgerTraceEvent{
		Type: agentruntime.TraceOutcome, Payload: jsonPayload(map[string]any{"evidence": len(evidence)}),
	})
	return r.Client.Complete(ctx, agentapi.WorkComplete{Handle: r.Handle, Summary: summarize(finalText), Evidence: evidence})
}

// toolsFromManifest turns the manifest's tool specs into the
// provider-neutral shape a Provider.Chat call takes.
func toolsFromManifest(specs []agentapi.ToolSpec) []provider.Tool {
	out := make([]provider.Tool, 0, len(specs))
	for _, s := range specs {
		var schema map[string]any
		if len(s.InputSchema) > 0 {
			_ = json.Unmarshal(s.InputSchema, &schema)
		}
		out = append(out, provider.Tool{Name: s.Name, Description: s.Description, InputSchema: schema})
	}
	return out
}

// summarize bounds a completion summary to a line's worth of the answer.
func summarize(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= 200 {
		return s
	}
	return truncate(s, 200)
}

// truncate bounds s to at most n bytes, on a rune boundary, appending a
// marker if it cut content off. A byte-for-byte copy of
// keystone/butler's own (gateway.go), which this package cannot import.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n...[truncated, %d bytes omitted]", len(s)-cut)
}

func toolNames(calls []provider.ToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// jsonPayload renders a small structural payload: counts and flags, not
// content.
func jsonPayload(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(body)
}
