package runtimedb

import (
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
