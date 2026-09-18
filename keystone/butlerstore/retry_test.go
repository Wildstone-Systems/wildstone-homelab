package butlerstore

// P4's evidence (ADR-W030): provider failure and restart preserve
// progress, late results are rejected, expired or disabled authority
// blocks continuation, notices deduplicate and wait through quiet hours,
// and exhausted total budgets require review instead of resetting.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// hangingProvider blocks until its context is done, which is what a
// stalled provider looks like from here.
type hangingProvider struct{ calls int }

func (p *hangingProvider) Name() string { return "hanging" }

func (p *hangingProvider) Chat(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	p.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

// A stalled provider is retried inside its budget, each abandoned
// attempt is fenced, and only then does the task wait.
func TestProviderStallFencesRetriesAndWaits(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := newCountingToolset()

	r := runner(ts, ledger)
	r.StallTimeout = 20 * time.Millisecond
	r.RetryLimit = 2

	p := &hangingProvider{}
	if _, err := r.Run(context.Background(), p, id, "diagnose"); err == nil {
		t.Fatal("a permanently stalled provider produced an answer")
	}
	if p.calls != 3 {
		t.Fatalf("provider calls = %d, want the first try plus two retries", p.calls)
	}
	task, err := db.GetButlerTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("state = %q, want waiting", task.State)
	}
	waits, _ := db.ListButlerTaskWaits(id)
	if len(waits) != 1 || waits[0].Reason != runtimedb.ButlerWaitModelUnavailable {
		t.Fatalf("waits = %+v", waits)
	}
	// Every abandoned attempt was fenced, and the lease moved with them.
	if task.LeaseEpoch < 2 {
		t.Fatalf("lease epoch = %d, want it advanced by the fencing", task.LeaseEpoch)
	}
	attempts, _ := db.ListButlerTaskAttempts(id)
	for _, a := range attempts {
		if a.State == runtimedb.ButlerAttemptDispatched {
			t.Fatalf("attempt %d is still dispatched after the stall: %+v", a.Seq, a)
		}
	}
}

// A result that arrives after its attempt was fenced is refused, not
// applied over what the retry established.
func TestLateResultFromAFencedAttemptIsRejected(t *testing.T) {
	db, _ := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)

	step, err := db.AppendButlerTaskStep(id, runtimedb.ButlerTaskStep{
		Kind: runtimedb.ButlerStepTool, Name: "get_health",
	})
	if err != nil {
		t.Fatal(err)
	}
	stalled, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FenceButlerTaskAttempts(id); err != nil {
		t.Fatal(err)
	}
	err = db.FinishButlerAttempt(stalled.Seq, runtimedb.ButlerAttemptSucceeded, `{"late":true}`, "")
	if !errors.Is(err, runtimedb.ErrAttemptFenced) {
		t.Fatalf("a late result was accepted: %v", err)
	}
}

// Authority that has gone away blocks continuation, and each way it can
// go away is checked before anything is dispatched.
func TestWithdrawnAuthorityBlocksContinuation(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	ts := newCountingToolset()

	answer := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{{Text: "done"}}}

	t.Run("cancelled", func(t *testing.T) {
		id := newTask(t, db)
		if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskCancelled, "user cancelled", "mika"); err != nil {
			t.Fatal(err)
		}
		if _, err := runner(ts, ledger).Run(context.Background(), answer, id, "go"); !errors.Is(err, ErrAuthorityWithdrawn) {
			t.Fatalf("a cancelled task continued: %v", err)
		}
	})

	t.Run("offboarded owner", func(t *testing.T) {
		id := newTask(t, db)
		if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskPausedOffboarded, "offboarded", "admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := runner(ts, ledger).Run(context.Background(), answer, id, "go"); !errors.Is(err, ErrAuthorityWithdrawn) {
			t.Fatalf("offboarded work continued: %v", err)
		}
	})

	t.Run("expired deadline", func(t *testing.T) {
		id, err := db.InsertButlerTask(runtimedb.ButlerTask{
			Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
			Surface: runtimedb.ButlerSurfaceAdmin, Profile: "operator",
			Audience:  runtimedb.ButlerAudiencePrivate,
			Objective: "slow question", CompletionCriteria: "answered",
			// Waiting does not extend the clock: the absolute deadline
			// expires anyway (A4).
			DeadlineAt: time.Now().Add(-time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner(ts, ledger).Run(context.Background(), answer, id, "go"); !errors.Is(err, ErrAuthorityWithdrawn) {
			t.Fatalf("an expired task continued: %v", err)
		}
	})

	t.Run("disabled owner", func(t *testing.T) {
		id := newTask(t, db)
		gated := *ledger
		gated.OwnerActive = func(owner string) error {
			return fmt.Errorf("%s is no longer admitted", owner)
		}
		r := runner(ts, &gated)
		if _, err := r.Run(context.Background(), answer, id, "go"); !errors.Is(err, ErrAuthorityWithdrawn) {
			t.Fatalf("a disabled owner's task continued: %v", err)
		}
		if len(ts.calls) != 0 {
			t.Fatalf("a refused task dispatched %d tools", len(ts.calls))
		}
	})
}

// A verified reading that has aged past the observation TTL is taken
// again rather than reused, and one that is still fresh is not.
func TestStaleObservationsAreRefreshed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ttl       time.Duration
		wantCalls int
	}{
		{"fresh reading is reused", time.Hour, 1},
		{"stale reading is taken again", time.Nanosecond, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keystone.db")
			db, ledger := testLedger(t, path)
			id := newTask(t, db)
			ts := newCountingToolset()

			first := &scriptedProvider{dieAt: 1, steps: []*provider.ChatResponse{readsFor("n1")}}
			if _, err := runner(ts, ledger).Run(context.Background(), first, id, "check n1"); err == nil {
				t.Fatal("the first slice was expected to end on a dead provider")
			}
			db.Close()

			db2, ledger2 := testLedger(t, path)
			defer db2.Close()
			r := runner(ts, ledger2)
			r.ObservationTTL = tc.ttl
			// A clock a moment in the future, so the "stale" case does
			// not depend on how fast the test machine is.
			r.Now = func() time.Time { return time.Now().Add(time.Millisecond) }
			second := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
				readsFor("n1"), {Text: "n1 is fine"},
			}}
			if _, err := r.Run(context.Background(), second, id, "check n1"); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if got := ts.calls["get_health/n1"]; got != tc.wantCalls {
				t.Fatalf("n1 was read %d times, want %d", got, tc.wantCalls)
			}
		})
	}
}

// The stall notice goes once, to the owner's own direct surface, and
// waits out the household's quiet hours instead of arriving at 3am.
func TestStallNoticeDeduplicatesAndWaitsForQuietHours(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)

	if err := db.SetButlerSettings(runtimedb.ButlerSettings{
		HouseholdRounds: 16, OperatorRounds: 24, MaxToolCalls: 64,
		StallSeconds: 120, RetryLimit: 2, ObservationTTL: 300,
		// 22:00 to 07:00, the shape a household actually configures.
		QuietStartMin: 22 * 60, QuietEndMin: 7 * 60, Timezone: "UTC",
	}); err != nil {
		t.Fatalf("SetButlerSettings: %v", err)
	}
	// 03:00 UTC, anchored to TOMORROW rather than to a fixed date.
	//
	// The notifier's clock is injected, but the outbound queue compares
	// hold_until against the real one — it is a database read, not a
	// caller's opinion about the time. A literal date here made the hold
	// end at 07:00 on that date, which is in the past for most of any
	// real day, so the test passed before 07:00 UTC and failed after it.
	// Anchoring to tomorrow keeps the computed hold ahead of the real
	// clock whenever this runs.
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	night := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 3, 0, 0, 0, time.UTC)
	ledger.Now = func() time.Time { return night }

	at := chatConnection(t, db, "alfred")
	n := &Notifier{Ledger: ledger, Binding: func(string) (runtimedb.ChatOutbound, bool, error) {
		return runtimedb.ChatOutbound{Connection: at.Seq, Target: "direct:mika"}, true, nil
	}}
	if err := n.NotifyStalled(id, "I could not reach the model."); err != nil {
		t.Fatalf("NotifyStalled: %v", err)
	}
	// A stall that recurs sends nothing new.
	if err := n.NotifyStalled(id, "I could not reach the model."); err != nil {
		t.Fatalf("second NotifyStalled: %v", err)
	}

	// Held: the adapter sees nothing yet.
	pending, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a notice was delivered during quiet hours: %+v", pending)
	}

	// It is queued, once, with a hold that ends when the window does —
	// held is visible, so "waiting for morning" is distinguishable from
	// "never sent".
	held, err := db.ChatOutboundHeld(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 {
		t.Fatalf("held notices = %d, want exactly one: %+v", len(held), held)
	}
	if held[0].DedupeKey != "butler-task-stalled:"+id {
		t.Fatalf("dedupe key = %q", held[0].DedupeKey)
	}
	wantHold := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 7, 0, 0, 0, time.UTC)
	if !held[0].HoldUntil.Equal(wantHold) {
		t.Fatalf("hold_until = %s, want the end of quiet hours (%s)", held[0].HoldUntil, wantHold)
	}

	// Outside the window the same notice would go straight out.
	ledger.Now = func() time.Time {
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 0, 0, 0, time.UTC)
	}
	other := newTask(t, db)
	if err := n.NotifyStalled(other, "again"); err != nil {
		t.Fatal(err)
	}
	sent, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].CorrelationID != other {
		t.Fatalf("daytime notice = %+v", sent)
	}
}

// Settings are validated on write, so a value nothing downstream can
// honour never reaches the runner.
func TestButlerSettingsAreValidated(t *testing.T) {
	db, _ := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()

	defaults, stored, err := db.GetButlerSettings()
	if err != nil {
		t.Fatal(err)
	}
	if stored {
		t.Fatal("settings were reported as stored before any were set")
	}
	if defaults.HouseholdRounds != 16 || defaults.OperatorRounds != 24 {
		t.Fatalf("A4's defaults are not the defaults: %+v", defaults)
	}

	good := defaults
	good.UpdatedBy = "mika"
	if err := db.SetButlerSettings(good); err != nil {
		t.Fatalf("SetButlerSettings: %v", err)
	}
	for name, mutate := range map[string]func(*runtimedb.ButlerSettings){
		"rounds too high":  func(s *runtimedb.ButlerSettings) { s.OperatorRounds = 500 },
		"rounds zero":      func(s *runtimedb.ButlerSettings) { s.HouseholdRounds = 0 },
		"stall too short":  func(s *runtimedb.ButlerSettings) { s.StallSeconds = 1 },
		"retries negative": func(s *runtimedb.ButlerSettings) { s.RetryLimit = -1 },
		"quiet out of day": func(s *runtimedb.ButlerSettings) { s.QuietStartMin = 2000 },
		"unknown timezone": func(s *runtimedb.ButlerSettings) { s.Timezone = "Mars/Olympus" },
		"no timezone":      func(s *runtimedb.ButlerSettings) { s.Timezone = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := good
			mutate(&bad)
			if err := db.SetButlerSettings(bad); err == nil {
				t.Fatal("an out-of-range setting was stored")
			}
		})
	}
	// The stored good settings survived every refused write.
	back, stored, err := db.GetButlerSettings()
	if err != nil || !stored {
		t.Fatalf("GetButlerSettings = %v, %t", err, stored)
	}
	if back.OperatorRounds != 24 || back.UpdatedBy != "mika" {
		t.Fatalf("stored settings = %+v", back)
	}
}

func TestQuietHoursWindowWrapsMidnight(t *testing.T) {
	s := runtimedb.DefaultButlerSettings()
	s.QuietStartMin, s.QuietEndMin = 22*60, 7*60
	day := func(h, m int) time.Time { return time.Date(2026, 9, 9, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{day(23, 0), true}, {day(3, 0), true}, {day(6, 59), true},
		{day(7, 0), false}, {day(12, 0), false}, {day(21, 59), false}, {day(22, 0), true},
	} {
		if got := s.InQuietHours(tc.at); got != tc.want {
			t.Fatalf("InQuietHours(%s) = %t, want %t", tc.at.Format("15:04"), got, tc.want)
		}
	}
	// Equal bounds mean a household that never configured quiet hours,
	// not a window covering the whole day.
	none := runtimedb.DefaultButlerSettings()
	if none.InQuietHours(day(3, 0)) {
		t.Fatal("unset quiet hours held a notice")
	}
	if got := s.QuietHoursEnd(day(23, 30)); !got.Equal(time.Date(2026, 9, 10, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("a hold starting before midnight ends at %s", got)
	}
}

func TestStallNoticeIsSilentWithNoDirectSurface(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	at := chatConnection(t, db, "alfred")
	n := &Notifier{Ledger: ledger, Binding: func(string) (runtimedb.ChatOutbound, bool, error) {
		return runtimedb.ChatOutbound{}, false, nil
	}}
	if err := n.NotifyStalled(id, "stalled"); err != nil {
		t.Fatalf("NotifyStalled with no binding: %v", err)
	}
	queued, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	heldMsgs, err := db.ChatOutboundHeld(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued)+len(heldMsgs) != 0 {
		t.Fatalf("%d notices were queued with nowhere to send them", len(queued)+len(heldMsgs))
	}
}

// The notifier is the runner's StallNotifier, checked at compile time
// rather than by hoping the signatures stay aligned.
var _ butler.StallNotifier = (*Notifier)(nil)
