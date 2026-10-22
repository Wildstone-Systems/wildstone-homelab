package butlerlive

// closureClient is the pre-Phase-2 wiring, kept reachable behind
// Butlers.UseAgentGateway=false (ADR-W039 Phase 2b, #337).
//
// keystone/agentgateway.Gateway resolves a task Handle afresh on every
// call, rebuilding the Toolset each time so a Console change is visible
// on the next one, and checks a runtime-principal binding. It cannot be
// built here: it imports this package (for Gateways, Turn, Scoped,
// SystemPromptFor), so this package importing it back would be a cycle.
//
// closureClient is what Serve builds itself instead: an agentapi.Client
// over the ONE Toolset already scoped for this turn (Serve.Scoped
// setup, unchanged from before Phase 2) and this package's own Ledger,
// with no handle resolution and no runtime-principal check — trust is
// the process's own, exactly as it was before Phase 1 minted a runtime
// principal at all. Its Invoke/Checkpoint/Work methods are, on purpose,
// the same sequence keystone/agentgateway's are (ADR-W039 §3.4): the
// two are meant to write identical ledger rows and traces for the same
// script, which is the regression proof the switch exists to let a test
// make (see agentgateway's switch-parity test).
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/operation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type closureClient struct {
	Toolset  butler.Toolset
	Ledger   *butlerstore.Ledger
	TaskID   string
	Label    string
	Audience string
	System   string
	ReadOnly bool
	Now      func() time.Time

	mu      sync.Mutex
	records []butler.ToolCallRecord
}

var _ agentapi.Client = (*closureClient)(nil)

func (c *closureClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// forThisTask refuses a call for any task but the one this client was
// built for — the closure path's own, minimal stand-in for a handle
// check: there is exactly one task in play for the lifetime of one
// Serve call, and this is the whole of what a Handle would otherwise
// have proven.
func (c *closureClient) forThisTask(h agentapi.Handle) error {
	if h.TaskID() != c.TaskID {
		return fmt.Errorf("butlerlive: closure client is bound to %s, not %s", c.TaskID, h.TaskID())
	}
	return nil
}

func (c *closureClient) record(rec butler.ToolCallRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
}

// Records returns the audit records of every call this client served,
// in call order — the same seam keystone/agentgateway.Gateway offers,
// so Serve reads either one identically after Run.
func (c *closureClient) Records(string) []butler.ToolCallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]butler.ToolCallRecord, len(c.records))
	copy(out, c.records)
	return out
}

// Forget drops the kept records. taskID is unused: a closureClient
// serves exactly one task for its whole life.
func (c *closureClient) Forget(string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = nil
}

func (c *closureClient) Manifest(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
	if err := c.forThisTask(h); err != nil {
		return agentapi.Manifest{}, err
	}
	caps := c.Toolset.Capabilities(c.ReadOnly)
	mutating := make(map[string]bool, len(caps.Tools))
	for _, t := range caps.Tools {
		mutating[t.Name] = t.Mutating
	}
	offered := c.Toolset.OfferedSpecs(c.ReadOnly)
	tools := make([]agentapi.ToolSpec, 0, len(offered))
	for _, spec := range offered {
		var schema json.RawMessage
		if spec.InputSchema != nil {
			body, err := json.Marshal(spec.InputSchema)
			if err != nil {
				return agentapi.Manifest{}, fmt.Errorf("butlerlive: encoding the input schema of %s: %w", spec.Name, err)
			}
			schema = body
		}
		tools = append(tools, agentapi.ToolSpec{
			Name: spec.Name, Description: spec.Description, InputSchema: schema, ReadOnlyHint: !mutating[spec.Name],
		})
	}
	actions := make([]agentapi.ActionSpec, 0, len(caps.Actions))
	for _, a := range caps.Actions {
		actions = append(actions, agentapi.ActionSpec{Tool: a.Tool, Tier: a.Tier})
	}
	return agentapi.Manifest{
		AgentClass:      agentapi.AgentClass(caps.AgentClass),
		SystemPrompt:    c.System,
		Tools:           tools,
		ReadOnly:        c.ReadOnly,
		ProposableKinds: caps.ProposableKinds,
		Actions:         actions,
		Unavailable:     caps.Unavailable,
		Fingerprint:     caps.Fingerprint(),
	}, nil
}

// Invoke mirrors keystone/agentgateway/invoke.go's Invoke step for
// step, against the Toolset already scoped for this turn instead of one
// resolved fresh from a handle.
func (c *closureClient) Invoke(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
	if err := c.forThisTask(req.Handle); err != nil {
		return agentapi.InvokeResult{}, err
	}
	if req.Tool == "" {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: "no tool was named"}, nil
	}
	var input map[string]any
	if len(req.Args) > 0 {
		if err := json.Unmarshal(req.Args, &input); err != nil {
			return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: err.Error()}, nil
		}
	}
	rec := butler.ToolCallRecord{Name: req.Tool, Input: input}

	// 1. The dispatch boundary.
	if err := c.Ledger.Authorize(c.TaskID); err != nil {
		c.trace(butler.LedgerTraceEvent{
			Type: butler.TraceDenial, Payload: jsonPayload(map[string]any{"authority": "withdrawn", "at": "tool dispatch"}),
		})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: err.Error()}, nil
	}

	// 2. The aggregate budget.
	used, limit, uncertain, err := c.Ledger.AggregateToolCalls(c.TaskID)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("butlerlive: reading conversation budget: %w", err)
	}
	if limit <= 0 {
		limit = conversationToolCalls
	}
	if uncertain {
		return agentapi.InvokeResult{
			Outcome: agentapi.OutcomeWaiting, WaitReason: agentapi.WaitBudgetExhausted,
			Reason: "an earlier turn has an unresolved effect outcome",
		}, nil
	}
	if limit > 0 && used >= limit {
		return agentapi.InvokeResult{
			Outcome: agentapi.OutcomeWaiting, WaitReason: agentapi.WaitBudgetExhausted,
			Reason: fmt.Sprintf("%d tool calls used; this task's budget needs review before it continues", used),
		}, nil
	}

	// 3. The ledger retrieval.
	fp := butler.CallFingerprint(req.Tool, input)
	steps, err := c.Ledger.Steps(c.TaskID)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("butlerlive: reading the step ledger: %w", err)
	}
	verified := butler.VerifiedResults(steps, c.now(), observationTTL)
	if content, ok := verified[fp]; ok && fp != "" && req.Tool != "recall" {
		if allowed, why := c.Toolset.Permits(req.Tool, c.ReadOnly); !allowed {
			c.trace(butler.LedgerTraceEvent{
				Type: butler.TraceDenial, SourceIDs: []string{req.Tool},
				Payload: jsonPayload(map[string]any{"authority": "withdrawn", "at": "ledger retrieval"}),
			})
			rec.Rejected, rec.Result = true, why
			c.record(rec)
			return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: why}, nil
		}
		rec.Result = content
		c.record(rec)
		c.trace(butler.LedgerTraceEvent{
			Type: butler.TraceToolResult, SourceIDs: []string{req.Tool}, Payload: jsonPayload(map[string]any{"servedFromLedger": true}),
		})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: content, ServedFromLedger: true}, nil
	}

	// 4-5. Fence and checkpoint before dispatch.
	if err := c.Ledger.Fence(); err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("butlerlive: %w", err)
	}
	step, err := c.Ledger.AppendStep(c.TaskID, butler.LedgerStepTool, req.Tool, jsonPayload(input))
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("butlerlive: checkpointing a tool call: %w", err)
	}
	rec.StepSeq = step
	attempt, err := c.Ledger.StartAttempt(step)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("butlerlive: checkpointing a tool call: %w", err)
	}
	c.trace(butler.LedgerTraceEvent{Type: butler.TraceToolDispatched, Step: step, Attempt: attempt.Seq, SourceIDs: []string{req.Tool}})

	// 6. The dispatch.
	id := operation.Identity{TaskID: c.TaskID, Step: step, PermissionLabel: c.Label, Audience: c.Audience}
	outcome, err := c.Toolset.Execute(operation.With(ctx, id), req.Tool, input, c.ReadOnly)
	if err != nil {
		rec.Error = err.Error()
		c.record(rec)
		_ = c.Ledger.FinishAttempt(attempt.Seq, false, "", err.Error())
		_ = c.Ledger.SetStepState(step, butler.LedgerStepFailed)
		c.trace(butler.LedgerTraceEvent{Type: butler.TraceToolResult, Step: step, Attempt: attempt.Seq, Payload: jsonPayload(map[string]any{"error": true})})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeError, Content: fmt.Sprintf("error: %v", err), Reason: err.Error()}, nil
	}
	rec.Result, rec.Plan, rec.ResourceYAML, rec.Rejected = outcome.Content, outcome.Plan, outcome.ResourceYAML, outcome.Rejected
	c.record(rec)

	// 7. The result checkpoint.
	result := butler.ToolStepResult{
		EffectPending: outcome.EffectPending && !outcome.Rejected, EffectConfirmed: outcome.EffectConfirmed && !outcome.Rejected,
		ProposalConfirmed: outcome.ProposalConfirmed && !outcome.Rejected, Version: 1, Fingerprint: fp,
		Content: outcome.Content, Rejected: outcome.Rejected,
	}
	body, _ := json.Marshal(result)
	if err := c.Ledger.FinishAttempt(attempt.Seq, true, string(body), ""); errors.Is(err, runtimedb.ErrAttemptFenced) {
		_ = c.Ledger.SetStepState(step, butler.LedgerStepFailed)
		c.trace(butler.LedgerTraceEvent{Type: butler.TraceAttemptFenced, Step: step, Attempt: attempt.Seq, SourceIDs: []string{req.Tool}})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeError, Reason: "the attempt was fenced while the tool ran, so its result was refused"}, nil
	}
	if outcome.Rejected {
		_ = c.Ledger.SetStepState(step, butler.LedgerStepFailed)
	} else {
		_ = c.Ledger.SetStepState(step, butler.LedgerStepVerified)
	}
	c.trace(butler.LedgerTraceEvent{
		Type: butler.TraceToolResult, Step: step, Attempt: attempt.Seq, SourceIDs: []string{req.Tool},
		Payload: jsonPayload(map[string]any{"rejected": outcome.Rejected, "bytes": len(outcome.Content)}),
	})
	switch {
	case outcome.Rejected && outcome.Plan != nil:
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeRejected, Content: outcome.Content, Reason: outcome.Content}, nil
	case outcome.Rejected:
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Content: outcome.Content, Reason: outcome.Content}, nil
	case outcome.EffectPending:
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeWaiting, WaitReason: agentapi.WaitConfirmPending, Content: outcome.Content}, nil
	}
	res := agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: outcome.Content}
	if outcome.ProposalConfirmed || (req.Tool == "propose_change" && outcome.Plan != nil) {
		res.Proposal = &agentapi.ProposalRef{ID: fmt.Sprintf("%s/step/%d", c.TaskID, step)}
	}
	return res, nil
}

func (c *closureClient) trace(e butler.LedgerTraceEvent) {
	e.PermissionLabel, e.Audience = c.Label, c.Audience
	_ = c.Ledger.Trace(c.TaskID, e)
}

func (c *closureClient) Checkpoint(ctx context.Context, ck agentapi.Checkpoint) (agentapi.CheckpointReceipt, error) {
	if err := c.forThisTask(ck.Handle); err != nil {
		return agentapi.CheckpointReceipt{}, err
	}
	if err := c.Ledger.Fence(); err != nil {
		return agentapi.CheckpointReceipt{}, fmt.Errorf("butlerlive: %w", err)
	}
	if err := c.Ledger.Authorize(c.TaskID); err != nil {
		return agentapi.CheckpointReceipt{}, err
	}
	if ck.State == agentapi.StepProposed {
		params := string(ck.Params)
		if params == "" {
			params = "{}"
		}
		step, err := c.Ledger.AppendStep(c.TaskID, string(ck.Kind), ck.Name, params)
		if err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: step}, nil
	}
	switch ck.State {
	case agentapi.StepRunning:
		attempt, err := c.Ledger.StartAttempt(ck.Step)
		if err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: ck.Step, Attempt: attempt.Seq, Epoch: attempt.Epoch}, nil
	case agentapi.StepVerified, agentapi.StepFailed:
		if ck.Attempt != 0 {
			if err := c.Ledger.FinishAttempt(ck.Attempt, ck.State == agentapi.StepVerified, string(ck.Result), ck.Error); err != nil {
				return agentapi.CheckpointReceipt{}, err
			}
		}
		if err := c.Ledger.SetStepState(ck.Step, string(ck.State)); err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: ck.Step, Attempt: ck.Attempt}, nil
	case agentapi.StepSkipped:
		if err := c.Ledger.SetStepState(ck.Step, string(ck.State)); err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: ck.Step}, nil
	}
	return agentapi.CheckpointReceipt{}, fmt.Errorf("butlerlive: %q is not a checkpoint state", ck.State)
}

func (c *closureClient) Start(ctx context.Context, w agentapi.WorkStart) error {
	if err := c.forThisTask(w.Handle); err != nil {
		return err
	}
	if err := c.Ledger.Fence(); err != nil {
		return fmt.Errorf("butlerlive: %w", err)
	}
	if err := c.Ledger.Authorize(c.TaskID); err != nil {
		return err
	}
	reason := w.Reason
	if reason == "" {
		reason = "slice started"
	}
	return c.Ledger.Running(c.TaskID, reason)
}

func (c *closureClient) Wait(ctx context.Context, w agentapi.WorkWait) error {
	if err := c.forThisTask(w.Handle); err != nil {
		return err
	}
	if err := c.Ledger.Fence(); err != nil {
		return fmt.Errorf("butlerlive: %w", err)
	}
	return c.Ledger.Wait(c.TaskID, string(w.Reason), w.Detail)
}

func (c *closureClient) Complete(ctx context.Context, w agentapi.WorkComplete) error {
	if err := c.forThisTask(w.Handle); err != nil {
		return err
	}
	if err := c.Ledger.Fence(); err != nil {
		return fmt.Errorf("butlerlive: refusing to complete work after this coordinator was superseded: %w", err)
	}
	evidence := make([]butler.LedgerEvidence, 0, len(w.Evidence))
	for _, e := range w.Evidence {
		evidence = append(evidence, butler.LedgerEvidence{
			ReceiptID: e.ReceiptID, ProposalOnly: e.ProposalOnly, ReceiptStep: e.ReceiptStep,
			Kind: e.Kind, Description: e.Description, SourceID: e.SourceID,
			PermissionLabel: c.Label, Audience: c.Audience,
		})
	}
	return c.Ledger.Complete(c.TaskID, evidence, w.Summary)
}

func (c *closureClient) Fail(ctx context.Context, w agentapi.WorkFail) error {
	if err := c.forThisTask(w.Handle); err != nil {
		return err
	}
	if err := c.Ledger.Fence(); err != nil {
		return fmt.Errorf("butlerlive: %w", err)
	}
	if err := c.Ledger.Fail(c.TaskID, w.Reason); err != nil {
		return err
	}
	_, err := c.Ledger.FenceAttempts(c.TaskID)
	return err
}

func (c *closureClient) Heartbeat(context.Context, agentapi.Heartbeat) error {
	return &agentapi.NotImplementedError{Method: "Heartbeat", Phase: "n/a", Reason: "the closure path has no runtime principal to report for"}
}

func (c *closureClient) Recall(context.Context, agentapi.RecallRequest) ([]agentapi.MemoryHit, error) {
	return nil, &agentapi.NotImplementedError{Method: "Recall", Phase: "Phase 3", Reason: "invoke the recall tool for the rendered form"}
}

func (c *closureClient) Remember(ctx context.Context, req agentapi.RememberRequest) error {
	args := map[string]any{"content": req.Content}
	if req.Scope != "" {
		args["scope"] = string(req.Scope)
	}
	if req.Kind != "" {
		args["kind"] = string(req.Kind)
	}
	if req.Subject != "" {
		args["subject"] = req.Subject
	}
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := c.Invoke(ctx, agentapi.InvokeRequest{Handle: req.Handle, Tool: "remember", Args: body})
	if err != nil {
		return err
	}
	if res.Outcome != agentapi.OutcomeOK {
		return fmt.Errorf("butlerlive: remember %s: %s", res.Outcome, res.Reason)
	}
	return nil
}

func (c *closureClient) Propose(context.Context, agentapi.ProposeRequest) (agentapi.ProposalRef, error) {
	return agentapi.ProposalRef{}, &agentapi.NotImplementedError{Method: "Propose", Phase: "Phase 3", Reason: "invoke propose_change instead"}
}

func (c *closureClient) Notify(context.Context, agentapi.NotifyRequest) error {
	return &agentapi.NotImplementedError{Method: "Notify", Phase: "Phase 4", Reason: "outbound delivery through the task's Connection is the Events family"}
}

func jsonPayload(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(body)
}
