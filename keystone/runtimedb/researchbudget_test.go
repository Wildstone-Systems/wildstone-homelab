package runtimedb

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func researchTask(t *testing.T, db *DB) (string, int64) {
	t.Helper()
	if _, err := db.AcquireCoordinator("research-test"); err != nil {
		t.Fatal(err)
	}
	id := newTask(t, db, nil)
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "test", ""); err != nil {
		t.Fatal(err)
	}
	step, err := db.AppendButlerTaskStep(id, ButlerTaskStep{Kind: ButlerStepTool, Name: "research-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	return id, a.Seq
}
func TestResearchConcurrentRestartAndContinuation(t *testing.T) {
	db := openWith(t, "mika")
	task, a := researchTask(t, db)
	ctx := context.Background()
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := db.ReserveResearchAttempt(ctx, task, a, "search", DefaultResearchLimits())
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrResearchBudgetExhausted) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 10 {
		t.Fatalf("admitted=%d", admitted.Load())
	}
	waits, err := db.ListButlerTaskWaits(task)
	if err != nil || len(waits) != 1 || waits[0].Reason != ButlerWaitBudgetExhausted {
		t.Fatalf("waits=%+v %v", waits, err)
	}
	steps, err := db.ListButlerTaskSteps(task)
	if err != nil || len(steps) != 1 {
		t.Fatalf("tool budget changed: %+v %v", steps, err)
	}
	path := db.path
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.AcquireCoordinator("research-test"); err != nil {
		t.Fatal(err)
	}
	_, b, err := db.ReserveResearchAttempt(ctx, task, a, "search", DefaultResearchLimits())
	if !errors.Is(err, ErrResearchBudgetExhausted) || b.SearchesUsed != 10 {
		t.Fatalf("reset: %+v %v", b, err)
	}
	for _, bad := range []ResearchLimits{{0, 20}, {101, 20}, {10, 20}, {12, 201}, {12, 19}} {
		if err = db.ContinueResearch(task, "test-human", b.Revision, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if err = db.ContinueResearch(task, "test-human", b.Revision, ResearchLimits{12, 20}); err != nil {
		t.Fatal(err)
	}
	if err = db.ContinueResearch(task, "test-human", b.Revision, ResearchLimits{12, 20}); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	_, b, err = db.ReserveResearchAttempt(ctx, task, a, "search", DefaultResearchLimits())
	if err != nil || b.SearchesUsed != 11 || b.Allowed.Searches != 12 {
		t.Fatalf("extension lost: %+v %v", b, err)
	}
	attempts, err := db.ListResearchAttempts(task)
	if err != nil || len(attempts) != 11 {
		t.Fatalf("history: %+v %v", attempts, err)
	}
	// Higher configuration never silently extends totals; lowering below use parks.
	_, b, err = db.ReserveResearchAttempt(ctx, task, a, "fetch", ResearchLimits{100, 200})
	if err != nil || b.Allowed.Searches != 12 {
		t.Fatalf("config increase: %+v %v", b, err)
	}
	_, b, err = db.ReserveResearchAttempt(ctx, task, a, "search", DefaultResearchLimits())
	if !errors.Is(err, ErrResearchBudgetExhausted) || b.Allowed.Searches != 10 || b.SearchesUsed != 11 {
		t.Fatalf("lowering: %+v %v", b, err)
	}
	var count int
	if err = db.sql.QueryRow(`SELECT count(*) FROM research_continuations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("history count=%d %v", count, err)
	}
}
func TestResearchCheckpointAndFences(t *testing.T) {
	db := openWith(t, "mika")
	task, a := researchTask(t, db)
	steps, _ := db.ListButlerTaskSteps(task)
	step := steps[0].Seq
	c := ResearchCheckpoint{Steps: []int64{step}, NextAction: "fetch"}
	if err := db.SaveResearchCheckpoint(task, 0, c); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("unverified: %v", err)
	}
	if err := db.FinishButlerAttempt(a, ButlerAttemptSucceeded, `{"source":"fixture"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerStepState(step, ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveResearchCheckpoint(task, 0, c); err != nil {
		t.Fatal(err)
	}
	got, err := db.LatestResearchCheckpoint(task)
	if err != nil || len(got.Steps) != 1 || got.Steps[0] != step {
		t.Fatalf("checkpoint=%+v %v", got, err)
	}
	other, b := researchTask(t, db)
	if err := db.SaveResearchCheckpoint(other, 0, c); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("cross task: %v", err)
	}
	if _, _, err := db.ReserveResearchAttempt(context.Background(), task, b, "search", DefaultResearchLimits()); err == nil {
		t.Fatal("cross-task attempt admitted")
	}
	if _, err := db.FenceButlerTaskAttempts(other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ReserveResearchAttempt(context.Background(), other, b, "search", DefaultResearchLimits()); !errors.Is(err, ErrAttemptFenced) {
		t.Fatalf("fenced: %v", err)
	}
	if err := db.SaveResearchCheckpoint(other, 0, ResearchCheckpoint{NextAction: "read"}); !errors.Is(err, ErrAttemptFenced) {
		t.Fatalf("stale checkpoint: %v", err)
	}
}
func TestResearchAccountingAndRollback(t *testing.T) {
	db := openWith(t, "mika")
	task, a := researchTask(t, db)
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := db.ReserveResearchAttempt(cancelled, task, a, "fetch", DefaultResearchLimits()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, state := range []string{"failed", "cancelled", "succeeded"} {
		seq, _, err := db.ReserveResearchAttempt(ctx, task, a, "fetch", DefaultResearchLimits())
		if err != nil {
			t.Fatal(err)
		}
		if err = db.FinishResearchAttempt(task, seq, state); err != nil {
			t.Fatal(err)
		}
		if err = db.FinishResearchAttempt(task, seq, state); !errors.Is(err, ErrResearchConflict) {
			t.Fatalf("duplicate settlement: %v", err)
		}
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER fail_research_insert BEFORE INSERT ON research_attempts BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ReserveResearchAttempt(ctx, task, a, "fetch", DefaultResearchLimits()); err == nil {
		t.Fatal("fault not raised")
	}
	b, err := db.GetResearchBudget(task)
	if err != nil || b.FetchesUsed != 3 || b.SearchesUsed != 0 {
		t.Fatalf("accounting=%+v %v", b, err)
	}
	if err := db.TransitionButlerTask(task, ButlerTaskCancelled, "test", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ReserveResearchAttempt(ctx, task, a, "search", DefaultResearchLimits()); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("cancelled task: %v", err)
	}
}
