package automation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func arrived(seq int64, subject string) runtimedb.HouseholdEvent {
	return runtimedb.HouseholdEvent{Seq: seq, EventID: "ha-" + subject, Source: "publisher-home",
		Type: "person.arrived_home", Subject: subject, OccurredAt: t0}
}

// TestMatchesSubscription is MatchesSubscription's own pure evidence: a
// disabled or unapproved subscription matches nothing however its match
// document reads, an unknown match kind is inert rather than an error,
// same-subject narrows, and the engine's own loop guard applies here
// too.
func TestMatchesSubscription(t *testing.T) {
	base := runtimedb.AgentSubscription{
		Enabled: true, ApprovedVersion: 1, MatchKind: "event_type",
		MatchJSON: `{"eventType":"person.arrived_home","subject":"mika","sameSubject":true}`,
	}
	if !MatchesSubscription(base, arrived(2, "mika")) {
		t.Error("a matching event_type/subject was refused")
	}
	if MatchesSubscription(base, arrived(2, "sam")) {
		t.Error("a same-subject subscription matched a different subject")
	}
	disabled := base
	disabled.Enabled = false
	if MatchesSubscription(disabled, arrived(2, "mika")) {
		t.Error("a disabled subscription matched")
	}
	unapproved := base
	unapproved.ApprovedVersion = 0
	if MatchesSubscription(unapproved, arrived(2, "mika")) {
		t.Error("an unapproved subscription matched")
	}
	unknown := base
	unknown.MatchKind = "something-a-later-release-invented"
	if MatchesSubscription(unknown, arrived(2, "mika")) {
		t.Error("an unrecognised match kind matched instead of staying inert")
	}
	// The engine's own record never triggers a subscription either.
	own := arrived(2, "mika")
	own.Source = Source
	if MatchesSubscription(base, own) {
		t.Error("a subscription fired on the deterministic engine's own record")
	}
}

// TestSubscriptionEngineFirstTickAdoptsTheStreamRatherThanReplayingIt
// mirrors automation.Engine's own "adopt, don't replay" rule: a
// subscription approved today must not fire on years of events it never
// watched.
func TestSubscriptionEngineFirstTickAdoptsTheStreamRatherThanReplayingIt(t *testing.T) {
	_, db := started(t) // adopts a marker at seq 1
	emit(t, db, "history-1", "person.arrived_home", "mika", t0)

	b, ledger := subscriptionButler(t, db)
	subID := mustSubscribe(t, db, b.Seq, "event_type", `{"eventType":"person.arrived_home","subject":"mika","sameSubject":true}`, []string{"peek"})

	se := &SubscriptionEngine{DB: db, Ledger: ledger}
	out, err := se.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Matched != 0 {
		t.Fatalf("a first tick matched %d; it should only adopt the stream's current end", out.Matched)
	}
	sub, err := db.GetAgentSubscription(subID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Cursor == 0 {
		t.Fatal("the first tick did not adopt a cursor")
	}
}

// TestSubscriptionMatchSpawnsABoundedTaskAndDelivery is the family's
// core evidence (ADR-W039 §3.6, Phase 4, #341): once adopted, a
// matching event opens a bounded Task, records a subscription delivery
// carrying the cursor, and advances the subscription's own cursor.
func TestSubscriptionMatchSpawnsABoundedTaskAndDelivery(t *testing.T) {
	_, db := started(t)
	b, ledger := subscriptionButler(t, db)
	subID := mustSubscribe(t, db, b.Seq, "event_type", `{"eventType":"person.arrived_home","subject":"mika","sameSubject":true}`, []string{"peek"})

	se := &SubscriptionEngine{DB: db, Ledger: ledger}
	if _, err := se.Tick(context.Background()); err != nil { // adopt
		t.Fatal(err)
	}

	seq := emit(t, db, "arrival-1", "person.arrived_home", "mika", t0)
	out, err := se.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Matched != 1 {
		t.Fatalf("Matched = %d, want 1", out.Matched)
	}

	sub, err := db.GetAgentSubscription(subID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Cursor != seq {
		t.Fatalf("cursor = %d, want %d (the matched event's own seq)", sub.Cursor, seq)
	}

	deliveries, err := db.ListAgentDeliveries(b.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v, want exactly one", deliveries)
	}
	d := deliveries[0]
	if string(d.Kind) != "subscription" {
		t.Errorf("delivery kind = %q, want subscription", d.Kind)
	}
	if d.Cursor == "" {
		t.Error("the delivery carries no cursor")
	}
	var payload struct {
		Handle       string `json:"handle"`
		Subscription int64  `json:"subscription"`
		Task         string `json:"task"`
	}
	if err := json.Unmarshal([]byte(d.Payload), &payload); err != nil {
		t.Fatalf("payload did not decode: %v", err)
	}
	if payload.Handle == "" || payload.Subscription != subID || payload.Task == "" {
		t.Fatalf("payload = %+v, missing a field", payload)
	}

	task, err := db.GetButlerTask(payload.Task)
	if err != nil {
		t.Fatal(err)
	}
	if origin, ok := SubscriptionOrigin(task.GrantsJSON); !ok || origin != subID {
		t.Errorf("SubscriptionOrigin(%q) = %d,%v, want %d,true", task.GrantsJSON, origin, ok, subID)
	}

	// The same event never matches twice: a second tick with nothing new
	// matches nothing.
	out2, err := se.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out2.Matched != 0 {
		t.Fatalf("a second tick with no new events matched %d", out2.Matched)
	}
}

// TestSubscriptionEngineSkipsOneBadSubscriptionAndContinues: a
// subscription whose spawn fails (here, an audience that no longer
// resolves) fails on its own and does not stop the rest of the family's
// tick.
func TestSubscriptionEngineSkipsOneBadSubscriptionAndContinues(t *testing.T) {
	_, db := started(t)
	b, ledger := subscriptionButler(t, db)
	good := mustSubscribe(t, db, b.Seq, "event_type", `{"eventType":"person.arrived_home","subject":"mika","sameSubject":true}`, []string{"peek"})
	badID, err := db.InsertAgentSubscription(b.Seq, "event_type",
		`{"eventType":"person.arrived_home","subject":"mika","sameSubject":true}`,
		"an-audience-nobody-ever-created", []string{"peek"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveAgentSubscription(badID, "mika"); err != nil {
		t.Fatal(err)
	}

	se := &SubscriptionEngine{DB: db, Ledger: ledger}
	if _, err := se.Tick(context.Background()); err != nil { // adopt
		t.Fatal(err)
	}
	emit(t, db, "arrival-2", "person.arrived_home", "mika", t0)
	out, err := se.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Errored != 1 {
		t.Fatalf("Errored = %d, want 1 (the bad subscription)", out.Errored)
	}
	if out.Matched != 1 {
		t.Fatalf("Matched = %d, want 1 (the good subscription still ran)", out.Matched)
	}
	_ = good
}

// subscriptionButler makes one Agent and a coordinator-fenced Ledger for
// the subscription engine's own tests.
func subscriptionButler(t *testing.T, db *runtimedb.DB) (runtimedb.Butler, *butlerstore.Ledger) {
	t.Helper()
	if err := db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateButler(runtimedb.Butler{
		Name: "alfred", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
		Model: "kitchen-box",
	}, "mika"); err != nil {
		t.Fatal(err)
	}
	b, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := db.AcquireCoordinator("subscriptions-test")
	if err != nil {
		t.Fatal(err)
	}
	ledger := butlerstore.New(db, lease)
	return b, ledger
}

// mustSubscribe inserts and immediately approves a subscription — this
// package's tests exercise the ENGINE, not the propose/approve path
// (that is api's own evidence), so it goes straight to the DB layer
// exactly like an approved Plan would.
func mustSubscribe(t *testing.T, db *runtimedb.DB, butlerSeq int64, matchKind, matchJSON string, caps []string) int64 {
	t.Helper()
	id, err := db.InsertAgentSubscription(butlerSeq, matchKind, matchJSON, "", caps, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveAgentSubscription(id, "mika"); err != nil {
		t.Fatal(err)
	}
	return id
}
