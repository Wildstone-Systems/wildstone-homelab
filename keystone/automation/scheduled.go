package automation

// Scheduled Agent subscriptions (epic #326 S03, #456; ADR-W039 §3.6
// amendment): the coarse cadence trigger a mailbox monitor needs, run by
// the same coordinator-held Keystone loop as the event family — never a
// host cron.
//
// The evaluator decides; runtimedb makes it durable. Each tick:
//
//  1. refuses outright unless this process holds execution authority
//     (coordinator lease), the database is not in restore quarantine and
//     no Owner has pulled the household emergency stop;
//  2. abandons any run whose lease expired with no report (the runner
//     crashed) — a failure with bounded backoff, checkpoint untouched;
//  3. for each due, unheld, not-in-flight subscription, runs the dispatch
//     gate: the exact approved version, the canonical Person still
//     admitted, the source connection still theirs and not fenced, and
//     the BackgroundAuthority — the Person's own live authorization for
//     unattended use of that Personal source (S02). A refusal is recorded
//     as health, not failure: it neither counts toward backoff nor moves
//     the due time, so lifting it yields exactly one run;
//  4. claims a run (one in flight per subscription, by schema) and hands
//     it, with the last COMMITTED checkpoint, to a ScheduledDispatcher.
//
// A run ends in CompleteRun. Only an ok/empty report commits a
// checkpoint, only by compare-and-swap, and only after the live gate
// still agrees; a crashed, fenced or late run commits nothing, so the
// next run re-reads what it did not commit (at-least-once; the consumer
// dedupes by fingerprint) and nothing committed is ever skipped.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// BackgroundUse is one scheduled dispatch as the authority sees it: the
// exact subscription version, its Agent, the source connection at its
// current lease epoch, and the canonical Person it runs for.
type BackgroundUse struct {
	Subscription        int64
	SubscriptionVersion int
	Butler              int64
	// Connection is the source connection's name, "" for none.
	Connection      string
	ConnectionEpoch int64
	// PersonID is the canonical Person a Personal source binds the run
	// to, "" for none.
	PersonID string
}

// BackgroundAuthority is the dispatch gate's authority check: whether
// the Person who owns a Personal source has authorized THIS exact
// subscription version to use it unattended (ADR-W033 D2; epic #326
// S02). Owner approval is necessary and checked separately; it never
// substitutes for this. Consulted before every dispatch and again before
// every checkpoint commit. A nil error allows; anything else refuses.
type BackgroundAuthority interface {
	AuthorizeBackgroundUse(ctx context.Context, use BackgroundUse) error
}

// BackgroundAuthorityFunc adapts a function to BackgroundAuthority.
type BackgroundAuthorityFunc func(ctx context.Context, use BackgroundUse) error

// AuthorizeBackgroundUse calls f.
func (f BackgroundAuthorityFunc) AuthorizeBackgroundUse(ctx context.Context, use BackgroundUse) error {
	return f(ctx, use)
}

// ErrBackgroundUseDenied is what DenyAllBackgroundAuthority refuses
// with.
var ErrBackgroundUseDenied = errors.New("automation: no Personal authorization for unattended use of this source")

// DenyAllBackgroundAuthority refuses everything. It is the default: until
// a real evaluator is wired, no scheduled subscription dispatches.
type DenyAllBackgroundAuthority struct{}

// AuthorizeBackgroundUse always refuses.
func (DenyAllBackgroundAuthority) AuthorizeBackgroundUse(context.Context, BackgroundUse) error {
	return ErrBackgroundUseDenied
}

// ScheduledRun is what a dispatcher is handed: the claimed run, the
// subscription it is for, the checkpoint to resume from, and who it runs
// as.
type ScheduledRun struct {
	Run          runtimedb.SubscriptionRun
	Subscription runtimedb.AgentSubscription
	Checkpoint   runtimedb.SubscriptionCheckpoint
	// Author is the handle the run's work is authored by and owned by:
	// the canonical Person for a Personal source, otherwise the
	// household's admitted Owner.
	Author string
}

// ScheduledDispatcher starts a claimed run. It may finish synchronously
// (calling SubscriptionEngine.CompleteRun itself) or hand the run to an
// Agent and return; either way the run stays in flight until CompleteRun
// or its lease expires. The returned task ID is recorded on the run.
// An error fails the run with backoff.
type ScheduledDispatcher interface {
	DispatchScheduled(ctx context.Context, run ScheduledRun) (taskID string, err error)
}

// ScheduledDispatcherFunc adapts a function to ScheduledDispatcher.
type ScheduledDispatcherFunc func(ctx context.Context, run ScheduledRun) (string, error)

// DispatchScheduled calls f.
func (f ScheduledDispatcherFunc) DispatchScheduled(ctx context.Context, run ScheduledRun) (string, error) {
	return f(ctx, run)
}

// ScheduledTickOutcome summarizes one TickScheduled.
type ScheduledTickOutcome struct {
	Dispatched int
	// Blocked counts due subscriptions the dispatch gate refused.
	Blocked int
	// InFlight counts due subscriptions skipped because a run is still
	// in flight (no overlap).
	InFlight int
	// Held counts paused or failure-held subscriptions.
	Held int
	// Abandoned counts runs whose lease expired this tick.
	Abandoned int
	Errored   int
}

func (e *SubscriptionEngine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *SubscriptionEngine) authority() BackgroundAuthority {
	if e.Authority != nil {
		return e.Authority
	}
	return DenyAllBackgroundAuthority{}
}

func (e *SubscriptionEngine) dispatcher(matchKind string) ScheduledDispatcher {
	if d, ok := e.Dispatchers[matchKind]; ok && d != nil {
		return d
	}
	return ScheduledDispatcherFunc(e.dispatchTask)
}

// TickScheduled is one pass of the scheduled family. A returned error
// means the whole family is held (no execution authority, quarantine,
// emergency stop, or an unreadable database); per-subscription trouble
// is counted in the outcome instead.
func (e *SubscriptionEngine) TickScheduled(ctx context.Context) (ScheduledTickOutcome, error) {
	var out ScheduledTickOutcome
	if err := e.DB.CheckHouseholdExecution(); err != nil {
		return out, err
	}
	now := e.now()
	abandoned, err := e.DB.AbandonExpiredSubscriptionRuns(now)
	if err != nil {
		return out, fmt.Errorf("automation: sweeping expired subscription runs: %w", err)
	}
	out.Abandoned = abandoned
	subs, err := e.DB.ListScheduledAgentSubscriptions()
	if err != nil {
		return out, fmt.Errorf("automation: listing scheduled subscriptions: %w", err)
	}
	for _, sub := range subs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if err := e.tickScheduledOne(ctx, sub, now, &out); err != nil {
			out.Errored++
			e.log().Error("scheduled subscription tick failed", "subscription", sub.ID, "butler", sub.Butler, "err", err)
		}
	}
	return out, nil
}

func (e *SubscriptionEngine) tickScheduledOne(ctx context.Context, sub runtimedb.AgentSubscription, now time.Time, out *ScheduledTickOutcome) error {
	sched, err := e.DB.GetSubscriptionSchedule(sub.ID)
	if err != nil {
		return err
	}
	if sched.Hold != runtimedb.SubscriptionHoldNone {
		out.Held++
		return nil
	}
	if !sched.Due(now) {
		return nil
	}
	if _, running, err := e.DB.RunningSubscriptionRun(sub.ID); err != nil {
		return err
	} else if running {
		out.InFlight++
		return nil
	}
	use, author, reason, err := e.gate(ctx, sub)
	if err != nil {
		return err
	}
	if reason != "" {
		out.Blocked++
		return e.DB.RecordSubscriptionBlocked(sub.ID, reason, now)
	}
	run, cp, err := e.DB.ClaimSubscriptionRun(runtimedb.SubscriptionRunClaim{
		Subscription: sub.ID, Version: sub.Version, SourceConnection: sub.SourceConnection,
		ConnectionEpoch: use.ConnectionEpoch, PersonID: sub.PersonID, At: now, Lease: e.RunLease,
	})
	switch {
	case errors.Is(err, runtimedb.ErrSubscriptionRunInFlight):
		out.InFlight++
		return nil
	case errors.Is(err, runtimedb.ErrSubscriptionNotDue), errors.Is(err, runtimedb.ErrSubscriptionNotDispatchable):
		// The row moved between the gate and the claim; the next tick
		// looks again.
		return nil
	case err != nil:
		return err
	}
	taskID, err := e.dispatcher(sub.MatchKind).DispatchScheduled(ctx, ScheduledRun{
		Run: run, Subscription: sub, Checkpoint: cp, Author: author,
	})
	if err != nil {
		if ferr := e.DB.FailSubscriptionRun(run.ID, err.Error(), e.now()); ferr != nil &&
			!errors.Is(ferr, runtimedb.ErrSubscriptionRunFenced) {
			return fmt.Errorf("dispatch failed (%v) and could not be recorded: %w", err, ferr)
		}
		return fmt.Errorf("dispatching run %d: %w", run.ID, err)
	}
	if taskID != "" {
		if err := e.DB.SetSubscriptionRunTask(run.ID, taskID); err != nil {
			return err
		}
	}
	out.Dispatched++
	return nil
}

// gate is the per-subscription dispatch gate. reason is non-empty when
// the subscription must not run now; err is reserved for being unable to
// decide at all.
func (e *SubscriptionEngine) gate(ctx context.Context, sub runtimedb.AgentSubscription) (use BackgroundUse, author, reason string, err error) {
	use = BackgroundUse{
		Subscription: sub.ID, SubscriptionVersion: sub.Version, Butler: sub.Butler,
		Connection: sub.SourceConnection, PersonID: sub.PersonID,
	}
	if !sub.Dispatchable() {
		return use, "", fmt.Sprintf("version %d awaits Owner approval (approved: %d)", sub.Version, sub.ApprovedVersion), nil
	}
	if sub.PersonID != "" {
		handle, err := e.DB.PersonHandle(sub.PersonID)
		if err != nil {
			return use, "", "the Person this subscription runs for is not on record", nil
		}
		member, err := e.DB.GetButlerMember(handle)
		if err != nil || !member.Active() {
			return use, "", "the Person this subscription runs for is not admitted, is disabled, or was offboarded", nil
		}
		author = handle
	}
	if sub.SourceConnection != "" {
		conn, err := e.DB.GetConnection(sub.SourceConnection)
		if errors.Is(err, runtimedb.ErrNotFound) {
			return use, "", "the source connection no longer exists", nil
		}
		if err != nil {
			return use, "", "", err
		}
		use.ConnectionEpoch = conn.LeaseEpoch
		if conn.State == runtimedb.ConnectionUnavailable {
			return use, "", "the source connection is unavailable: " + conn.DegradedReason, nil
		}
		if conn.Ownership == runtimedb.OwnershipPersonal {
			owner, err := e.DB.GetButlerMember(conn.Owner)
			if err != nil || sub.PersonID == "" || owner.PersonID != sub.PersonID {
				return use, "", "the source connection does not belong to the Person this subscription runs for", nil
			}
		}
	}
	if err := e.authority().AuthorizeBackgroundUse(ctx, use); err != nil {
		return use, "", "background use is not authorized: " + err.Error(), nil
	}
	if author == "" {
		author, err = e.primaryOwner()
		if err != nil {
			return use, "", err.Error(), nil
		}
	}
	return use, author, "", nil
}

// CompleteRun is how a scheduled run reports. For a report that would
// commit a checkpoint it first re-runs the dispatch gate live — a Person
// who revoked, was offboarded, or had their connection fenced while the
// run was in flight gets no checkpoint committed on their behalf — and
// then commits through runtimedb's compare-and-swap. A zero At means the
// engine's clock.
func (e *SubscriptionEngine) CompleteRun(ctx context.Context, runID int64, rep runtimedb.SubscriptionRunReport) (runtimedb.SubscriptionRun, error) {
	if rep.At.IsZero() {
		rep.At = e.now()
	}
	run, err := e.DB.GetSubscriptionRun(runID)
	if err != nil {
		return runtimedb.SubscriptionRun{}, err
	}
	if run.State != runtimedb.SubscriptionRunRunning {
		return run, fmt.Errorf("%w: run %d is already %s", runtimedb.ErrSubscriptionRunFenced, runID, run.State)
	}
	if rep.Checkpoint != nil {
		sub, err := e.DB.GetAgentSubscription(run.Subscription)
		if err != nil {
			return run, err
		}
		_, _, reason, err := e.gate(ctx, sub)
		if err != nil {
			return run, err
		}
		if reason != "" {
			if ferr := e.DB.FenceSubscriptionRun(runID, reason, rep.At); ferr != nil {
				return run, ferr
			}
			return run, fmt.Errorf("%w: %s", runtimedb.ErrSubscriptionRunFenced, reason)
		}
	}
	return e.DB.CompleteSubscriptionRun(runID, rep)
}

// dispatchTask is the default dispatcher: the same bounded Task and
// `subscription` delivery an event match opens, authored by the run's
// Person, carrying the run and the checkpoint it resumes from so the
// Agent runtime can do the work and report through CompleteRun.
func (e *SubscriptionEngine) dispatchTask(_ context.Context, run ScheduledRun) (string, error) {
	cursor := "run:" + strconv.FormatInt(run.Run.ID, 10)
	objective := fmt.Sprintf("subscription %d: scheduled run %d", run.Subscription.ID, run.Run.ID)
	extra := map[string]any{
		"trigger": runtimedb.SubscriptionTriggerSchedule, "run": run.Run.ID,
		"source": run.Subscription.SourceConnection, "checkpointVersion": run.Checkpoint.Version,
	}
	if run.Checkpoint.Cursor != nil {
		extra["checkpoint"] = json.RawMessage(run.Checkpoint.Cursor)
	}
	return e.openTask(run.Subscription, run.Author, objective, cursor, extra)
}

// RunScheduled ticks the scheduled family every interval until ctx is
// done. Call it only in the process that holds the coordinator lease;
// TickScheduled refuses anyway if it does not.
func (e *SubscriptionEngine) RunScheduled(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			out, err := e.TickScheduled(ctx)
			if err != nil {
				e.log().Warn("scheduled subscriptions held", "err", err)
				continue
			}
			if out.Dispatched+out.Abandoned+out.Errored > 0 {
				e.log().Info("scheduled subscriptions tick", "dispatched", out.Dispatched, "blocked", out.Blocked,
					"inFlight", out.InFlight, "abandoned", out.Abandoned, "errored", out.Errored)
			}
		}
	}
}
