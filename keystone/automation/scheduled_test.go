package automation

// Scheduled subscriptions (epic #326 S03, #456), proved against a
// deterministic clock: cadence, no overlap, bounded catch-up, retry,
// pause, disable and restart; crash boundaries that neither skip
// committed work nor replay it in a storm; and every dispatch gate.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *fakeClock) Set(t time.Time)         { c.t = t }

// recorder is a dispatcher that remembers what it was handed and does
// nothing else — a run it receives stays in flight until the test
// completes it, exactly as a real runner's would.
type recorder struct{ runs []ScheduledRun }

func (r *recorder) DispatchScheduled(_ context.Context, run ScheduledRun) (string, error) {
	r.runs = append(r.runs, run)
	return "", nil
}

func (r *recorder) last(t *testing.T) ScheduledRun {
	t.Helper()
	if len(r.runs) == 0 {
		t.Fatal("nothing was dispatched")
	}
	return r.runs[len(r.runs)-1]
}

// allow is a BackgroundAuthority the test can switch.
type allow struct{ denied atomic.Bool }

func (a *allow) AuthorizeBackgroundUse(context.Context, BackgroundUse) error {
	if a.denied.Load() {
		return errors.New("sam revoked background use")
	}
	return nil
}

type schedFixture struct {
	db    *runtimedb.DB
	eng   *SubscriptionEngine
	clock *fakeClock
	rec   *recorder
	auth  *allow
	sub   runtimedb.AgentSubscription
	path  string
}

// newSchedFixture: Owner mika, member sam, Agent alfred, sam's Personal
// mail connection, and an approved 15-minute scheduled subscription on
// it. The authority allows until a test says otherwise.
func newSchedFixture(t *testing.T) *schedFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := runtimedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b, ledger := subscriptionButler(t, db)
	if err := db.RecordButlerMember("sam", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateConnection(runtimedb.IntegrationConnection{
		Name: "mail-sam", Adapter: "mail", Ownership: runtimedb.OwnershipPersonal, Owner: "sam", CreatedBy: "sam",
	}); err != nil {
		t.Fatal(err)
	}
	id, err := db.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{
		Butler: b.Seq, TriggerKind: runtimedb.SubscriptionTriggerSchedule, MatchKind: "mail.changes",
		Capabilities: []string{"peek"}, SourceConnection: "mail-sam", Cadence: 15 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveAgentSubscription(id, "mika"); err != nil {
		t.Fatal(err)
	}
	sub, err := db.GetAgentSubscription(id)
	if err != nil {
		t.Fatal(err)
	}
	f := &schedFixture{db: db, clock: &fakeClock{t: t0}, rec: &recorder{}, auth: &allow{}, sub: sub, path: path}
	f.eng = f.engine(ledger)
	return f
}

func (f *schedFixture) engine(ledger *butlerstore.Ledger) *SubscriptionEngine {
	return &SubscriptionEngine{
		DB: f.db, Ledger: ledger, Now: f.clock.Now, Authority: f.auth,
		Dispatchers: map[string]ScheduledDispatcher{"mail.changes": f.rec},
	}
}

func (f *schedFixture) tick(t *testing.T) ScheduledTickOutcome {
	t.Helper()
	out, err := f.eng.TickScheduled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *schedFixture) complete(t *testing.T, outcome string, cursor string) runtimedb.SubscriptionRun {
	t.Helper()
	var cp json.RawMessage
	if cursor != "" {
		cp = json.RawMessage(cursor)
	}
	run, err := f.eng.CompleteRun(context.Background(), f.rec.last(t).Run.ID,
		runtimedb.SubscriptionRunReport{Outcome: outcome, Checkpoint: cp})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *schedFixture) schedule(t *testing.T) runtimedb.SubscriptionSchedule {
	t.Helper()
	s, err := f.db.GetSubscriptionSchedule(f.sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScheduledCadence(t *testing.T) {
	f := newSchedFixture(t)
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("a freshly approved subscription dispatched %d, want 1", out.Dispatched)
	}
	f.complete(t, runtimedb.RunOutcomeEmpty, "")
	f.clock.Advance(14*time.Minute + 59*time.Second)
	if out := f.tick(t); out.Dispatched != 0 {
		t.Fatalf("dispatched %d before the cadence elapsed", out.Dispatched)
	}
	f.clock.Advance(time.Second)
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("dispatched %d at the cadence, want 1", out.Dispatched)
	}
	if got := f.schedule(t).NextDueAt; !got.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("next due = %s, want %s", got, t0.Add(30*time.Minute))
	}
}

func TestScheduledRunsNeverOverlap(t *testing.T) {
	f := newSchedFixture(t)
	f.eng.RunLease = 3 * time.Hour
	f.tick(t)
	for i := 0; i < 8; i++ { // two hours of cadences, the run still in flight
		f.clock.Advance(15 * time.Minute)
		out := f.tick(t)
		if out.Dispatched != 0 || out.InFlight != 1 {
			t.Fatalf("tick %d with a run in flight: %+v", i, out)
		}
	}
	if len(f.rec.runs) != 1 {
		t.Fatalf("%d runs dispatched while one was in flight", len(f.rec.runs))
	}
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":3}`)
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("after the run finished: %+v, want one catch-up dispatch", out)
	}
}

func TestScheduledCatchUpAfterDowntimeIsOneRun(t *testing.T) {
	f := newSchedFixture(t)
	f.tick(t)
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":1}`)
	f.clock.Advance(10 * time.Hour) // forty missed cadences
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("after downtime dispatched %d, want exactly 1", out.Dispatched)
	}
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":2}`)
	for i := 0; i < 5; i++ {
		f.clock.Advance(time.Minute)
		if out := f.tick(t); out.Dispatched != 0 {
			t.Fatalf("a backlog run followed the catch-up: %+v", out)
		}
	}
	f.clock.Set(t0.Add(10*time.Hour + 15*time.Minute))
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("the cadence did not resume from the catch-up: %+v", out)
	}
}

func TestScheduledRetryBacksOffAndIsBounded(t *testing.T) {
	f := newSchedFixture(t)
	f.tick(t)
	var delays []time.Duration
	for i := 0; i < 7; i++ {
		f.complete(t, runtimedb.RunOutcomeUnavailable, "")
		failedAt := f.clock.Now()
		next := f.schedule(t).NextDueAt
		delays = append(delays, next.Sub(failedAt))
		f.clock.Set(next.Add(-time.Second))
		if out := f.tick(t); out.Dispatched != 0 {
			t.Fatalf("retry %d dispatched before its backoff elapsed", i)
		}
		f.clock.Set(next)
		if out := f.tick(t); out.Dispatched != 1 {
			t.Fatalf("retry %d did not dispatch when due: %+v", i, out)
		}
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", delays, want)
		}
	}
	// Stale is a failure too: nothing committed, backoff continues.
	f.complete(t, runtimedb.RunOutcomeStale, "")
	if cp, _ := f.db.GetSubscriptionCheckpoint(f.sub.ID, "mail-sam"); cp.Version != 0 {
		t.Fatal("a failed run committed a checkpoint")
	}
	// Success resets the count.
	f.clock.Set(f.schedule(t).NextDueAt)
	f.tick(t)
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":5}`)
	if s := f.schedule(t); s.ConsecutiveFailures != 0 || s.LastOutcome != runtimedb.RunOutcomeOK || s.LastSuccessAt.IsZero() {
		t.Fatalf("after success: %+v", s)
	}
}

func TestScheduledAuthAndConfigFailuresHoldUntilResumed(t *testing.T) {
	for _, outcome := range []string{runtimedb.RunOutcomeAuthFailed, runtimedb.RunOutcomeConfigFailed} {
		t.Run(outcome, func(t *testing.T) {
			f := newSchedFixture(t)
			f.tick(t)
			f.complete(t, outcome, "")
			f.clock.Advance(6 * time.Hour)
			if out := f.tick(t); out.Dispatched != 0 || out.Held != 1 {
				t.Fatalf("a %s subscription kept polling: %+v", outcome, out)
			}
			if err := f.db.ResumeAgentSubscription(f.sub.ID, "sam", f.clock.Now()); err != nil {
				t.Fatal(err)
			}
			if out := f.tick(t); out.Dispatched != 1 {
				t.Fatalf("resume did not yield a run: %+v", out)
			}
		})
	}
}

func TestScheduledPauseAndResume(t *testing.T) {
	f := newSchedFixture(t)
	f.tick(t)
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":1}`)
	if err := f.db.PauseAgentSubscription(f.sub.ID, "sam", "on holiday", f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		f.clock.Advance(15 * time.Minute)
		if out := f.tick(t); out.Dispatched != 0 {
			t.Fatalf("a paused subscription dispatched: %+v", out)
		}
	}
	if s := f.schedule(t); s.Hold != runtimedb.SubscriptionHoldPaused || s.HeldBy != "sam" {
		t.Fatalf("pause not recorded: %+v", s)
	}
	if err := f.db.ResumeAgentSubscription(f.sub.ID, "sam", f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("resume gave %+v, want exactly one catch-up run", out)
	}
	if out := f.tick(t); out.Dispatched != 0 {
		t.Fatalf("resume gave a second run: %+v", out)
	}
}

func TestScheduledDisableFencesTheRunInFlight(t *testing.T) {
	f := newSchedFixture(t)
	f.tick(t)
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":1}`)
	f.clock.Advance(15 * time.Minute)
	f.tick(t)
	if err := f.db.SetAgentSubscriptionEnabled(f.sub.ID, false, "sam"); err != nil {
		t.Fatal(err)
	}
	_, err := f.eng.CompleteRun(context.Background(), f.rec.last(t).Run.ID,
		runtimedb.SubscriptionRunReport{Outcome: runtimedb.RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":2}`)})
	if !errors.Is(err, runtimedb.ErrSubscriptionRunFenced) {
		t.Fatalf("completing after disable = %v, want fenced", err)
	}
	if cp, _ := f.db.GetSubscriptionCheckpoint(f.sub.ID, "mail-sam"); string(cp.Cursor) != `{"uid":1}` {
		t.Fatalf("a disabled subscription's run committed %s", cp.Cursor)
	}
	f.clock.Advance(24 * time.Hour)
	if out := f.tick(t); out.Dispatched != 0 {
		t.Fatalf("a disabled subscription dispatched: %+v", out)
	}
}

// TestScheduledStateSurvivesRestart: a restarted Keystone (fresh engine,
// reopened database, same coordinator) neither re-runs early nor
// overlaps the run the previous process left in flight.
func TestScheduledStateSurvivesRestart(t *testing.T) {
	f := newSchedFixture(t)
	f.eng.RunLease = time.Hour
	f.tick(t)
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":1}`)
	f.clock.Advance(15 * time.Minute)
	f.tick(t) // in flight at "shutdown"
	inflight := f.rec.last(t).Run.ID
	f.db.Close()

	db, err := runtimedb.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	lease, err := db.AcquireCoordinator("subscriptions-test") // same holder: a restart
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	f.rec = &recorder{}
	f.eng = f.engine(butlerstore.New(db, lease))

	f.clock.Advance(5 * time.Minute)
	if out := f.tick(t); out.Dispatched != 0 {
		t.Fatalf("restart ran early: %+v", out)
	}
	f.clock.Set(t0.Add(30 * time.Minute)) // due again, the old run still in flight
	if out := f.tick(t); out.Dispatched != 0 || out.InFlight != 1 {
		t.Fatalf("after restart with a run in flight: %+v", out)
	}
	// The pre-restart run can still report under the same coordinator.
	if _, err := f.eng.CompleteRun(context.Background(), inflight,
		runtimedb.SubscriptionRunReport{Outcome: runtimedb.RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":2}`)}); err != nil {
		t.Fatal(err)
	}
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("restart lost the cadence: %+v", out)
	}
	if cp := f.rec.last(t).Checkpoint; cp.Version != 2 || string(cp.Cursor) != `{"uid":2}` {
		t.Fatalf("after restart the run resumed from %+v, want v2 {uid:2}", cp)
	}
}

// TestScheduledCrashedRunSkipsNothingAndCommitsNothing: a run that dies
// without reporting is abandoned when its lease expires; the next run
// resumes from the last COMMITTED checkpoint, and the dead run's late
// report can neither regress nor double-commit it.
func TestScheduledCrashedRunSkipsNothingAndCommitsNothing(t *testing.T) {
	f := newSchedFixture(t)
	f.tick(t)
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":10}`)
	f.clock.Advance(15 * time.Minute)
	f.tick(t)
	crashed := f.rec.last(t).Run.ID // processes uid 11..20, crashes before commit

	f.clock.Advance(runtimedb.DefaultSubscriptionRunLease)
	out := f.tick(t)
	if out.Abandoned != 1 || out.Dispatched != 0 {
		t.Fatalf("lease expiry: %+v, want one abandoned run and backoff before retry", out)
	}
	if s := f.schedule(t); s.LastOutcome != runtimedb.RunOutcomeAbandoned || s.ConsecutiveFailures != 1 {
		t.Fatalf("abandonment not recorded as a failure: %+v", s)
	}
	f.clock.Set(f.schedule(t).NextDueAt)
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("no retry after backoff: %+v", out)
	}
	retry := f.rec.last(t)
	if retry.Checkpoint.Version != 1 || string(retry.Checkpoint.Cursor) != `{"uid":10}` {
		t.Fatalf("retry resumed from %+v; uid 11..20 would be skipped", retry.Checkpoint)
	}
	// The dead run's late report is refused and commits nothing.
	if _, err := f.eng.CompleteRun(context.Background(), crashed, runtimedb.SubscriptionRunReport{
		Outcome: runtimedb.RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":20}`)}); !errors.Is(err, runtimedb.ErrSubscriptionRunFenced) {
		t.Fatalf("late report from an abandoned run = %v, want fenced", err)
	}
	if cp, _ := f.db.GetSubscriptionCheckpoint(f.sub.ID, "mail-sam"); cp.Version != 1 {
		t.Fatalf("the abandoned run committed: %+v", cp)
	}
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":20}`)
	if cp, _ := f.db.GetSubscriptionCheckpoint(f.sub.ID, "mail-sam"); cp.Version != 2 || string(cp.Cursor) != `{"uid":20}` {
		t.Fatalf("the retry's commit = %+v", cp)
	}
}

// TestScheduledRepeatedCrashesDoNotStorm: a runner that crashes on every
// run, ticked every 30 seconds for six hours, gets a bounded number of
// attempts — never more than one per lease plus backoff, never two at
// once.
func TestScheduledRepeatedCrashesDoNotStorm(t *testing.T) {
	f := newSchedFixture(t)
	end := f.clock.Now().Add(6 * time.Hour)
	for f.clock.Now().Before(end) {
		out := f.tick(t)
		if out.Dispatched > 1 {
			t.Fatalf("one tick dispatched %d runs", out.Dispatched)
		}
		f.clock.Advance(30 * time.Second)
	}
	// Each attempt holds the subscription for a full lease (10m) and
	// then backs off at least 1m: at most 6h/11m attempts.
	if n := len(f.rec.runs); n > int((6*time.Hour)/(11*time.Minute))+1 {
		t.Fatalf("%d attempts in six hours of crashes — a replay storm", n)
	}
	for i := 1; i < len(f.rec.runs); i++ {
		prev, cur := f.rec.runs[i-1].Run, f.rec.runs[i].Run
		if cur.StartedAt.Before(prev.LeaseExpiresAt) {
			t.Fatalf("run %d started before run %d's lease expired", cur.ID, prev.ID)
		}
		if cur.BaseCheckpointVersion != 0 {
			t.Fatal("a crashed run advanced the checkpoint")
		}
	}
}

// --- The dispatch gate ------------------------------------------------

func TestScheduledDefaultAuthorityDeniesEverything(t *testing.T) {
	f := newSchedFixture(t)
	f.eng.Authority = nil
	f.clock.Advance(3 * time.Hour)
	for i := 0; i < 3; i++ {
		out := f.tick(t)
		if out.Dispatched != 0 || out.Blocked != 1 {
			t.Fatalf("with no authority wired: %+v", out)
		}
	}
	s := f.schedule(t)
	if s.BlockedReason == "" || s.BlockedAt.IsZero() || s.ConsecutiveFailures != 0 {
		t.Fatalf("a block must be recorded as health, not failure: %+v", s)
	}
	f.eng.Authority = f.auth
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("lifting the block gave %+v, want exactly one run", out)
	}
	if s := f.schedule(t); s.BlockedReason != "" {
		t.Fatalf("a successful claim left the block recorded: %+v", s)
	}
}

func TestScheduledRevocationBlocksDispatchAndCommit(t *testing.T) {
	f := newSchedFixture(t)
	f.tick(t)
	f.auth.denied.Store(true) // sam revokes while the run is in flight
	_, err := f.eng.CompleteRun(context.Background(), f.rec.last(t).Run.ID,
		runtimedb.SubscriptionRunReport{Outcome: runtimedb.RunOutcomeOK, Checkpoint: json.RawMessage(`{"uid":1}`)})
	if !errors.Is(err, runtimedb.ErrSubscriptionRunFenced) {
		t.Fatalf("commit after revocation = %v, want fenced", err)
	}
	if cp, _ := f.db.GetSubscriptionCheckpoint(f.sub.ID, "mail-sam"); cp.Version != 0 {
		t.Fatal("a revoked Person's run committed a checkpoint")
	}
	f.clock.Advance(15 * time.Minute)
	if out := f.tick(t); out.Dispatched != 0 || out.Blocked != 1 {
		t.Fatalf("dispatch after revocation: %+v", out)
	}
}

func TestScheduledOffboardingBlocksDispatchAndFencesTheRun(t *testing.T) {
	f := newSchedFixture(t)
	f.tick(t)
	if _, err := f.db.OffboardMember("sam", "mika", "mika"); err != nil {
		t.Fatal(err)
	}
	// Offboarding bumped the connection's lease epoch: the run in flight
	// is fenced even for a report that commits nothing.
	_, err := f.db.CompleteSubscriptionRun(f.rec.last(t).Run.ID,
		runtimedb.SubscriptionRunReport{Outcome: runtimedb.RunOutcomeEmpty, At: f.clock.Now()})
	if !errors.Is(err, runtimedb.ErrSubscriptionRunFenced) {
		t.Fatalf("completing after offboarding = %v, want fenced", err)
	}
	f.clock.Advance(15 * time.Minute)
	if out := f.tick(t); out.Dispatched != 0 || out.Blocked != 1 {
		t.Fatalf("dispatch after offboarding: %+v", out)
	}
}

func TestScheduledEditBlocksUntilThatVersionIsApproved(t *testing.T) {
	f := newSchedFixture(t)
	f.tick(t)
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":1}`)
	match := `{"folders":["INBOX","School"]}`
	v, err := f.db.ReviseAgentSubscription(f.sub.ID, runtimedb.AgentSubscriptionRevision{MatchJSON: &match}, "sam")
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(15 * time.Minute)
	if out := f.tick(t); out.Dispatched != 0 || out.Blocked != 1 {
		t.Fatalf("an unapproved edit dispatched: %+v", out)
	}
	if err := f.db.ApproveAgentSubscriptionVersion(f.sub.ID, v, "mika"); err != nil {
		t.Fatal(err)
	}
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("approving the edit gave %+v", out)
	}
	if got := f.rec.last(t).Run.SubscriptionVersion; got != v {
		t.Fatalf("run dispatched under version %d, want %d", got, v)
	}
}

func TestScheduledEmergencyStopAndQuarantineHoldTheFamily(t *testing.T) {
	f := newSchedFixture(t)
	if err := f.db.StopHouseholdExecution("mika", "incident drill"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.eng.TickScheduled(context.Background()); !errors.Is(err, runtimedb.ErrHouseholdStopped) {
		t.Fatalf("tick under emergency stop = %v", err)
	}
	if err := f.db.ResumeHouseholdExecution("mika"); err != nil {
		t.Fatal(err)
	}
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("after resume: %+v", out)
	}
	f.complete(t, runtimedb.RunOutcomeOK, `{"uid":1}`)
	if err := f.db.EnterRestoreQuarantine("test", t0); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	if _, err := f.eng.TickScheduled(context.Background()); !errors.Is(err, runtimedb.ErrQuarantined) {
		t.Fatalf("tick under restore quarantine = %v", err)
	}
	if len(f.rec.runs) != 1 {
		t.Fatalf("%d runs dispatched, want the one before the stop", len(f.rec.runs))
	}
}

func TestScheduledWithoutCoordinatorLeaseDispatchesNothing(t *testing.T) {
	f := newSchedFixture(t)
	f.db.Close()
	db, err := runtimedb.Open(f.path) // a second process that never acquired the lease
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &SubscriptionEngine{DB: db, Now: f.clock.Now, Authority: f.auth,
		Dispatchers: map[string]ScheduledDispatcher{"mail.changes": f.rec}}
	if _, err := e.TickScheduled(context.Background()); !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("tick without the coordinator lease = %v", err)
	}
}

// TestScheduledRunIsAuthoredByThePersonNotAnOwner: the default dispatcher
// opens the bounded Task as the mailbox's Person, never as the
// household's first Owner.
func TestScheduledRunIsAuthoredByThePersonNotAnOwner(t *testing.T) {
	f := newSchedFixture(t)
	f.eng.Dispatchers = nil // default: bounded Task + delivery
	if out := f.tick(t); out.Dispatched != 1 {
		t.Fatalf("%+v", out)
	}
	runs, err := f.db.ListSubscriptionRuns(f.sub.ID, 1)
	if err != nil || len(runs) != 1 || runs[0].TaskID == "" {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	task, err := f.db.GetButlerTask(runs[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Author != "sam" || task.Owner != "sam" {
		t.Fatalf("task author/owner = %s/%s, want sam/sam", task.Author, task.Owner)
	}
	if origin, ok := SubscriptionOrigin(task.GrantsJSON); !ok || origin != f.sub.ID {
		t.Fatalf("task origin = %d,%v", origin, ok)
	}
	deliveries, err := f.db.ListAgentDeliveries(f.sub.Butler, 10)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v, %v", deliveries, err)
	}
	var payload struct {
		Trigger string `json:"trigger"`
		Run     int64  `json:"run"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal([]byte(deliveries[0].Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Trigger != "schedule" || payload.Run != runs[0].ID || payload.Source != "mail-sam" {
		t.Fatalf("delivery payload = %s", deliveries[0].Payload)
	}
}

// TestEventEvaluatorIgnoresScheduledSubscriptions: a scheduled row is
// never matched against household events, whatever its match document.
func TestEventEvaluatorIgnoresScheduledSubscriptions(t *testing.T) {
	sub := runtimedb.AgentSubscription{
		Enabled: true, ApprovedVersion: 1, Version: 1, MatchKind: "event_type",
		TriggerKind: runtimedb.SubscriptionTriggerSchedule,
		MatchJSON:   `{"eventType":"person.arrived_home"}`,
	}
	if MatchesSubscription(sub, arrived(2, "mika")) {
		t.Fatal("a scheduled subscription matched an event")
	}
	sub.TriggerKind, sub.Version = runtimedb.SubscriptionTriggerEvent, 2
	if MatchesSubscription(sub, arrived(2, "mika")) {
		t.Fatal("an edited, unapproved event subscription matched")
	}
}
