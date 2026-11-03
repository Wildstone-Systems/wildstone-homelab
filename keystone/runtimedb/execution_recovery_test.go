package runtimedb

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// These fixtures use only the task ledger's supported operations. The one
// simulated boundary is explicit: StartButlerAttempt commits immediately
// before dispatch, and the test stops without calling FinishButlerAttempt.
// No provider, network, shell, or household effect is invoked.
func TestInterruptedButlerAttemptsRecoverByExecutionBoundary(t *testing.T) {
	path := migrated.Path(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record(t, db, "mika")
	if _, err := db.AcquireCoordinator("keystone-a"); err != nil {
		t.Fatal(err)
	}

	t.Run("model work retries at a fenced epoch", func(t *testing.T) {
		id := newTask(t, db, nil)
		if err := db.TransitionButlerTask(id, ButlerTaskRunning, "started", "butler"); err != nil {
			t.Fatal(err)
		}
		step, err := db.AppendButlerTaskStep(id, ButlerTaskStep{Kind: ButlerStepModel, Name: "answer"})
		if err != nil {
			t.Fatal(err)
		}
		first, err := db.StartButlerAttempt(step)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RecoverInterruptedButlerAttempts(); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetButlerTask(id)
		if err != nil || got.State != ButlerTaskRunning || got.LeaseEpoch != 1 {
			t.Fatalf("recovered model task = %+v, %v", got, err)
		}
		if err := db.FinishButlerAttempt(first.Seq, ButlerAttemptSucceeded, `{}`, ""); !errors.Is(err, ErrAttemptFenced) {
			t.Fatalf("late model result = %v, want ErrAttemptFenced", err)
		}
		retry, err := db.StartButlerAttempt(step)
		if err != nil || retry.Attempt != 2 || retry.Epoch != 1 {
			t.Fatalf("model retry = %+v, %v", retry, err)
		}
	})

	t.Run("external work waits instead of repeating", func(t *testing.T) {
		id := newTask(t, db, nil)
		if err := db.TransitionButlerTask(id, ButlerTaskRunning, "started", "butler"); err != nil {
			t.Fatal(err)
		}
		step, err := db.AppendButlerTaskStep(id, ButlerTaskStep{
			Kind: ButlerStepTool, Name: "synthetic.inert", ParamsJSON: `{"value":"fixture"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := db.StartButlerAttempt(step)
		if err != nil {
			t.Fatal(err)
		}

		// The relevant Keystone components restart over the same SQLite file.
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.AcquireCoordinator("keystone-a"); err != nil {
			t.Fatal(err)
		}
		if err := db.RecoverInterruptedButlerAttempts(); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetButlerTask(id)
		if err != nil || got.State != ButlerTaskWaiting || got.LeaseEpoch != 1 {
			t.Fatalf("recovered tool task = %+v, %v", got, err)
		}
		waits, err := db.ListButlerTaskWaits(id)
		if err != nil || len(waits) != 1 || waits[0].Reason != ButlerWaitUncertainOutcome {
			t.Fatalf("uncertain waits = %+v, %v", waits, err)
		}
		if err := db.FinishButlerAttempt(attempt.Seq, ButlerAttemptSucceeded, `{}`, ""); !errors.Is(err, ErrAttemptFenced) {
			t.Fatalf("late external result = %v, want ErrAttemptFenced", err)
		}
		// Startup recovery is idempotent and cannot create a second wait or
		// advance the task epoch repeatedly.
		if err := db.RecoverInterruptedButlerAttempts(); err != nil {
			t.Fatal(err)
		}
		again, _ := db.GetButlerTask(id)
		waits, _ = db.ListButlerTaskWaits(id)
		if again.LeaseEpoch != 1 || len(waits) != 1 {
			t.Fatalf("second recovery changed task=%+v waits=%+v", again, waits)
		}
	})
	db.Close()
}

func TestNativeTaskHistoryAndReceiptsSurviveRestoreAlongside(t *testing.T) {
	live, err := Open(migrated.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	record(t, live, "mika")
	if _, err := live.AcquireCoordinator("keystone-live"); err != nil {
		t.Fatal(err)
	}
	id := newTask(t, live, func(task *ButlerTask) {
		task.ObjectiveKind = ObjectiveEffectful
		task.Objective = "record one inert synthetic effect"
	})
	if err := live.TransitionButlerTask(id, ButlerTaskRunning, "started", "butler"); err != nil {
		t.Fatal(err)
	}
	step, err := live.AppendButlerTaskStep(id, ButlerTaskStep{Kind: ButlerStepTool, Name: "synthetic.inert", ParamsJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := live.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.FinishButlerAttempt(attempt.Seq, ButlerAttemptSucceeded, `{"v":1,"content":"ok","effectConfirmed":true}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := live.SetButlerStepState(step, ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	if err := live.ClassifyButlerObjectiveEffects(id, ObjectiveEffectful, false, []RequiredEffect{{Description: "inert effect", SourceKey: "step:" + itoa(step)}}); err != nil {
		t.Fatal(err)
	}
	receipt, err := live.RecordButlerEffectReceipt(id, EffectReceipt{
		Kind: "tool", SourceID: "synthetic.inert", Step: step, Outcome: "succeeded",
		PermissionLabel: "operator", Audience: ButlerAudiencePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := live.CompleteButlerTask(id, []ButlerTaskEvidence{{
		Kind: ButlerEvidenceToolResult, Description: "inert effect confirmed", SourceID: "synthetic.inert",
		PermissionLabel: "operator", Audience: ButlerAudiencePrivate, ReceiptID: receipt,
	}}, "done", "butler"); err != nil {
		t.Fatal(err)
	}
	// An identical retry resolves to the immutable receipt rather than
	// creating a second completed effect.
	duplicate, err := live.RecordButlerEffectReceipt(id, EffectReceipt{
		Kind: "tool", SourceID: "synthetic.inert", Step: step, Outcome: "succeeded",
		PermissionLabel: "operator", Audience: ButlerAudiencePrivate,
	})
	if err != nil || duplicate != receipt {
		t.Fatalf("duplicate receipt = %d, %v; want %d", duplicate, err, receipt)
	}

	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := live.BackupTo(snapshot); err != nil {
		t.Fatal(err)
	}
	pub, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	snapshotAt := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	fencedAt := time.Date(2099, 11, 2, 13, 0, 0, 0, time.UTC)
	journalEvidence, err := live.BuildRecoveryJournalEvidence(snapshot, "keystone-live", "independent test custody",
		snapshotAt, fencedAt, fencedAt.Add(time.Minute), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreBackup(snapshot, filepath.Join(t.TempDir(), "restored.db"), "synthetic restore alongside", time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	task, err := restored.GetButlerTask(id)
	if err != nil || task.State != ButlerTaskCompleted || task.ResultSummary != "done" {
		t.Fatalf("restored task = %+v, %v", task, err)
	}
	steps, _ := restored.ListButlerTaskSteps(id)
	attempts, _ := restored.ListButlerTaskAttempts(id)
	evidence, _ := restored.ListButlerTaskEvidence(id)
	if len(steps) != 1 || steps[0].State != ButlerStepVerified || len(attempts) != 1 ||
		attempts[0].State != ButlerAttemptSucceeded || len(evidence) != 1 || evidence[0].ReceiptID != receipt {
		t.Fatalf("restored history: steps=%+v attempts=%+v evidence=%+v", steps, attempts, evidence)
	}
	// Restore quarantine and the absent in-memory lease independently prevent
	// this alongside copy from dispatching against the live fixture.
	if err := restored.CheckHouseholdExecution(); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("restored copy execution = %v, want quarantine", err)
	}
	if _, err := restored.ReconcileRestoreEvidence(journalEvidence, pub, "mika", fencedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ReleaseRestoreQuarantine("mika", pub); err != nil {
		t.Fatal(err)
	}
	after, err := restored.GetButlerTask(id)
	if err != nil || after.State != ButlerTaskCompleted || after.ResultSummary != "done" {
		t.Fatalf("release changed completed task = %+v, %v", after, err)
	}
	afterAttempts, _ := restored.ListButlerTaskAttempts(id)
	afterEvidence, _ := restored.ListButlerTaskEvidence(id)
	if len(afterAttempts) != 1 || afterAttempts[0].State != ButlerAttemptSucceeded || len(afterEvidence) != 1 || afterEvidence[0].ReceiptID != receipt {
		t.Fatalf("release repeated completed effects: attempts=%+v evidence=%+v", afterAttempts, afterEvidence)
	}
	if err := restored.CheckHouseholdExecution(); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("release made completed work dispatchable = %v", err)
	}
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
