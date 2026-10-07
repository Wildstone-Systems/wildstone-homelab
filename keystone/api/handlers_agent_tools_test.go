package api

// §6.4's default assignment set, at the endpoint that creates an Agent.
//
// The rule has three clauses and they pull apart: an Owner Agent gets
// Member AND Owner tools, a Member Agent gets only Member ones, and
// neither gets an opt-in tool. A test that checked only the first would
// pass with the other two broken.

import (
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// fakeInventory is a tool surface with one opt-in tool per class.
//
// Injected rather than taken from the real gateways because no shipped
// tool sets OptIn, and the alternative — marking a real one opt-in so a
// test has something to find — would change what every household's
// Agents are given in order to make an assertion convenient.
func fakeInventory(class string, _ bool) []butler.CapabilityTool {
	switch class {
	case runtimedb.AgentClassMember:
		return []butler.CapabilityTool{
			{Name: "read_the_calendar", Service: "system",
				AgentClass: butler.AgentClassMember, Available: true},
			{Name: "empty_the_house", Service: "system",
				AgentClass: butler.AgentClassMember, Available: true, OptIn: true},
			// Available: false — wired nowhere on this deployment, so
			// not part of "currently available" either.
			{Name: "never_wired", Service: "system",
				AgentClass: butler.AgentClassMember, Available: false},
		}
	case runtimedb.AgentClassOwner:
		return []butler.CapabilityTool{
			{Name: "read_the_statefile", Service: "system",
				AgentClass: butler.AgentClassOwner, Available: true},
			{Name: "rotate_every_secret", Service: "system",
				AgentClass: butler.AgentClassOwner, Available: true, OptIn: true},
		}
	}
	return nil
}

func assignedNames(t *testing.T, db *runtimedb.DB, name string) map[string]bool {
	t.Helper()
	b, err := db.GetButler(name)
	if err != nil {
		t.Fatalf("GetButler(%s): %v", name, err)
	}
	rows, err := db.ListToolAssignments(b.Seq)
	if err != nil {
		t.Fatalf("ListToolAssignments: %v", err)
	}
	out := map[string]bool{}
	for _, a := range rows {
		out[a.Tool] = true
	}
	return out
}

// THE ACCEPTANCE CASE (§18.3, §6.4).
func TestNewOwnerAgentExcludesOptInTools(t *testing.T) {
	h := butlerConsole(t)
	h.s.ToolInventory = fakeInventory

	var made map[string]any
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers",
		map[string]any{"name": "alfred", "agentClass": "owner", "model": "kitchen-box"}, &made)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("creating an owner agent = %d", res.StatusCode)
	}

	got := assignedNames(t, h.s.DB, "alfred")

	// It gets BOTH classes' ordinary tools (§6.4, §6.1's ordering).
	for _, want := range []string{"read_the_calendar", "read_the_statefile"} {
		if !got[want] {
			t.Errorf("a new owner agent was not given %q — §6.4 gives it the member AND "+
				"the owner tools", want)
		}
	}

	// And NEITHER class's opt-in tool. This is the clause under test:
	// an opt-in tool's authority class matches, and it is still left out.
	for _, unwanted := range []string{"empty_the_house", "rotate_every_secret"} {
		if got[unwanted] {
			t.Errorf("a new owner agent was given the opt-in tool %q — §6.2 keeps such a "+
				"tool out of broad grants so it can be unusually consequential without "+
				"inventing a third agent class", unwanted)
		}
	}

	// A tool nothing wired is not "currently available", so it is not
	// assigned either — otherwise the default set would promise tools
	// that refuse.
	if got["never_wired"] {
		t.Error("a tool this deployment has not wired was assigned")
	}

	// The Agent is enrolled, or none of the above means anything: an
	// unenrolled Agent reaches everything regardless of its assignments.
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	governed, err := h.s.DB.AgentIsGoverned(b.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !governed {
		t.Error("a newly created agent was not enrolled in the assigned-tools model, so " +
			"its assignments decide nothing")
	}
}

// NEGATIVE: a Member Agent gets only Member tools (§6.4).
//
// Separate from the case above because the failure is different in kind:
// giving an Owner Agent an opt-in tool is too generous, and giving a
// Member Agent an owner tool is a class violation.
func TestNewMemberAgentGetsOnlyMemberTools(t *testing.T) {
	h := butlerConsole(t)
	h.s.ToolInventory = fakeInventory

	var made map[string]any
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers",
		map[string]any{"name": "hobson", "agentClass": "member", "model": "kitchen-box"}, &made)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("creating a member agent = %d", res.StatusCode)
	}

	got := assignedNames(t, h.s.DB, "hobson")
	if !got["read_the_calendar"] {
		t.Error("a new member agent was not given the ordinary member tools")
	}
	if got["read_the_statefile"] {
		t.Error("a new member agent was given an OWNER tool — §6.1 says owner tools may " +
			"be assigned only to owner agents")
	}
	if got["empty_the_house"] {
		t.Error("a new member agent was given the opt-in member tool")
	}
}

// The one-time backfill brings pre-0059 Agents onto the model.
//
// An Agent made before the migration has no assignments and is not
// enrolled. It must end up with both, or the two-model coexistence this
// slice deliberately removed comes back as a permanent state nobody
// chose.
func TestTheBackfillEnrollsEveryExistingAgent(t *testing.T) {
	h := butlerConsole(t)
	h.s.ToolInventory = fakeInventory

	// Made through the database, the way an Agent that predates 0059
	// exists: no assignments, no enrollment.
	if _, err := h.s.DB.CreateButler(runtimedb.Butler{
		Name: "oldtimer", Directive: "from before",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
		Model: "kitchen-box",
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	old, err := h.s.DB.GetButler("oldtimer")
	if err != nil {
		t.Fatal(err)
	}
	if governed, _ := h.s.DB.AgentIsGoverned(old.Seq); governed {
		t.Fatal("an agent created straight through the database was already enrolled")
	}

	if err := h.s.BackfillAgentTools(); err != nil {
		t.Fatalf("BackfillAgentTools: %v", err)
	}

	governed, err := h.s.DB.AgentIsGoverned(old.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !governed {
		t.Error("the backfill left an existing agent unenrolled, so the old model is " +
			"still in force for it")
	}
	got := assignedNames(t, h.s.DB, "oldtimer")
	if !got["read_the_calendar"] || !got["read_the_statefile"] {
		t.Error("the backfill did not give an existing owner agent its default tools")
	}
	if got["empty_the_house"] || got["rotate_every_secret"] {
		t.Error("the backfill handed out opt-in tools — §6.2 keeps them out of defaults " +
			"whether an agent is new or being migrated")
	}
}

// NEGATIVE: the backfill does not undo an Owner's removals.
//
// It runs at every start, so it has to be idempotent in the direction
// that matters: a tool somebody deliberately took away must not come
// back on the next restart. That is what the enrollment record buys,
// and without this test the backfill could be written to re-seed every
// agent every time and look correct.
func TestTheBackfillDoesNotRestoreARemovedTool(t *testing.T) {
	h := butlerConsole(t)
	h.s.ToolInventory = fakeInventory

	var made map[string]any
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers",
		map[string]any{"name": "alfred", "agentClass": "owner", "model": "kitchen-box"}, &made)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("creating = %d", res.StatusCode)
	}
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.UnassignTool(b.Seq, "read_the_statefile", "mika"); err != nil {
		t.Fatal(err)
	}

	if err := h.s.BackfillAgentTools(); err != nil {
		t.Fatalf("BackfillAgentTools: %v", err)
	}
	if assignedNames(t, h.s.DB, "alfred")["read_the_statefile"] {
		t.Error("a restart gave back a tool an Owner had removed — the backfill must skip " +
			"an agent that is already enrolled, not re-seed it")
	}
}

// Calendar actions are granted per action (ADR-W034 A5) but live in the
// household manifest's Actions, not its Tools. A new Agent must still be
// seeded with them, or it would hold do_household_action and be refused
// every calendar action.
func TestNewMemberAgentIsSeededWithCalendarActions(t *testing.T) {
	h := butlerConsole(t)
	h.s.ToolInventory = fakeInventory
	h.s.ButlerHost = &ButlerHost{Household: butler.HouseholdSeams{
		Vocabulary: func() (butler.HouseholdVocabulary, error) {
			return butler.HouseholdVocabulary{Actions: []butler.HouseholdAction{
				{Tool: "calendar.get_agenda", Tier: "domestic"},
				{Tool: "notifications.notify", Tier: "domestic"},
			}}, nil
		},
	}}

	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers",
		map[string]any{"name": "jeeves", "agentClass": "member", "model": "kitchen-box"}, nil)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("creating a member agent = %d", res.StatusCode)
	}
	got := assignedNames(t, h.s.DB, "jeeves")
	if !got["calendar.get_agenda"] {
		t.Errorf("a new agent was not given the calendar action it needs: %v", got)
	}
	if got["notifications.notify"] {
		t.Error("an action not granted per action was assigned as a tool")
	}

	b, err := h.s.DB.GetButler("jeeves")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.s.assignableTool(b, "calendar.get_agenda"); !ok {
		t.Error("a calendar action cannot be assigned through the endpoint")
	}
}
