package automation

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// TestPostPromotionRecoveryAndExecutionFencing is the repository-level #287
// fixture. It follows the supported snapshot, signed-journal, release,
// promotion, close/reopen, acquisition, and startup-recovery path. Providers
// are never invoked: task and routine interruptions stop at their durable
// checkpoint-before-dispatch APIs.
func TestPostPromotionRecoveryAndExecutionFencing(t *testing.T) {
	livePath := filepath.Join(t.TempDir(), "live.db")
	live, err := runtimedb.Open(livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := live.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := live.AdmitButlerMember("mika", "synthetic-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := live.CreatePrincipal("revoked-operator", runtimedb.RoleOperator, ""); err != nil {
		t.Fatal(err)
	}
	if err := live.InsertToken("revoked-token", "revoked-operator", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	oldLease, err := live.AcquireCoordinator("keystone-before-recovery")
	if err != nil {
		t.Fatal(err)
	}

	completed, completedStep, receipt := completedNativeEffect(t, live)
	modelTask, modelAttempt := interruptedNativeAttempt(t, live, runtimedb.ButlerStepModel, "synthetic.model")
	toolTask, toolAttempt := interruptedNativeAttempt(t, live, runtimedb.ButlerStepDelivery, "synthetic.delivery")
	plan, completedRun, claimedRun := reviewedRoutineFixture(t, live)

	snapshotAt := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := live.BackupTo(snapshot); err != nil {
		t.Fatal(err)
	}
	// This revocation is deliberately newer than the snapshot. The signed
	// independent journal, not the restored database's own opinion, carries it.
	if err := live.Journal("link", "api-tokens:revoked-operator", "revoked", "mika", "post-snapshot revocation"); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fencedAt := time.Date(2099, 11, 3, 11, 0, 0, 0, time.UTC)
	evidence, err := live.BuildRecoveryJournalEvidence(snapshot, oldLease.Holder, "offline synthetic custody",
		snapshotAt, fencedAt, fencedAt.Add(time.Minute), privateKey)
	if err != nil {
		t.Fatal(err)
	}

	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	restored, err := runtimedb.RestoreBackup(snapshot, restoredPath, "#287 synthetic recovery", snapshotAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.AcquireCoordinator("keystone-after-recovery"); !errors.Is(err, runtimedb.ErrCoordinatorRestoreFenced) {
		t.Fatalf("restart before reconciliation acquired execution: %v", err)
	}
	if _, err := restored.ReconcileRestoreEvidence(evidence, publicKey, "mika", fencedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := restored.LiveTokensFor("revoked-operator"); err != nil || n != 0 {
		t.Fatalf("post-snapshot revocation replay = %d live tokens, %v", n, err)
	}
	if _, err := restored.AcquireCoordinator("keystone-after-recovery"); !errors.Is(err, runtimedb.ErrCoordinatorRestoreFenced) {
		t.Fatalf("restart before quarantine release acquired execution: %v", err)
	}
	if _, err := restored.ReleaseRestoreQuarantine("mika", publicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.AcquireCoordinator("keystone-after-recovery"); !errors.Is(err, runtimedb.ErrCoordinatorRestoreFenced) {
		t.Fatalf("restart before promotion acquired execution: %v", err)
	}
	promotion, err := restored.PromoteRestoredCoordinator("keystone-after-recovery", "mika", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if promotion.Promoted.Epoch != oldLease.Epoch+1 || promotion.Previous != oldLease {
		t.Fatalf("promotion = %+v, old lease = %+v", promotion, oldLease)
	}
	if err := restored.CheckHouseholdExecution(); !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("promotion without restart granted execution: %v", err)
	}
	if _, err := restored.AcquireCoordinator(promotion.Promoted.Holder); !errors.Is(err, runtimedb.ErrCoordinatorRestoreFenced) {
		t.Fatalf("promoting process acquired execution without restart: %v", err)
	}
	if _, err := restored.PromoteRestoredCoordinator("another-holder", "mika", publicKey); err == nil {
		t.Fatal("repeated promotion advanced the epoch")
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}

	// A restarted process with any identity other than the exact promoted one
	// remains fenced and cannot turn an old identity into a new lease.
	mismatched, err := runtimedb.Open(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatched.AcquireCoordinator(oldLease.Holder); !errors.Is(err, runtimedb.ErrCoordinatorRestoreFenced) {
		t.Fatalf("stale holder acquired promoted restore: %v", err)
	}
	if err := mismatched.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := runtimedb.Open(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	lease, err := restarted.AcquireCoordinator(promotion.Promoted.Holder)
	if err != nil || lease.Holder != promotion.Promoted.Holder || lease.Epoch != promotion.Promoted.Epoch {
		t.Fatalf("post-restart acquisition = %+v, %v; want %+v", lease, err, promotion.Promoted)
	}
	if _, err := restarted.RecoverWorkspaceWrites(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverCoordinatorStartup(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.CheckHouseholdExecution(); err != nil {
		t.Fatalf("recovered coordinator not eligible: %v", err)
	}

	assertCompletedNativeEffect(t, restarted, completed, completedStep, receipt)
	assertRecoveredNativeAttempt(t, restarted, modelTask, modelAttempt, false)
	assertRecoveredNativeAttempt(t, restarted, toolTask, toolAttempt, true)
	if err := restarted.FinishButlerAttempt(toolAttempt.Seq, runtimedb.ButlerAttemptSucceeded, `{}`, ""); !errors.Is(err, runtimedb.ErrAttemptFenced) {
		t.Fatalf("stale tool result committed: %v", err)
	}
	if err := restarted.CheckCoordinator(oldLease.Holder, oldLease.Epoch); !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("stale coordinator remained eligible: %v", err)
	}
	completedAutomation, err := restarted.GetRoutineRun(completedRun.ID)
	if err != nil || completedAutomation.State != "completed" || completedAutomation.EffectState != "confirmed" {
		t.Fatalf("completed automation changed: %+v, %v", completedAutomation, err)
	}
	uncertainAutomation, err := restarted.GetRoutineRun(claimedRun.ID)
	if err != nil || uncertainAutomation.State != "uncertain" || uncertainAutomation.EffectState != "uncertain" {
		t.Fatalf("claimed automation was not held: %+v, %v", uncertainAutomation, err)
	}
	if err := restarted.RecordRoutineEffect(claimedRun.ID, "confirmed", "stale completion"); !errors.Is(err, runtimedb.ErrRoutineDenied) {
		t.Fatalf("stale automation completion committed: %v", err)
	}
	versions, err := restarted.ApprovedRoutineVersions()
	if err != nil || len(versions) != 1 || versions[0].PlanID != plan {
		t.Fatalf("reviewed automation authority = %+v, %v", versions, err)
	}
	// Recovery is repeatable and does not add waits, receipts, or attempts.
	if err := restarted.RecoverCoordinatorStartup(); err != nil {
		t.Fatal(err)
	}
	assertCompletedNativeEffect(t, restarted, completed, completedStep, receipt)
	assertRecoveredNativeAttempt(t, restarted, toolTask, toolAttempt, true)
}

func completedNativeEffect(t *testing.T, db *runtimedb.DB) (string, int64, int64) {
	t.Helper()
	id, err := db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation,
		Author: "mika", Owner: "mika", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
		Audience: runtimedb.ButlerAudiencePrivate, Objective: "record one inert synthetic effect", ObjectiveKind: runtimedb.ObjectiveEffectful})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskRunning, "started", "synthetic-agent"); err != nil {
		t.Fatal(err)
	}
	step, err := db.AppendButlerTaskStep(id, runtimedb.ButlerTaskStep{Kind: runtimedb.ButlerStepTool, Name: "synthetic.inert", ParamsJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishButlerAttempt(attempt.Seq, runtimedb.ButlerAttemptSucceeded, `{"v":1,"content":"ok","effectConfirmed":true}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerStepState(step, runtimedb.ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	if err := db.ClassifyButlerObjectiveEffects(id, runtimedb.ObjectiveEffectful, false,
		[]runtimedb.RequiredEffect{{Description: "inert effect", SourceKey: "step:" + fmtInt(step)}}); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.RecordButlerEffectReceipt(id, runtimedb.EffectReceipt{Kind: "tool", SourceID: "synthetic.inert", Step: step,
		Outcome: "succeeded", PermissionLabel: "operator", Audience: runtimedb.ButlerAudiencePrivate})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteButlerTask(id, []runtimedb.ButlerTaskEvidence{{Kind: runtimedb.ButlerEvidenceToolResult,
		Description: "inert effect confirmed", SourceID: "synthetic.inert", PermissionLabel: "operator",
		Audience: runtimedb.ButlerAudiencePrivate, ReceiptID: receipt}}, "done", "synthetic-agent"); err != nil {
		t.Fatal(err)
	}
	return id, step, receipt
}

func interruptedNativeAttempt(t *testing.T, db *runtimedb.DB, kind runtimedb.ButlerStepKind, name string) (string, runtimedb.ButlerTaskAttempt) {
	t.Helper()
	id, err := db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation,
		Author: "mika", Owner: "mika", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
		Audience: runtimedb.ButlerAudiencePrivate, Objective: "synthetic interrupted work", ObjectiveKind: runtimedb.ObjectiveInformational})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskRunning, "started", "synthetic-agent"); err != nil {
		t.Fatal(err)
	}
	step, err := db.AppendButlerTaskStep(id, runtimedb.ButlerTaskStep{Kind: kind, Name: name, ParamsJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	return id, attempt
}

func reviewedRoutineFixture(t *testing.T, db *runtimedb.DB) (string, runtimedb.RoutineRun, runtimedb.RoutineRun) {
	t.Helper()
	r := rule("post-promotion-recovery")
	r.Automation.Escalation = nil
	r.Automation.Action.Tool = "chat-test.send"
	r.Automation.Action.Params = map[string]string{"member": "mika", "message": "synthetic inert notice"}
	raw, err := resource.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDraft(string(raw)); err != nil {
		t.Fatalf("invalid deterministic automation fixture: %v", err)
	}
	if _, err := db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: r.Metadata.Name, Ownership: runtimedb.RoutineWildstone,
		Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	plan, err := db.SubmitRoutinePlan(r.Metadata.Name, "mika", "", runtimedb.AudienceHousehold, "local inert action", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(plan, "mika"); err != nil {
		t.Fatal(err)
	}
	event1, _, err := db.AppendHouseholdEvent(runtimedb.HouseholdEvent{EventID: "completed-effect", Source: "synthetic",
		Type: "person.left_home", OccurredAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), Subject: "mika"})
	if err != nil {
		t.Fatal(err)
	}
	completed, _, err := db.ClaimRoutineRun(plan, event1)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimRoutineEffect(completed.ID); err != nil || !claimed {
		t.Fatalf("claim completed routine effect = %v, %v", claimed, err)
	}
	if err := db.RecordRoutineEffect(completed.ID, "confirmed", "synthetic receipt"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRoutineRunState(completed.ID, "completed", "synthetic complete"); err != nil {
		t.Fatal(err)
	}
	event2, _, err := db.AppendHouseholdEvent(runtimedb.HouseholdEvent{EventID: "claimed-effect", Source: "synthetic",
		Type: "person.left_home", OccurredAt: time.Date(2026, 11, 3, 9, 1, 0, 0, time.UTC), Subject: "mika"})
	if err != nil {
		t.Fatal(err)
	}
	claimedRun, _, err := db.ClaimRoutineRun(plan, event2)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimRoutineEffect(claimedRun.ID); err != nil || !claimed {
		t.Fatalf("claim interrupted routine effect = %v, %v", claimed, err)
	}
	return plan, completed, claimedRun
}

func assertCompletedNativeEffect(t *testing.T, db *runtimedb.DB, id string, step, receipt int64) {
	t.Helper()
	task, err := db.GetButlerTask(id)
	if err != nil || task.State != runtimedb.ButlerTaskCompleted || task.ResultSummary != "done" {
		t.Fatalf("completed task = %+v, %v", task, err)
	}
	duplicate, err := db.RecordButlerEffectReceipt(id, runtimedb.EffectReceipt{Kind: "tool", SourceID: "synthetic.inert", Step: step,
		Outcome: "succeeded", PermissionLabel: "operator", Audience: runtimedb.ButlerAudiencePrivate})
	if err != nil || duplicate != receipt {
		t.Fatalf("duplicate completion receipt = %d, %v; want immutable %d", duplicate, err, receipt)
	}
}

func assertRecoveredNativeAttempt(t *testing.T, db *runtimedb.DB, id string, attempt runtimedb.ButlerTaskAttempt, uncertain bool) {
	t.Helper()
	attempts, err := db.ListButlerTaskAttempts(id)
	if err != nil || len(attempts) != 1 || attempts[0].Seq != attempt.Seq || attempts[0].State != runtimedb.ButlerAttemptFenced {
		t.Fatalf("recovered attempts for %s = %+v, %v", id, attempts, err)
	}
	waits, err := db.ListButlerTaskWaits(id)
	if err != nil {
		t.Fatal(err)
	}
	if uncertain && (len(waits) != 1 || waits[0].Reason != runtimedb.ButlerWaitUncertainOutcome) {
		t.Fatalf("uncertain waits for %s = %+v", id, waits)
	}
	if !uncertain && len(waits) != 0 {
		t.Fatalf("model retry was incorrectly held: %+v", waits)
	}
}

func fmtInt(n int64) string {
	return fmt.Sprintf("%d", n)
}
