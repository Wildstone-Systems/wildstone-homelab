package mailmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/automation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// MatchKind is the scheduled subscription match kind this dispatcher
// serves. Register it as
//
//	engine.Dispatchers[mailmonitor.MatchKind] = mailmonitor.NewDispatcher(cfg)
const MatchKind = "mail.changes"

// Bounds on one run.
const (
	// BatchLimit is the per-run message cap (the brief's 50, also the
	// adapter's own maximum).
	BatchLimit = 50
	// DefaultWorkDeadline is how long the adapter has to answer the
	// read. Well inside the run lease (10m), so a lost answer fails the
	// run instead of stranding it.
	DefaultWorkDeadline = 2 * time.Minute
	defaultPoll         = time.Second
	// collectGrace is how long past the work deadline the collector
	// waits for a late state change before cancelling the work itself.
	collectGrace = 10 * time.Second
)

// RunCompleter is how a run reports. *automation.SubscriptionEngine is
// one: it re-runs the live dispatch gate before committing a checkpoint.
type RunCompleter interface {
	CompleteRun(ctx context.Context, runID int64, rep runtimedb.SubscriptionRunReport) (runtimedb.SubscriptionRun, error)
}

// Config builds a Dispatcher.
type Config struct {
	DB        *runtimedb.DB
	Processor *Processor
	Runs      RunCompleter
	// WorkDeadline bounds the adapter's answer; zero is 2m.
	WorkDeadline time.Duration
	// Poll is how often the collector looks for the answer; zero is 1s.
	Poll time.Duration
	// Go runs the collector; nil starts a goroutine. Tests run it inline.
	Go  func(func())
	Now func() time.Time
	Log *slog.Logger
}

// Dispatcher is the mail.changes ScheduledDispatcher.
type Dispatcher struct{ cfg Config }

var _ automation.ScheduledDispatcher = (*Dispatcher)(nil)

// NewDispatcher returns the mail.changes dispatcher.
func NewDispatcher(cfg Config) *Dispatcher {
	if cfg.WorkDeadline <= 0 {
		cfg.WorkDeadline = DefaultWorkDeadline
	}
	if cfg.Poll <= 0 {
		cfg.Poll = defaultPoll
	}
	if cfg.Go == nil {
		cfg.Go = func(f func()) { go f() }
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Dispatcher{cfg: cfg}
}

func (d *Dispatcher) now() time.Time {
	if d.cfg.Now != nil {
		return d.cfg.Now().UTC()
	}
	return time.Now().UTC()
}

// changesRequest is the mail.changes request: every approved folder,
// from the committed cursor.
type changesRequest struct {
	Cursor json.RawMessage `json:"cursor,omitempty"`
	Limit  int             `json:"limit"`
}

// changesAnswer is the part of the adapter's answer this reads.
type changesAnswer struct {
	Outcome  string          `json:"outcome"`
	Messages []Message       `json:"messages"`
	Next     json.RawMessage `json:"next"`
	More     bool            `json:"more"`
	Folders  []struct {
		Folder  string `json:"folder"`
		Outcome string `json:"outcome"`
	} `json:"folders"`
}

// DispatchScheduled queues one bounded mail.changes read under the live
// grant and returns; the collector finishes the run when the adapter
// answers. No Task id is returned.
func (d *Dispatcher) DispatchScheduled(ctx context.Context, run automation.ScheduledRun) (string, error) {
	sub := run.Subscription
	if sub.SourceConnection == "" || sub.PersonID == "" {
		return "", errors.New("mailmonitor: a mail subscription needs a Personal source connection")
	}
	grant, err := d.cfg.DB.AuthorizeBackgroundUse(ctx, sub.SourceConnection, sub.ID, sub.ApprovedVersion)
	if err != nil {
		return "", err
	}
	if grant.LeaseEpoch != run.Run.ConnectionEpoch {
		return "", fmt.Errorf("mailmonitor: connection %q moved to epoch %d after the run was claimed", sub.SourceConnection, grant.LeaseEpoch)
	}
	req := changesRequest{Limit: BatchLimit}
	if len(run.Checkpoint.Cursor) > 0 && string(run.Checkpoint.Cursor) != "null" {
		req.Cursor = run.Checkpoint.Cursor
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	seq, err := d.cfg.DB.EnqueueIntegrationWork(sub.SourceConnection, runtimedb.IntegrationWork{
		Kind: runtimedb.WorkQuery, Capability: MatchKind,
		CorrelationID: "subscription-run:" + strconv.FormatInt(run.Run.ID, 10), RequestJSON: string(body),
	}, d.cfg.WorkDeadline)
	if err != nil {
		return "", err
	}
	src := SourceFromGrant(grant, sub.PersonID, run.Run.ID)
	d.cfg.Go(func() { d.collect(ctx, run.Run.ID, seq, src, sub) })
	// No Task is opened: the run is not Agent work and nothing about the
	// mail enters a task trace. The work item is correlated by run id.
	return "", nil
}

// collect waits for the read, processes it and completes the run. Every
// exit completes the run exactly once, or leaves it to lease expiry when
// ctx ended (Keystone stopping) — which re-reads from the committed
// cursor, never past it.
func (d *Dispatcher) collect(ctx context.Context, runID, seq int64, src Source, sub runtimedb.AgentSubscription) {
	complete := func(rep runtimedb.SubscriptionRunReport) {
		if _, err := d.cfg.Runs.CompleteRun(ctx, runID, rep); err != nil {
			d.cfg.Log.Warn("mail monitor: completing run failed", "run", runID, "outcome", rep.Outcome, "err", err)
		}
	}
	fail := func(outcome, reason string) {
		complete(runtimedb.SubscriptionRunReport{Outcome: outcome, Error: reason, At: d.now()})
	}
	w, err := d.await(ctx, seq)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fail(runtimedb.RunOutcomeError, "reading the mail answer: "+err.Error())
		return
	}
	defer func() {
		// The answer carries normalized message text; it does not stay
		// in the work queue once processed.
		if err := d.cfg.DB.ScrubIntegrationWorkResult(seq); err != nil {
			d.cfg.Log.Warn("mail monitor: scrubbing a mail answer failed", "work", seq, "err", err)
		}
	}()
	switch w.State {
	case runtimedb.WorkCancelled:
		fail(runtimedb.RunOutcomeUnavailable, "the mail read was cancelled: "+w.Error)
		return
	case runtimedb.WorkExpired:
		fail(runtimedb.RunOutcomeUnavailable, "the mail adapter did not answer in time")
		return
	}
	switch w.ResultType {
	case runtimedb.ResultOK, runtimedb.ResultEmpty:
	case runtimedb.ResultUnavailable:
		outcome, reason := classifyUnavailable(w.Error)
		fail(outcome, reason)
		return
	default:
		fail(runtimedb.RunOutcomeStale, "the mail adapter offered only a stale answer")
		return
	}
	var ans changesAnswer
	if err := json.Unmarshal([]byte(w.ResultJSON), &ans); err != nil || len(ans.Next) == 0 || string(ans.Next) == "null" {
		fail(runtimedb.RunOutcomeError, "the mail answer was not a mail.changes result")
		return
	}
	// The grant is re-read live before anything is recorded: a Person who
	// revoked while the adapter was reading gets nothing recorded for
	// them. RecordMailMessage re-checks the epoch in its own transaction.
	grant, err := d.cfg.DB.AuthorizeBackgroundUse(ctx, sub.SourceConnection, sub.ID, sub.ApprovedVersion)
	if err != nil || grant.LeaseEpoch != src.Epoch || grant.TargetCalendar != src.TargetCalendar ||
		grant.ProcessingLocation != src.ProcessingLocation {
		fail(runtimedb.RunOutcomeUnavailable, "background use is no longer authorized as it was when the run started")
		return
	}
	if len(ans.Messages) > BatchLimit {
		ans.Messages = ans.Messages[:BatchLimit]
		ans.More = true
	}
	for _, f := range ans.Folders {
		if f.Outcome == "cursor_reset" {
			// Already-processed mail comes back under new UIDs with the
			// same fingerprints and dedupes; the new cursor is committed.
			d.cfg.Log.Info("mail monitor: folder numbering reset", "connection", src.Connection, "run", runID)
		}
	}
	outcomes, err := d.cfg.Processor.ProcessBatch(ctx, src, ans.Messages)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrModelUnavailable) {
			fail(runtimedb.RunOutcomeError, "model extraction was unavailable; the batch will be retried")
			return
		}
		if errors.Is(err, runtimedb.ErrMailConnectionFenced) || errors.Is(err, runtimedb.ErrQuarantined) {
			fail(runtimedb.RunOutcomeUnavailable, "the mailbox connection was fenced during the run")
			return
		}
		fail(runtimedb.RunOutcomeError, "recording mail results failed")
		d.cfg.Log.Warn("mail monitor: recording failed", "run", runID, "err", err)
		return
	}
	outcome := runtimedb.RunOutcomeOK
	if w.ResultType == runtimedb.ResultEmpty || len(ans.Messages) == 0 && ans.Outcome != "cursor_reset" {
		outcome = runtimedb.RunOutcomeEmpty
	}
	candidates := 0
	for _, o := range outcomes {
		candidates += len(o.Candidates)
	}
	d.cfg.Log.Info("mail monitor run", "connection", src.Connection, "run", runID, "messages", len(outcomes),
		"candidates", candidates, "more", ans.More)
	complete(runtimedb.SubscriptionRunReport{Outcome: outcome, Checkpoint: ans.Next, At: d.now()})
}

// await polls the work item until it has finished, cancelling it itself
// once its deadline is well past (no adapter claimed it).
func (d *Dispatcher) await(ctx context.Context, seq int64) (runtimedb.IntegrationWork, error) {
	t := time.NewTicker(d.cfg.Poll)
	defer t.Stop()
	for {
		w, err := d.cfg.DB.GetIntegrationWork(seq)
		if err != nil {
			return runtimedb.IntegrationWork{}, err
		}
		switch w.State {
		case runtimedb.WorkDone, runtimedb.WorkExpired, runtimedb.WorkCancelled:
			return w, nil
		}
		if d.now().After(w.DeadlineAt.Add(collectGrace)) {
			if err := d.cfg.DB.CancelIntegrationWork(seq, "deadline passed"); err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
				return runtimedb.IntegrationWork{}, err
			}
			w.State = runtimedb.WorkExpired
			return w, nil
		}
		select {
		case <-ctx.Done():
			return runtimedb.IntegrationWork{}, ctx.Err()
		case <-t.C:
		}
	}
}

// classifyUnavailable maps the mail contract's unavailability text
// (`…unavailable: <outcome>[: <reason>]`) onto run outcomes, keeping
// only the fixed category.
func classifyUnavailable(text string) (string, string) {
	_, rest, _ := strings.Cut(text, "unavailable: ")
	code, _, _ := strings.Cut(rest, ":")
	code = strings.TrimSpace(code)
	switch code {
	case "auth_failed":
		return runtimedb.RunOutcomeAuthFailed, "the mail provider refused the credential"
	case "bad_request":
		return runtimedb.RunOutcomeConfigFailed, "the mail read was refused as out of scope or malformed"
	case "not_configured":
		return runtimedb.RunOutcomeUnavailable, "the mail adapter is not configured yet"
	case "stale":
		return runtimedb.RunOutcomeUnavailable, "the mail reference was stale"
	case "provider_unavailable":
		return runtimedb.RunOutcomeUnavailable, "the mail provider could not be reached"
	}
	return runtimedb.RunOutcomeUnavailable, "the mail adapter was unavailable"
}
