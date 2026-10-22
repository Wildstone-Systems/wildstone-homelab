package report

import (
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// migrated is the schema, built once for this whole test binary rather
// than replayed into every database these tests open (#231). Each test
// still gets its own file.
var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

func testDB(t *testing.T) *runtimedb.DB {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
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

func TestMorningFlagsStaleOffsiteFailedVerificationAndSourceShrink(t *testing.T) {
	db := testDB(t)
	started := time.Now().UTC()
	finish := func(kind runtimedb.BackupRunKind, status runtimedb.BackupRunStatus, detail string) {
		t.Helper()
		seq, err := db.StartBackupRun("photos", kind, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishBackupRun(seq, status, detail); err != nil {
			t.Fatal(err)
		}
	}
	finish(runtimedb.BackupRunOffsite, runtimedb.BackupRunSucceeded, "")
	finish(runtimedb.BackupRunOffsiteCheck, runtimedb.BackupRunFailed, "repository damaged")
	finish(runtimedb.BackupRunOffsiteDrill, runtimedb.BackupRunFailed, "restore failed")
	if err := db.RecordBackupSourceSample("photos", 1000, started); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordBackupSourceSample("photos", 500, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan := resource.Resource{
		Kind: resource.KindBackupPlan, Metadata: resource.Metadata{Name: "photos"},
		BackupPlan: &resource.BackupPlanSpec{Offsite: &resource.BackupOffsite{Schedule: "daily"}},
	}

	rep, err := Morning(db, started.Add(48*time.Hour), plan)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BackupsOK || len(rep.BackupAlerts) != 4 {
		t.Fatalf("backup health = %v alerts = %+v", rep.BackupsOK, rep.BackupAlerts)
	}
	kinds := map[string]bool{}
	for _, alert := range rep.BackupAlerts {
		kinds[alert.Kind] = true
	}
	for _, want := range []string{"stale-offsite", "failed-check", "failed-drill", "source-shrink"} {
		if !kinds[want] {
			t.Errorf("alerts %+v missing %s", rep.BackupAlerts, want)
		}
	}
	for _, want := range []string{"photos/stale-offsite", "photos/failed-check", "photos/failed-drill", "photos/source-shrink"} {
		if !strings.Contains(rep.Summary, want) {
			t.Errorf("summary %q missing %q", rep.Summary, want)
		}
	}
}

func TestMorningKeepsFailedUploadVisibleAfterSuccessfulCheck(t *testing.T) {
	db := testDB(t)
	finish := func(kind runtimedb.BackupRunKind, status runtimedb.BackupRunStatus) {
		t.Helper()
		seq, err := db.StartBackupRun("photos", kind, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishBackupRun(seq, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	finish(runtimedb.BackupRunOffsite, runtimedb.BackupRunFailed)
	finish(runtimedb.BackupRunOffsiteCheck, runtimedb.BackupRunSucceeded)
	plan := resource.Resource{
		Kind: resource.KindBackupPlan, Metadata: resource.Metadata{Name: "photos"},
		BackupPlan: &resource.BackupPlanSpec{Offsite: &resource.BackupOffsite{Schedule: "daily"}},
	}
	rep, err := Morning(db, time.Now(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BackupsOK {
		t.Fatal("successful repository check hid the failed off-site upload")
	}
	found := false
	for _, alert := range rep.BackupAlerts {
		if alert.Kind == "failed-backup" && alert.RunKind == runtimedb.BackupRunOffsite {
			found = true
		}
	}
	if !found {
		t.Fatalf("alerts = %+v; missing failed off-site backup", rep.BackupAlerts)
	}
}
