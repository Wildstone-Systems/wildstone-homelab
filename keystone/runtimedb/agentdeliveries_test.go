package runtimedb

// agent_deliveries (migration 0084, ADR-W039 Phase 3, #339): the write
// path Phase 3 uses (a proposal outcome), and that it reads back what it
// wrote — the read side Phase 4's long-poll consumer will build on.

import (
	"errors"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

func runtimeDeliveryFixture(t *testing.T, name string) (*DB, int64, RuntimeDeliveryCredential) {
	t.Helper()
	db := butlerFixture(t)
	seq, err := db.CreateButler(Butler{
		Name: name, Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Model: "kitchen-box",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerRuntime(seq, ButlerRuntime{
		Type: agentapi.RuntimeNative, Delivery: agentapi.DeliveryPull, ExecutionProfile: "platform-only",
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	principal := "runtime-" + name + "-1"
	if err := db.CreatePrincipal(principal, RoleButler, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRuntimePrincipal(principal, seq); err != nil {
		t.Fatal(err)
	}
	hash := "hash-" + name
	if err := db.InsertToken(hash, principal, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	return db, seq, RuntimeDeliveryCredential{Principal: principal, TokenHash: hash}
}

func TestRecordAgentDeliveryReadsBack(t *testing.T) {
	db := butlerFixture(t)
	seq, err := db.CreateButler(Butler{
		Name: "alfred", Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Model: "kitchen-box",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordAgentDelivery(seq, "proposal_outcome", `{"plan":"pl-1","outcome":"approved"}`); err != nil {
		t.Fatalf("RecordAgentDelivery: %v", err)
	}
	list, err := db.ListAgentDeliveries(seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("deliveries = %+v, want one", list)
	}
	d := list[0]
	if d.Kind != "proposal_outcome" || d.Payload == "" || d.CreatedAt.IsZero() {
		t.Fatalf("delivery = %+v", d)
	}
	if !d.AckedAt.IsZero() {
		t.Fatal("a fresh delivery is already acknowledged")
	}

	// A delivery belongs to the Agent it names, never any other.
	other, err := db.CreateButler(Butler{
		Name: "joan", Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Model: "kitchen-box",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := db.ListAgentDeliveries(other, 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("another agent's deliveries = %+v, %v", empty, err)
	}
}

func TestRecordAgentDeliveryRefusesAnUnknownKind(t *testing.T) {
	db := butlerFixture(t)
	seq, err := db.CreateButler(Butler{
		Name: "alfred", Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Model: "kitchen-box",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordAgentDelivery(seq, "not-a-kind", `{}`); err == nil {
		t.Fatal("an unknown delivery kind was accepted")
	}
}

func TestRuntimeDeliveryAuthorityIsLiveAndCurrent(t *testing.T) {
	t.Run("principal disabled", func(t *testing.T) {
		db, seq, cred := runtimeDeliveryFixture(t, "disabled")
		if _, err := db.RecordAgentDelivery(seq, agentapi.DeliveryTurn, `{}`); err != nil {
			t.Fatal(err)
		}
		if err := db.SetPrincipalDisabled(cred.Principal, true); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PollRuntimeDeliveries(cred, 0, 10, time.Now()); !errors.Is(err, ErrRuntimeDeliveryDenied) {
			t.Fatalf("poll with a disabled principal = %v, want ErrRuntimeDeliveryDenied", err)
		}
	})

	t.Run("token revoked or expired", func(t *testing.T) {
		db, seq, cred := runtimeDeliveryFixture(t, "token")
		id, err := db.RecordAgentDelivery(seq, agentapi.DeliveryTurn, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RevokeToken(cred.TokenHash); err != nil {
			t.Fatal(err)
		}
		if err := db.AckRuntimeDelivery(cred, id, time.Now()); !errors.Is(err, ErrRuntimeDeliveryDenied) {
			t.Fatalf("ack with a revoked token = %v, want ErrRuntimeDeliveryDenied", err)
		}
		expired := RuntimeDeliveryCredential{Principal: cred.Principal, TokenHash: "expired-token"}
		if err := db.InsertToken(expired.TokenHash, expired.Principal, "", time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PollRuntimeDeliveries(expired, 0, 10, time.Now()); !errors.Is(err, ErrRuntimeDeliveryDenied) {
			t.Fatalf("poll with an expired token = %v, want ErrRuntimeDeliveryDenied", err)
		}
	})

	t.Run("credential superseded", func(t *testing.T) {
		db, seq, old := runtimeDeliveryFixture(t, "rotated")
		if err := db.CreatePrincipal("runtime-rotated-2", RoleButler, ""); err != nil {
			t.Fatal(err)
		}
		if err := db.BindRuntimePrincipal("runtime-rotated-2", seq); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertToken("hash-rotated-2", "runtime-rotated-2", "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PollRuntimeDeliveries(old, 0, 10, time.Now()); !errors.Is(err, ErrRuntimeDeliveryDenied) {
			t.Fatalf("predecessor poll = %v, want ErrRuntimeDeliveryDenied", err)
		}
		current := RuntimeDeliveryCredential{Principal: "runtime-rotated-2", TokenHash: "hash-rotated-2"}
		if _, err := db.PollRuntimeDeliveries(current, 0, 10, time.Now()); err != nil {
			t.Fatalf("successor poll = %v", err)
		}
	})

	t.Run("runtime revoked or Agent retired", func(t *testing.T) {
		db, seq, cred := runtimeDeliveryFixture(t, "lifecycle")
		if err := db.SetRuntimeHealth(seq, agentapi.HealthRevoked); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PollRuntimeDeliveries(cred, 0, 10, time.Now()); !errors.Is(err, ErrRuntimeDeliveryDenied) {
			t.Fatalf("revoked runtime poll = %v, want ErrRuntimeDeliveryDenied", err)
		}
		if err := db.SetRuntimeHealth(seq, agentapi.HealthConnected); err != nil {
			t.Fatal(err)
		}
		if err := db.RetireButler("lifecycle", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PollRuntimeDeliveries(cred, 0, 10, time.Now()); !errors.Is(err, ErrRuntimeDeliveryDenied) {
			t.Fatalf("retired Agent poll = %v, want ErrRuntimeDeliveryDenied", err)
		}
	})
}

func TestRuntimeDeliveryGatesRestoreAndHouseholdStop(t *testing.T) {
	t.Run("restore quarantine", func(t *testing.T) {
		db, _, cred := runtimeDeliveryFixture(t, "quarantine")
		if err := db.EnterRestoreQuarantine("test restore", time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PollRuntimeDeliveries(cred, 0, 10, time.Now()); !errors.Is(err, ErrQuarantined) {
			t.Fatalf("poll in quarantine = %v, want ErrQuarantined", err)
		}
	})

	t.Run("household execution stop", func(t *testing.T) {
		db, seq, cred := runtimeDeliveryFixture(t, "stopped")
		id, err := db.RecordAgentDelivery(seq, agentapi.DeliveryTurn, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.StopHouseholdExecution("mika", "test stop"); err != nil {
			t.Fatal(err)
		}
		if err := db.AckRuntimeDelivery(cred, id, time.Now()); !errors.Is(err, ErrHouseholdStopped) {
			t.Fatalf("ack during household stop = %v, want ErrHouseholdStopped", err)
		}
	})
}

func TestAckRuntimeDeliveryLinearizesWithRevocationAndIsIdempotent(t *testing.T) {
	t.Run("cross-Agent ack changes no row", func(t *testing.T) {
		db, _, cred := runtimeDeliveryFixture(t, "cross-source")
		other, err := db.CreateButler(Butler{
			Name: "cross-target", Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Model: "kitchen-box",
		}, "root-admin")
		if err != nil {
			t.Fatal(err)
		}
		id, err := db.RecordAgentDelivery(other, agentapi.DeliveryTurn, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AckRuntimeDelivery(cred, id, time.Now()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-Agent ack = %v, want ErrNotFound", err)
		}
		rows, err := db.ListAgentDeliveries(other, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || !rows[0].AckedAt.IsZero() {
			t.Fatalf("cross-Agent ack changed row: %+v", rows)
		}
	})

	t.Run("ack first remains committed", func(t *testing.T) {
		db, seq, cred := runtimeDeliveryFixture(t, "ack-first")
		id, err := db.RecordAgentDelivery(seq, agentapi.DeliveryTurn, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AckRuntimeDelivery(cred, id, time.Now()); err != nil {
			t.Fatal(err)
		}
		first, _ := db.ListAgentDeliveries(seq, 10)
		if err := db.AckRuntimeDelivery(cred, id, time.Now()); err != nil {
			t.Fatal(err)
		}
		second, _ := db.ListAgentDeliveries(seq, 10)
		if first[0].AckedAt.IsZero() || !first[0].AckedAt.Equal(second[0].AckedAt) {
			t.Fatalf("repeated ack changed timestamp: %s -> %s", first[0].AckedAt, second[0].AckedAt)
		}
		if err := db.SetPrincipalDisabled(cred.Principal, true); err != nil {
			t.Fatal(err)
		}
		if got, _ := db.ListAgentDeliveries(seq, 10); got[0].AckedAt.IsZero() {
			t.Fatal("revocation erased an acknowledgment that committed first")
		}
	})

	t.Run("completed revoke wins before ack", func(t *testing.T) {
		db, seq, cred := runtimeDeliveryFixture(t, "revoke-first")
		id, err := db.RecordAgentDelivery(seq, agentapi.DeliveryTurn, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.sql.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE principals SET disabled = 1 WHERE name = ?`, cred.Principal); err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			close(started)
			done <- db.AckRuntimeDelivery(cred, id, time.Now())
		}()
		<-started
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrRuntimeDeliveryDenied) {
			t.Fatalf("ack behind committed revoke = %v, want ErrRuntimeDeliveryDenied", err)
		}
		if got, _ := db.ListAgentDeliveries(seq, 10); !got[0].AckedAt.IsZero() {
			t.Fatalf("revoked ack changed row at %s", got[0].AckedAt)
		}
	})
}
