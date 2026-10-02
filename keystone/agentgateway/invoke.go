package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Invoke implements agentapi.Client: one capability call, §3.4 steps
// 2–7.
//
// Duplicated-until-2b: this is Runner.toolCall (runner.go:702-800)
// with the runner's pre-dispatch authority check (stillAuthorized,
// "tool dispatch") and its aggregate-budget check folded in, so the
// ledger rows and trace events it writes are the ones the runner
// writes today, row for row. The differences are the ones the ADR
// asks for: the operation.Identity comes from the handle, and the
// result is an Outcome rather than a ToolCallRecord — the record is
// still kept, on the gateway, for the handler that files proposals
// after a turn (Records).
//
// Ordering, preserved from the runner because a resume depends on it:
//
//  1. Authorize — the task may still continue (A2.1: "at every
//     retrieval and dispatch").
//  2. Budget — the aggregate tool budget is not exhausted.
//  3. Ledger retrieval — a verified result for this exact call, still
//     fresh, is served WITHOUT a step, after re-asking the Toolset
//     whether the tool is permitted right now (Permits). Recall is
//     never served this way.
//  4. Fence — this coordinator still holds the database.
//  5. AppendStep, StartAttempt, trace tool.dispatched.
//  6. Execute under the handle's Identity.
//  7. FinishAttempt, then SetStepState (verified, or failed for an
//     error or a rejection), then trace tool.result.
//
// Step 7's order matters: FinishAttempt is what carries the epoch
// check, and a step marked verified before its attempt was accepted
// would be served on a resume even though its result was refused.
func (g *Gateway) Invoke(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
	s, err := g.open(ctx, req.Handle)
	if err != nil {
		return refusalOutcome(err)
	}
	if req.Tool == "" {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: "no tool was named"}, nil
	}
	input, err := decodeArgs(req.Args)
	if err != nil {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: err.Error()}, nil
	}
	taskID := s.hc.Task.ID
	label, audience := s.hc.Butler.Name, string(s.hc.Audience)
	rec := butler.ToolCallRecord{Name: req.Tool, Input: input}

	// 1. The dispatch boundary (runner.go:493, stillAuthorized "tool
	// dispatch").
	if err := g.Ledger.Authorize(taskID); err != nil {
		g.trace(s, butler.LedgerTraceEvent{
			Type:    butler.TraceDenial,
			Payload: jsonPayload(map[string]any{"authority": "withdrawn", "at": "tool dispatch"}),
		})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: err.Error()}, nil
	}

	// 1b. A Task a subscription spawned may only use ITS capability set,
	// re-read live from the subscription row (ADR-W039 §3.6): narrowing
	// or disabling a subscription after it already matched still binds
	// the Task it spawned, and a tool absent from that set is refused
	// here however the Toolset itself would answer.
	if err := g.checkSubscriptionScope(s, req.Tool); err != nil {
		g.trace(s, butler.LedgerTraceEvent{
			Type:      butler.TraceDenial,
			SourceIDs: []string{req.Tool},
			Payload:   jsonPayload(map[string]any{"authority": "withdrawn", "at": "subscription scope"}),
		})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: err.Error()}, nil
	}

	// Restore and check source taint before effects or cached ledger output.
	class, err := g.disclosureClass(s.hc.Butler.Seq)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("agentgateway: resolving the runtime's processing class: %w", err)
	}
	dctx, disclosure, err := g.beginDisclosureTurn(ctx, class, s.turn.MemoryDestination(), taskID)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("agentgateway: opening the disclosure turn: %w", err)
	}
	if err := disclosure.Check(); err != nil {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: "privacy_denied: task source policy refuses this destination"}, nil
	}

	// Steps 2–5 are one reservation: see Gateway.reserve.
	g.reserve.Lock()
	release := sync.OnceFunc(g.reserve.Unlock)
	defer release()

	// 2. The aggregate budget (runner.go:474-488). The runner moves the
	// task to waiting when it hits this itself; the gateway only
	// refuses, because the slice's state transition is the runner's
	// until Phase 4 makes waiting a Work call.
	if exhausted, used, err := g.budgetExhausted(taskID); err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("agentgateway: reading conversation budget: %w", err)
	} else if exhausted {
		return agentapi.InvokeResult{
			Outcome:    agentapi.OutcomeWaiting,
			WaitReason: agentapi.WaitBudgetExhausted,
			Reason:     fmt.Sprintf("%d tool calls used; this task's budget needs review before it continues", used),
		}, nil
	}

	// 3. The ledger retrieval (runner.go:706-742). Workspace resources must
	// re-enter their resource/receipt seam on every call; tool permission
	// alone cannot reauthorize cached private content.
	fp := butler.CallFingerprint(req.Tool, input)
	steps, err := g.Ledger.Steps(taskID)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("agentgateway: reading the step ledger: %w", err)
	}
	verified := butler.VerifiedResults(steps, g.now(), g.observationTTL())
	if content, ok := verified[fp]; ok && fp != "" && req.Tool != "recall" && !strings.HasPrefix(req.Tool, "workspace_") && req.Tool != "artifact_export" {
		if allowed, why := s.toolset.Permits(req.Tool, s.readOnly); !allowed {
			g.trace(s, butler.LedgerTraceEvent{
				Type:      butler.TraceDenial,
				SourceIDs: []string{req.Tool},
				Payload:   jsonPayload(map[string]any{"authority": "withdrawn", "at": "ledger retrieval"}),
			})
			rec.Rejected, rec.Result = true, why
			g.record(taskID, rec)
			return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: why}, nil
		}
		rec.Result = content
		g.record(taskID, rec)
		g.trace(s, butler.LedgerTraceEvent{
			Type:      butler.TraceToolResult,
			SourceIDs: []string{req.Tool},
			Payload:   jsonPayload(map[string]any{"servedFromLedger": true}),
		})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: content, ServedFromLedger: true}, nil
	}

	// 4–5. Fence and checkpoint before dispatch.
	if err := g.Ledger.Fence(); err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("agentgateway: %w", err)
	}
	step, err := g.Ledger.AppendStep(taskID, butler.LedgerStepTool, req.Tool, jsonPayload(input))
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("agentgateway: checkpointing a tool call: %w", err)
	}
	rec.StepSeq = step
	// The step is written, so this call now counts against the budget
	// whatever happens to it: the reservation is complete.
	release()
	attempt, err := g.Ledger.StartAttempt(step)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("agentgateway: checkpointing a tool call: %w", err)
	}
	g.trace(s, butler.LedgerTraceEvent{
		Type: butler.TraceToolDispatched, Step: step, Attempt: attempt.Seq,
		SourceIDs: []string{req.Tool},
	})

	// 6. The dispatch, under an identity the runtime never sees. Every
	// field comes from the handle: the task, this step, the Butler's
	// name as the permission label, the task's admitted audience.
	//
	// The disclosure turn combines the restored task/session sources with
	// this call's reads. Its result check persists new sources before the
	// runtime receives content, so retries cannot shed earlier restrictions.
	id := operation.Identity{TaskID: taskID, Step: step, PermissionLabel: label, Audience: audience}
	outcome, err := s.toolset.Execute(operation.With(dctx, id), req.Tool, input, s.readOnly)
	if err != nil {
		rec.Error = err.Error()
		g.record(taskID, rec)
		_ = g.Ledger.FinishAttempt(attempt.Seq, false, "", err.Error())
		_ = g.Ledger.SetStepState(step, butler.LedgerStepFailed)
		g.trace(s, butler.LedgerTraceEvent{
			Type: butler.TraceToolResult, Step: step, Attempt: attempt.Seq,
			Payload: jsonPayload(map[string]any{"error": true}),
		})
		// Research admission has already parked this task atomically with its
		// durable counters. Preserve the existing waiting wire outcome so both
		// native and legacy slices stop instead of treating exhaustion as a
		// retriable upstream error. A typed error alone cannot invent a wait.
		if errors.Is(err, runtimedb.ErrResearchBudgetExhausted) {
			parked, checkErr := g.DB.IsResearchBudgetWait(taskID)
			if checkErr != nil {
				return agentapi.InvokeResult{}, checkErr
			}
			if parked {
				return agentapi.InvokeResult{Outcome: agentapi.OutcomeWaiting,
					WaitReason: agentapi.WaitBudgetExhausted, Reason: runtimedb.ErrResearchBudgetExhausted.Error()}, nil
			}
		}
		return agentapi.InvokeResult{
			Outcome: agentapi.OutcomeError,
			Content: fmt.Sprintf("error: %v", err),
			Reason:  err.Error(),
		}, nil
	}

	// The result of a real, successful call is refused here rather than
	// handed back — the SAME refusal runtimedb.ErrPrivacy carries and
	// privacy_test.go:32 pins, now enforced at the gateway boundary
	// instead of by wrapping the provider (butlerlive/privacy.go). The
	// step is failed, not verified, so a later resume never serves this
	// content from the ledger cache either.
	if derr := disclosure.Check(); derr != nil {
		refusal := disclosureRefusalError("invoke", derr)
		rec.Rejected, rec.Result = true, refusal.Error()
		g.record(taskID, rec)
		_ = g.Ledger.FinishAttempt(attempt.Seq, false, "", refusal.Error())
		_ = g.Ledger.SetStepState(step, butler.LedgerStepFailed)
		g.trace(s, butler.LedgerTraceEvent{
			Type: butler.TraceToolResult, Step: step, Attempt: attempt.Seq,
			SourceIDs: []string{req.Tool},
			Payload:   jsonPayload(map[string]any{"disclosureRefused": true}),
		})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: refusal.Error()}, nil
	}

	rec.Result, rec.Plan, rec.ResourceYAML, rec.Rejected = outcome.Content, outcome.Plan, outcome.ResourceYAML, outcome.Rejected

	// File an accepted propose_change's Plan right here, under the
	// runtime principal, rather than leaving it for the caller to read
	// ResourceYAML back off the ToolCallRecord after the whole turn
	// (ADR-W039 §3.2 Proposals, Phase 3, #339 — replacing C9).
	// Gated on g.Proposals being wired at all: an unwired gateway (most
	// existing fixtures and the conformance harness) keeps exactly the
	// pre-Phase-3 behaviour of returning the Plan for a caller to file
	// itself. Once wired, ResourceYAML is cleared either way — filed or
	// refused — so a caller reading ToolCallRecord afterwards (the
	// legacy single-provider path's own fallback) never re-files this
	// call under a different identity.
	if req.Tool == "propose_change" && !outcome.Rejected && outcome.Plan != nil && g.Proposals.DB != nil {
		rec.ResourceYAML = ""
		summary := "butler proposal"
		if outcome.Plan.Summary != "" {
			summary = fmt.Sprintf("%s: %s", s.hc.Butler.Name, outcome.Plan.Summary)
		}
		if pid, err := g.runtimeIdentity(ctx, s); err != nil {
			rec.ProposalError = err.Error()
		} else if view, err := g.fileProposal(pid, proposals.Request{
			Summary: summary, Put: []string{outcome.ResourceYAML},
		}); err != nil {
			rec.ProposalError = err.Error()
		} else {
			rec.ProposalID = view.ID
		}
	}
	g.record(taskID, rec)

	// 7. The result checkpoint, in the runner's envelope. A rejected
	// call is a real, established outcome but not a verified READ, so
	// it is not served from the ledger on a resume.
	result := butler.ToolStepResult{
		EffectPending:     outcome.EffectPending && !outcome.Rejected,
		EffectConfirmed:   outcome.EffectConfirmed && !outcome.Rejected,
		ProposalConfirmed: outcome.ProposalConfirmed && !outcome.Rejected,
		Version:           1,
		Fingerprint:       fp,
		Content:           outcome.Content,
		Rejected:          outcome.Rejected,
	}
	body, _ := json.Marshal(result)
	if err := g.Ledger.FinishAttempt(attempt.Seq, true, string(body), ""); errors.Is(err, runtimedb.ErrAttemptFenced) {
		// The lease moved while the tool ran: this attempt was fenced
		// and its result is refused (§3.7, ADR-W030 A4). The step is
		// failed rather than verified so no resume serves it.
		_ = g.Ledger.SetStepState(step, butler.LedgerStepFailed)
		g.trace(s, butler.LedgerTraceEvent{
			Type: butler.TraceAttemptFenced, Step: step, Attempt: attempt.Seq,
			SourceIDs: []string{req.Tool},
		})
		return agentapi.InvokeResult{
			Outcome: agentapi.OutcomeError,
			Reason:  "the attempt was fenced while the tool ran, so its result was refused",
		}, nil
	}
	if outcome.Rejected {
		_ = g.Ledger.SetStepState(step, butler.LedgerStepFailed)
	} else {
		_ = g.Ledger.SetStepState(step, butler.LedgerStepVerified)
	}
	g.trace(s, butler.LedgerTraceEvent{
		Type: butler.TraceToolResult, Step: step, Attempt: attempt.Seq,
		SourceIDs: []string{req.Tool},
		Payload:   jsonPayload(map[string]any{"rejected": outcome.Rejected, "bytes": len(outcome.Content)}),
	})
	res := resultFor(taskID, step, req.Tool, outcome)
	// The real filed Plan id, when this call filed one above — replacing
	// resultFor's ledger-step placeholder, which is what an unwired
	// gateway (g.Proposals.DB == nil) still returns.
	if req.Tool == "propose_change" && !outcome.Rejected && outcome.Plan != nil && g.Proposals.DB != nil {
		switch {
		case rec.ProposalID != "":
			res.Proposal = &agentapi.ProposalRef{ID: rec.ProposalID}
		case rec.ProposalError != "":
			res.Outcome, res.Reason, res.Proposal = agentapi.OutcomeError, rec.ProposalError, nil
		}
	}
	return res, nil
}

// resultFor maps a Toolset outcome onto the closed outcome set.
//
// The Toolset does not say whether a rejection came from authority or
// from policy — both arrive as Rejected with the reason in Content —
// so the split is by evidence: propose_change's policy check is the
// one rejection that produces a Plan (for audit, never approvable),
// and that is OutcomeRejected; every other rejection is a refusal to
// run, OutcomeDenied. Phase 3 files proposals inside the gateway and
// can name the policy verdict directly.
func resultFor(taskID string, step int64, tool string, outcome butler.ToolOutcome) agentapi.InvokeResult {
	switch {
	case outcome.Rejected && outcome.Plan != nil:
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeRejected, Content: outcome.Content, Reason: outcome.Content}
	case outcome.Rejected:
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Content: outcome.Content, Reason: outcome.Content}
	case outcome.EffectPending:
		return agentapi.InvokeResult{
			Outcome: agentapi.OutcomeWaiting, WaitReason: agentapi.WaitConfirmPending, Content: outcome.Content,
		}
	}
	res := agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: outcome.Content}
	if outcome.ProposalConfirmed || (tool == "propose_change" && outcome.Plan != nil) {
		// The proposal is not FILED here yet — the handler still does
		// that after the turn (C9, Phase 3) — so the reference is the
		// ledger step that recorded it. Phase 3 replaces it with the
		// filed Plan's id; the runtime treats it as opaque either way.
		res.Proposal = &agentapi.ProposalRef{ID: fmt.Sprintf("%s/step/%d", taskID, step)}
	}
	return res
}

// budgetExhausted is the runner's aggregate budget check
// (runner.go:403-419, 474): the durable conversation aggregate when
// the ledger keeps one (Serve sets UseParentBudget), else the task's
// own tool-step count against the conversation limit.
func (g *Gateway) budgetExhausted(taskID string) (bool, int, error) {
	used, limit, _, err := g.Ledger.AggregateToolCalls(taskID)
	if err != nil {
		return false, 0, err
	}
	if limit <= 0 {
		limit = g.toolCalls()
	}
	return limit > 0 && used >= limit, used, nil
}

// trace records one work-trace event stamped with the task's own label,
// audience and person (the runner's PermissionLabel, Audience and
// Actor, which Serve sets from the same facts), ignoring a write failure as the runner does: a trace
// that cannot be written must not take down the work it describes.
func (g *Gateway) trace(s *session, e butler.LedgerTraceEvent) {
	e.PermissionLabel, e.Audience = s.hc.Butler.Name, string(s.hc.Audience)
	if e.Actor == "" {
		e.Actor = s.hc.Person
	}
	_ = g.Ledger.Trace(s.hc.Task.ID, e)
}

// decodeArgs turns the wire arguments into the map a Toolset takes.
// Absent or null arguments are a nil map, which is what a model call
// with no input carries today, so the fingerprint agrees.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("the arguments are not a JSON object: %v", err)
	}
	return input, nil
}

// checkSubscriptionScope is the ONE thing an Owner-approved
// subscription's capability set can do to a bounded Task it spawned
// (ADR-W039 §3.6): narrow. A Task the handle's own Task.GrantsJSON does
// not mark as a subscription's is untouched by this check — every other
// Task's authority is exactly what the rest of Invoke already computes.
// For one that IS marked, the subscription row is re-read fresh on
// every call, never cached on the Task: disabling or narrowing it after
// it already matched takes effect on the Task it spawned immediately,
// and a tool absent from its CURRENT capability set is refused however
// the subscription's stored document once named it.
func (g *Gateway) checkSubscriptionScope(s *session, tool string) error {
	subID, ok := automation.SubscriptionOrigin(s.hc.Task.GrantsJSON)
	if !ok {
		return nil
	}
	caps, enabled, err := g.DB.SubscriptionCapabilities(subID)
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("%w: the subscription behind this task is no longer enabled", ErrRefused)
	}
	for _, c := range caps {
		if c == tool {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not in this subscription's capability set", ErrRefused, tool)
}

// jsonPayload renders a small structural payload, as the runner does.
func jsonPayload(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(body)
}
