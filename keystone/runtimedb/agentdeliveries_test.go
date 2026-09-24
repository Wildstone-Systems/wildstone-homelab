package runtimedb

// agent_deliveries (migration 0084, ADR-W039 Phase 3, #339): the write
// path Phase 3 uses (a proposal outcome), and that it reads back what it
// wrote — the read side Phase 4's long-poll consumer will build on.

import (
	"testing"
)

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
