package butlerlive

// ADR-W039 §3.7, Phase 4 (#340): a Butler whose runtime is not native
// waits rather than falling back to the in-process model when that
// runtime is not connected, and reconnecting resumes it — the
// Wildstone-side half proven here; the reconnect-side re-offer is
// api.reofferWaitingTasks, proven in the api package.

import (
	"context"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// TestATaskWaitsWhenItsRuntimeIsNotConnected proves the three "not
// connected" health states all park the task the same way, and that no
// model ever sees the turn meant for the offline runtime.
func TestATaskWaitsWhenItsRuntimeIsNotConnected(t *testing.T) {
	for _, health := range []agentapi.RuntimeHealth{
		agentapi.HealthUnregistered, agentapi.HealthOffline, agentapi.HealthRevoked,
	} {
		t.Run(string(health), func(t *testing.T) {
			f := newFixture(t)
			alfred, err := f.db.GetButler("alfred")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetButlerRuntime(alfred.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeHermes}, "mika"); err != nil {
				t.Fatal(err)
			}
			if health != agentapi.HealthUnregistered {
				if err := f.db.SetRuntimeHealth(alfred.Seq, health); err != nil {
					t.Fatal(err)
				}
			}

			answer, err := f.butlers.Serve(context.Background(), f.turn("alfred", "hello"))
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if answer.TaskID == "" {
				t.Fatal("no task was opened for the turn")
			}
			if f.local.rounds != 0 || f.cloud.rounds != 0 {
				t.Errorf("a model ran the offline runtime's turn: local=%d cloud=%d", f.local.rounds, f.cloud.rounds)
			}
			if answer.FinalText == "" {
				t.Error("the caller got no explanation that the turn is queued")
			}

			task, err := f.db.GetButlerTask(answer.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if task.State != runtimedb.ButlerTaskWaiting {
				t.Fatalf("task state = %s, want waiting", task.State)
			}
			waits, err := f.db.ListButlerTaskWaits(answer.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if len(waits) == 0 || waits[len(waits)-1].Reason != runtimedb.ButlerWaitRuntimeOffline || !waits[len(waits)-1].ResolvedAt.IsZero() {
				t.Fatalf("waits = %+v, want an OPEN runtime-offline wait", waits)
			}
		})
	}
}

// TestANativeRuntimeIsNeverParkedOffline is the converse: the deployment
// default (native, unregistered/connected as the platform sees fit)
// must never trip the offline wait, because the whole family is
// meaningless for a runtime that runs in this process.
func TestANativeRuntimeIsNeverParkedOffline(t *testing.T) {
	f := newFixture(t)
	ans, err := f.butlers.Serve(context.Background(), f.turn("alfred", "hello"))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !f.local.sawText("hello") {
		t.Fatal("the native runtime's own model never saw the turn")
	}
	task, err := f.db.GetButlerTask(ans.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State == runtimedb.ButlerTaskWaiting {
		t.Fatalf("a native-runtime task waited, want it to have run")
	}
}
