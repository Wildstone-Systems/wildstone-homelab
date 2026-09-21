package runtimedb

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFenceIsOnTheStoreNotTheRoute(t *testing.T) {
	db := open(t)
	if err := db.CheckHouseholdExecution(); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("unclaimed store allowed effect: %v", err)
	}
	if _, err := db.AcquireCoordinator("one"); err != nil {
		t.Fatal(err)
	}
	other, err := Open(db.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = other.AcquireCoordinator("two"); !errors.Is(err, ErrCoordinatorHeld) {
		t.Fatal(err)
	}
	newCaller := func(db *DB) error { return db.CheckHouseholdExecution() }
	if err = newCaller(other); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("non-coordinator allowed: %v", err)
	}
	if err = newCaller(db); err != nil {
		t.Fatal(err)
	}
	if _, err = other.PromoteCoordinator("two"); err != nil {
		t.Fatal(err)
	}
	if err = newCaller(db); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("superseded process allowed: %v", err)
	}
}
func TestNonCoordinatorStillServesStoreReads(t *testing.T) {
	db := open(t)
	if _, _, err := db.GetButlerSettings(); err != nil {
		t.Fatal(err)
	}
}

func TestChatPublicationAndDeliveryAreStoreFenced(t *testing.T) {
	db, at, _ := uncoordinatedChatTestDB(t)
	msg := ChatOutbound{Connection: at.Seq, Target: "bound-target", Body: "test", DedupeKey: "one"}
	if _, err := db.EnqueueChatOutbound(msg); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("new caller bypassed store: %v", err)
	}
	if _, err := db.AcquireCoordinator("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueChatOutbound(msg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PromoteCoordinator("two"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ChatOutboundAfter(at, 0, 10); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("superseded reader emitted queued messages: %v", err)
	}
	msg.DedupeKey = "two"
	if _, err := db.EnqueueChatOutbound(msg); !errors.Is(err, ErrCoordinatorFenced) {
		t.Fatalf("superseded writer queued new message: %v", err)
	}
}

func TestCoordinatorHeartbeatRecordsLivenessAndStopsOnPromotion(t *testing.T) {
	db := open(t)
	before, err := db.AcquireCoordinator("one")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- db.RunCoordinatorHeartbeat(ctx, 5*time.Millisecond) }()
	deadline := time.After(2 * time.Second)
	for {
		current, err := db.GetCoordinator()
		if err != nil {
			t.Fatal(err)
		}
		if current.HeartbeatAt.After(before.HeartbeatAt) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("heartbeat never advanced")
		case <-time.After(5 * time.Millisecond):
		}
	}
	promoted, err := db.PromoteCoordinator("two")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, ErrCoordinatorFenced) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old heartbeat did not stop")
	}
	after, err := db.GetCoordinator()
	if err != nil || !after.HeartbeatAt.Equal(promoted.HeartbeatAt) {
		t.Fatalf("old process changed new heartbeat: %+v %v", after, err)
	}
}

func TestHeartbeatRecoversAfterTemporaryDatabaseFailure(t *testing.T) {
	db := open(t)
	before, err := db.AcquireCoordinator("one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec(`CREATE TRIGGER temporarily_reject_heartbeat BEFORE UPDATE OF heartbeat_at ON coordinator_lease BEGIN SELECT RAISE(ABORT,'temporary write failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- db.RunCoordinatorHeartbeat(ctx, 5*time.Millisecond) }()
	select {
	case err := <-done:
		t.Fatalf("heartbeat stopped on temporary failure: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if _, err = db.sql.Exec("DROP TRIGGER temporarily_reject_heartbeat"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		current, err := db.GetCoordinator()
		if err != nil {
			t.Fatal(err)
		}
		if current.HeartbeatAt.After(before.HeartbeatAt) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("heartbeat did not recover")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
