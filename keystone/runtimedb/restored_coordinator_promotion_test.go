package runtimedb

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoredCoordinatorPromotionMigrationPreservesLeaseAndFencesIncompleteRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := openThrough(t, path, "migrations/0117_journal_gated_quarantine_release.sql")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stamp := now()
	if _, err := db.sql.Exec(`INSERT INTO coordinator_lease(id,holder,epoch,acquired_at,heartbeat_at,promoted_from)
		VALUES(1,'keystone-original',7,?,?,'keystone-before')`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO restore_state(id,quarantined,entered_at,reason,snapshot_at,restore_generation)
		VALUES(1,1,?,'migration fixture',?,'restore-generation')`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	state, err := db.GetRestoreState()
	if err != nil || state.RestoreGeneration != "restore-generation" || !state.CoordinatorPromotedAt.IsZero() ||
		state.CoordinatorPromotedBy != "" || state.CoordinatorHolder != "" || state.CoordinatorEpoch != 0 {
		t.Fatalf("migrated restore state = %+v, %v", state, err)
	}
	lease, err := db.GetCoordinator()
	if err != nil || lease.Holder != "keystone-original" || lease.Epoch != 7 || lease.PromotedFrom != "keystone-before" {
		t.Fatalf("migrated lease = %+v, %v", lease, err)
	}
	if _, err := db.PromoteCoordinator("keystone-restored"); err == nil {
		t.Fatal("migration left ordinary promotion open on an incomplete restore")
	}
}

func promotableRestore(t *testing.T) (*recoveryJournalFixture, *DB, CoordinatorLease) {
	t.Helper()
	f := newRecoveryJournalFixture(t)
	old, err := f.live.AcquireCoordinator("keystone-original")
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot(t)
	restored := f.restore(t)
	if _, err := restored.ReconcileRestoreEvidence(f.evidence(t), f.publicKey, "mika", journalVerifiedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ReleaseRestoreQuarantine("mika", f.publicKey); err != nil {
		t.Fatal(err)
	}
	return f, restored, old
}

func tableCounts(t *testing.T, db *DB, tables ...string) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := db.sql.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

func TestRestoredCoordinatorPromotionIsDurableAtomicAndRestartGated(t *testing.T) {
	f, restored, old := promotableRestore(t)
	tables := []string{"butler_task_attempts", "butler_effect_receipts", "agent_subscription_runs", "mail_candidates", "integration_work"}
	before := tableCounts(t, restored, tables...)

	promotion, err := restored.PromoteRestoredCoordinator("keystone-restored", "mika", f.publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if promotion.Previous.Holder != old.Holder || promotion.Previous.Epoch != old.Epoch ||
		promotion.Promoted.Holder != "keystone-restored" || promotion.Promoted.Epoch != old.Epoch+1 ||
		promotion.PromotedBy != "mika" || promotion.RestoreGeneration == "" || promotion.EvidenceID == "" {
		t.Fatalf("promotion = %+v", promotion)
	}
	if err := restored.CheckCoordinator(old.Holder, old.Epoch); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("old holder still authorized: %v", err)
	}
	if err := restored.CheckHouseholdExecution(); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("promotion granted immediate execution: %v", err)
	}
	if _, err := restored.AcquireCoordinator("keystone-restored"); !errors.Is(err, ErrCoordinatorRestoreFenced) {
		t.Fatalf("same process acquired promoted epoch without restart: %v", err)
	}
	if after := tableCounts(t, restored, tables...); !equalCounts(before, after) {
		t.Fatalf("promotion dispatched work: before=%v after=%v", before, after)
	}
	state, err := restored.GetRestoreState()
	if err != nil || state.CoordinatorPromotedAt.IsZero() || state.CoordinatorPromotedBy != "mika" ||
		state.CoordinatorHolder != promotion.Promoted.Holder || state.CoordinatorEpoch != promotion.Promoted.Epoch {
		t.Fatalf("promotion projection = %+v, %v", state, err)
	}
	var previousHolder, promotedHolder, promotedBy string
	var previousEpoch, promotedEpoch int64
	if err := restored.sql.QueryRow(`SELECT previous_holder,previous_epoch,promoted_holder,promoted_epoch,promoted_by
		FROM recovery_coordinator_promotions WHERE restore_generation=?`, promotion.RestoreGeneration).
		Scan(&previousHolder, &previousEpoch, &promotedHolder, &promotedEpoch, &promotedBy); err != nil {
		t.Fatal(err)
	}
	if previousHolder != old.Holder || previousEpoch != old.Epoch || promotedHolder != promotion.Promoted.Holder ||
		promotedEpoch != promotion.Promoted.Epoch || promotedBy != "mika" {
		t.Fatalf("promotion audit = %s/%d -> %s/%d by %s", previousHolder, previousEpoch, promotedHolder, promotedEpoch, promotedBy)
	}
	if _, err := restored.PromoteRestoredCoordinator("another", "mika", f.publicKey); err == nil {
		t.Fatal("repeated promotion succeeded")
	}

	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	lease, err := reopened.GetCoordinator()
	if err != nil || lease.Holder != promotion.Promoted.Holder || lease.Epoch != promotion.Promoted.Epoch {
		t.Fatalf("persisted lease = %+v, %v", lease, err)
	}
	if err := reopened.CheckHouseholdExecution(); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("database reopen alone granted execution: %v", err)
	}
	acquired, err := reopened.AcquireCoordinator("keystone-restored")
	if err != nil || acquired.Epoch != promotion.Promoted.Epoch {
		t.Fatalf("controlled acquisition = %+v, %v", acquired, err)
	}
	if err := reopened.CheckHouseholdExecution(); err != nil {
		t.Fatalf("promoted epoch remained fenced after controlled acquisition: %v", err)
	}
	if recovered, err := reopened.RecoverWorkspaceWrites(); err != nil || recovered != 0 {
		t.Fatalf("startup workspace recovery after controlled acquisition = %d, %v", recovered, err)
	}
	if after := tableCounts(t, reopened, tables...); !equalCounts(before, after) {
		t.Fatalf("acquisition dispatched work: before=%v after=%v", before, after)
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func TestRestoredCoordinatorPromotionRefusesUnreleasedMissingAndMismatchedEvidence(t *testing.T) {
	t.Run("quarantine active", func(t *testing.T) {
		f := newRecoveryJournalFixture(t)
		if _, err := f.live.AcquireCoordinator("keystone-original"); err != nil {
			t.Fatal(err)
		}
		f.snapshot(t)
		restored := f.restore(t)
		if _, err := restored.ReconcileRestoreEvidence(f.evidence(t), f.publicKey, "mika", journalVerifiedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := restored.PromoteRestoredCoordinator("keystone-restored", "mika", f.publicKey); err == nil ||
			!strings.Contains(err.Error(), "explicit quarantine release") {
			t.Fatalf("promotion before release = %v", err)
		}
	})

	t.Run("missing evidence", func(t *testing.T) {
		f := newRecoveryJournalFixture(t)
		if _, err := f.live.AcquireCoordinator("keystone-original"); err != nil {
			t.Fatal(err)
		}
		f.snapshot(t)
		restored := f.restore(t)
		if _, err := restored.PromoteRestoredCoordinator("keystone-restored", "mika", f.publicKey); err == nil ||
			!strings.Contains(err.Error(), "verified journal evidence") {
			t.Fatalf("promotion without evidence = %v", err)
		}
	})

	t.Run("mismatched current restore", func(t *testing.T) {
		f, restored, old := promotableRestore(t)
		if _, err := restored.sql.Exec(`UPDATE restore_state SET snapshot_sha256=? WHERE id=1`, strings.Repeat("0", 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := restored.PromoteRestoredCoordinator("keystone-restored", "mika", f.publicKey); err == nil ||
			!strings.Contains(err.Error(), "current restore generation") {
			t.Fatalf("promotion with mismatched evidence = %v", err)
		}
		lease, err := restored.GetCoordinator()
		if err != nil || lease.Holder != old.Holder || lease.Epoch != old.Epoch {
			t.Fatalf("refused promotion changed lease: %+v, %v", lease, err)
		}
	})

	t.Run("changed recovery trust root", func(t *testing.T) {
		_, restored, old := promotableRestore(t)
		changed, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := restored.PromoteRestoredCoordinator("keystone-restored", "mika", changed); err == nil ||
			!strings.Contains(err.Error(), "current trust root") {
			t.Fatalf("promotion with changed trust root = %v", err)
		}
		lease, err := restored.GetCoordinator()
		if err != nil || lease.Holder != old.Holder || lease.Epoch != old.Epoch {
			t.Fatalf("untrusted promotion changed lease: %+v, %v", lease, err)
		}
	})
}

func TestOrdinaryPromotionCannotBypassRestoredCoordinatorAuthorization(t *testing.T) {
	f, restored, old := promotableRestore(t)
	if _, err := restored.PromoteCoordinator("keystone-restored"); err == nil ||
		!strings.Contains(err.Error(), "audited Owner promotion") {
		t.Fatalf("ordinary promotion bypass = %v", err)
	}
	lease, err := restored.GetCoordinator()
	if err != nil || lease.Holder != old.Holder || lease.Epoch != old.Epoch {
		t.Fatalf("bypass attempt changed lease: %+v, %v", lease, err)
	}
	if _, err := restored.PromoteRestoredCoordinator("keystone-restored", "mika", f.publicKey); err != nil {
		t.Fatalf("authorized path failed after bypass refusal: %v", err)
	}
}

func TestRestoredCoordinatorPromotionPreservesReceiptsAndHoldsUncertainEffects(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	if err := f.live.RecordButlerMember("mika", RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.live.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.live.AcquireCoordinator("keystone-original"); err != nil {
		t.Fatal(err)
	}

	completed := newTask(t, f.live, func(task *ButlerTask) {
		task.ObjectiveKind = ObjectiveEffectful
		task.Objective = "record one inert synthetic effect"
	})
	if err := f.live.TransitionButlerTask(completed, ButlerTaskRunning, "started", "butler"); err != nil {
		t.Fatal(err)
	}
	completedStep, err := f.live.AppendButlerTaskStep(completed, ButlerTaskStep{Kind: ButlerStepTool, Name: "synthetic.inert", ParamsJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	completedAttempt, err := f.live.StartButlerAttempt(completedStep)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.live.FinishButlerAttempt(completedAttempt.Seq, ButlerAttemptSucceeded, `{"v":1,"content":"ok","effectConfirmed":true}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.live.SetButlerStepState(completedStep, ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	if err := f.live.ClassifyButlerObjectiveEffects(completed, ObjectiveEffectful, false,
		[]RequiredEffect{{Description: "inert effect", SourceKey: "step:" + itoa(completedStep)}}); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.live.RecordButlerEffectReceipt(completed, EffectReceipt{
		Kind: "tool", SourceID: "synthetic.inert", Step: completedStep, Outcome: "succeeded",
		PermissionLabel: "operator", Audience: ButlerAudiencePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.live.CompleteButlerTask(completed, []ButlerTaskEvidence{{
		Kind: ButlerEvidenceToolResult, Description: "inert effect confirmed", SourceID: "synthetic.inert",
		PermissionLabel: "operator", Audience: ButlerAudiencePrivate, ReceiptID: receipt,
	}}, "done", "butler"); err != nil {
		t.Fatal(err)
	}

	uncertain := newTask(t, f.live, nil)
	if err := f.live.TransitionButlerTask(uncertain, ButlerTaskRunning, "started", "butler"); err != nil {
		t.Fatal(err)
	}
	uncertainStep, err := f.live.AppendButlerTaskStep(uncertain, ButlerTaskStep{Kind: ButlerStepTool, Name: "synthetic.uncertain", ParamsJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.live.StartButlerAttempt(uncertainStep); err != nil {
		t.Fatal(err)
	}

	f.snapshot(t)
	restored := f.restore(t)
	if _, err := restored.ReconcileRestoreEvidence(f.evidence(t), f.publicKey, "mika", journalVerifiedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ReleaseRestoreQuarantine("mika", f.publicKey); err != nil {
		t.Fatal(err)
	}
	receiptBefore := receiptProjection(t, restored, receipt)
	completedBefore, err := restored.GetButlerTask(completed)
	if err != nil || completedBefore.State != ButlerTaskCompleted {
		t.Fatalf("completed task before promotion = %+v, %v", completedBefore, err)
	}
	uncertainBefore, err := restored.GetButlerTask(uncertain)
	if err != nil || uncertainBefore.State != ButlerTaskWaiting {
		t.Fatalf("uncertain task before promotion = %+v, %v", uncertainBefore, err)
	}
	waits, err := restored.ListButlerTaskWaits(uncertain)
	if err != nil || len(waits) != 1 || waits[0].Reason != ButlerWaitUncertainOutcome {
		t.Fatalf("uncertain task wait before promotion = %+v, %v", waits, err)
	}

	if _, err := restored.PromoteRestoredCoordinator("keystone-restored", "mika", f.publicKey); err != nil {
		t.Fatal(err)
	}
	completedAfter, _ := restored.GetButlerTask(completed)
	uncertainAfter, _ := restored.GetButlerTask(uncertain)
	if completedAfter.State != ButlerTaskCompleted || completedAfter.ResultSummary != completedBefore.ResultSummary {
		t.Fatalf("promotion changed completed task: before=%+v after=%+v", completedBefore, completedAfter)
	}
	if got := receiptProjection(t, restored, receipt); got != receiptBefore {
		t.Fatalf("promotion changed immutable receipt: before=%q after=%q", receiptBefore, got)
	}
	if uncertainAfter.State != uncertainBefore.State || uncertainAfter.LeaseEpoch != uncertainBefore.LeaseEpoch {
		t.Fatalf("promotion changed uncertain work: before=%+v after=%+v", uncertainBefore, uncertainAfter)
	}
	if err := restored.CheckHouseholdExecution(); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("uncertain effect was no longer held: %v", err)
	}
}

func receiptProjection(t *testing.T, db *DB, receipt int64) string {
	t.Helper()
	var recordedAt, kind, sourceID, outcome, permission, audience string
	var task, step int64
	var proposal bool
	if err := db.sql.QueryRow(`SELECT task,recorded_at,kind,source_id,COALESCE(step,0),outcome,permission_label,audience,proposal_only
		FROM butler_effect_receipts WHERE seq=?`, receipt).
		Scan(&task, &recordedAt, &kind, &sourceID, &step, &outcome, &permission, &audience, &proposal); err != nil {
		t.Fatal(err)
	}
	return strings.Join([]string{itoa(task), recordedAt, kind, sourceID, itoa(step), outcome, permission, audience,
		map[bool]string{false: "false", true: "true"}[proposal]}, "|")
}
