package report

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func testDB(t *testing.T) *runtimedb.DB {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMorningEmptySystem(t *testing.T) {
	rep, err := Morning(testDB(t), time.Now())
	if err != nil {
		t.Fatalf("Morning: %v", err)
	}
	if len(rep.Unhealthy) != 0 || len(rep.PendingPlans) != 0 || rep.EventTotal24h != 0 {
		t.Fatalf("empty system reported findings: %+v", rep)
	}
	if !rep.BackupsOK {
		t.Fatalf("no backup runs must not read as failing")
	}
	for _, want := range []string{"all observed resources healthy", "0 plans awaiting approval", "no runs recorded"} {
		if !strings.Contains(rep.Summary, want) {
			t.Fatalf("summary %q missing %q", rep.Summary, want)
		}
	}
}

func TestMorningCollectsFindings(t *testing.T) {
	db := testDB(t)

	// One unhealthy + one healthy observation.
	for _, o := range []runtimedb.Observed{
		{Kind: "Container", Name: "immich-postgres", Healthy: false, Detail: "exited"},
		{Kind: "Route", Name: "photos", Healthy: true},
	} {
		if err := db.UpsertObserved(o); err != nil {
			t.Fatalf("UpsertObserved: %v", err)
		}
	}

	// One pending plan, one approved (only the pending one reports).
	p1, err := db.InsertPlan("butler", "op", "fix photos", "medium", "{}")
	if err != nil {
		t.Fatalf("InsertPlan: %v", err)
	}
	p2, err := db.InsertPlan("root-admin", "", "done thing", "low", "{}")
	if err != nil {
		t.Fatalf("InsertPlan: %v", err)
	}
	if err := db.TransitionPlan(p2, runtimedb.PlanApproved, "root-admin"); err != nil {
		t.Fatalf("TransitionPlan: %v", err)
	}

	// Events within the window.
	for i := 0; i < 3; i++ {
		if _, err := db.AppendEvent(runtimedb.Event{Type: "reconcile.applied"}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// Backup ledger: photos' LATEST run failed (older success must not
	// mask it); documents' latest succeeded.
	seq, err := db.StartBackupRun("photos-backup", runtimedb.BackupRunBackup, "")
	if err != nil {
		t.Fatalf("StartBackupRun: %v", err)
	}
	if err := db.FinishBackupRun(seq, runtimedb.BackupRunSucceeded, "ok"); err != nil {
		t.Fatalf("FinishBackupRun: %v", err)
	}
	seq, _ = db.StartBackupRun("photos-backup", runtimedb.BackupRunBackup, "")
	if err := db.FinishBackupRun(seq, runtimedb.BackupRunFailed, "repo locked"); err != nil {
		t.Fatalf("FinishBackupRun: %v", err)
	}
	seq, _ = db.StartBackupRun("documents-backup", runtimedb.BackupRunBackup, "")
	if err := db.FinishBackupRun(seq, runtimedb.BackupRunSucceeded, "ok"); err != nil {
		t.Fatalf("FinishBackupRun: %v", err)
	}

	rep, err := Morning(db, time.Now())
	if err != nil {
		t.Fatalf("Morning: %v", err)
	}
	if len(rep.Unhealthy) != 1 || rep.Unhealthy[0].Name != "immich-postgres" {
		t.Fatalf("unhealthy = %+v", rep.Unhealthy)
	}
	if len(rep.PendingPlans) != 1 || rep.PendingPlans[0].ID != p1 {
		t.Fatalf("pending = %+v", rep.PendingPlans)
	}
	if rep.EventTotal24h < 3 || rep.EventCounts24h["reconcile.applied"] != 3 {
		t.Fatalf("events = %d %v", rep.EventTotal24h, rep.EventCounts24h)
	}
	if rep.BackupsOK {
		t.Fatalf("failed latest photos-backup run not flagged: %+v", rep.Backups)
	}
	if len(rep.Backups) != 2 || rep.Backups[0].PlanName != "documents-backup" || rep.Backups[1].Status != runtimedb.BackupRunFailed {
		t.Fatalf("backup verdicts = %+v", rep.Backups)
	}
	for _, want := range []string{"1 unhealthy", "Container immich-postgres", "1 plans awaiting approval", "backups: FAILING for photos-backup"} {
		if !strings.Contains(rep.Summary, want) {
			t.Fatalf("summary %q missing %q", rep.Summary, want)
		}
	}
}
