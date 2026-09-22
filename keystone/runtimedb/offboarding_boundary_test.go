package runtimedb

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestQueuedDeliveryIsNotDeliveredAfterRevocation(t *testing.T) {
	db := householdWith(t, "mika", "sandy")
	if err := db.SetButlerMemberRole("mika", RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("offboarding-test"); err != nil {
		t.Fatal(err)
	}
	at := connect(t, db, "household", "telegram", ButlerSurfaceFamily)
	binding := bind(t, db, at, "sandy", "12345")
	seq := mustEnqueue(t, db, ChatOutbound{
		Connection: at.Seq, Target: "conv-1", Body: "private delivery payload",
		BindingID: binding.ID, DedupeKey: "offboarding-queued-reply",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	before, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(before) != 1 || before[0].Seq != seq {
		t.Fatalf("precondition: queued delivery = %+v, %v", before, err)
	}
	if _, err := db.OffboardMember("sandy", "mika", "mika"); err != nil {
		t.Fatal(err)
	}
	after, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range after {
		if m.Seq == seq || m.Body == "private delivery payload" {
			t.Fatalf("offboarded Person's queued payload reached the adapter: %+v", m)
		}
	}
	if _, err := db.ChatOutboundReady(at, seq); !errors.Is(err, ErrNotFound) {
		t.Fatalf("already-polled delivery survived pre-send recheck: %v", err)
	}
}

func TestOffboardingIsNotDemotion(t *testing.T) {
	db := householdWith(t, "mika", "sandy")
	for _, member := range []string{"mika", "sandy"} {
		if err := db.SetButlerMemberRole(member, RoleOwner, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	before, err := db.GetButlerMember("sandy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.OffboardMember("sandy", "mika", "mika"); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetButlerMember("sandy")
	if err != nil {
		t.Fatal(err)
	}
	if after.Active() || after.OffboardedAt.IsZero() || after.Generation <= before.Generation {
		t.Fatalf("offboarding preserved Member admission as demotion would: before=%+v after=%+v", before, after)
	}
	if after.Role != RoleOwner {
		t.Fatalf("offboarding silently changed the Person's role: %s", after.Role)
	}
	records, err := db.ListOffboardingRecords("sandy")
	if err != nil || len(records) == 0 {
		t.Fatalf("full revocation left no offboarding record: %+v, %v", records, err)
	}
}

func TestSharedResourcesAreNotSilentlyDestroyed(t *testing.T) {
	db := householdWith(t, "mika", "sandy")
	if err := db.SetButlerMemberRole("mika", RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	personal := fact("private departure detail", "sandy", ButlerAudiencePrivate)
	personal.Scope, personal.Author = MemoryScopePersonal, "sandy"
	privateID, err := db.RecordMemory(personal)
	if err != nil {
		t.Fatal(err)
	}
	shared := fact("shared household detail", HouseholdOwner, ButlerAudienceShared)
	shared.Scope, shared.Author = MemoryScopeHousehold, "sandy"
	sharedID, err := db.RecordMemory(shared)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.InsertButlerTask(ButlerTask{Kind: ButlerTaskConversation, Author: "mika", Owner: "sandy", Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Audience: ButlerAudienceShared, Objective: "shared departure work", CompletionCriteria: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateHouseholdRoutine(HouseholdRoutine{Name: "departure-routine", Ownership: RoutineWildstone, Author: "sandy", Document: "household routine document"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OffboardMember("sandy", "mika", "mika"); err != nil {
		t.Fatal(err)
	}
	gotPrivate, err := db.GetMemory(privateID)
	if err != nil || gotPrivate.ForgottenAt.IsZero() {
		t.Fatalf("Personal memory remained live: %+v %v", gotPrivate, err)
	}
	gotShared, err := db.GetMemory(sharedID)
	if err != nil || !gotShared.ForgottenAt.IsZero() || gotShared.Content != shared.Content {
		t.Fatalf("shared memory was destroyed: %+v %v", gotShared, err)
	}
	r, err := db.GetHouseholdRoutine("departure-routine")
	if err != nil || r.Document != "household routine document" {
		t.Fatalf("shared routine was destroyed: %+v %v", r, err)
	}
	decisions, err := db.ListOffboardingResourceDispositions("sandy")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"task:" + taskID: "transferred",
		"memory:" + strconv.FormatInt(privateID, 10): "retained-quarantined",
		"memory:" + strconv.FormatInt(sharedID, 10):  "retained",
		"routine:departure-routine":                  "retained",
	}
	for _, d := range decisions {
		key := d.Kind + ":" + d.Ref
		if expected, ok := want[key]; ok {
			if d.Disposition != expected {
				t.Fatalf("%s disposition = %s, want %s", key, d.Disposition, expected)
			}
			delete(want, key)
		}
	}
	if len(want) > 0 {
		t.Fatalf("missing recorded resource decisions: %v; got %+v", want, decisions)
	}
}
