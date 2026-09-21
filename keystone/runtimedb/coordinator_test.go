package runtimedb

// The single active coordinator (ADR-W030 A11, P3): no election, no
// automatic takeover, explicit fenced promotion.

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCoordinatorLeaseIsSingleAndFenced(t *testing.T) {
	db := open(t)

	if _, err := db.GetCoordinator(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a lease existed before anyone took one: %v", err)
	}
	first, err := db.AcquireCoordinator("keystone-a")
	if err != nil {
		t.Fatalf("AcquireCoordinator: %v", err)
	}
	if first.Epoch != 1 || first.Holder != "keystone-a" {
		t.Fatalf("first lease = %+v", first)
	}
	// A restart of the same coordinator keeps the epoch: it owns the
	// same work it owned before.
	again, err := db.AcquireCoordinator("keystone-a")
	if err != nil || again.Epoch != 1 {
		t.Fatalf("re-acquire = %+v, %v", again, err)
	}
	// A second process stops rather than waiting or taking over.
	if _, err := db.AcquireCoordinator("keystone-b"); !errors.Is(err, ErrCoordinatorHeld) {
		t.Fatalf("second coordinator got the lease: %v", err)
	}
	if err := db.CheckCoordinator("keystone-b", 1); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("a non-holder passed the fence: %v", err)
	}
	if err := db.CheckCoordinator("keystone-a", 1); err != nil {
		t.Fatalf("the holder failed its own fence: %v", err)
	}

	promoted, err := db.PromoteCoordinator("keystone-b")
	if err != nil {
		t.Fatalf("PromoteCoordinator: %v", err)
	}
	if promoted.Epoch != 2 || promoted.PromotedFrom != "keystone-a" {
		t.Fatalf("promotion = %+v", promoted)
	}
	if err := db.CheckCoordinator("keystone-a", 1); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("the promoted-from coordinator still passes the fence: %v", err)
	}
	if err := db.HeartbeatCoordinator("keystone-a", 1); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("a fenced coordinator recorded a heartbeat: %v", err)
	}
	if err := db.HeartbeatCoordinator("keystone-b", 2); err != nil {
		t.Fatalf("the holder could not heartbeat: %v", err)
	}
	// Promotion is an assertion that somebody else stopped; asserting it
	// about yourself is a confused caller, not a no-op.
	if _, err := db.PromoteCoordinator("keystone-b"); err == nil {
		t.Fatal("a coordinator promoted itself")
	}
	// And the fence cannot be rewound.
	if _, err := db.sql.Exec(`UPDATE coordinator_lease SET epoch = 1 WHERE id = 1`); err == nil {
		t.Fatal("the coordinator epoch was rewound, un-fencing a stopped coordinator")
	}
}

// The lease survives a restart, which is what makes "the same
// coordinator came back" distinguishable from "a second one started".
func TestCoordinatorLeaseSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("keystone-a"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	lease, err := db2.GetCoordinator()
	if err != nil {
		t.Fatalf("GetCoordinator after restart: %v", err)
	}
	if lease.Holder != "keystone-a" || lease.Epoch != 1 {
		t.Fatalf("lease after restart = %+v", lease)
	}
	if _, err := db2.AcquireCoordinator("keystone-b"); !errors.Is(err, ErrCoordinatorHeld) {
		t.Fatalf("a different process took the lease after a restart: %v", err)
	}
}

func TestTwoLiveProcessesCannotReacquireTheSameHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := first.AcquireCoordinator("same-deployment"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.AcquireCoordinator("same-deployment"); !errors.Is(err, ErrCoordinatorHeld) {
		t.Fatalf("a second live process acquired the same holder: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := second.AcquireCoordinator("same-deployment")
	if err != nil || lease.Epoch != 1 {
		t.Fatalf("restart: %+v, %v", lease, err)
	}
}
