package butlerstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestResearchCoordinatorAccountingAndAuthority(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
	defer db.Close()
	task := newTask(t, db)
	if err := l.Running(task, "test"); err != nil {
		t.Fatal(err)
	}
	step, err := l.AppendStep(task, "tool", "research-fixture", "{}")
	if err != nil {
		t.Fatal(err)
	}
	a, err := l.StartAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	calls := 0
	allowed := true
	r := Research{Ledger: l, Authorize: func(context.Context, string, string) (runtimedb.ResearchLimits, error) {
		if !allowed {
			return runtimedb.ResearchLimits{}, ErrAuthorityWithdrawn
		}
		return runtimedb.ResearchLimits{Searches: 2, Fetches: 2}, nil
	}}
	dispatch := func(context.Context) error { calls++; return errors.New("synthetic upstream failure") }
	// Default-unwired policy and withdrawn authority refuse before reservations.
	if _, err := (&Research{Ledger: l}).Attempt(ctx, task, a.Seq, "search", dispatch); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatal(err)
	}
	allowed = false
	if _, err := r.Attempt(ctx, task, a.Seq, "search", dispatch); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatal(err)
	}
	allowed = true
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Attempt(cancelled, task, a.Seq, "search", dispatch); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("denied/pre-cancelled attempt dispatched")
	}
	for i := 0; i < 2; i++ {
		b, err := r.Attempt(ctx, task, a.Seq, "search", dispatch)
		if err == nil || b.SearchesUsed != i+1 {
			t.Fatalf("retry=%+v %v", b, err)
		}
	}
	if _, err := r.Attempt(ctx, task, a.Seq, "search", dispatch); !errors.Is(err, runtimedb.ErrResearchBudgetExhausted) {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("over-budget dispatch: %d", calls)
	}
	// Retained reads still reauthorize but spend no upstream unit.
	reads := 0
	for i := 0; i < 3; i++ {
		if err := r.ReadRetained(ctx, task, func(context.Context) error { reads++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	allowed = false
	if err := r.ReadRetained(ctx, task, func(context.Context) error { reads++; return nil }); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatal(err)
	}
	if reads != 3 {
		t.Fatal("revoked retained read")
	}
	allowed = true
	b, err := db.GetResearchBudget(task)
	if err != nil || b.SearchesUsed != 2 || b.FetchesUsed != 0 {
		t.Fatalf("read accounting=%+v %v", b, err)
	}
	limits := runtimedb.ResearchLimits{Searches: 3, Fetches: 2}
	if err := r.Continue(ctx, task, b.Revision, limits); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatalf("missing human policy: %v", err)
	}
	r.AuthorizeContinuation = func(context.Context, string, int64, runtimedb.ResearchLimits) (string, error) {
		return "", ErrAuthorityWithdrawn
	}
	if err := r.Continue(ctx, task, b.Revision, limits); !errors.Is(err, ErrAuthorityWithdrawn) {
		t.Fatalf("human denied: %v", err)
	}
	r.AuthorizeContinuation = func(context.Context, string, int64, runtimedb.ResearchLimits) (string, error) {
		return "test-human", nil
	}
	if err := r.Continue(ctx, task, b.Revision, limits); err != nil {
		t.Fatal(err)
	}
	inflight, cancelFlight := context.WithCancel(ctx)
	if _, err := r.Attempt(inflight, task, a.Seq, "fetch", func(c context.Context) error { cancelFlight(); return c.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	attempts, err := db.ListResearchAttempts(task)
	if err != nil || len(attempts) != 3 || attempts[2].State != "cancelled" {
		t.Fatalf("attempts=%+v %v", attempts, err)
	}
	b, err = db.GetResearchBudget(task)
	if err != nil || b.SearchesUsed != 2 || b.FetchesUsed != 1 {
		t.Fatalf("after cancellation=%+v %v", b, err)
	}
	steps, err := db.ListButlerTaskSteps(task)
	if err != nil || len(steps) != 1 {
		t.Fatalf("changed platform tool accounting: %+v %v", steps, err)
	}
}
