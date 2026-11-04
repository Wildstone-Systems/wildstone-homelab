package runtimedb

import (
	"sync"
	"testing"
	"time"
)

func TestBackupVerificationHistory(t *testing.T) {
	db, err := Open(migrated.Path(t)) // a copy of the template, not a fresh replay (#231)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	seq, err := db.StartBackupRun("photos", BackupRunCheck, "plan-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishBackupRun(seq, BackupRunFailed, "repository damaged"); err != nil {
		t.Fatal(err)
	}
	last, err := db.LastBackupRunAny("photos", BackupRunCheck)
	if err != nil {
		t.Fatal(err)
	}
	if last.Status != BackupRunFailed || last.Detail != "repository damaged" {
		t.Fatalf("last check = %+v", last)
	}

	older := time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	if err := db.RecordBackupSourceSample("photos", 1000, older); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordBackupSourceSample("photos", 500, newer); err != nil {
		t.Fatal(err)
	}
	samples, err := db.LatestBackupSourceSamples("photos", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || samples[0].Bytes != 500 || samples[1].Bytes != 1000 || !samples[0].MeasuredAt.Equal(newer) {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestBackupDependencyUnavailableDeduplicatesUntilConditionChanges(t *testing.T) {
	db, err := Open(migrated.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	detail := "backup dependency unavailable: password secret(s) secret/restic-photos are not present on node n1"
	first, inserted, err := db.RecordBackupDependencyUnavailable("photos", BackupRunBackup, detail)
	if err != nil || !inserted {
		t.Fatalf("first record: seq=%d inserted=%v err=%v", first, inserted, err)
	}
	second, inserted, err := db.RecordBackupDependencyUnavailable("photos", BackupRunBackup, detail)
	if err != nil || inserted || second != first {
		t.Fatalf("duplicate record: first=%d second=%d inserted=%v err=%v", first, second, inserted, err)
	}

	run, err := db.StartBackupRun("photos", BackupRunBackup, "plan-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishBackupRun(run, BackupRunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	third, inserted, err := db.RecordBackupDependencyUnavailable("photos", BackupRunBackup, detail)
	if err != nil || !inserted || third == first {
		t.Fatalf("new condition record: first=%d third=%d inserted=%v err=%v", first, third, inserted, err)
	}
}

func TestConcurrentBackupDependencyEvaluationCreatesOneRow(t *testing.T) {
	db, err := Open(migrated.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	detail := "backup dependency unavailable: cannot confirm password secrets on node n1"
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := db.RecordBackupDependencyUnavailable("photos", BackupRunBackup, detail)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	runs, err := db.ListBackupRuns(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != BackupRunDependencyUnavailable {
		t.Fatalf("concurrent records = %+v", runs)
	}
}
