package main

// The delivery loop: long-poll Wildstone, turn each delivery into a
// Hermes run (or a documented no-op), report back, and acknowledge only
// once that report has landed.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi/httpclient"
)

// keystoneClient is what the delivery loop needs from Wildstone,
// narrowed to an interface so a test can drive it against an in-memory
// fake instead of a real HTTP server.
type keystoneClient interface {
	agentapi.Client
	agentapi.DeliveryClient
}

// heartbeatInterval is how often the bridge tells Wildstone it is still
// here (H3 brief: "heartbeat every 30s").
const heartbeatInterval = 30 * time.Second

// bridge is the whole program's state: two loops and the registry they
// share with the MCP server.
type bridge struct {
	keystone keystoneClient
	hermes   hermesRunner
	runs     *runRegistry
	log      *slog.Logger
	pollWait time.Duration
	version  string
	// health is whether Hermes is answering (health.go). nil skips the
	// check, for tests that are not about it.
	health *healthGate

	// noHealthLogged makes the "no /health endpoint" notice one line.
	noHealthLogged atomic.Bool

	// escalationLogged makes the "no answer path yet" notice a one-time
	// thing (H3 brief: "log once"), not one line per escalation delivery.
	escalationLogged atomic.Bool
}

// heartbeatLoop reports liveness on heartbeatInterval, starting
// immediately rather than waiting a full interval for the first one —
// Wildstone's own health state (unregistered/offline → connected) turns
// on this arriving, and the tasks it re-offers should not wait 30s for
// nothing.
//
// Each beat is sent only if Hermes answers its health probe first
// (health.go). While it does not, the probe repeats on the shorter
// healthProbeDownInterval so a recovery is reported promptly.
func (b *bridge) heartbeatLoop(ctx context.Context) {
	for {
		wait := heartbeatInterval
		if b.probeHermes(ctx) {
			if err := b.keystone.Heartbeat(ctx, agentapi.Heartbeat{At: time.Now().UTC(), Version: b.version}); err != nil && ctx.Err() == nil {
				b.log.Warn("heartbeat", "err", err)
			}
		} else {
			wait = healthProbeDownInterval
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// deliveryLoop long-polls Wildstone's deliveries endpoint and processes
// each delivery in order, oldest first (agentapi.DeliveryClient's own
// contract). after tracks the delivery id the bridge has fully settled
// up to; it only advances past a delivery this bridge has acknowledged,
// so a delivery it could not act on (Hermes unreachable) is offered
// again on the very next poll rather than skipped (ADR-W040 §4: "queued,
// not lost").
func (b *bridge) deliveryLoop(ctx context.Context) {
	after := ""
	backoff := time.Second
	for ctx.Err() == nil {
		if b.health != nil && b.health.wait(ctx) != nil {
			return
		}
		deliveries, err := b.keystone.Deliveries(ctx, after, b.pollWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log.Error("polling deliveries", "err", err)
			backoff = sleepBackoff(ctx, backoff)
			continue
		}
		settled := true
		for _, d := range deliveries {
			if !b.handleDelivery(ctx, d) {
				// Stop at the first delivery this poll could not settle:
				// everything after it in the batch (by id order) is
				// offered again next time regardless, so there is
				// nothing to gain by racing ahead of it, and doing so
				// would let `after` skip past the one delivery that
				// still needs a retry.
				settled = false
				break
			}
			after = d.ID
		}
		if settled {
			backoff = time.Second
		} else {
			// The next poll hands the same delivery straight back, so
			// retrying at once is a hot loop against Keystone (and its
			// rate limiter) until whatever failed recovers.
			backoff = sleepBackoff(ctx, backoff)
		}
	}
}

// handleDelivery dispatches one delivery by kind and reports whether it
// was settled (acknowledged, one way or another). false means: leave it
// unacked, try again on the next poll.
func (b *bridge) handleDelivery(ctx context.Context, d agentapi.Delivery) bool {
	switch d.Kind {
	case agentapi.DeliveryEscalation:
		// H3 brief: "ack with a Fail-free no-op and log once (no answer
		// path exists yet; documented)." An escalation delivery carries
		// no handle at all (keystone/cmd/keystone/reviewed_escalator.go
		// writes {"rule","instruction","fallback"}, no "handle" key), so
		// there is no Work call this bridge could even make on it yet.
		if !b.escalationLogged.Swap(true) {
			b.log.Warn("escalation delivery received; this runtime has no answer path yet (ADR-W039 Phase 4; see agent-bridge's own doc comment)",
				"delivery", d.ID)
		}
		return b.ack(ctx, d.ID)
	case agentapi.DeliveryTurn, agentapi.DeliverySubscription:
		return b.handleTurn(ctx, d)
	default:
		// agentapi.DeliveryProposalOutcome and anything future: nothing
		// this bridge knows how to act on. Acknowledging rather than
		// looping forever on a kind it will never understand better by
		// waiting.
		b.log.Info("delivery of a kind this bridge does not act on; acknowledging", "kind", d.Kind, "delivery", d.ID)
		return b.ack(ctx, d.ID)
	}
}

// handleTurn is the delivery→run→report→ack path for a turn or
// subscription delivery.
func (b *bridge) handleTurn(ctx context.Context, d agentapi.Delivery) bool {
	if d.Handle.IsZero() {
		// Should not happen (deliveriesFor populates Handle from the
		// payload for exactly these two kinds) — acknowledge rather than
		// retry forever on a delivery this bridge can never complete.
		b.log.Error("turn/subscription delivery carries no handle", "delivery", d.ID, "kind", d.Kind)
		return b.ack(ctx, d.ID)
	}

	manifest, err := b.keystone.Manifest(ctx, d.Handle)
	if err != nil {
		if refused(err) {
			return b.dropRefused(ctx, d, err)
		}
		b.log.Error("fetching manifest", "delivery", d.ID, "handle", d.Handle, "err", err)
		return false
	}

	if err := b.keystone.Start(ctx, agentapi.WorkStart{Handle: d.Handle, Reason: "delivery"}); err != nil {
		if refused(err) {
			return b.dropRefused(ctx, d, err)
		}
		b.log.Error("starting task", "delivery", d.ID, "handle", d.Handle, "err", err)
		return false
	}

	req := buildRunRequest(manifest, d.Payload)
	runID, err := b.hermes.StartRun(ctx, req, d.ID)
	if err != nil {
		b.log.Warn("hermes is unreachable; leaving this delivery unacked", "delivery", d.ID, "err", err)
		b.waitOnHermesFailure(ctx, d.Handle, "the hermes runtime could not start a run")
		return false
	}
	b.runs.set(runID, d.Handle)
	defer b.runs.delete(runID)
	b.log.Info("hermes run started", "delivery", d.ID, "kind", d.Kind, "run", runID)

	outcome, err := b.hermes.WaitForRun(ctx, runID)
	if err != nil {
		b.log.Error("watching hermes run", "delivery", d.ID, "run", runID, "err", err)
		b.waitOnHermesFailure(ctx, d.Handle, "lost contact with the hermes run")
		return false
	}

	if outcome.Terminal == "completed" && strings.TrimSpace(outcome.Output) != "" {
		// The answer is reported, not self-certified: Wildstone judges
		// it and completes the task (agentapi.WorkComplete.Reply).
		if err := b.keystone.Complete(ctx, agentapi.WorkComplete{
			Handle: d.Handle, Summary: "hermes run " + runID + " completed", Reply: outcome.Output,
		}); err != nil {
			if refused(err) {
				return b.dropRefused(ctx, d, err)
			}
			b.log.Error("reporting the run's answer", "delivery", d.ID, "run", runID, "err", err)
			return false
		}
		b.log.Info("hermes run answered", "delivery", d.ID, "run", runID)
	} else {
		reason := outcome.Terminal
		if outcome.Detail != "" {
			reason += ": " + outcome.Detail
		}
		if outcome.Terminal == "completed" {
			reason = "the hermes run completed with no answer"
		}
		b.log.Warn("hermes run did not answer", "delivery", d.ID, "run", runID, "reason", reason)
		if err := b.keystone.Fail(ctx, agentapi.WorkFail{Handle: d.Handle, Reason: reason}); err != nil {
			b.log.Error("failing task", "delivery", d.ID, "run", runID, "err", err)
			return false
		}
	}
	return b.ack(ctx, d.ID)
}

// refused reports whether Keystone refused the call outright (a 403): a
// handle it no longer accepts — the task finished, was cancelled, or was
// re-issued a handle by a later turn — or a credential it does not bind.
// Retrying cannot change that answer.
func refused(err error) bool { return errors.Is(err, httpclient.ErrRefused) }

// dropRefused acknowledges a delivery whose handle Keystone refused, so
// it stops blocking every delivery queued behind it. Nothing is lost:
// the task's handle is Keystone's to issue, and a task that still needs
// this runtime is handed to it again with a live one.
func (b *bridge) dropRefused(ctx context.Context, d agentapi.Delivery, err error) bool {
	b.log.Warn("Wildstone refused this delivery's task handle; acknowledging it without a run",
		"delivery", d.ID, "handle", d.Handle, "err", err)
	return b.ack(ctx, d.ID)
}

// waitOnHermesFailure records that the task is waiting on its own
// runtime rather than failing it outright: Hermes being unreachable is
// exactly the transient condition agentapi.WorkWaitModelUnavailable
// names, and the delivery being left unacked (the caller's job) means
// Wildstone will offer it again. A failure here is logged, not
// escalated: the caller already knows this delivery did not settle.
func (b *bridge) waitOnHermesFailure(ctx context.Context, h agentapi.Handle, detail string) {
	if err := b.keystone.Wait(ctx, agentapi.WorkWait{Handle: h, Reason: agentapi.WorkWaitModelUnavailable, Detail: detail}); err != nil {
		b.log.Error("recording wait after a hermes failure", "handle", h, "err", err)
	}
}

// ack acknowledges one delivery, logging and reporting failure rather
// than panicking: an unacked delivery is simply retried.
func (b *bridge) ack(ctx context.Context, id string) bool {
	if err := b.keystone.Ack(ctx, id); err != nil {
		b.log.Error("acking delivery", "delivery", id, "err", err)
		return false
	}
	return true
}
