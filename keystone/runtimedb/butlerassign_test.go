package runtimedb

// B3/B4/B5's evidence (ADR-W031 A5/A6/A7): a routine names its Butler
// rather than having one chosen, a message reaches a Butler by the
// address it arrived on rather than by what it says, and promoting into
// the shared pool tells you who gains before you do it.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func newRoutine(t *testing.T, db *DB, name string) {
	t.Helper()
	doc, _ := json.Marshal(map[string]any{"name": name})
	if _, err := db.CreateHouseholdRoutine(HouseholdRoutine{
		Name: name, Ownership: "wildstone", Author: "mika",
		State: RoutineEnabled, Document: string(doc), UpdatedBy: "mika",
	}); err != nil {
		t.Fatalf("CreateHouseholdRoutine(%s): %v", name, err)
	}
}

// A4/B4: "assign the sensitive routine to the local Butler" is a thing
// you can actually do, and it is checked when the routine is written
// rather than when it fires.
func TestARoutineNamesItsButler(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	newRoutine(t, db, "morning-weigh-in")

	// Before assignment it has no Butler, which is a real answer: most
	// routines are deterministic and need no model at all.
	if _, assigned, err := db.RoutineButler("morning-weigh-in"); err != nil || assigned {
		t.Fatalf("a fresh routine reported assigned=%v err=%v", assigned, err)
	}
	// And asking for one anyway refuses rather than picking.
	if _, err := db.ResolveRoutineButler("morning-weigh-in"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unassigned routine got a model chosen for it: %v", err)
	}

	if err := db.AssignRoutineButler("morning-weigh-in", "health", "mika"); err != nil {
		t.Fatal(err)
	}
	b, err := db.ResolveRoutineButler("morning-weigh-in")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "health" || b.RouteKind != RouteLocal {
		t.Fatalf("the routine resolved to %s on a %s route", b.Name, b.RouteKind)
	}

	// A6: if that Butler's model goes away, the routine WAITS. It does
	// not run on the cloud Butler sitting right there.
	newButler(t, db, "nutrition", "cloud")
	if err := db.RecordRouteReadiness("kitchen-box", RouteUnavailable, "the box is off"); err != nil {
		t.Fatal(err)
	}
	_, err = db.ResolveRoutineButler("morning-weigh-in")
	if !errors.Is(err, ErrButlerUnreachable) {
		t.Fatalf("a routine whose model was gone did not wait: %v", err)
	}
	if !strings.Contains(err.Error(), "waits") {
		t.Fatalf("the refusal does not say it is waiting: %v", err)
	}

	// A retired Butler cannot be assigned new work.
	if err := db.RetireButler("nutrition", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.AssignRoutineButler("morning-weigh-in", "nutrition", "mika"); !errors.Is(err, ErrButlerUnreachable) {
		t.Fatalf("a routine was assigned to a retired butler: %v", err)
	}
}

// B4: a task snapshots its Butler, so which model answered a piece of
// work stays answerable after the Butler is reassigned.
func TestATaskRemembersWhichButlerAnsweredIt(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	id := newAcceptanceTask(t, db, "mika", ButlerAudiencePrivate, "how did I sleep?")

	if err := db.SetTaskButler(id, "health"); err != nil {
		t.Fatal(err)
	}
	// Reassigning the Butler's model afterwards does not rewrite what
	// answered this task.
	if _, _, err := db.SetButlerRoute("health", "cloud", "mika"); err != nil {
		t.Fatal(err)
	}
	got, err := db.TaskButler(id)
	if err != nil {
		t.Fatal(err)
	}
	if got != "health" {
		t.Fatalf("task butler = %q, want health", got)
	}
}

// A7: a message reaches a Butler by WHERE IT ARRIVED, never by what it
// says. Nothing reads the message to decide.
func TestAMessageReachesAButlerByAddressNotByContent(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	newButler(t, db, "nutrition", "cloud")
	for _, b := range []string{"health", "nutrition"} {
		if err := db.GrantButlerAccess(b, "sam", "mika"); err != nil {
			t.Fatal(err)
		}
	}
	// Two addresses. Whether these are two bots or two topics in one
	// bot is a deployment's choice — the schema only knows they are
	// different places.
	if _, err := db.AddButlerAddress("health", "telegram", "bot-health", "the health bot", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddButlerAddress("nutrition", "telegram", "topic-42", "#nutrition", "mika"); err != nil {
		t.Fatal(err)
	}

	got, err := db.ResolveButlerAtAddress("telegram", "bot-health", "sam")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "health" {
		t.Fatalf("a message to the health address reached %s", got.Name)
	}
	if got, err = db.ResolveButlerAtAddress("telegram", "topic-42", "sam"); err != nil || got.Name != "nutrition" {
		t.Fatalf("a message to the nutrition address reached %s (%v)", got.Name, err)
	}

	// An address nobody answers is a configuration gap, not something
	// to recover from by picking a Butler.
	if _, err := db.ResolveButlerAtAddress("telegram", "topic-99", "sam"); !errors.Is(err, ErrNoButlerForAddress) {
		t.Fatalf("an unknown address resolved to something: %v", err)
	}

	// One address, one Butler. Two would mean the same message could be
	// answered by either, which is a dispatcher hidden in a race.
	if _, err := db.AddButlerAddress("nutrition", "telegram", "bot-health", "", "mika"); err == nil {
		t.Fatal("two butlers were given the same address")
	}

	// Arriving at the address is not permission to talk: A10's access
	// list still decides.
	if err := db.RevokeButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveButlerAtAddress("telegram", "bot-health", "sam"); !errors.Is(err, ErrNoButlerAccess) {
		t.Fatalf("reaching the address bypassed the access list: %v", err)
	}

	// Revoking the address stops it answering there.
	if err := db.RevokeButlerAddress("telegram", "topic-42", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveButlerAtAddress("telegram", "topic-42", "sam"); !errors.Is(err, ErrNoButlerForAddress) {
		t.Fatalf("a revoked address still answered: %v", err)
	}
	// And a retired Butler cannot be handed a new one, which would
	// quietly undo the retirement.
	if err := db.RetireButler("health", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddButlerAddress("health", "telegram", "bot-again", "", "mika"); err == nil {
		t.Fatal("a retired butler was given a new address")
	}
}

// A5: the disclosure is available BEFORE the decision, because a
// decision can only be weighed beforehand — and it is recorded
// afterwards, because it has to stay answerable when the roster has
// changed.
func TestPoolPromotionIsPreviewedAndRecorded(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box") // local
	newButler(t, db, "nutrition", "cloud")    // cloud

	seq, err := db.RecordButlerMemory("health", MemoryRecord{
		Kind: MemoryPreference, Subject: "household", Content: "we are vegetarian",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "sam", SourceKind: "conversation", SourceID: "p-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Read first, decide second.
	preview, err := db.PreviewPoolPromotion(seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Butlers) != 2 || !preview.LeavesHouse {
		t.Fatalf("preview = %+v, want both butlers and a cloud warning", preview)
	}
	if len(preview.CloudButlers) != 1 || preview.CloudButlers[0] != "nutrition" {
		t.Fatalf("the preview does not name which butler sends it out of the house: %+v", preview.CloudButlers)
	}
	// Nothing happened yet.
	if got, _ := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "nutrition"}); len(got) != 0 {
		t.Fatal("previewing a promotion promoted it")
	}

	if _, _, err := db.PromoteMemoryToSharedPool(seq, "mika"); err != nil {
		t.Fatal(err)
	}
	// And it is written down, as it was at the time.
	records, err := db.PoolDisclosuresFor(seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("promotion left %d disclosure records, want 1", len(records))
	}
	if !records[0].LeavesHouse {
		t.Fatal("the record does not say the material left the house")
	}
	// Retiring the cloud Butler afterwards does not rewrite what was
	// disclosed at the time.
	if err := db.RetireButler("nutrition", "mika"); err != nil {
		t.Fatal(err)
	}
	after, _ := db.PoolDisclosuresFor(seq)
	if !after[0].LeavesHouse || len(after[0].Butlers) != 2 {
		t.Fatalf("the record changed when the roster did: %+v", after[0])
	}
}
