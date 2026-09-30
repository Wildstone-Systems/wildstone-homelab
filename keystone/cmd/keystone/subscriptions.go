package main

// The scheduled Agent-subscription family (ADR-W039 §3.6.1, epic #326
// S03) as this process runs it: coordinator-held, with S02's live
// Personal-authorization evaluator as its dispatch authority.
//
// Started only in the process that holds the coordinator lease, beside
// the Butler runner and on its ledger, for the reason the runner is:
// a claimed run is durable work, and a second process claiming under a
// lease it does not hold is the racing-schedulers failure the lease
// refuses. TickScheduled re-checks execution authority every pass
// anyway, so a lease lost later holds the family rather than racing.
//
// The EVENT family (automation.SubscriptionEngine.Tick) is not started
// here: it must tick after automation.Engine.Tick in the same loop
// ("deterministic first"), which a second goroutine cannot promise.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// scheduledSubscriptionEvery is how often the scheduled family looks
// for due subscriptions. Well under the 5-minute cadence floor, so a
// run starts within half a minute of falling due.
const scheduledSubscriptionEvery = 30 * time.Second

// MailChangesMatchKind is the scheduled match kind a mailbox monitor's
// subscription carries (epic #326).
const MailChangesMatchKind = "mail.changes"

// newScheduledSubscriptions builds the scheduled engine this process
// runs. Its Authority is the real one: nothing dispatches without the
// Person-authorized, Owner-approved intersection, live.
func newScheduledSubscriptions(db *runtimedb.DB, ledger *butlerstore.Ledger, log *slog.Logger) *automation.SubscriptionEngine {
	e := &automation.SubscriptionEngine{
		DB: db, Ledger: ledger, Log: log,
		Authority: automation.PersonalBackgroundAuthority{DB: db},
	}
	e.Dispatchers = scheduledDispatchers(e)
	return e
}

// scheduledDispatchers is THE registration point for scheduled match
// kinds. A kind with no entry falls back to the engine's default: a
// bounded Task and a `subscription` delivery for its Agent.
//
// mail.changes has an entry that refuses until the mailbox consumer
// (epic #326 S05-S07: read changes through the adapter, extract
// candidates, commit the checkpoint via e.CompleteRun) registers here.
// The default would open an Agent Task every cadence that nothing
// completes, each ending as an abandoned run; refusing records an
// honest failure with backoff instead.
func scheduledDispatchers(e *automation.SubscriptionEngine) map[string]automation.ScheduledDispatcher {
	_ = e // the mail consumer will need e.CompleteRun
	return map[string]automation.ScheduledDispatcher{
		MailChangesMatchKind: automation.ScheduledDispatcherFunc(
			func(context.Context, automation.ScheduledRun) (string, error) {
				return "", fmt.Errorf("no %s consumer is installed in this Keystone yet", MailChangesMatchKind)
			}),
	}
}

// startScheduledSubscriptions runs the family until ctx is done.
func startScheduledSubscriptions(ctx context.Context, e *automation.SubscriptionEngine, log *slog.Logger) {
	log.Info("scheduled subscriptions running", "every", scheduledSubscriptionEvery)
	go e.RunScheduled(ctx, scheduledSubscriptionEvery)
}
