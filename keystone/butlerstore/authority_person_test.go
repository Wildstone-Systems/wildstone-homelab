package butlerstore

// Layer 1 and layer 2 are separate answers, in that order (design §10,
// §18.6). These are the tests that keep them separate: every one of
// them is a case where the AGENT would have said yes and the PERSON is
// what decides.

import (
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// personFacing is the query shape every test below varies from: a real
// person, over a real Connection, for a tool the gateway registers.
func personFacing(person, tool string) butler.ToolAuthorityQuery {
	return butler.ToolAuthorityQuery{
		Caller: butler.CallerPersonFacing, Person: person, Connection: "test_connection",
		AgentClass: runtimedb.AgentClassOwner, Tool: tool, Structural: true,
	}
}

// An Agent holding a grant does not authorize the person asking
// (§18.6). The grant is layer 2 — it says what the AGENT may reach —
// and it says nothing at all about whether this person may have work
// done for them.
//
// Slice 9's assignment model makes this newly easy to get wrong, which
// is why the grant here is live, unexpired and for exactly the tool
// being asked for. Everything below layer 1 says yes.
//
// Mutation: moving the PersonAuthority check below the resolver call —
// so a permitted tool short-circuits it — makes this fail. So does
// treating a nil PersonAuthority as "allow", which is the permissive
// default one layer down.
func TestAgentGrantIsNotCallerAuthorization(t *testing.T) {
	db, auth := authorityDB(t)

	// A person the household has heard of and never admitted. Not a
	// stranger: a half-onboarded member, which is the case that would
	// slip through a check that only looked for a missing row.
	if err := db.RecordButlerMember("guest", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}

	id := newAuthTask(t, db)
	if _, err := db.GrantTool(runtimedb.ToolGrant{
		Scope: runtimedb.GrantScopeTask, ScopeID: id, AgentClass: runtimedb.AgentClassOwner,
		Tool: "get_system_overview", GrantedBy: "root-admin",
	}, time.Hour); err != nil {
		t.Fatal(err)
	}

	q := personFacing("guest", "get_system_overview")
	q.GrantRequired, q.TaskID = true, id

	d, err := auth.AuthorizeTool(q)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("an Agent's grant authorized a person who is not admitted")
	}
	if !strings.Contains(d.Reason, "guest") {
		t.Fatalf("the refusal does not name who was refused: %q", d.Reason)
	}

	// The same grant, the same Agent, the same tool — and an admitted
	// person is allowed. Without this the test would pass against an
	// implementation that refuses everything, which is the
	// over-correction the issue asks to be mutated for.
	if !mustAuthorize(t, auth, func() butler.ToolAuthorityQuery {
		a := personFacing("mika", "get_system_overview")
		a.GrantRequired, a.TaskID = true, id
		return a
	}()).Allowed {
		t.Fatal("an admitted person was refused a tool their Agent holds a grant for")
	}
}

// Layer 1 admits; layer 2 still decides. Belonging to the household —
// or to a group within it — is what lets a person have work done for
// them at all, and it is not a tool grant wearing a different name.
//
// Mutation: making PersonIsAdmitted consult group membership and return
// allowed — folding layer 1 and layer 2 into one answer — makes the
// first case fail.
func TestGroupMembershipAloneDoesNotAuthorize(t *testing.T) {
	db, auth := authorityDB(t)

	g, err := db.CreateGroup("gardeners")
	if err != nil {
		t.Fatal(err)
	}
	m, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddPersonToGroup(g.ID, m.PersonID, "root-admin"); err != nil {
		t.Fatal(err)
	}

	// sam is admitted AND in a group, and an administrator has turned
	// the tool off. Layer 1 passing does not reach past layer 2.
	if err := db.SetToolControl(runtimedb.AgentClassOwner, "get_system_overview",
		runtimedb.ToolDisabled, "root-admin", "noisy"); err != nil {
		t.Fatal(err)
	}
	if mustAuthorize(t, auth, personFacing("sam", "get_system_overview")).Allowed {
		t.Fatal("group membership reached past an administrator's disable")
	}

	// And the group does not make an unadmitted person admitted either:
	// layer 1 is about the PERSON, and a group they are in cannot
	// admit them.
	if err := db.RecordButlerMember("visitor", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	v, err := db.GetButlerMember("visitor")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddPersonToGroup(g.ID, v.PersonID, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if mustAuthorize(t, auth, personFacing("visitor", "get_household_events")).Allowed {
		t.Fatal("a group admitted a person the household never did")
	}
}

// A resolver nobody wired layer 1 onto refuses every person-facing
// check. The wiring is the act (§10, and Ledger.AdmissionCheck's own
// note) — and the ADMINISTRATIVE path is untouched by it, which is what
// keeps an unconfigured deployment's inventory readable.
//
// Mutation: defaulting PersonAuthority to "allow when nil" makes the
// first case fail; applying the layer-1 check regardless of caller kind
// makes the second fail.
func TestAnUnwiredPersonCheckRefusesOnlyThePersonFacingPath(t *testing.T) {
	db, _ := authorityDB(t)
	bare := &Authority{DB: db}

	if mustAuthorize(t, bare, personFacing("mika", "get_system_overview")).Allowed {
		t.Fatal("a resolver with no person check allowed a person's tool call")
	}

	admin := personFacing("", "get_system_overview")
	admin.Caller = butler.CallerAdministrative
	if !mustAuthorize(t, bare, admin).Allowed {
		t.Fatal("an administrative check was refused by a person check that does not apply to it")
	}
}

func mustAuthorize(t *testing.T, a *Authority, q butler.ToolAuthorityQuery) butler.ToolDecision {
	t.Helper()
	d, err := a.AuthorizeTool(q)
	if err != nil {
		t.Fatalf("AuthorizeTool(%s for %s): %v", q.Tool, q.Person, err)
	}
	return d
}
