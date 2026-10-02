package runtimedb

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestWorkspaceQuotaSerializesConcurrentScopes(t *testing.T) {
	db, private, task := workspaceFixture(t)
	if _, err := db.AcquireCoordinator("quota-test"); err != nil {
		t.Fatal(err)
	}
	shared := Workspace{ID: "ws-shared", PersonID: private.PersonID, AgentID: private.AgentID, Visibility: "shared"}
	if err := db.CreateWorkspace(shared); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorkspaceQuotaPolicy(private.ID, 10, "owner-config-1"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, scope := range []string{private.ID, shared.ID} {
		wg.Add(1)
		go func(i int, scope string) {
			defer wg.Done()
			<-start
			results <- db.ReserveWorkspaceBytes(WorkspaceReservation{ID: fmt.Sprintf("concurrent-%d", i), WorkspaceID: scope, TaskID: task, SizeBytes: 6, Purpose: "scratch"})
		}(i, scope)
	}
	close(start)
	wg.Wait()
	close(results)
	var accepted, refused int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrWorkspaceQuota):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("concurrent admission: accepted=%d refused=%d", accepted, refused)
	}
	stats, err := db.WorkspaceQuotaStats(private.ID)
	if err != nil || stats.LimitBytes != 10 || stats.HeldBytes != 6 || stats.CommittedBytes != 0 || stats.PrivateBytes+stats.SharedBytes != 6 {
		t.Fatalf("accounting: %+v %v", stats, err)
	}
	if _, err := db.WorkspaceQuotaStats("missing"); err == nil {
		t.Fatal("missing scope returned usage")
	}
}

func TestWorkspaceQuotaCountsHistoryTrashAndHeldBytesAfterReopen(t *testing.T) {
	db, w, task := workspaceFixture(t)
	if _, err := db.AcquireCoordinator("quota-test"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorkspaceQuotaPolicy(w.ID, 11, "owner-config-1"); err != nil {
		t.Fatal(err)
	}
	first := workspaceIntent(t, db, w, task, "report", "r1", "", 4)
	workspacePublish(t, db, first, "receipt-1")
	second := workspaceIntent(t, db, w, task, "report", "r2", "r1", 4)
	workspacePublish(t, db, second, "receipt-2")
	if err := db.TrashWorkspaceArtifact(w.ID, "report", "r2"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "scratch-3", WorkspaceID: w.ID, TaskID: task, SizeBytes: 3, Purpose: "scratch"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "over-quota", WorkspaceID: w.ID, TaskID: task, SizeBytes: 1, Purpose: "scratch"}); !errors.Is(err, ErrWorkspaceQuota) {
		t.Fatalf("history or trash escaped accounting: %v", err)
	}
	path := db.path
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.AcquireCoordinator("quota-test"); err != nil {
		t.Fatal(err)
	}
	stats, err := reopened.WorkspaceQuotaStats(w.ID)
	if err != nil || stats.LimitBytes != 11 || stats.CommittedBytes != 8 || stats.HeldBytes != 3 || stats.PrivateBytes != 11 {
		t.Fatalf("restart accounting: %+v %v", stats, err)
	}
	if err := reopened.SetWorkspaceQuotaPolicy(w.ID, 10, "owner-config-2"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ReserveWorkspaceBytes(WorkspaceReservation{ID: "lowered-quota", WorkspaceID: w.ID, TaskID: task, SizeBytes: 1, Purpose: "scratch"}); !errors.Is(err, ErrWorkspaceQuota) {
		t.Fatalf("lowered setting admitted bytes: %v", err)
	}
	if err := reopened.ReleaseWorkspaceReservation(w.ID, "scratch-3"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ReserveWorkspaceBytes(WorkspaceReservation{ID: "after-release", WorkspaceID: w.ID, TaskID: task, SizeBytes: 2, Purpose: "scratch"}); err != nil {
		t.Fatalf("released bytes not reusable: %v", err)
	}
}

func TestWorkspaceQuotaOwnerLimitCanExceedOriginalDefault(t *testing.T) {
	db, w, task := workspaceFixture(t)
	if _, err := db.AcquireCoordinator("quota-test"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorkspaceQuotaPolicy(w.ID, 2<<30, "owner-config-large"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []WorkspaceReservation{
		{ID: "first-gib", WorkspaceID: w.ID, TaskID: task, SizeBytes: 1 << 30, Purpose: "scratch"},
		{ID: "above-original-default", WorkspaceID: w.ID, TaskID: task, SizeBytes: 1, Purpose: "scratch"},
	} {
		if err := db.ReserveWorkspaceBytes(r); err != nil {
			t.Fatalf("approved larger limit refused %s: %v", r.ID, err)
		}
	}
	if err := db.ReserveWorkspaceBytes(WorkspaceReservation{ID: "over-large-limit", WorkspaceID: w.ID, TaskID: task, SizeBytes: 1 << 30, Purpose: "scratch"}); !errors.Is(err, ErrWorkspaceQuota) {
		t.Fatalf("larger limit not enforced: %v", err)
	}
}
