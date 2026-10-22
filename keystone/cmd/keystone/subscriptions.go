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
	"path/filepath"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/automation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/householdtz"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/mailmonitor"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/mailreview"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
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
// Person-authorized, Owner-approved intersection, live. mail is the
// mailbox monitor's processor (newMailMonitor); nil keeps mail.changes
// refusing.
func newScheduledSubscriptions(db *runtimedb.DB, ledger *butlerstore.Ledger, log *slog.Logger, mail *mailmonitor.Processor) *automation.SubscriptionEngine {
	e := &automation.SubscriptionEngine{
		DB: db, Ledger: ledger, Log: log,
		Authority: automation.PersonalBackgroundAuthority{DB: db},
	}
	e.Dispatchers = scheduledDispatchers(e, mail)
	return e
}

// newMailMonitor builds the mailbox monitor's processor (epic #326
// S05): excerpts sealed under <dataDir>/mail-excerpts.key -- a key kept
// out of the recovery set and every backup, so a restored database's
// excerpts are unreadable and discarded -- times resolved in the
// household zone, and extraction on the monitor Agent's own model
// route, filtered by the Person's processing location. The review hook
// is the no-op here; the caller replaces it with the private review
// (newMailReview, S06) once the calendar path exists.
func newMailMonitor(db *runtimedb.DB, dataDir string, zone *householdtz.Zone, log *slog.Logger) (*mailmonitor.Processor, error) {
	vault, err := db.OpenMailExcerptVault(filepath.Join(dataDir, "mail-excerpts.key"))
	if err != nil {
		return nil, fmt.Errorf("opening the mail excerpt key: %w", err)
	}
	return &mailmonitor.Processor{
		DB: db, Vault: vault, Zone: zone, Log: redact.Logger(log),
		Extractor: &mailmonitor.Extractor{Router: mailmonitor.ButlerModelRouter{DB: db, Build: butlerProviderFor}},
		Hook:      mailmonitor.NoopHook{},
	}, nil
}

// newMailReview builds the private review engine for the monitor's
// candidates (epic #326 S06). Its calendar path is the calendar
// capability's own exact, Person-scoped one — the Person's current
// calendar access, the deployment's calendar credential, never the
// mailbox credential. With no calendar configured, candidates are still
// offered (Edit and Dismiss work) but nothing can be added.
func newMailReview(db *runtimedb.DB, mail *mailmonitor.Processor, cal *domestic.Calendar, log *slog.Logger) *mailreview.Engine {
	e := &mailreview.Engine{DB: db, Vault: mail.Vault, Log: redact.Logger(log)}
	if cal != nil {
		e.Calendar = cal
	}
	return e
}

// scheduledDispatchers is THE registration point for scheduled match
// kinds. A kind with no entry falls back to the engine's default: a
// bounded Task and a `subscription` delivery for its Agent.
//
// mail.changes is served by the mailbox monitor, which reads changes
// through the adapter, records candidates, and only then commits the
// checkpoint through e.CompleteRun. Without a monitor (mail == nil) the
// entry refuses: the default would open an Agent Task every cadence that
// nothing completes, each ending as an abandoned run; refusing records
// an honest failure with backoff instead.
func scheduledDispatchers(e *automation.SubscriptionEngine, mail *mailmonitor.Processor) map[string]automation.ScheduledDispatcher {
	if mail != nil {
		return map[string]automation.ScheduledDispatcher{
			MailChangesMatchKind: mailmonitor.NewDispatcher(mailmonitor.Config{
				DB: e.DB, Processor: mail, Runs: e, Log: redact.Logger(e.Log),
			}),
		}
	}
	return map[string]automation.ScheduledDispatcher{
		MailChangesMatchKind: automation.ScheduledDispatcherFunc(
			func(context.Context, automation.ScheduledRun) (string, error) {
				return "", fmt.Errorf("no %s consumer is installed in this Keystone", MailChangesMatchKind)
			}),
	}
}

// startScheduledSubscriptions runs the family until ctx is done, and
// the mailbox monitor's expiry and retention sweep beside it when there
// is one.
func startScheduledSubscriptions(ctx context.Context, e *automation.SubscriptionEngine, mail *mailmonitor.Processor, log *slog.Logger) {
	log.Info("scheduled subscriptions running", "every", scheduledSubscriptionEvery)
	go e.RunScheduled(ctx, scheduledSubscriptionEvery)
	if mail != nil {
		go mailmonitor.RunSweeper(ctx, e.DB, 0, runtimedb.DefaultMailRetention(), redact.Logger(log))
	}
}
