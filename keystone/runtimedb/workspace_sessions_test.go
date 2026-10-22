package runtimedb

import (
	"errors"
	"strings"
	"testing"
)

func TestWorkspaceWriteDurabilityAndAtomicReceipt(t *testing.T) {
	db, w, task := workspaceFixture(t)
	var synchronous int
	if err := db.sql.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("journal durability: %d %v", synchronous, err)
	}
	if _, err := db.AcquireCoordinator("workspace-test"); err != nil {
		t.Fatal(err)
	}
	s := WorkspaceWriteSession{ID: strings.Repeat("a", 32), WorkspaceID: w.ID, TaskID: task, ArtifactID: "artifact", Path: "report.md", BeginKey: "begin", RequestHash: strings.Repeat("b", 64), ConfigRevision: "config", MaxBytes: 1024, Privacy: "normal"}
	s, err := db.BeginWorkspaceWrite(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SealWorkspaceWrite(s, strings.Repeat("c", 64), "text/markdown", "commit", 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"prepared", "bytes_published"} {
		if err = db.AdvanceWorkspaceWrite(w.ID, s.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.sql.Exec(`CREATE TRIGGER test_receipt_failure BEFORE INSERT ON workspace_receipts BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err = db.CommitWorkspaceWrite(w.ID, s.ID); err == nil {
		t.Fatal("receipt failure ignored")
	}
	session, err := db.WorkspaceWriteSession(w.ID, s.ID)
	if err != nil || session.State != "sealed" {
		t.Fatal(session, err)
	}
	p, err := db.WorkspacePublication(w.ID, s.ID)
	if err != nil || p.State != "bytes_published" {
		t.Fatal(p, err)
	}
	q, err := db.WorkspaceReservation(w.ID, s.ID)
	if err != nil || q.State != "held" {
		t.Fatal(q, err)
	}
	if _, err = db.sql.Exec(`DROP TRIGGER test_receipt_failure`); err != nil {
		t.Fatal(err)
	}
	if err = db.CommitWorkspaceWrite(w.ID, s.ID); err != nil {
		t.Fatal(err)
	}
	session, err = db.WorkspaceWriteSession(w.ID, s.ID)
	if err != nil || session.State != "committed" {
		t.Fatal(session, err)
	}
	if _, err = db.PromoteCoordinator("other-workspace-test"); err != nil {
		t.Fatal(err)
	}
	// Even the legacy trusted accessor must not bypass the S05 process fence.
	if err = db.CommitWorkspacePublication(w.ID, s.ID, s.ID); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("legacy accessor bypassed fence: %v", err)
	}
	if _, err = db.RecoverWorkspaceWrites(); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("startup recovery bypassed fence: %v", err)
	}
}
