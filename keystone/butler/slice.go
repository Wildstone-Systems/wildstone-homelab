package butler

// Slice: one slice of durable work whose reasoning loop runs somewhere
// else (ADR-W039).
//
// Runner.Run owns its loop: it calls the model, reads the answer, runs
// the tools. An agent runtime (keystone/agentruntime) owns a loop too,
// and the one thing it must not do is walk around the boundaries Run
// keeps — the fence, the authority re-checks, the step ledger, the
// aggregate budget, completion with evidence. Slice is those boundaries
// with the loop taken out: the runtime tells it when a model attempt
// starts and ends and asks it to run each tool, and every decision about
// what that means for the task is made here, by the same code Run uses.
//
// It adds nothing Run does not do. The one new piece of vocabulary is
// Blocked, for an agent that says it cannot continue; it parks the task
// the way a spent slice budget does.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// Slice is one slice's durable state. Its methods are safe for
// concurrent use: a runtime may abandon a tool call that ignores its
// deadline and carry on, so a late call can overlap the next event.
type Slice struct {
	r           *Runner
	p           provider.Provider
	taskID      string
	userMessage string

	mu        sync.Mutex
	start     sliceStart
	tr        *Transcript
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
func (r *Runner) BeginSlice(p provider.Provider, taskID, userMessage string) (*Slice, *Transcript, error) {
	s, tr, err := r.begin(p, taskID)
	if s == nil {
		return nil, tr, err
	}
	return &Slice{r: r, p: p, taskID: taskID, userMessage: userMessage, start: *s, tr: tr}, tr, nil
}

// Transcript is the slice's transcript so far: every tool call made, and
// FinalText once the slice has ended.
func (s *Slice) Transcript() *Transcript {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tr
}

// Specs is the tool set a model is offered.
func (s *Slice) Specs() []provider.Tool { return s.r.Gateway.OfferedSpecs(s.r.ReadOnly) }

// SystemPrompt is the surface's prompt, as Run would send it.
func (s *Slice) SystemPrompt() string { return s.r.Gateway.SystemPrompt() }

// ModelStarted is the checkpoint before a model attempt, retries
// included: authority is re-checked, the coordinator fenced, and a model
// step and attempt recorded. An error means the attempt must not be
// made.
func (s *Slice) ModelStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelOpen {
		// The previous attempt never reported back. Close it as failed
		// rather than leave an attempt open forever.
		s.closeModelLocked(false, nil)
	}
	if err := s.r.stillAuthorized(s.taskID, "model attempt"); err != nil {
		return err
	}
	if err := s.r.Ledger.Fence(); err != nil {
		return fmt.Errorf("butler runner: %w", err)
	}
	step, err := s.r.Ledger.AppendStep(s.taskID, LedgerStepModel, s.p.Name(),
		jsonPayload(map[string]any{"runtime": "native", "tools": len(s.Specs())}))
	if err != nil {
		return fmt.Errorf("butler runner: checkpointing a model call: %w", err)
	}
	attempt, err := s.r.Ledger.StartAttempt(step)
	if err != nil {
		return fmt.Errorf("butler runner: checkpointing a model call: %w", err)
	}
	s.modelStep, s.modelTry, s.modelOpen = step, attempt.Seq, true
	s.r.trace(s.taskID, LedgerTraceEvent{
		Type: TraceModelCall, Step: step, Attempt: attempt.Seq,
		ModelEndpoint: s.p.Name(), ModelVerdict: s.start.manifest.ModelVerdict,
		ManifestVersion: s.start.manifest.Fingerprint(),
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
		_ = s.r.Ledger.FinishAttempt(attempt, false, "", "provider call failed")
		_ = s.r.Ledger.SetStepState(step, LedgerStepFailed)
		s.r.trace(s.taskID, LedgerTraceEvent{
			Type: TraceOutcome, Step: step, Attempt: attempt,
			Payload: jsonPayload(map[string]any{"providerError": true}),
		})
		if epoch, err := s.r.Ledger.FenceAttempts(s.taskID); err == nil {
			s.r.trace(s.taskID, LedgerTraceEvent{
				Type: TraceAttemptFenced, Payload: jsonPayload(map[string]any{"epoch": epoch}),
			})
		}
		return nil
	}
	if err := s.r.Ledger.Fence(); err != nil {
		s.r.trace(s.taskID, LedgerTraceEvent{
			Type: TraceDenial, Step: step, Attempt: attempt,
			Payload: jsonPayload(map[string]any{"reason": "coordinator changed during the model call"}),
		})
		return fmt.Errorf("butler runner: a model answer arrived after this coordinator was superseded: %w", err)
	}
	if err := s.r.Ledger.FinishAttempt(attempt, true,
		jsonPayload(map[string]any{"toolCalls": len(toolCalls)}), ""); err != nil {
		return fmt.Errorf("butler runner: closing a model checkpoint: %w", err)
	}
	if err := s.r.Ledger.SetStepState(step, LedgerStepVerified); err != nil {
		return fmt.Errorf("butler runner: closing a model checkpoint: %w", err)
	}
	names := append([]string{}, toolCalls...)
	s.r.trace(s.taskID, LedgerTraceEvent{
		Type: TraceModelAnswer, Step: step, Attempt: attempt,
		Payload: jsonPayload(map[string]any{"toolCalls": names}),
	})
	return nil
}

// Call runs one tool call through the ledger, exactly as Run's loop
// does: the aggregate budget, the dispatch authority check, and toolCall
// (which serves a still-fresh verified result instead of dispatching
// again). An error means the whole slice must stop.
func (s *Slice) Call(ctx context.Context, name string, input map[string]any) (ToolCallResult, error) {
	s.mu.Lock()
	if s.start.toolLimit > 0 && s.start.toolsUsed >= s.start.toolLimit {
		err := s.r.budgetExhausted(s.taskID, s.tr, s.start.toolsUsed)
		s.mu.Unlock()
		return ToolCallResult{Parked: true}, err
	}
	// The budget is RESERVED here, before the lock is released, and
	// handed back below if nothing was dispatched. Counting only after
	// the call returned let a call the runtime had abandoned (a tool
	// that ignored its deadline) leave the count stale, so the next call
	// passed the check too and the task went one past its cap.
	s.start.toolsUsed++
	s.calls++
	id := fmt.Sprintf("native-%d", s.calls)
	// A copy: toolCall writes into the map it is given, and the lock is
	// NOT held while the tool runs. A tool that ignores its deadline is
	// abandoned by the runtime, and it must not hold up the next
	// checkpoint while it finishes.
	verified := make(map[string]string, len(s.start.verified))
	for k, v := range s.start.verified {
		verified[k] = v
	}
	s.mu.Unlock()

	if err := s.r.stillAuthorized(s.taskID, "tool dispatch"); err != nil {
		s.mu.Lock()
		s.start.toolsUsed--
		s.mu.Unlock()
		return ToolCallResult{}, err
	}
	rec, content, dispatched := s.r.toolCall(ctx, s.taskID, provider.ToolCall{ID: id, Name: name, Input: input}, verified)

	s.mu.Lock()
	if !dispatched {
		// Served from the ledger, or refused before it went out: it
		// spent nothing, exactly as Run counts it.
		s.start.toolsUsed--
	}
	s.tr.Audit.Calls = append(s.tr.Audit.Calls, rec)
	if rec.Plan != nil {
		s.tr.Audit.Plans = append(s.tr.Audit.Plans, rec.Plan)
	}
	if fresh, err := s.r.Ledger.Steps(s.taskID); err == nil {
		s.start.steps = fresh
		s.start.verified = verifiedResults(fresh, s.r.now(), s.r.ObservationTTL)
	}
	s.mu.Unlock()
	return ToolCallResult{Content: content, Failed: rec.Error != "" || rec.Rejected}, nil
}

// Finish accepts the runtime's final answer, as Run accepts one with no
// tool calls: authority, the objective check, completion with evidence.
func (s *Slice) Finish(ctx context.Context, answer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.finish(ctx, s.p, s.taskID, s.userMessage, answer, s.start.manifest, s.tr, s.start.steps)
}

// Pause parks the task at the slice limit, as Run does.
func (s *Slice) Pause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.pause(s.taskID, s.tr)
}

// AskUser parks the task waiting for the person's reply to question.
// The question is what they are shown.
func (s *Slice) AskUser(question string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.r.stillAuthorized(s.taskID, "question"); err != nil {
		return err
	}
	s.tr.FinalText = question
	s.r.trace(s.taskID, LedgerTraceEvent{
		Type:    TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": LedgerWaitReply, "asked": true}),
	})
	return s.r.Ledger.Wait(s.taskID, LedgerWaitReply, "the Butler asked a question")
}

// Blocked parks the task because the agent could not make progress. The
// reason is shown to the person, who can say how to continue.
func (s *Slice) Blocked(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.r.stillAuthorized(s.taskID, "blocked"); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	s.tr.CappedIterations = true
	s.tr.FinalText = "I couldn't finish this: " + reason + ". Tell me how you'd like me to continue."
	s.r.trace(s.taskID, LedgerTraceEvent{
		Type:    TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": LedgerWaitReply, "blocked": true}),
	})
	return s.r.Ledger.Wait(s.taskID, LedgerWaitReply, "the Butler could not make progress")
}

// Unavailable parks the task because the model or the runtime could not
// be used, and sends the one stalled-task notice, as Run does when its
// retries are spent. It returns the error the caller reports.
func (s *Slice) Unavailable(detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.r.Ledger.Wait(s.taskID, LedgerWaitModelUnavailable, "provider call failed")
	s.r.trace(s.taskID, LedgerTraceEvent{
		Type:    TraceWaitOpened,
		Payload: jsonPayload(map[string]any{"reason": LedgerWaitModelUnavailable, "runtime": "native"}),
	})
	if s.r.Notifier != nil {
		_ = s.r.Notifier.NotifyStalled(s.taskID,
			"I could not reach the model to finish this. Nothing is lost — say continue and I will pick up where I left off.")
	}
	return fmt.Errorf("butler runner: chat: %s", detail)
}
