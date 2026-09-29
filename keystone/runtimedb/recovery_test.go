package runtimedb

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOnlineBackupAndRestoreStartsQuarantined(t *testing.T) {
	live, err := Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := live.AppendEvent(Event{Type: "recovery-test", Message: "committed WAL content"}); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "keystone.snapshot.db")
	if err := live.BackupTo(snapshot); err != nil {
		t.Fatal(err)
	}
	snapshotAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	restored, err := RestoreBackup(snapshot, filepath.Join(t.TempDir(), "keystone.db"), "whole-site recovery", snapshotAt)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	state, err := restored.GetRestoreState()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Quarantined || state.Reason != "whole-site recovery" || !state.SnapshotAt.Equal(snapshotAt) {
		t.Fatalf("restore state = %+v", state)
	}
	events, err := restored.EventsAfter(0, 10)
	if err != nil || len(events) != 1 || events[0].Message != "committed WAL content" {
		t.Fatalf("restored events = %+v, %v", events, err)
	}
}

func TestBackupRefusesOverwrite(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	destination := filepath.Join(t.TempDir(), "snapshot.db")
	if err := db.BackupTo(destination); err != nil {
		t.Fatal(err)
	}
	if err := db.BackupTo(destination); err == nil {
		t.Fatal("backup overwrite was accepted")
	}
}
