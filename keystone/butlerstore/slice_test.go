package butlerstore

// legacy.Slice against the real ledger: an external reasoning loop (the
// native agent runtime, ADR-W041) gets exactly the durable guarantees
// Runner.Run gives its own loop.

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentruntime/legacy"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// sliceRunner is a Runner for a Slice: the loop is the runtime's, so the
// provider is only named, never called.
func sliceRunner(t *testing.T, ts butler.Toolset, l *Ledger, id string) *legacy.Runner {
	t.Helper()
	r := newRunnerWithProvider(t, ts, l, id, &scriptedProvider{dieAt: -1})
	r.Client.(*testGateway).ObservationTTL = time.Hour
	return r
}

func beginSlice(t *testing.T, r *legacy.Runner) *legacy.Slice {
	t.Helper()
	s, _, err := r.BeginSlice(context.Background(), "why is photos unhealthy?")
	if err != nil || s == nil {
		t.Fatalf("BeginSlice: %v", err)
	}
	return s
}

func taskState(t *testing.T, db *runtimedb.DB, id string) runtimedb.ButlerTaskState {
	t.Helper()
	task, err := db.GetButlerTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return task.State
}

func TestSliceCompletesWithEvidenceAndDoesNotRepeatReads(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := newCountingToolset()
	s := beginSlice(t, sliceRunner(t, ts, ledger, id))
	ctx := context.Background()

	if err := s.ModelStarted(); err != nil {
		t.Fatal(err)
	}
	if err := s.ModelEnded(true, []string{"get_health"}); err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"n1", "n2", "n1"} {
		res, err := s.Call(ctx, "get_health", map[string]any{"node": node})
		if err != nil || res.Failed || res.Parked || !strings.Contains(res.Content, node) {
			t.Fatalf("call %s: %+v %v", node, res, err)
		}
	}
	if ts.calls["get_health/n1"] != 1 {
		t.Fatalf("n1 dispatched %d times; the repeat must be served from the ledger", ts.calls["get_health/n1"])
	}
	if err := s.ModelStarted(); err != nil {
		t.Fatal(err)
	}
	if err := s.ModelEnded(true, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, "n2 is fine; n1 is fine"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := taskState(t, db, id); got != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task %s, want completed", got)
	}
	ev, err := db.ListButlerTaskEvidence(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 3 {
		t.Fatalf("evidence rows = %d, want two verified reads plus the delivery", len(ev))
	}
	if tr := s.Transcript(); tr.FinalText != "n2 is fine; n1 is fine" || len(tr.Audit.Calls) != 3 {
		t.Fatalf("transcript %+v", tr)
	}
}

func TestSliceParksOnTheAggregateBudget(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := newCountingToolset()
	r := sliceRunner(t, ts, ledger, id)
	r.Client.(*testGateway).ToolCalls = 1
	s := beginSlice(t, r)

	if _, err := s.Call(context.Background(), "get_health", map[string]any{"node": "n1"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(context.Background(), "get_health", map[string]any{"node": "n2"})
	if err != nil || !res.Parked {
		t.Fatalf("second call: %+v %v", res, err)
	}
	if ts.calls["get_health/n2"] != 0 {
		t.Fatal("a call past the budget was dispatched")
	}
	if got := taskState(t, db, id); got != runtimedb.ButlerTaskWaiting {
		t.Fatalf("task %s, want waiting", got)
	}
	if !strings.Contains(s.Transcript().FinalText, "budget") {
		t.Fatalf("final text %q", s.Transcript().FinalText)
	}
}

func TestSliceParksForAQuestionABlockAndAnOutage(t *testing.T) {
	for name, park := range map[string]func(*legacy.Slice) error{
		"question": func(s *legacy.Slice) error { return s.AskUser("Which node?") },
		"blocked":  func(s *legacy.Slice) error { return s.Blocked("no access to the NAS") },
		"pause":    func(s *legacy.Slice) error { return s.Pause() },
		"outage":   func(s *legacy.Slice) error { _ = s.Unavailable("model unreachable"); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
			defer db.Close()
			id := newTask(t, db)
			s := beginSlice(t, sliceRunner(t, newCountingToolset(), ledger, id))
			if err := park(s); err != nil {
				t.Fatal(err)
			}
			if got := taskState(t, db, id); got != runtimedb.ButlerTaskWaiting {
				t.Fatalf("task %s, want waiting", got)
			}
		})
	}
}

func TestSliceFailedAttemptIsClosedAndTheNextOneProceeds(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	s := beginSlice(t, sliceRunner(t, newCountingToolset(), ledger, id))
	if err := s.ModelStarted(); err != nil {
		t.Fatal(err)
	}
	if err := s.ModelEnded(false, nil); err != nil {
		t.Fatal(err)
	}
	// The retry's own checkpoint opens and closes normally.
	if err := s.ModelStarted(); err != nil {
		t.Fatalf("retry refused: %v", err)
	}
	if err := s.ModelEnded(true, nil); err != nil {
		t.Fatalf("retry's answer refused: %v", err)
	}
	steps, err := ledger.Steps(id)
	if err != nil {
		t.Fatal(err)
	}
	var failed, verified int
	for _, st := range steps {
		if st.Kind == butler.LedgerStepModel {
			switch st.State {
			case butler.LedgerStepFailed:
				failed++
			case butler.LedgerStepVerified:
				verified++
			}
		}
	}
	if failed != 1 || verified != 1 {
		t.Fatalf("model steps: %d failed, %d verified; want 1 and 1", failed, verified)
	}
}

func TestSliceStopsWhenAuthorityIsWithdrawn(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := newCountingToolset()
	s := beginSlice(t, sliceRunner(t, ts, ledger, id))
	if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskCancelled, "stopped by owner", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.ModelStarted(); err == nil {
		t.Fatal("a model attempt went ahead on a cancelled task")
	}
	if _, err := s.Call(context.Background(), "get_health", map[string]any{"node": "n1"}); err == nil {
		t.Fatal("a tool ran on a cancelled task")
	}
	if len(ts.calls) != 0 {
		t.Fatalf("dispatched %v", ts.calls)
	}
	if err := s.Finish(context.Background(), "done"); err == nil {
		t.Fatal("a cancelled task accepted an answer")
	}
}

// blockingToolset holds every dispatch until released, standing in for
// a tool the runtime abandoned after its deadline and that is still
// running.
type blockingToolset struct {
	*countingToolset
	mu      sync.Mutex
	release chan struct{}
	started chan struct{}
}

func (b *blockingToolset) Execute(ctx context.Context, name string, input map[string]any, ro bool) (butler.ToolOutcome, error) {
	b.started <- struct{}{}
	<-b.release
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.countingToolset.Execute(ctx, name, input, ro)
}

func TestSliceBudgetCountsACallStillInFlight(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := &blockingToolset{countingToolset: newCountingToolset(), release: make(chan struct{}), started: make(chan struct{}, 1)}
	r := sliceRunner(t, ts.countingToolset, ledger, id)
	r.Client.(*testGateway).Toolset = ts
	r.Client.(*testGateway).ToolCalls = 1
	s := beginSlice(t, r)

	done := make(chan legacy.ToolCallResult, 1)
	go func() {
		res, _ := s.Call(context.Background(), "get_health", map[string]any{"node": "n1"})
		done <- res
	}()
	<-ts.started // the first call is out and has not come back

	res, err := s.Call(context.Background(), "get_health", map[string]any{"node": "n2"})
	if err != nil || !res.Parked {
		t.Fatalf("a second call went past a budget of 1 while the first was in flight: %+v %v", res, err)
	}
	close(ts.release)
	<-done
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.calls["get_health/n2"] != 0 {
		t.Fatal("the call past the budget was dispatched")
	}
}

func TestSliceBudgetIsNotSpentByLedgerHits(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	r := sliceRunner(t, newCountingToolset(), ledger, id)
	r.Client.(*testGateway).ToolCalls = 2
	s := beginSlice(t, r)
	ctx := context.Background()
	// n1 dispatched, n1 again served from the ledger (free), n2 dispatched.
	for _, node := range []string{"n1", "n1", "n2"} {
		if res, err := s.Call(ctx, "get_health", map[string]any{"node": node}); err != nil || res.Parked {
			t.Fatalf("%s: %+v %v", node, res, err)
		}
	}
}
