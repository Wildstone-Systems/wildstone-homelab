package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The Work family over butlerstore.Ledger (ADR-W039 §3.2 Work: the
// Wildstone-side implementation is unchanged and already here, so the
// in-process client serves it). Each method
// resolves the handle first: a checkpoint for a task the caller holds
// no handle for is refused before the ledger hears of it.

// Checkpoint implements agentapi.Client. One call is one ledger
// checkpoint, chosen by State, mirroring the runner's own sequence for
// a model call (runner.go:623-684):
//
//   - proposed, Step == 0: AppendStep for Kind and Name with Params;
//     the receipt carries the new step.
//   - running, Step set: StartAttempt on that step; the receipt
//     carries the attempt and the lease epoch it runs under.
//   - verified or failed, Step and Attempt set: FinishAttempt (ok for
//     verified, with Result; failed with Error), then SetStepState.
//   - skipped, Step set: SetStepState alone.
//
// The step must belong to the handle's task; a checkpoint naming
// another task's step is refused. A checkpoint under a fenced attempt
// is refused by the ledger's own epoch check, which is the guarantee
// §3.7 rests on.
func (g *Gateway) Checkpoint(ctx context.Context, c agentapi.Checkpoint) (agentapi.CheckpointReceipt, error) {
	s, err := g.open(ctx, c.Handle)
	if err != nil {
		return agentapi.CheckpointReceipt{}, err
	}
	taskID := s.hc.Task.ID
	if err := g.Ledger.Fence(); err != nil {
		return agentapi.CheckpointReceipt{}, fmt.Errorf("agentgateway: %w", err)
	}
	if err := g.Ledger.Authorize(taskID); err != nil {
		return agentapi.CheckpointReceipt{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	if !c.State.Valid() {
		return agentapi.CheckpointReceipt{}, fmt.Errorf("%w: %q is not a checkpoint state", ErrRefused, c.State)
	}
	if c.State == agentapi.StepProposed {
		if c.Step != 0 {
			return agentapi.CheckpointReceipt{}, fmt.Errorf("%w: a proposed checkpoint opens a step and cannot name one", ErrRefused)
		}
		if !c.Kind.Valid() {
			return agentapi.CheckpointReceipt{}, fmt.Errorf("%w: %q is not a step kind", ErrRefused, c.Kind)
		}
		params := string(c.Params)
		if params == "" {
			params = "{}"
		}
		step, err := g.Ledger.AppendStep(taskID, string(c.Kind), c.Name, params)
		if err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: step}, nil
	}
	if err := g.stepBelongs(taskID, c.Step); err != nil {
		return agentapi.CheckpointReceipt{}, err
	}
	switch c.State {
	case agentapi.StepRunning:
		attempt, err := g.Ledger.StartAttempt(c.Step)
		if err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: c.Step, Attempt: attempt.Seq, Epoch: attempt.Epoch}, nil
	case agentapi.StepVerified, agentapi.StepFailed:
		if c.Attempt != 0 {
			ok := c.State == agentapi.StepVerified
			if err := g.Ledger.FinishAttempt(c.Attempt, ok, string(c.Result), c.Error); err != nil {
				return agentapi.CheckpointReceipt{}, err
			}
		}
		if err := g.Ledger.SetStepState(c.Step, string(c.State)); err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: c.Step, Attempt: c.Attempt}, nil
	case agentapi.StepSkipped:
		if err := g.Ledger.SetStepState(c.Step, string(c.State)); err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: c.Step}, nil
	}
	return agentapi.CheckpointReceipt{}, fmt.Errorf("%w: %q is not a checkpoint state", ErrRefused, c.State)
}

// stepBelongs refuses a step that is not in the handle's task. The
// ledger keys attempts by step alone, so the task binding is the
// gateway's to check.
func (g *Gateway) stepBelongs(taskID string, step int64) error {
	if step == 0 {
		return fmt.Errorf("%w: the checkpoint names no step", ErrRefused)
	}
	steps, err := g.Ledger.Steps(taskID)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if s.Seq == step {
			return nil
		}
	}
	return fmt.Errorf("%w: step %d is not in %s", ErrRefused, step, taskID)
}

// Start implements agentapi.Client: the task moves to running. The
// ledger's Running enforces the state machine.
func (g *Gateway) Start(ctx context.Context, w agentapi.WorkStart) error {
	s, err := g.open(ctx, w.Handle)
	if err != nil {
		return err
	}
	if err := g.Ledger.Fence(); err != nil {
		return fmt.Errorf("agentgateway: %w", err)
	}
	if err := g.Ledger.Authorize(s.hc.Task.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	reason := w.Reason
	if reason == "" {
		reason = "slice started"
	}
	return g.Ledger.Running(s.hc.Task.ID, reason)
}

// Wait implements agentapi.Client: the task parks with a closed
// reason. The stall notice is not sent here — it is the runner's
// today and Wildstone's own in Phase 4.
func (g *Gateway) Wait(ctx context.Context, w agentapi.WorkWait) error {
	s, err := g.open(ctx, w.Handle)
	if err != nil {
		return err
	}
	if !w.Reason.Valid() {
		return fmt.Errorf("%w: %q is not a wait reason", ErrRefused, w.Reason)
	}
	if err := g.Ledger.Fence(); err != nil {
		return fmt.Errorf("agentgateway: %w", err)
	}
	return g.Ledger.Wait(s.hc.Task.ID, string(w.Reason), w.Detail)
}

// Complete implements agentapi.Client: evidence and completion,
// atomically, with the permission label and audience stamped from the
// task's own admission — never from the request, which cannot carry
// them.
func (g *Gateway) Complete(ctx context.Context, w agentapi.WorkComplete) error {
	s, err := g.open(ctx, w.Handle)
	if err != nil {
		return err
	}
	if err := g.Ledger.Fence(); err != nil {
		return fmt.Errorf("agentgateway: refusing to complete work after this coordinator was superseded: %w", err)
	}
	if w.Reply != "" && g.Replies != nil {
		// An answer, not a completion: Wildstone judges it and
		// completes the task itself (agentapi.WorkComplete's doc).
		if err := g.Ledger.Authorize(s.hc.Task.ID); err != nil {
			return fmt.Errorf("%w: %v", ErrRefused, err)
		}
		return g.Replies.RuntimeReplied(ctx, s.hc.Task.ID, w.Reply)
	}
	evidence := make([]butler.LedgerEvidence, 0, len(w.Evidence))
	for _, e := range w.Evidence {
		evidence = append(evidence, butler.LedgerEvidence{
			ReceiptID:       e.ReceiptID,
			ProposalOnly:    e.ProposalOnly,
			ReceiptStep:     e.ReceiptStep,
			Kind:            e.Kind,
			Description:     e.Description,
			SourceID:        e.SourceID,
			PermissionLabel: s.hc.Butler.Name,
			Audience:        string(s.hc.Audience),
		})
	}
	return g.Ledger.Complete(s.hc.Task.ID, evidence, w.Summary)
}

// Fail implements agentapi.Client: the task moves to failed and its
// in-flight attempts are fenced so a late result is refused.
func (g *Gateway) Fail(ctx context.Context, w agentapi.WorkFail) error {
	s, err := g.open(ctx, w.Handle)
	if err != nil {
		return err
	}
	if err := g.Ledger.Fence(); err != nil {
		return fmt.Errorf("agentgateway: %w", err)
	}
	if err := g.Ledger.Fail(s.hc.Task.ID, w.Reason); err != nil {
		return err
	}
	if _, err := g.Ledger.FenceAttempts(s.hc.Task.ID); err != nil {
		return err
	}
	if g.Replies != nil {
		g.Replies.RuntimeFailed(s.hc.Task.ID, w.Reason)
	}
	return nil
}

// Deliveries implements agentapi.DeliveryClient in-process: no HTTP, no
// polling loop needed at all — the caller already holds the answer as
// soon as it exists. after and wait are honoured exactly as the HTTP
// route interprets them (api/handlers_agent.go's agentDeliveries), so a
// runtime built against this interface behaves identically switched
// between the two transports.
func (g *Gateway) Deliveries(ctx context.Context, after string, wait time.Duration) ([]agentapi.Delivery, error) {
	principal, ok := principalFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: deliveries needs the runtime principal it is from", ErrRefused)
	}
	seq, bound, err := g.DB.RuntimePrincipalButler(principal)
	if err != nil {
		return nil, err
	}
	if !bound {
		return nil, fmt.Errorf("%w: %s is not a runtime principal", ErrRefused, principal)
	}
	var afterID int64
	if after != "" {
		if _, err := fmt.Sscanf(after, "%d", &afterID); err != nil {
			return nil, fmt.Errorf("%w: after must be a delivery id", ErrRefused)
		}
	}
	if wait < 0 {
		wait = 0
	}
	deadline := g.now().Add(wait)
	for {
		if _, err := g.DB.MarkExpiredAgentDeliveries(seq, g.now()); err != nil {
			return nil, err
		}
		rows, err := g.DB.ListUnackedAgentDeliveries(seq, afterID, 50)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return deliveriesFromRows(FilterDeliveries(g.DB, rows)), nil
		}
		if g.now().After(deadline) || ctx.Err() != nil {
			return []agentapi.Delivery{}, nil
		}
		select {
		case <-ctx.Done():
			return []agentapi.Delivery{}, nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// deliveriesFromRows is the in-process client's own version of
// api/handlers_agent.go's deliveriesFor — the identical mapping, kept
// as a second copy rather than a shared export because agentgateway
// cannot import keystone/api (the structural boundary Phase 2 pins) and
// this is the side that owns runtimedb.AgentDelivery.
func deliveriesFromRows(rows []runtimedb.AgentDelivery) []agentapi.Delivery {
	out := make([]agentapi.Delivery, 0, len(rows))
	for _, row := range rows {
		var carried struct {
			Handle       string `json:"handle"`
			Subscription int64  `json:"subscription"`
		}
		_ = json.Unmarshal([]byte(row.Payload), &carried)
		subscription := ""
		if carried.Subscription != 0 {
			subscription = fmt.Sprintf("%d", carried.Subscription)
		}
		out = append(out, agentapi.Delivery{
			ID: fmt.Sprintf("%d", row.ID), Kind: row.Kind, Subscription: subscription,
			Cursor: row.Cursor, Handle: agentapi.Handle(carried.Handle),
			Payload: json.RawMessage(row.Payload), Attempt: 1, ExpiresAt: row.ExpiresAt,
		})
	}
	return out
}

// Ack implements agentapi.DeliveryClient in-process.
func (g *Gateway) Ack(ctx context.Context, id string) error {
	principal, ok := principalFrom(ctx)
	if !ok {
		return fmt.Errorf("%w: ack needs the runtime principal it is from", ErrRefused)
	}
	seq, bound, err := g.DB.RuntimePrincipalButler(principal)
	if err != nil {
		return err
	}
	if !bound {
		return fmt.Errorf("%w: %s is not a runtime principal", ErrRefused, principal)
	}
	var deliveryID int64
	if _, err := fmt.Sscanf(id, "%d", &deliveryID); err != nil {
		return fmt.Errorf("%w: delivery id must be a number", ErrRefused)
	}
	if err := g.DB.AckAgentDelivery(seq, deliveryID); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return fmt.Errorf("%w: no such delivery for this agent", ErrRefused)
		}
		return err
	}
	return nil
}

var _ agentapi.DeliveryClient = (*Gateway)(nil)

// Heartbeat implements agentapi.Client. Liveness is a fact about a
// runtime, not a task, so the call carries no handle: the runtime is
// the principal the context authenticated, and the heartbeat lands on
// the Agent it is bound to. A context without a principal has no
// runtime to report for and is refused. Health stays Wildstone's own
// determination — TouchRuntimeHeartbeat records the arrival and
// refuses a revoked runtime; it never sets connected on the runtime's
// say-so.
func (g *Gateway) Heartbeat(ctx context.Context, hb agentapi.Heartbeat) error {
	if g.DB == nil {
		return fmt.Errorf("%w: the gateway is not wired", ErrRefused)
	}
	principal, ok := principalFrom(ctx)
	if !ok {
		return fmt.Errorf("%w: a heartbeat needs the runtime principal it is from", ErrRefused)
	}
	seq, bound, err := g.DB.RuntimePrincipalButler(principal)
	if err != nil {
		return err
	}
	if !bound {
		return fmt.Errorf("%w: %s is not a runtime principal", ErrRefused, principal)
	}
	return g.DB.TouchRuntimeHeartbeat(seq)
}
