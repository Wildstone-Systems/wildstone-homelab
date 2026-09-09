package butlerstore

// Found in review of the ADR-W030 Part C branch: the aggregate tool
// budget charged for calls that were served from the ledger.
//
// A ledger-served call dispatches nothing — it appends no step, spends
// no provider capacity, and touches nothing outside this process. It is
// the mechanism that stops a task re-reading a fact it already
// established. Charging it means a task talks itself out of a budget it
// never spent, and the count then disagrees with the persisted steps on
// the next resume.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestRereadingAnEstablishedFactDoesNotSpendBudget(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	ts := newCountingToolset()

	id := newTask(t, db)
	r := runner(ts, ledger)
	r.MaxSliceRounds = 8
	r.MaxToolCalls = 2

	// Four turns asking for the SAME read. One dispatch; the rest come
	// back from the ledger.
	p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
		readsFor("a1"), readsFor("a1"), readsFor("a1"), readsFor("a1"),
	}}
	tr, err := r.Run(context.Background(), p, id, "diagnose")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(tr.FinalText, "budget") {
		t.Fatalf("re-reading one established fact exhausted a 2-call budget: %q", tr.FinalText)
	}
	if n := ts.calls["get_health/a1"]; n != 1 {
		t.Fatalf("the read was dispatched %d times, want once", n)
	}
	if task, _ := db.GetButlerTask(id); task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task state = %q, want completed", task.State)
	}

	// The budget still bites on real dispatches: three DIFFERENT reads
	// against a budget of two stops the run.
	id2 := newTask(t, db)
	r2 := runner(ts, ledger)
	r2.MaxSliceRounds = 8
	r2.MaxToolCalls = 2
	p2 := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
		readsFor("b1"), readsFor("b2"), readsFor("b3"),
	}}
	tr2, err := r2.Run(context.Background(), p2, id2, "diagnose")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(tr2.FinalText, "budget") {
		t.Fatalf("three real dispatches did not exhaust a 2-call budget: %q", tr2.FinalText)
	}
}
