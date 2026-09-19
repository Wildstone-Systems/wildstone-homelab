package runtimedb

// Slice 9 PR 2's evidence (ADR-W033 design §6.2, §6.3, §6.4): a bundle
// absorbs what matches and nothing else, an opt-in tool is only ever
// reached by name, and enrolling an Agent cannot be undone by emptying
// it.

import (
	"strings"
	"testing"
)

// governed returns an agentFixture with alfred (owner) enrolled.
func governedFixture(t *testing.T) (*DB, int64) {
	t.Helper()
	db := agentFixture(t)
	alfred, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GovernAgent(alfred.Seq, "mika"); err != nil {
		t.Fatalf("GovernAgent: %v", err)
	}
	return db, alfred.Seq
}

// ask resolves one tool for alfred, with the service/class/opt-in facts
// a gateway would supply.
func ask(t *testing.T, db *DB, service, class, tool string, optIn bool) ToolAuthority {
	t.Helper()
	got, err := db.ResolveButlerTool("alfred", ToolQuery{
		Tool: tool, Structural: true, Service: service, ToolClass: class, OptIn: optIn,
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// THE ACCEPTANCE NEGATIVE (§18.3, §6.3).
//
// A bundle is "all <class> tools for <service>". It absorbs a tool
// nobody had heard of when the bundle was granted — that is its whole
// reason for existing — which is exactly why the two things it must
// NEVER absorb are worth a test of their own.
func TestDynamicGrantAbsorbsMatchingToolsButNotHigherClassOrOptIn(t *testing.T) {
	db, alfred := governedFixture(t)

	// "All member tools for photos."
	if err := db.GrantToolBundle(alfred, "photos", AgentClassMember, "mika"); err != nil {
		t.Fatalf("GrantToolBundle: %v", err)
	}

	// ABSORBS: a matching tool, including one that did not exist when
	// the bundle was granted. Nothing was told about this tool name in
	// advance — the bundle stores the question, not the answer.
	if got := ask(t, db, "photos", AgentClassMember, "a_tool_invented_later", false); !got.Allowed {
		t.Errorf("the bundle did not absorb a matching member tool: %s", got.Reason)
	}

	// NOT HIGHER CLASS. An owner-class tool in the same service is not
	// covered by a member-class bundle, or "all Member tools" would be
	// a way to hand over the owner ones by writing the service twice.
	got := ask(t, db, "photos", AgentClassOwner, "delete_everything", false)
	if got.Allowed {
		t.Error("a member-class bundle absorbed an OWNER-class tool in the same service")
	}

	// NOT OPT-IN, even though its service and class both match. §6.2
	// exists precisely so an unusually consequential tool can sit inside
	// a service without being swept up.
	got = ask(t, db, "photos", AgentClassMember, "purge_originals", true)
	if got.Allowed {
		t.Error("a bundle absorbed an opt-in tool whose service and class matched — " +
			"that is the one thing §6.2 exists to prevent")
	}
	if !strings.Contains(got.Reason, "opt-in") {
		t.Errorf("the refusal does not say it was opt-in, so it reads as a missing "+
			"bundle somebody could add: %q", got.Reason)
	}

	// NOT ANOTHER SERVICE, which is the third way a bundle could be too
	// wide and the one a "matches" check gets wrong by leaving out a
	// clause rather than by getting a rule backwards.
	if got := ask(t, db, "documents", AgentClassMember, "a_tool_invented_later", false); got.Allowed {
		t.Error("a photos bundle absorbed a tool from documents")
	}
}

// NEGATIVE: an opt-in tool IS reachable when somebody names it.
//
// The counterpart to the case above, and the reason opt-in is a filter
// on bundles rather than a ban. Without this, "excluded from bundles"
// could be implemented as "never allowed" and every test above would
// still pass.
func TestAnOptInToolCanStillBeAssignedByName(t *testing.T) {
	db, alfred := governedFixture(t)
	if err := db.GrantToolBundle(alfred, "photos", AgentClassMember, "mika"); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, db, "photos", AgentClassMember, "purge_originals", true); got.Allowed {
		t.Fatal("the fixture's opt-in tool was already reachable")
	}

	if err := db.AssignTool(alfred, "purge_originals", "mika"); err != nil {
		t.Fatalf("AssignTool: %v", err)
	}
	got := ask(t, db, "photos", AgentClassMember, "purge_originals", true)
	if !got.Allowed {
		t.Errorf("an opt-in tool assigned BY NAME is still refused: %s — §6.2 keeps such "+
			"a tool out of bundles, not out of the agent", got.Reason)
	}
}

// NEGATIVE: enrolling is one-way, and emptying an Agent does not widen
// it.
//
// The obvious implementation of "governed" is "has at least one
// assignment", and it inverts: an Owner removing the last tool would
// hand the Agent everything the gateway registers. Revoking all access
// would grant all access, at the exact moment somebody was locking an
// Agent down.
func TestRemovingTheLastAssignmentDoesNotWidenTheAgent(t *testing.T) {
	db, alfred := governedFixture(t)
	if err := db.AssignTool(alfred, "get_health", "mika"); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, db, ServiceSystem, AgentClassOwner, "get_health", false); !got.Allowed {
		t.Fatalf("the assigned tool was not reachable: %s", got.Reason)
	}

	if err := db.UnassignTool(alfred, "get_health", "mika"); err != nil {
		t.Fatal(err)
	}
	// Still governed, so still default-deny — for the tool just removed
	// AND for everything else.
	if got := ask(t, db, ServiceSystem, AgentClassOwner, "get_health", false); got.Allowed {
		t.Error("the tool came back after being unassigned")
	}
	if got := ask(t, db, ServiceSystem, AgentClassOwner, "propose_change", false); got.Allowed {
		t.Error("emptying a governed agent handed it everything the gateway registers — " +
			"governance must be its own fact, never inferred from having assignments")
	}
	governed, err := db.AgentIsGoverned(alfred)
	if err != nil {
		t.Fatal(err)
	}
	if !governed {
		t.Error("the agent stopped being governed when its last assignment went")
	}
}

// The upgrade window fails open.
//
// Every Agent ends up governed, but there is a moment between 0059
// applying and BackfillAgentTools running when one is not. An Agent
// caught in it must reach what it always did rather than nothing — a
// restart that left every Agent unable to act until a backfill finished
// would be an outage, and this is the one place in the model that
// deliberately fails open.
func TestAnUnenrolledAgentStillWorksDuringTheUpgradeWindow(t *testing.T) {
	db := agentFixture(t)
	hobson, err := db.GetButler("hobson")
	if err != nil {
		t.Fatal(err)
	}
	governed, err := db.AgentIsGoverned(hobson.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if governed {
		t.Fatal("an agent nobody enrolled is governed")
	}

	// No assignments, no bundles, and it still reaches a structurally
	// present tool — exactly as before 0059.
	got, err := db.ResolveButlerTool("hobson", ToolQuery{
		Tool: "get_household_events", Structural: true,
		Service: ServiceSystem, ToolClass: AgentClassMember,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Errorf("an agent caught mid-upgrade lost a tool it could reach before 0059: %s — "+
			"the window before the backfill must fail open", got.Reason)
	}

	// And its narrowings still work, which is the model it is still on.
	if err := db.NarrowButlerTool("hobson", "get_household_events", "not for this one", "mika"); err != nil {
		t.Fatalf("NarrowButlerTool: %v", err)
	}
	got, err = db.ResolveButlerTool("hobson", ToolQuery{
		Tool: "get_household_events", Structural: true,
		Service: ServiceSystem, ToolClass: AgentClassMember,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Error("a narrowing stopped working for an agent caught mid-upgrade")
	}
}

// NEGATIVE: a governed tool that the gateway does not register is
// ABSENT, not unassigned.
//
// The two refusals send a person to different places — one to the
// assignment screen, one to "this surface does not have that" — and
// collapsing them would have an Owner adding an assignment that could
// never help.
func TestAnAbsentToolIsStructuralNotUnassigned(t *testing.T) {
	db, _ := governedFixture(t)
	got, err := db.ResolveButlerTool("alfred", ToolQuery{
		Tool: "not_a_real_tool", Structural: false,
		Service: ServiceSystem, ToolClass: AgentClassOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatal("a tool the gateway does not register was allowed")
	}
	if got.Source != "structural" {
		t.Errorf("an absent tool was refused as %q, want structural", got.Source)
	}
}

// NEGATIVE: a Member Agent cannot be given an owner-class bundle.
//
// The bundle equivalent of PR 1's grant rule (§6.1). Without it the row
// would cover nothing today and become real the moment the Agent was
// upgraded — a permission arriving through a change nobody connected
// to it.
func TestAMemberAgentCannotHoldAnOwnerClassBundle(t *testing.T) {
	db := agentFixture(t)
	hobson, err := db.GetButler("hobson")
	if err != nil {
		t.Fatal(err)
	}
	err = db.GrantToolBundle(hobson.Seq, "photos", AgentClassOwner, "mika")
	if err == nil {
		t.Fatal("a member agent was given an owner-class bundle")
	}
	if !strings.Contains(err.Error(), "member agent") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	// But a member-class bundle is fine, and an OWNER agent may hold
	// one too — §6.1's ordering, same as grants.
	if err := db.GrantToolBundle(hobson.Seq, "photos", AgentClassMember, "mika"); err != nil {
		t.Errorf("a member agent was refused a member-class bundle: %v", err)
	}
	alfred, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GrantToolBundle(alfred.Seq, "photos", AgentClassMember, "mika"); err != nil {
		t.Errorf("an owner agent was refused a member-class bundle: %v", err)
	}
}
