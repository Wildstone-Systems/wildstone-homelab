package butlerlive

import (
	"context"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// A parked task read as waiting whose offline wait the reconnecting
// runtime resolves before the waits are read is held by that runtime:
// Work Start moved it to running and the runtime has its handle. Reading
// the resolved wait alone said "not held", and the next message resumed
// the task and re-issued its handle out from under the runtime (#389).
func TestAParkedTaskTheRuntimeJustStartedIsHeld(t *testing.T) {
	f := newFixture(t)
	alfred, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetButlerRuntime(alfred.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeHermes}, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetRuntimeHealth(alfred.Seq, agentapi.HealthOffline); err != nil {
		t.Fatal(err)
	}
	answer, err := f.butlers.Serve(context.Background(), f.turn("alfred", "hello"))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	stale, err := f.db.GetButlerTask(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if stale.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("parked task state = %s, want waiting", stale.State)
	}

	// The runtime reconnects and starts it: the wait resolves and the
	// task runs, after the state above was read.
	if err := f.butlers.Ledger.Running(answer.TaskID, "delivery"); err != nil {
		t.Fatal(err)
	}
	held, err := f.butlers.heldByExternalRuntime(alfred, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("a task the runtime just started reads as not held")
	}
}
