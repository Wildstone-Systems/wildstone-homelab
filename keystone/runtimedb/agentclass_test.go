package runtimedb

// Slice 9's evidence (ADR-W033 design §5.2, §18.3): an Agent's class is
// its own axis, it can move, and moving it takes the right things with
// it and leaves the wrong things alone.
//
// Four of these are negative cases. That is not padding — the whole
// slice is about what CANNOT happen, and a suite of positives would pass
// with every hole still open.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// agentFixture gives a database two models, two admitted people, and
// one Agent of each class.
func agentFixture(t *testing.T) *DB {
	t.Helper()
	db := butlerFixture(t)
	for _, b := range []struct {
		name    string
		class   string
		surface ButlerSurface
	}{
		{"alfred", AgentClassOwner, ButlerSurfaceAdmin},
		{"hobson", AgentClassMember, ButlerSurfaceFamily},
	} {
		if _, err := db.CreateButler(Butler{
			Name: b.name, Directive: "look after the house",
			Surface: b.surface, AgentClass: b.class, Model: "kitchen-box",
		}, "root-admin"); err != nil {
			t.Fatalf("creating %s: %v", b.name, err)
		}
	}
	return db
}

// grantFor issues a live grant to one Agent against a task of its own,
// at the Agent's own class.
func grantFor(t *testing.T, db *DB, butler, tool string) (grantSeq int64, taskID string) {
	t.Helper()
	b, err := db.GetButler(butler)
	if err != nil {
		t.Fatalf("GetButler(%s): %v", butler, err)
	}
	return grantForAt(t, db, butler, b.AgentClass, tool)
}

// grantForAt is the same, at a class the caller names — so a test can
// give an Owner Agent a MEMBER grant, which §6.1 allows and §6.4's
// downgrade has to take away along with everything else.
func grantForAt(t *testing.T, db *DB, butler, class, tool string) (grantSeq int64, taskID string) {
	t.Helper()
	b, err := db.GetButler(butler)
	if err != nil {
		t.Fatalf("GetButler(%s): %v", butler, err)
	}
	owner := "sam"
	if b.AgentClass == AgentClassOwner {
		owner = "mika"
	}
	taskID, err = db.InsertButlerTask(ButlerTask{
		Kind: ButlerTaskConversation, Author: owner, Owner: owner,
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: ButlerAudienceShared,
		Objective: "something to hang a grant on", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	grantSeq, err = db.GrantTool(ToolGrant{
		Scope: GrantScopeTask, ScopeID: taskID, AgentClass: class,
		Butler: b.Seq, Tool: tool, GrantedBy: "root-admin",
	}, time.Hour)
	if err != nil {
		t.Fatalf("GrantTool: %v", err)
	}
	return grantSeq, taskID
}

// live reports whether a grant is still in force.
func live(t *testing.T, db *DB, seq int64) bool {
	t.Helper()
	var revoked string
	if err := db.sql.QueryRow(
		`SELECT revoked_at FROM butler_tool_grants WHERE seq = ?`, seq).Scan(&revoked); err != nil {
		t.Fatalf("reading grant %d: %v", seq, err)
	}
	return revoked == ""
}

// THE CENTRAL NEGATIVE CASE (§18.3).
//
// Three separate doors, because "cannot be configured with" has to mean
// all three or it means none of them: the class itself cannot be written
// onto the wrong surface, a grant cannot be issued across classes, and a
// control that names the wrong class cannot make an absent tool present.
func TestMemberAgentCannotBeConfiguredWithAnOwnerTool(t *testing.T) {
	db := agentFixture(t)

	// 1. A Member Agent cannot be put on the surface that has the owner
	//    tools. The class and the surface are one fact.
	//
	//    The MESSAGE is asserted, not just the refusal. The schema's
	//    paired CHECK refuses this too, so a test that only asked "did
	//    it fail" would pass with the write path's check deleted — and
	//    an Owner would get "CHECK constraint failed: butlers" where
	//    they should get a sentence naming what disagreed with what.
	//    Both layers are wanted; only this pins which one answered.
	_, err := db.CreateButler(Butler{
		Name: "impostor", Directive: "x",
		Surface: ButlerSurfaceAdmin, AgentClass: AgentClassMember, Model: "kitchen-box",
	}, "root-admin")
	if err == nil {
		t.Fatal("a member agent was created on the admin surface — the class names the " +
			"ceiling and the surface enforces it, so a mismatch is a ceiling nobody enforces")
	}
	if !strings.Contains(err.Error(), "does not belong on the admin surface") {
		t.Errorf("the refusal came from the schema rather than the write path: %v", err)
	}

	// 2. An owner-class grant cannot be issued against a Member Agent.
	//    Without this the row would belong to nobody: invisible to that
	//    Agent's resolution and untouched by its downgrade.
	hobson, err := db.GetButler("hobson")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.InsertButlerTask(ButlerTask{
		Kind: ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember,
		Audience: ButlerAudienceShared, Objective: "x", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.GrantTool(ToolGrant{
		Scope: GrantScopeTask, ScopeID: taskID, AgentClass: AgentClassOwner,
		Butler: hobson.Seq, Tool: "propose_change", GrantedBy: "root-admin",
	}, time.Hour)
	if err == nil {
		t.Error("an owner-class grant was issued to a member agent")
	} else if !strings.Contains(err.Error(), "member agent") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	//    But the rule is an ORDERING, not an equality (§6.1): "member
	//    tools may be assigned to Member or Owner Agents". An Owner
	//    Agent holding a member-class grant is the ordinary case — §6.4
	//    gives it every member tool by default — and refusing it would
	//    break most of what any Agent does.
	if _, _ = grantForAt(t, db, "alfred", AgentClassMember, "get_household_events"); true {
		// grantForAt fatals on refusal, so reaching here is the
		// assertion: an equality check would have failed above.
		_ = hobson
	}

	// 3. And a control naming the member class cannot conjure a tool the
	//    member gateway does not register. The control stores fine —
	//    it is an intent — but the resolver still refuses, because
	//    structural comes first and nothing below it can widen it.
	if err := db.SetToolControl(AgentClassMember, "propose_change", ToolEnabled,
		"root-admin", "let them"); err != nil {
		t.Fatalf("SetToolControl: %v", err)
	}
	got, err := db.ResolveButlerTool("hobson", ToolQuery{
		Tool: "propose_change", Structural: false, TaskID: taskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Error("an enabled control let a member agent reach an owner tool — a control " +
			"can turn an existing capability off and on, never add one")
	}
	if got.Source != "structural" {
		t.Errorf("the refusal came from %q, want structural: a tool this class does not "+
			"have is ABSENT, not switched off, and reporting it as a switch invites "+
			"somebody to go looking for the switch", got.Source)
	}
}

// Downgrading strips ALL of this Agent's grants (§18.3, §6.4).
//
// All, not only the owner-class ones. §6.4 is explicit — "strips all
// existing tool grants, after which appropriate Member grants can be
// reapplied" — and the wider rule is the safer one: a downgrade is a
// decision that this Agent should be trusted with less, so it starts
// from nothing and an Owner puts back what they meant.
func TestDowngradingAnOwnerAgentStripsOwnerToolGrants(t *testing.T) {
	db := agentFixture(t)
	ownerGrant, _ := grantFor(t, db, "alfred", "propose_change")
	memberGrant, _ := grantForAt(t, db, "alfred", AgentClassMember, "get_household_events")
	if !live(t, db, ownerGrant) || !live(t, db, memberGrant) {
		t.Fatal("the fixture's grants were not live to begin with")
	}

	revoked, err := db.SetAgentClass("alfred", AgentClassMember, "mika")
	if err != nil {
		t.Fatalf("SetAgentClass: %v", err)
	}
	if revoked != 2 {
		t.Errorf("the downgrade reported %d grants revoked, want 2 — §6.4 strips ALL "+
			"existing grants, not only the owner-class ones", revoked)
	}
	if live(t, db, ownerGrant) {
		t.Error("an owner-class grant survived the downgrade — the agent can no longer " +
			"reach that tool structurally, and a grant that outlives the class comes " +
			"back the moment somebody upgrades the agent again")
	}
	if live(t, db, memberGrant) {
		t.Error("a member-class grant survived the downgrade — §6.4 says a downgrade is " +
			"a clean slate, so an Owner reapplies what they meant rather than the " +
			"agent keeping whatever happened to be member-class already")
	}

	// The surface moved with the class, or the record says one thing and
	// the gateway enforces another.
	b, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if b.AgentClass != AgentClassMember {
		t.Errorf("agent class = %q, want member", b.AgentClass)
	}
	if b.Surface != ButlerSurfaceFamily {
		t.Errorf("surface = %q, want family — the class moved and the surface did not", b.Surface)
	}

	// NEGATIVE: moving one without the other is refused, and refused
	// with a sentence.
	//
	// The paired CHECK would reject this row anyway, so the assertion is
	// on the MESSAGE: without the trigger an Owner gets "CHECK
	// constraint failed: butlers", which says nothing about what they
	// did. "You moved one of them" and "this row is invalid" are very
	// different things to debug at two in the morning.
	_, err = db.sql.Exec(`UPDATE butlers SET agent_class = ? WHERE name = 'hobson'`, AgentClassOwner)
	if err == nil {
		t.Fatal("an agent's class moved without its surface — the class would then name " +
			"a ceiling the gateway does not enforce")
	}
	if !strings.Contains(err.Error(), "move together or not at all") {
		t.Errorf("the refusal came from the schema's CHECK rather than the trigger, so it "+
			"does not say what went wrong: %v", err)
	}
}

// NEGATIVE: upgrading adds nothing back, and specifically does not
// revive what the downgrade took (§18.3).
//
// The asymmetry is the safety argument for letting the class move at
// all. A wider gateway with no grants on it is a wider gateway nobody
// can use yet.
func TestUpgradingAMemberAgentDoesNotAutoAddOwnerTools(t *testing.T) {
	db := agentFixture(t)
	grant, taskID := grantFor(t, db, "alfred", "propose_change")
	if _, err := db.SetAgentClass("alfred", AgentClassMember, "mika"); err != nil {
		t.Fatal(err)
	}

	// Back up again.
	revoked, err := db.SetAgentClass("alfred", AgentClassOwner, "mika")
	if err != nil {
		t.Fatalf("SetAgentClass: %v", err)
	}
	if revoked != 0 {
		t.Errorf("an upgrade revoked %d grants; it should touch none", revoked)
	}
	if live(t, db, grant) {
		t.Fatal("the upgrade revived a revoked grant — a permission nobody decided to " +
			"give back, arriving as a side effect of a class change")
	}
	got, err := db.ResolveButlerTool("alfred", ToolQuery{
		Tool: "propose_change", Structural: true, GrantRequired: true, TaskID: taskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Error("an upgraded agent holds an owner tool it was never granted")
	}

	// And a Member Agent upgraded for the first time gains no owner
	// tools — but KEEPS the member grants it already had. §6.4 is
	// explicit about both halves, and they pull in opposite directions:
	// an upgrade that stripped member grants would punish the agent for
	// being promoted.
	kept, keptTask := grantFor(t, db, "hobson", "get_household_events")
	if _, err := db.SetAgentClass("hobson", AgentClassOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	if !live(t, db, kept) {
		t.Error("upgrading a member agent revoked the member grants it already held — " +
			"§6.4 preserves those; only the owner tools are withheld")
	}
	stillWorks, err := db.ResolveButlerTool("hobson", ToolQuery{
		Tool: "get_household_events", Structural: true, GrantRequired: true, TaskID: keptTask,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !stillWorks.Allowed {
		t.Errorf("the preserved grant no longer resolves: %s", stillWorks.Reason)
	}
	grants, err := db.ListToolGrants(GrantScopeTask, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		hobson, err := db.GetButler("hobson")
		if err != nil {
			t.Fatal(err)
		}
		if g.Butler == hobson.Seq && g.RevokedAt.IsZero() {
			t.Errorf("a freshly upgraded agent holds grant %d", g.Seq)
		}
	}
}

// NEGATIVE: one Agent's downgrade is not another's.
//
// This is the whole reason 0058 put a butler column on the grants table.
// Before it, grants belonged to a CLASS, so "strip its owner grants"
// could only mean "strip every owner agent's".
func TestDowngradeDoesNotStripAnotherAgentsGrants(t *testing.T) {
	db := agentFixture(t)
	if _, err := db.CreateButler(Butler{
		Name: "jarvis", Directive: "the other one",
		Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Model: "kitchen-box",
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	alfreds, _ := grantFor(t, db, "alfred", "propose_change")
	jarvis, _ := grantFor(t, db, "jarvis", "propose_change")

	// A class-level grant too: the pre-0058 shape, belonging to no
	// Agent, which a downgrade has no business touching either.
	classLevel, err := db.GrantTool(ToolGrant{
		Scope: GrantScopeRoutine, ScopeID: "nightly", AgentClass: AgentClassOwner,
		Tool: "propose_change", GrantedBy: "root-admin",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	revoked, err := db.SetAgentClass("alfred", AgentClassMember, "mika")
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Errorf("the downgrade revoked %d grants, want exactly its own 1", revoked)
	}
	if live(t, db, alfreds) {
		t.Error("the downgraded agent kept its own grant")
	}
	if !live(t, db, jarvis) {
		t.Error("downgrading alfred revoked jarvis's grant — a permission taken from an " +
			"agent nobody changed")
	}
	if !live(t, db, classLevel) {
		t.Error("downgrading alfred revoked a class-level grant that belongs to no agent")
	}
}

// NEGATIVE: one Agent cannot use another Agent's grant.
//
// This is the security core of the butler column, and it is a
// DIFFERENT question from the downgrade. The downgrade test proves a
// strip does not reach too far; this proves a RESOLUTION does not.
// Both were needed: without this, deleting the Agent scope from
// liveGrant left every test passing while any Agent of a class could
// pick up any other's live grant.
//
// Same class, same tool, same task. Only the Agent differs — so
// nothing but the Agent scope can be what refuses.
func TestOneAgentsGrantIsNotAnothers(t *testing.T) {
	db := agentFixture(t)
	if _, err := db.CreateButler(Butler{
		Name: "jarvis", Directive: "the other one",
		Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Model: "kitchen-box",
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	_, taskID := grantFor(t, db, "jarvis", "propose_change")

	q := ToolQuery{Tool: "propose_change", Structural: true, GrantRequired: true, TaskID: taskID}

	// jarvis holds it.
	got, err := db.ResolveButlerTool("jarvis", q)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("the agent that was granted the tool cannot use it: %s", got.Reason)
	}

	// alfred does not, though it is the same class and the same work.
	got, err = db.ResolveButlerTool("alfred", q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Error("one agent used another agent's grant — the grant names an Agent, and a " +
			"resolution that ignores it hands every agent of a class every grant issued " +
			"to any of them")
	}

	// And a session with no Agent scope at all reaches only the
	// class-level rows of the pre-0058 shape, never an Agent's.
	got, err = db.ResolveToolAuthority(ToolQuery{
		AgentClass: AgentClassOwner, Tool: "propose_change",
		Structural: true, GrantRequired: true, TaskID: taskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Error("an unscoped resolution picked up an agent's grant")
	}

	// The class-level grant it SHOULD reach still works, or the scoping
	// has been tightened into refusing the legacy path outright.
	if _, err := db.GrantTool(ToolGrant{
		Scope: GrantScopeTask, ScopeID: taskID, AgentClass: AgentClassOwner,
		Tool: "propose_change", GrantedBy: "root-admin",
	}, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err = db.ResolveToolAuthority(ToolQuery{
		AgentClass: AgentClassOwner, Tool: "propose_change",
		Structural: true, GrantRequired: true, TaskID: taskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Error("a deployment with no Butlers lost its class-level grants — the pre-0058 " +
			"shape is still the one such a deployment runs on")
	}
	// ...and that class-level grant is still not an Agent's to borrow
	// in the other direction either.
	got, err = db.ResolveButlerTool("alfred", q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Error("an agent picked up a class-level grant that belongs to no agent")
	}
}

// NEGATIVE: the axes do not leak.
//
// An Owner Agent is a thing a MEMBER can be given access to, and being
// given it makes them no more an Owner than borrowing a car makes
// somebody a mechanic. §5.3 is explicit that one Agent serves several
// people at once, which is exactly why its class cannot be read as a
// statement about any of them.
func TestAgentClassIsNotPersonRole(t *testing.T) {
	db := agentFixture(t)

	// sam is a Member. Give them access to the Owner Agent.
	if err := db.GrantButlerAccess("alfred", "sam", "mika"); err != nil {
		t.Fatalf("granting access: %v", err)
	}

	m, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != RoleMember {
		t.Errorf("sam's role became %q after being given an owner agent — an agent's "+
			"class is not a grant of anything to the person using it", m.Role)
	}

	// And the reverse: an Owner using a Member Agent does not lower them.
	if err := db.GrantButlerAccess("hobson", "mika", "mika"); err != nil {
		t.Fatalf("granting access: %v", err)
	}
	owner, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	if owner.Role != RoleOwner {
		t.Errorf("mika's role became %q after using a member agent", owner.Role)
	}

	// The person's profile preset is its own axis too, and moving an
	// Agent's class must not write to it.
	if _, err := db.SetAgentClass("hobson", AgentClassOwner, "mika"); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if after.Role != m.Role || after.Profile != m.Profile {
		t.Errorf("changing an agent's class changed a person: role %q -> %q, profile %q -> %q",
			m.Role, after.Role, m.Profile, after.Profile)
	}
}

// NEGATIVE: the third axis is untouched.
//
// principals.role is a MACHINE credential's role, and 'operator' there
// has never meant what 'operator' meant on a Butler. This slice renamed
// the second and deliberately left the third alone (#201 decision 6), so
// the test that matters is that nothing moved.
func TestLegacyRoleOperatorIsUntouched(t *testing.T) {
	db := agentFixture(t)

	if err := db.CreatePrincipal("some-runner", RoleOperator, "root-admin"); err != nil {
		t.Fatalf("a machine principal could no longer be created as an operator: %v", err)
	}
	p, err := db.GetPrincipal("some-runner")
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != RoleOperator {
		t.Errorf("the principal's role came back %q, want operator", p.Role)
	}
	if !ValidRole(RoleOperator) {
		t.Error("RoleOperator stopped being a valid principal role")
	}

	// And the vocabularies really are disjoint now: neither of the
	// Agent classes is a principal role, and the legacy role is not an
	// Agent class. Before 0058 'operator' was both, which is the
	// confusion this slice exists to end.
	if ValidRole(Role(AgentClassOwner)) || ValidRole(Role(AgentClassMember)) {
		t.Error("an agent class is also a machine principal role — the axes overlap again")
	}
	if ValidAgentClass(string(RoleOperator)) {
		t.Error("the legacy machine role is also an agent class")
	}

	// A principal is not a Butler, so nothing here should have created
	// one, and SetAgentClass must not reach principals at all.
	if _, err := db.GetButler("some-runner"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a machine principal turned up as a butler: %v", err)
	}
}
