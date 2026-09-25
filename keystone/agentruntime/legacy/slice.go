package legacy

// Slice: one slice of durable work whose reasoning loop runs somewhere
// else (ADR-W041; the OpenAI Agents SDK sidecar in keystone/agentruntime/
// native, #357).
//
// Runner.Run owns its loop: it calls the model, reads the answer, runs
// the tools. An agent runtime owns a loop too, and the one thing it must
// not do is walk around the boundaries Run keeps — the fence, the
// authority re-checks, the step ledger, the aggregate budget, completion
// with evidence. Slice is those boundaries with the loop taken out: the
// runtime tells it when a model attempt starts and ends and asks it to
// run each tool, and every decision about what that means for the task
// is made here, by the same code Run uses.
//
// Like Run, a Slice reaches the task only through agentapi.Client: the
// model checkpoints are Client.Checkpoint, a tool call is Client.Invoke
// (which rechecks authority, spends the aggregate budget and serves a
// still-verified read from the ledger), and every way the slice can end
// is Client.Complete or Client.Wait. An SDK-driven Butler is therefore
// held to exactly the contract an external runtime is (ADR-W039 §2).
//
// It adds nothing Run does not do. The one new piece of vocabulary is
// Blocked, for an agent that says it cannot continue; it parks the task
// the way a spent slice budget does.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// Slice is one slice's durable state. Its methods are safe for
// concurrent use: a runtime may abandon a tool call that ignores its
// deadline and carry on, so a late call can overlap the next event.
type Slice struct {
	r           *Runner
	taskID      string
	userMessage string
	manifest    agentapi.Manifest
	// ctx carries what the client needs to resolve the handle (the
	// runtime principal, on the gateway path) with the caller's
	// cancellation removed: parking a task because the person went away
	// must still be written after their request is cancelled.
	ctx context.Context

	mu        sync.Mutex
	tr        *agentruntime.Transcript
	calls     int
	modelStep int64
	modelTry  int64
	modelOpen bool
}

// ToolCallResult is what one Slice.Call produced.
type ToolCallResult struct {
	// Content is what the model is shown.
	Content string
	// Failed is true when the call errored or was refused by policy.
	Failed bool
	// Parked is true when the task has been parked (its aggregate budget
	// is spent): the slice must end, and Transcript().FinalText says why.
	Parked bool
}

// BeginSlice opens a slice for an external loop. It does everything Run
// does before its first model call. A nil *Slice means the slice must
// not proceed; the transcript and error are then the caller's answer, as
// they would be from Run.
func (r *Runner) BeginSlice(ctx context.Context, userMessage string) (*Slice, *agentruntime.Transcript, error) {
	tr := &agentruntime.Transcript{Audit: agentruntime.Audit{Provider: r.providerName()}}
	if r.Client == nil {
		return nil, tr, fmt.Errorf("agentruntime/legacy: no agent API client configured")
	}
	if r.Handle.IsZero() {
		return nil, tr, fmt.Errorf("agentruntime/legacy: no task handle configured")
	}
	taskID := r.Handle.TaskID()
	manifest, err := r.start(ctx, taskID)
	if err != nil {
		return nil, tr, err
	}
	return &Slice{
		r: r, taskID: taskID, userMessage: userMessage, manifest: manifest,
		ctx: context.WithoutCancel(ctx), tr: tr,
	}, tr, nil
}

// Transcript is the slice's transcript so far: every tool call made, and
// FinalText once the slice has ended.
func (s *Slice) Transcript() *agentruntime.Transcript {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tr
}

// Specs is the tool set a model is offered: the manifest's, as Run
// offers it.
func (s *Slice) Specs() []provider.Tool { return toolsFromManifest(s.manifest.Tools) }

// SystemPrompt is the surface's prompt, as Run would send it.
func (s *Slice) SystemPrompt() string { return s.manifest.SystemPrompt }

// ModelStarted is the checkpoint before a model attempt, retries
// included: authority is re-checked and a model step and attempt
// recorded. An error means the attempt must not be made.
func (s *Slice) ModelStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelOpen {
		// The previous attempt never reported back. Close it as failed
		// rather than leave an attempt open forever.
		_ = s.closeModelLocked(false, nil)
	}
	if err := s.r.checkAuthority(s.taskID, "model attempt"); err != nil {
		return err
	}
	opened, err := s.r.Client.Checkpoint(s.ctx, agentapi.Checkpoint{
		Handle: s.r.Handle, Kind: agentapi.StepModel, Name: s.r.providerName(), State: agentapi.StepProposed,
		Params: json.RawMessage(jsonPayload(map[string]any{"runtime": "native", "tools": len(s.manifest.Tools)})),
	})
	if err != nil {
		return fmt.Errorf("agentruntime/legacy: checkpointing a model call: %w", err)
	}
	started, err := s.r.Client.Checkpoint(s.ctx, agentapi.Checkpoint{Handle: s.r.Handle, Step: opened.Step, State: agentapi.StepRunning})
	if err != nil {
		return fmt.Errorf("agentruntime/legacy: checkpointing a model call: %w", err)
	}
	s.modelStep, s.modelTry, s.modelOpen = opened.Step, started.Attempt, true
	s.r.trace(s.taskID, agentruntime.LedgerTraceEvent{
		Type: agentruntime.TraceModelCall, Step: opened.Step, Attempt: started.Attempt,
		ModelEndpoint: s.r.providerName(), ManifestVersion: s.manifest.Fingerprint,
	})
	return nil
}

// ModelEnded is the checkpoint after an attempt. A failed attempt is
// closed failed and the task's attempts are fenced, so a late answer
// from it is refused, exactly as Run does before a retry. A successful
// one is accepted only if this coordinator still holds the database.
func (s *Slice) ModelEnded(ok bool, toolCalls []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.modelOpen {
		return nil
	}
	return s.closeModelLocked(ok, toolCalls)
}

func (s *Slice) closeModelLocked(ok bool, toolCalls []string) error {
	s.modelOpen = false
	step, attempt := s.modelStep, s.modelTry
	if !ok {
		_, _ = s.r.Client.Checkpoint(s.ctx, agentapi.Checkpoint{
			Handle: s.r.Handle, Step: step, Attempt: attempt, State: agentapi.StepFailed, Error: "provider call failed",
		})
		s.r.trace(s.taskID, agentruntime.LedgerTraceEvent{
			Type: agentruntime.TraceOutcome, Step: step, Attempt: attempt,
			Payload: jsonPayload(map[string]any{"providerError": true}),
		})
		if s.r.Ledger != nil {
			if epoch, err := s.r.Ledger.FenceAttempts(s.taskID); err == nil {
				s.r.trace(s.taskID, agentruntime.LedgerTraceEvent{
					Type: agentruntime.TraceAttemptFenced, Payload: jsonPayload(map[string]any{"epoch": epoch}),
				})
			}
		}
		return nil
	}
	// The coordinator is rechecked before the answer is accepted, as in
	// Run's modelAttempt: an answer with no tool calls goes straight to
	// completion, so no later Checkpoint's epoch check would catch it.
	if s.r.Ledger != nil {
		if err := s.r.Ledger.Fence(); err != nil {
			s.r.trace(s.taskID, agentruntime.LedgerTraceEvent{
				Type: agentruntime.TraceDenial, Step: step, Attempt: attempt,
				Payload: jsonPayload(map[string]any{"reason": "coordinator changed during the model call"}),
			})
			return fmt.Errorf("agentruntime/legacy: a model answer arrived after this coordinator was superseded: %w", err)
		}
	}
	if _, err := s.r.Client.Checkpoint(s.ctx, agentapi.Checkpoint{
		Handle: s.r.Handle, Step: step, Attempt: attempt, State: agentapi.StepVerified,
		Result: json.RawMessage(jsonPayload(map[string]any{"toolCalls": len(toolCalls)})),
	}); err != nil {
		return fmt.Errorf("agentruntime/legacy: closing a model checkpoint: %w", err)
	}
	names := append([]string{}, toolCalls...)
	s.r.trace(s.taskID, agentruntime.LedgerTraceEvent{
		Type: agentruntime.TraceModelAnswer, Step: step, Attempt: attempt,
		Payload: jsonPayload(map[string]any{"toolCalls": names}),
	})
	return nil
}

// Call runs one tool call exactly as Run's loop does: through
// Client.Invoke, which rechecks authority, spends the aggregate budget
// and serves a still-fresh verified result instead of dispatching
// again. A budget the gateway reports spent parks the task. An error
// means the whole slice must stop.
//
// The lock is NOT held while the tool runs. A tool that ignores its
// deadline is abandoned by the runtime, and it must not hold up the next
// checkpoint while it finishes. The budget itself is the gateway's, kept
// in the ledger per task, so an abandoned call still counts against it.
func (s *Slice) Call(_ context.Context, name string, input map[string]any) (ToolCallResult, error) {
	s.mu.Lock()
	s.calls++
	id := fmt.Sprintf("native-%d", s.calls)
	s.mu.Unlock()

	// Invoke rechecks authority too, but reports a refusal as the call's
	// result, which Run's loop then shows the model before its next
	// model checkpoint stops it. A runtime's loop may run several more
	// tools in that time, so a withdrawn authority ends the slice here,
	// as an error, before anything is dispatched.
	if err := s.r.checkAuthority(s.taskID, "tool dispatch"); err != nil {
		return ToolCallResult{}, err
	}

	// s.ctx, not the runtime's per-call context: the runtime cancels
	// that when it abandons a call, and a dispatch the gateway already
	// made must still be checkpointed with its outcome.
	rec, content, wait := s.r.toolCall(s.ctx, s.taskID, provider.ToolCall{ID: id, Name: name, Input: input})

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tr.Audit.Calls = append(s.tr.Audit.Calls, rec)
	if wait != nil {
		s.tr.CappedIterations = true
		s.tr.FinalText = fmt.Sprintf(
			"(stopped after this task's tool-call budget — it needs review before it continues: %s)", wait.Reason)
		return ToolCallResult{Parked: true}, wait.err(s.r, s.ctx)
	}
	return ToolCallResult{Content: content, Failed: rec.Error != "" || rec.Rejected}, nil
}

// Finish accepts the runtime's final answer, as Run accepts one with no
// tool calls: authority, the objective check, completion with evidence.
func (s *Slice) Finish(_ context.Context, answer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.finish(s.ctx, s.taskID, s.userMessage, answer, s.manifest, s.tr)
}

// Pause parks the task at the slice limit, as Run does.
func (s *Slice) Pause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.pause(s.ctx, s.taskID, s.tr)
}

// AskUser parks the task waiting for the person's reply to question.
// The question is what they are shown.
func (s *Slice) AskUser(question string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.r.checkAuthority(s.taskID, "question"); err != nil {
		return err
	}
	s.tr.FinalText = question
	s.r.trace(s.taskID, agentruntime.LedgerTraceEvent{
		Type:    agentruntime.TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": string(agentapi.WorkWaitReply), "asked": true}),
	})
	return s.r.Client.Wait(s.ctx, agentapi.WorkWait{
		Handle: s.r.Handle, Reason: agentapi.WorkWaitReply, Detail: "the Butler asked a question",
	})
}

// Blocked parks the task because the agent could not make progress. The
// reason is shown to the person, who can say how to continue.
func (s *Slice) Blocked(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.r.checkAuthority(s.taskID, "blocked"); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	s.tr.CappedIterations = true
	s.tr.FinalText = "I couldn't finish this: " + reason + ". Tell me how you'd like me to continue."
	s.r.trace(s.taskID, agentruntime.LedgerTraceEvent{
		Type:    agentruntime.TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": string(agentapi.WorkWaitReply), "blocked": true}),
	})
	return s.r.Client.Wait(s.ctx, agentapi.WorkWait{
		Handle: s.r.Handle, Reason: agentapi.WorkWaitReply, Detail: "the Butler could not make progress",
	})
}

// Unavailable parks the task because the model or the runtime could not
// be used, and sends the one stalled-task notice, as Run does when its
// retries are spent. It returns the error the caller reports.
func (s *Slice) Unavailable(detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.r.Client.Wait(s.ctx, agentapi.WorkWait{
		Handle: s.r.Handle, Reason: agentapi.WorkWaitModelUnavailable, Detail: "provider call failed",
	})
	s.r.trace(s.taskID, agentruntime.LedgerTraceEvent{
		Type:    agentruntime.TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": string(agentapi.WorkWaitModelUnavailable), "runtime": "native"}),
	})
	if s.r.Notifier != nil {
		_ = s.r.Notifier.NotifyStalled(s.taskID,
			"I could not reach the model to finish this. Nothing is lost — say continue and I will pick up where I left off.")
	}
	return fmt.Errorf("agentruntime/legacy: chat: %s", detail)
}
