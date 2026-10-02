package runtimedb

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func researchTask(t *testing.T, db *DB) (string, int64) {
	t.Helper()
	if _, err := db.AcquireCoordinator("research-test"); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "test-admin"); err != nil {
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
		if err = db.ContinueResearch(task, "mika", b.Revision, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if err = db.ContinueResearch(task, "mika", b.Revision, ResearchLimits{12, 20}); err != nil {
		t.Fatal(err)
	}
	if err = db.ContinueResearch(task, "mika", b.Revision, ResearchLimits{12, 20}); !errors.Is(err, ErrResearchConflict) {
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
	_, b, err = db.ReserveResearchAttempt(ctx, task, a, "fetch", DefaultResearchLimits())
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

func TestResearchWaitAndContinuationRollback(t *testing.T) {
	db := openWith(t, "mika")
	task, a := researchTask(t, db)
	ctx := context.Background()
	limits := ResearchLimits{1, 2}
	if _, _, err := db.ReserveResearchAttempt(ctx, task, a, "search", limits); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER fail_research_wait BEFORE INSERT ON butler_task_waits BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ReserveResearchAttempt(ctx, task, a, "search", limits); err == nil || errors.Is(err, ErrResearchBudgetExhausted) {
		t.Fatalf("false park: %v", err)
	}
	current, err := db.GetButlerTask(task)
	if err != nil || current.State != ButlerTaskRunning {
		t.Fatalf("partial park: %+v %v", current, err)
	}
	if _, err := db.sql.Exec(`DROP TRIGGER fail_research_wait`); err != nil {
		t.Fatal(err)
	}
	_, b, err := db.ReserveResearchAttempt(ctx, task, a, "search", limits)
	if !errors.Is(err, ErrResearchBudgetExhausted) {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER fail_research_continue BEFORE INSERT ON research_continuations BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.ContinueResearch(task, "mika", b.Revision, ResearchLimits{2, 2}); err == nil {
		t.Fatal("fault ignored")
	}
	after, err := db.GetResearchBudget(task)
	if err != nil || after != b {
		t.Fatalf("partial continuation: %+v %v", after, err)
	}
	current, err = db.GetButlerTask(task)
	if err != nil || current.State != ButlerTaskWaiting {
		t.Fatalf("resumed without continuation: %+v %v", current, err)
	}
	// Ordinary resume cannot replenish research allowance.
	if err := db.ResolveButlerTaskWait(task, "mika", "generic resume"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ReserveResearchAttempt(ctx, task, a, "search", limits); !errors.Is(err, ErrResearchBudgetExhausted) {
		t.Fatalf("reset through resume: %v", err)
	}
}

func TestResearchCheckpointPersistsAndCannotResetBudgetJSON(t *testing.T) {
	db := openWith(t, "mika")
	task, a := researchTask(t, db)
	if _, _, err := db.ReserveResearchAttempt(context.Background(), task, a, "fetch", DefaultResearchLimits()); err != nil {
		t.Fatal(err)
	}
	steps, err := db.ListButlerTaskSteps(task)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.FinishButlerAttempt(a, ButlerAttemptSucceeded, `{"source":"fixture"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err = db.SetButlerStepState(steps[0].Seq, ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	if err = db.SaveResearchCheckpoint(task, 0, ResearchCheckpoint{Steps: []int64{steps[0].Seq}, NextAction: "report"}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec(`UPDATE butler_tasks SET budget_json='{"searchesUsed":0,"fetchesUsed":0,"searchesAllowed":99999}'`); err != nil {
		t.Fatal(err)
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
	c, err := db.LatestResearchCheckpoint(task)
	if err != nil || c.NextAction != "report" || len(c.Steps) != 1 || c.Steps[0] != steps[0].Seq {
		t.Fatalf("checkpoint=%+v %v", c, err)
	}
	b, err := db.GetResearchBudget(task)
	if err != nil || b.FetchesUsed != 1 || b.Allowed != DefaultResearchLimits() {
		t.Fatalf("reset through task JSON: %+v %v", b, err)
	}
}

func TestResearchDoesNotResumeUnrelatedWait(t *testing.T) {
	db := openWith(t, "mika")
	task, a := researchTask(t, db)
	if err := db.OpenButlerTaskWait(task, ButlerTaskWait{Reason: ButlerWaitApproval}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ReserveResearchAttempt(context.Background(), task, a, "search", DefaultResearchLimits()); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("unrelated wait: %v", err)
	}
	if err := db.ContinueResearch(task, "mika", 1, ResearchLimits{20, 40}); err == nil {
		t.Fatal("continued unrelated wait")
	}
}

func TestResearchMigrationRetainsExistingLedger(t *testing.T) {
	// Upgrade the genuine pre-privacy/pre-budget schema. Current task writers
	// require origin_conversation, which S03 adds after this historical boundary.
	db, err := openUnmigrated(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.migrateThrough("migrations/0098_search_providers.sql"); err != nil {
		t.Fatal(err)
	}
	record(t, db, "mika")
	if _, err = db.AcquireCoordinator("research-test"); err != nil {
		t.Fatal(err)
	}
	if err = db.AdmitButlerMember("mika", "test-admin"); err != nil {
		t.Fatal(err)
	}
	person, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := db.sql.Exec(`INSERT INTO conversation_work(owner_person_id,created_at,updated_at) VALUES(?,?,?)`, person.PersonID, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	parentID, err := parent.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.sql.Exec(`INSERT INTO butler_tasks(created_at,updated_at,kind,state,author_person_id,owner_person_id,surface,agent_class,audience,objective,completion_criteria,retention,envelope_version,objective_kind,conversation_work)
 VALUES(?,?,'conversation','pending',?,?,'admin','operator','aud_owner','historical research task','fixture retained','manual',?,?,?)`, now(), now(), person.PersonID, person.PersonID, TaskEnvelopeVersion, ObjectiveInformational, parentID)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := row.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	task := butlerTaskID(seq)
	if err = db.TransitionButlerTask(task, ButlerTaskRunning, "test", ""); err != nil {
		t.Fatal(err)
	}
	step, err := db.AppendButlerTaskStep(task, ButlerTaskStep{Kind: ButlerStepTool, Name: "research-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	before := ButlerTask{ID: task}
	err = db.sql.QueryRow(`SELECT objective,lease_epoch FROM butler_tasks WHERE seq=?`, seq).Scan(&before.Objective, &before.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.migrate(); err != nil {
		t.Fatal(err)
	}
	if err = db.migrate(); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetButlerTask(task)
	if err != nil || before.ID != after.ID || before.Objective != after.Objective || before.LeaseEpoch != after.LeaseEpoch {
		t.Fatalf("task changed: %+v %v", after, err)
	}
	if _, _, err = db.ReserveResearchAttempt(context.Background(), task, a.Seq, "search", DefaultResearchLimits()); err != nil {
		t.Fatal(err)
	}
	steps, err := db.ListButlerTaskSteps(task)
	if err != nil || len(steps) != 1 {
		t.Fatalf("ledger changed: %+v %v", steps, err)
	}
	var toolCalls int
	if err = db.sql.QueryRow(`SELECT tool_calls FROM conversation_work WHERE seq=?`, parentID).Scan(&toolCalls); err != nil || toolCalls != 1 {
		t.Fatalf("conversation ledger changed: %d %v", toolCalls, err)
	}
}

func TestResearchExecutionGates(t *testing.T) {
	for _, gate := range []string{"stop", "restore", "promotion"} {
		t.Run(gate, func(t *testing.T) {
			db := openWith(t, "mika")
			task, a := researchTask(t, db)
			var want error
			switch gate {
			case "stop":
				if _, err := db.sql.Exec(`UPDATE household_execution_stop SET active=1 WHERE singleton=1`); err != nil {
					t.Fatal(err)
				}
				want = ErrHouseholdStopped
			case "restore":
				if err := db.EnterRestoreQuarantine("fixture", time.Now()); err != nil {
					t.Fatal(err)
				}
				want = ErrQuarantined
			case "promotion":
				if _, err := db.PromoteCoordinator("new-test-coordinator"); err != nil {
					t.Fatal(err)
				}
				want = ErrCoordinatorFenced
			}
			if _, _, err := db.ReserveResearchAttempt(context.Background(), task, a, "search", DefaultResearchLimits()); !errors.Is(err, want) {
				t.Fatalf("gate=%v want=%v", err, want)
			}
			// The transaction boundary enforces the same gate even after an earlier
			// preflight succeeded and was followed by a concurrent stop/promotion.
			tx, err := db.sql.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err = db.researchFenceTx(tx); !errors.Is(err, want) {
				t.Fatalf("transaction gate=%v want=%v", err, want)
			}
		})
	}
}

func TestResearchContinuationRequiresLivePerson(t *testing.T) {
	db := openWith(t, "mika")
	task, a := researchTask(t, db)
	ctx := context.Background()
	limits := ResearchLimits{1, 2}
	if _, _, err := db.ReserveResearchAttempt(ctx, task, a, "search", limits); err != nil {
		t.Fatal(err)
	}
	_, b, err := db.ReserveResearchAttempt(ctx, task, a, "search", limits)
	if !errors.Is(err, ErrResearchBudgetExhausted) {
		t.Fatal(err)
	}
	raised := ResearchLimits{2, 2}
	if err = db.ContinueResearch(task, "machine-fixture", b.Revision, raised); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("machine actor: %v", err)
	}
	if _, err = db.sql.Exec(`UPDATE butler_members SET admitted=0`); err != nil {
		t.Fatal(err)
	}
	if err = db.ContinueResearch(task, "mika", b.Revision, raised); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("unadmitted actor: %v", err)
	}
	after, err := db.GetResearchBudget(task)
	if err != nil || after != b {
		t.Fatalf("denial changed budget: %+v %v", after, err)
	}
}
