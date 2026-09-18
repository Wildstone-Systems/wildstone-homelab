package runtimedb

// B2's evidence (ADR-W031 A3/A4/A5): one Butler's material does not
// reach another Butler's model, a Butler's tools only ever narrow, and
// connections fail closed.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The claim the whole design rests on: material given to a Butler on a
// local model is not reachable by a Butler on a cloud one (A4).
func TestOneButlersMaterialDoesNotReachAnothersModel(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box") // local
	newButler(t, db, "nutrition", "cloud")    // cloud
	for _, b := range []string{"health", "nutrition"} {
		if err := db.GrantButlerAccess(b, "sam", "mika"); err != nil {
			t.Fatal(err)
		}
	}

	// Sam tells the health Butler something. It is SAM'S material —
	// they own it — and the health Butler is what holds it.
	private, err := db.RecordButlerMemory("health", MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "sam's knee gives out on stairs",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "sam", SourceKind: "conversation", SourceID: "h-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.IndexDocumentInto(IndexDocument{
		SourceKind: "memory", SourceID: "h-1", MemorySeq: private, Owner: "household",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Butler: "health",
		Content: "sam's knee gives out on stairs", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// The health Butler can retrieve it.
	mine, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Seq != private {
		t.Fatalf("the butler holding the material could not retrieve it: %+v", mine)
	}

	// The nutrition Butler cannot — and this is the point. Sam is the
	// same person, with the same permissions, asking the same database.
	// What changed is which Butler is asking, and which model would
	// therefore see the answer.
	theirs, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "nutrition"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range theirs {
		if r.Seq == private {
			t.Fatal("material held by the local-model butler reached the cloud-model one")
		}
	}
	hits, err := db.SearchIndex(IndexQuery{Reader: "sam", Butler: "nutrition", Text: "knee"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("the cloud butler found the local butler's material by search: %+v", hits)
	}
	// And it is still there for the one that holds it.
	if hits, _ := db.SearchIndex(IndexQuery{Reader: "sam", Butler: "health", Text: "knee"}); len(hits) != 1 {
		t.Fatalf("the holding butler lost its own material to the scope filter: %+v", hits)
	}
}

// The Butler scope narrows ON TOP OF the reader's permission and never
// instead of it. A Butler is not a way to read someone else's private
// material.
func TestAButlerIsNotARouteAroundAPersonsPermission(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	if err := db.GrantButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatal(err)
	}

	// Mika's private material, held by the same Butler sam uses.
	secret, err := db.RecordButlerMemory("health", MemoryRecord{
		Kind: MemoryFact, Subject: "mika", Content: "mika's blood pressure reading",
		Audience: ButlerAudiencePrivate, PermissionLabel: "mika", Owner: "mika",
		Author: "mika", SourceKind: "conversation", SourceID: "h-2",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Seq == secret {
			t.Fatal("sharing a butler with someone exposed their private material")
		}
	}
	// Its owner still sees it through that Butler.
	if own, _ := db.RetrieveMemory(MemoryQuery{Reader: "mika", Butler: "health"}); len(own) != 1 {
		t.Fatalf("the owner could not see their own record: %+v", own)
	}
}

// Omitting the Butler must fail CLOSED. A caller that forgets to name
// one sees the shared pool and nothing specialised.
func TestForgettingTheButlerScopeShowsOnlyTheSharedPool(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")

	if _, err := db.RecordButlerMemory("health", MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "specialised material",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "sam", SourceKind: "conversation", SourceID: "h-3",
	}); err != nil {
		t.Fatal(err)
	}
	pool, err := db.RecordButlerMemory(SharedPool, MemoryRecord{
		Kind: MemoryPreference, Subject: "household", Content: "we are vegetarian",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "mika", SourceKind: "conversation", SourceID: "h-4",
	})
	if err != nil {
		t.Fatal(err)
	}

	// No Butler named at all.
	got, err := db.RetrieveMemory(MemoryQuery{Reader: "sam"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != pool {
		t.Fatalf("an unscoped read returned %+v, want only the shared pool", got)
	}
	// The Console holder's view is explicit, and sees everything.
	all, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: AllButlers})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("the console view returned %d records, want both", len(all))
	}
	// And a named Butler sees its own material plus the pool, because
	// household facts are not somebody's specialism.
	scoped, _ := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "health"})
	if len(scoped) != 2 {
		t.Fatalf("a butler saw %d records, want its own and the shared pool", len(scoped))
	}
}

// A5: promoting to the shared pool is a disclosure decision, so it says
// which Butlers gain access and whether any of them sends material out
// of the house.
func TestPromotingToTheSharedPoolNamesWhoGains(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box") // local
	newButler(t, db, "nutrition", "cloud")    // cloud

	seq, err := db.RecordButlerMemory("health", MemoryRecord{
		Kind: MemoryPreference, Subject: "household", Content: "we are vegetarian",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "sam", SourceKind: "conversation", SourceID: "h-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	gains, leavesHouse, err := db.PromoteMemoryToSharedPool(seq, "mika")
	if err != nil {
		t.Fatal(err)
	}
	if len(gains) != 2 {
		t.Fatalf("promotion reported %d butlers gaining access, want both", len(gains))
	}
	if !leavesHouse {
		t.Fatal("promotion did not report that a cloud-model butler gains access, so nothing could warn")
	}
	// It really is in the pool now.
	if got, _ := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "nutrition"}); len(got) != 1 {
		t.Fatalf("a promoted record did not reach the other butler: %+v", got)
	}

	// A private record cannot be promoted: that would widen its
	// audience as a side effect of widening its scope — two decisions
	// wearing one button.
	priv, err := db.RecordButlerMemory("health", MemoryRecord{
		Kind: MemoryFact, Subject: "mika", Content: "mika's reading",
		Audience: ButlerAudiencePrivate, PermissionLabel: "mika", Owner: "mika",
		Author: "mika", SourceKind: "conversation", SourceID: "h-6",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PromoteMemoryToSharedPool(priv, "mika"); err == nil {
		t.Fatal("a private record was promoted into the pool every butler reads")
	}
}

// A3: a Butler's tools only ever narrow. A Butler-level control can
// switch a tool off and can never switch one on.
func TestAButlersToolsOnlyNarrow(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")

	// The profile allows it; the Butler does not need it.
	if err := db.NarrowButlerTool("health", "send_message", "it only reads", "mika"); err != nil {
		t.Fatal(err)
	}
	a, err := db.ResolveButlerTool("health", ToolQuery{
		Tool: "send_message", Structural: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed {
		t.Fatal("a narrowed tool was still allowed")
	}
	if !strings.Contains(a.Reason, "health") {
		t.Fatalf("the refusal does not name the butler: %q", a.Reason)
	}
	if !a.Requestable {
		t.Fatal("a narrowed tool is not requestable, so nobody can ask for it back")
	}

	// The schema refuses an 'enabled' row outright, so no resolver bug
	// can turn this table into a widening one.
	if _, err := db.ExecForTest(
		`INSERT INTO butler_tool_scope (butler, tool, state, updated_at, updated_by)
		 VALUES ((SELECT seq FROM butlers WHERE name='health'), 'unlock_door', 'enabled', 'now', 'mika')`,
	); err == nil {
		t.Fatal("a butler-level control enabled a tool, which would make the ceiling advisory")
	}

	// Removing the narrowing returns the tool to what the profile says
	// — not to something wider.
	if err := db.WidenButlerTool("health", "send_message"); err != nil {
		t.Fatal(err)
	}
	back, err := db.ResolveButlerTool("health", ToolQuery{Tool: "send_message", Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	if !back.Allowed {
		t.Fatalf("removing a narrowing did not restore the profile's answer: %+v", back)
	}
	// A tool the profile itself disabled stays disabled: the narrowing
	// was never what was holding it.
	if err := db.SetToolControl(ProfileHousehold, "unlock_door", ToolDisabled,
		"root-admin", "not on this surface"); err != nil {
		t.Fatal(err)
	}
	if err := db.NarrowButlerTool("health", "unlock_door", "", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.WidenButlerTool("health", "unlock_door"); err != nil {
		t.Fatal(err)
	}
	still, err := db.ResolveButlerTool("health", ToolQuery{Tool: "unlock_door", Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	if still.Allowed {
		t.Fatal("removing a butler narrowing re-enabled a tool the profile had disabled")
	}
}

// Connections fail closed: absent means no.
func TestAButlerReadsOnlyTheConnectionsItWasGiven(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	newButler(t, db, "accounting", "cloud")
	for _, c := range []string{"gym-tracker", "bank-feed"} {
		if _, err := db.CreateConnection(IntegrationConnection{
			Name: c, Adapter: "caldav", Ownership: OwnershipHousehold,
			Owner: "household", CreatedBy: "root-admin",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A brand-new Butler reads nothing until it is given something.
	if got, _ := db.ButlerConnections("health"); len(got) != 0 {
		t.Fatalf("a new butler already reads %v", got)
	}
	if err := db.ButlerMayReadConnection("health", "gym-tracker"); !errors.Is(err, ErrNoButlerAccess) {
		t.Fatalf("an ungranted connection was readable: %v", err)
	}

	if err := db.GrantButlerConnection("health", "gym-tracker", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.ButlerMayReadConnection("health", "gym-tracker"); err != nil {
		t.Fatalf("a granted connection was refused: %v", err)
	}
	// The accounting Butler still cannot see the gym data, and the
	// health Butler cannot see the bank feed. Neither was refused —
	// neither was ever given it.
	if err := db.ButlerMayReadConnection("accounting", "gym-tracker"); !errors.Is(err, ErrNoButlerAccess) {
		t.Fatalf("the accounting butler read gym data: %v", err)
	}
	if err := db.ButlerMayReadConnection("health", "bank-feed"); !errors.Is(err, ErrNoButlerAccess) {
		t.Fatalf("the health butler read the bank feed: %v", err)
	}

	if err := db.RevokeButlerConnection("health", "gym-tracker", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.ButlerMayReadConnection("health", "gym-tracker"); !errors.Is(err, ErrNoButlerAccess) {
		t.Fatalf("a revoked connection was still readable: %v", err)
	}
}

// One place to answer "what does this Butler see", so a person does not
// have to check three screens to find out.
func TestButlerScopeSummarisesTheReach(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	if _, err := db.CreateConnection(IntegrationConnection{
		Name: "gym-tracker", Adapter: "caldav", Ownership: OwnershipHousehold,
		Owner: "household", CreatedBy: "root-admin",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantButlerConnection("health", "gym-tracker", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.NarrowButlerTool("health", "send_message", "it only reads", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordButlerMemory("health", MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "sam runs on tuesdays",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "sam", SourceKind: "conversation", SourceID: "h-7",
	}); err != nil {
		t.Fatal(err)
	}

	s, err := db.ButlerScope("health")
	if err != nil {
		t.Fatal(err)
	}
	if s.Model != "kitchen-box" || s.ModelKind != ModelLocal {
		t.Fatalf("scope reports model %q (%s)", s.Model, s.ModelKind)
	}
	if len(s.Connections) != 1 || s.Connections[0] != "gym-tracker" {
		t.Fatalf("scope connections = %v", s.Connections)
	}
	if _, ok := s.Narrowed["send_message"]; !ok {
		t.Fatalf("scope did not report the narrowed tool: %+v", s.Narrowed)
	}
	if s.MemoryCount != 1 {
		t.Fatalf("scope memory count = %d, want 1", s.MemoryCount)
	}
}

// Found in review of B1–B7: the Butler scope was carried by a separate
// UPDATE rather than by the record, so every path that DERIVES or
// REPLACES a record dropped it into the shared pool — the same leak A4
// forbids, arriving through the edit path instead of the read path.
//
// This is the third time this shape of bug has appeared: P8 had a
// derivative inherit the wrong owner, and these two had it inherit no
// butler. Hence the test covers both edit paths rather than the one
// that was reported.
func TestEditingAScopedRecordKeepsItScoped(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	newButler(t, db, "nutrition", "cloud")

	held, err := db.RecordButlerMemory("health", MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "sam's knee gives out on stairs",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "sam", SourceKind: "conversation", SourceID: "e-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A correction stays in the scope it corrects.
	corrected, err := db.CorrectMemory(held, MemoryRecord{
		Content: "sam's knee gives out on stairs and hills",
	}, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := db.GetMemory(corrected); m.Butler != "health" {
		t.Fatalf("a correction moved the record to butler %q — the shared pool is every model", m.Butler)
	}
	if got, _ := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "nutrition"}); len(got) != 0 {
		t.Fatalf("a correction published scoped material to another butler: %+v", got)
	}

	// So does a derivative, whatever its audience.
	derived, err := db.DeriveMemory(MemoryRecord{
		Kind: MemoryInference, Subject: "sam", Content: "sam should avoid stairs",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "sam", SourceKind: "derivation", SourceID: "e-2",
	}, []int64{corrected})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := db.GetMemory(derived); m.Butler != "health" {
		t.Fatalf("a derivative landed in butler %q rather than its source's scope", m.Butler)
	}
	if got, _ := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "nutrition"}); len(got) != 0 {
		t.Fatalf("a summary published scoped material to another butler: %+v", got)
	}

	// Two Butlers' material has no correct scope for a derivative, so
	// the derivation is refused rather than guessed.
	other, err := db.RecordButlerMemory("nutrition", MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "sam eats late",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "sam", SourceKind: "conversation", SourceID: "e-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeriveMemory(MemoryRecord{
		Kind: MemoryInference, Subject: "sam", Content: "a summary of both",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "butler", SourceKind: "derivation", SourceID: "e-4",
	}, []int64{corrected, other}); err == nil {
		t.Fatal("a derivative mixed two butlers' material and was given one of their scopes")
	}
}

// A Butler literally named "*" would match the AllButlers sentinel in
// every scope filter — A4 defeated by a name.
func TestAButlerCannotBeNamedTheEveryButlerSentinel(t *testing.T) {
	db := butlerFixture(t)
	if _, err := db.CreateButler(Butler{
		Name: AllButlers, Surface: ButlerSurfaceFamily, Profile: ProfileHousehold,
		Model: "kitchen-box",
	}, "mika"); err == nil {
		t.Fatal("a butler was created with the name that means every butler")
	}
}

// Disabling a route has to stop the Butlers already assigned to it. The
// 0032 triggers only fire when a butler's route is WRITTEN, so a route
// turned off afterwards would otherwise keep answering.
func TestDisablingARouteStopsTheButlersAlreadyOnIt(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	if err := db.GrantButlerAccess("health", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResolveButlerFor("health", "sam"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetModelEnabled("kitchen-box", false, "mika"); err != nil {
		t.Fatal(err)
	}
	_, err := db.ResolveButlerFor("health", "sam")
	if !errors.Is(err, ErrButlerUnreachable) {
		t.Fatalf("a butler on a disabled route still answered: %v", err)
	}
	if !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("the refusal does not say the model was turned off: %v", err)
	}
}
