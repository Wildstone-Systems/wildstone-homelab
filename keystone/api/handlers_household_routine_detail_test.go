package api

// The routine detail read (#169): a routine's spec, its edit history and
// what it has actually been doing.
//
// Two things here are the point. The revision history has been written
// since migration 0028 and read by nothing, so this file is the first
// place a revision row is asserted to exist at all. And runs are matched
// on the rule's metadata.name rather than the registry name it is filed
// under, because those need not be equal and matching on the wrong one
// serves an empty table for a routine that fired all week.

import (
	"net/http"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// routineDoc is one valid Automation document, named independently of
// the routine it is filed under.
func routineDoc(ruleName string) string {
	return `apiVersion: wildstone.dev/v1
kind: Automation
metadata: { name: ` + ruleName + `, owner: household }
spec:
  description: Ask whether the mail was collected on arriving home
  trigger: { eventType: person.arrived_home }
  conditions:
    - { kind: timeOfDay, after: "12:00" }
  action: { tool: notifications.notify, tier: domestic, params: { message: "Did you get the mail?" } }
`
}

func TestRoutineDetailServesSpecRevisionsAndRuns(t *testing.T) {
	h := newHarness(t)

	// Filed as "evening-mail", but the rule the engine knows is
	// "mail-check". The detail read has to follow the document.
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "evening-mail", Ownership: runtimedb.RoutineWildstone,
		Document: routineDoc("mail-check"), Author: "sam",
	}); err != nil {
		t.Fatalf("CreateHouseholdRoutine: %v", err)
	}
	if _, err := h.s.DB.EditHouseholdRoutine("evening-mail", routineDoc("mail-check"),
		"alex", "narrowed the window"); err != nil {
		t.Fatalf("EditHouseholdRoutine: %v", err)
	}

	// Two records the engine would have written, and one from another
	// rule that must not appear.
	for _, ev := range []runtimedb.HouseholdEvent{
		{EventID: "e1", OccurredAt: time.Now().Add(-2 * time.Hour), Source: automation.Source,
			Type: automation.TypeFired, Entity: "mail-check", SchemaVersion: 1},
		{EventID: "e2", OccurredAt: time.Now().Add(-time.Hour), Source: automation.Source,
			Type: automation.TypeSuppressed, Entity: "mail-check", SchemaVersion: 1},
		{EventID: "e3", OccurredAt: time.Now(), Source: automation.Source,
			Type: automation.TypeFired, Entity: "someone-elses-rule", SchemaVersion: 1},
	} {
		if _, _, err := h.s.DB.AppendHouseholdEvent(ev); err != nil {
			t.Fatalf("AppendHouseholdEvent(%s): %v", ev.EventID, err)
		}
	}

	var got routineDetail
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet,
		"/api/v1/household/routines/evening-mail", nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %s", resp.Status)
	}

	if got.SpecError != "" {
		t.Fatalf("a valid document reported an error: %s", got.SpecError)
	}
	if got.Spec == nil || got.Spec.Trigger.EventType != "person.arrived_home" {
		t.Fatalf("spec not served as parsed: %+v", got.Spec)
	}

	// The history the table has always held and nothing ever read.
	if len(got.Revisions) != 2 {
		t.Fatalf("want the creation and the edit, got %d revisions: %+v", len(got.Revisions), got.Revisions)
	}
	if got.Revisions[0].Revision != 2 || got.Revisions[0].Author != "alex" ||
		got.Revisions[0].Note != "narrowed the window" {
		t.Fatalf("newest revision wrong: %+v", got.Revisions[0])
	}
	if got.Revisions[1].Revision != 1 || got.Revisions[1].Author != "sam" {
		t.Fatalf("oldest revision wrong: %+v", got.Revisions[1])
	}
	if got.Revisions[0].At.IsZero() {
		t.Fatal("a revision with no timestamp cannot answer when it changed")
	}
	// Listing every past document would be a lot of bytes for a card
	// showing three lines each.
	if got.Revisions[0].Document != "" {
		t.Fatal("the revision listing carries documents")
	}

	// Runs follow the rule name, not the registry name, and stop at
	// this rule.
	if len(got.Runs) != 2 {
		t.Fatalf("want this rule's two records, got %d: %+v", len(got.Runs), got.Runs)
	}
	for _, r := range got.Runs {
		if r.Entity != "mail-check" {
			t.Fatalf("another rule's record leaked in: %+v", r)
		}
	}
	if got.Runs[0].EventID != "e2" {
		t.Fatalf("runs are not newest first: %+v", got.Runs)
	}
}

func TestRoutineDetailUnparseableDocumentExplainsItself(t *testing.T) {
	h := newHarness(t)
	// A routine whose document is not a rule at all. It is exactly the
	// one someone needs to look at, so the read must succeed.
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "broken", Ownership: runtimedb.RoutineWildstone,
		Document: "this is not yaml: [", Author: "sam",
	}); err != nil {
		t.Fatalf("CreateHouseholdRoutine: %v", err)
	}

	var got routineDetail
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet,
		"/api/v1/household/routines/broken", nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("an unreadable spec should still serve its routine, got %s", resp.Status)
	}
	if got.Spec != nil || got.SpecError == "" {
		t.Fatalf("want spec null and a reason, got spec=%+v err=%q", got.Spec, got.SpecError)
	}
	if got.Routine.Name != "broken" {
		t.Fatalf("routine not served: %+v", got.Routine)
	}
}

func TestRoutineDetailNativeRoutineHasNoRuns(t *testing.T) {
	h := newHarness(t)
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "ha-porch-light", Ownership: runtimedb.RoutineNative,
		ExternalID: "automation.porch", Connection: "home-assistant",
		Document: "opaque to us", Author: "sam",
	}); err != nil {
		t.Fatalf("CreateHouseholdRoutine: %v", err)
	}
	// A record that would match if we queried by the registry name.
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "n1", OccurredAt: time.Now(), Source: automation.Source,
		Type: automation.TypeFired, Entity: "ha-porch-light", SchemaVersion: 1,
	}); err != nil {
		t.Fatalf("AppendHouseholdEvent: %v", err)
	}

	var got routineDetail
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet,
		"/api/v1/household/routines/ha-porch-light", nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %s", resp.Status)
	}
	// Wildstone does not run this one, so it must not claim to know how
	// it has been going.
	if len(got.Runs) != 0 {
		t.Fatalf("a native routine reported runs: %+v", got.Runs)
	}
	if len(got.Revisions) != 1 {
		t.Fatalf("want the creation revision, got %+v", got.Revisions)
	}
}

func TestRoutineDetailUnknownNameAndRoles(t *testing.T) {
	h := newHarness(t)
	h.addPublisher("publisher-home")
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "evening-mail", Ownership: runtimedb.RoutineWildstone,
		Document: routineDoc("mail-check"), Author: "sam",
	}); err != nil {
		t.Fatalf("CreateHouseholdRoutine: %v", err)
	}

	if resp := h.do(runtimedb.RoleOperator, http.MethodGet,
		"/api/v1/household/routines/no-such-thing", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for an unknown routine, got %s", resp.Status)
	}

	// Reading a routine is a read. A publisher may emit events and see
	// nothing else.
	for _, c := range []struct {
		role runtimedb.Role
		want int
	}{
		{runtimedb.RoleAdmin, http.StatusOK},
		{runtimedb.RoleOperator, http.StatusOK},
		{runtimedb.RoleButler, http.StatusOK},
		{runtimedb.RolePublisher, http.StatusForbidden},
		{"", http.StatusUnauthorized},
	} {
		if resp := h.do(c.role, http.MethodGet,
			"/api/v1/household/routines/evening-mail", nil, nil); resp.StatusCode != c.want {
			t.Fatalf("role %q: want %d, got %s", c.role, c.want, resp.Status)
		}
	}
}

func TestRoutineListServesParsedSpecs(t *testing.T) {
	h := newHarness(t)
	for _, r := range []runtimedb.HouseholdRoutine{
		{Name: "evening-mail", Ownership: runtimedb.RoutineWildstone,
			Document: routineDoc("mail-check"), Author: "sam"},
		{Name: "broken", Ownership: runtimedb.RoutineWildstone,
			Document: "this is not yaml: [", Author: "sam"},
		{Name: "ha-porch-light", Ownership: runtimedb.RoutineNative,
			ExternalID: "automation.porch", Connection: "home-assistant",
			Document: "opaque to us", Author: "sam"},
	} {
		if _, err := h.s.DB.CreateHouseholdRoutine(r); err != nil {
			t.Fatalf("CreateHouseholdRoutine(%s): %v", r.Name, err)
		}
	}

	var got struct {
		Routines []runtimedb.HouseholdRoutine        `json:"routines"`
		Specs    map[string]*resource.AutomationSpec `json:"specs"`
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet,
		"/api/v1/household/routines", nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %s", resp.Status)
	}
	if len(got.Routines) != 3 {
		t.Fatalf("want every routine listed, got %d", len(got.Routines))
	}
	// The parsed spec is what lets a row show its trigger and tier
	// without a YAML parser in the browser.
	spec, ok := got.Specs["evening-mail"]
	if !ok || spec == nil || spec.Trigger.EventType != "person.arrived_home" {
		t.Fatalf("spec for a readable routine: %+v", got.Specs)
	}
	if spec.Action.Tier != "domestic" {
		t.Fatalf("action tier lost: %+v", spec.Action)
	}
	// A document that will not parse costs its chips, not its row.
	if _, ok := got.Specs["broken"]; ok {
		t.Fatal("an unparseable document produced a spec")
	}
	// Wildstone does not hold a native routine's spec and must not
	// pretend to.
	if _, ok := got.Specs["ha-porch-light"]; ok {
		t.Fatal("a native routine produced a spec")
	}
}

// A rule renamed by an edit keeps its history.
//
// Events are recorded against the document's metadata.name AS IT WAS
// when the rule fired, and nothing in the edit path pins that name. So
// a routine renamed on Tuesday would report "nothing triggered it this
// week" about Monday's firings — the same wrong answer the registry-name
// case avoids, one revision further back.
func TestRoutineDetailFindsRunsUnderPreviousRuleNames(t *testing.T) {
	h := newHarness(t)
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "evening-mail", Ownership: runtimedb.RoutineWildstone,
		Document: routineDoc("mail-check"), Author: "sam",
	}); err != nil {
		t.Fatalf("CreateHouseholdRoutine: %v", err)
	}
	// It fired under its first name.
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "old-1", OccurredAt: time.Now().Add(-48 * time.Hour), Source: automation.Source,
		Type: automation.TypeFired, Entity: "mail-check", SchemaVersion: 1,
	}); err != nil {
		t.Fatalf("AppendHouseholdEvent: %v", err)
	}
	// Then someone renamed the rule inside the document.
	if _, err := h.s.DB.EditHouseholdRoutine("evening-mail", routineDoc("post-check"),
		"alex", "renamed the rule"); err != nil {
		t.Fatalf("EditHouseholdRoutine: %v", err)
	}
	if _, _, err := h.s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID: "new-1", OccurredAt: time.Now().Add(-time.Hour), Source: automation.Source,
		Type: automation.TypeFired, Entity: "post-check", SchemaVersion: 1,
	}); err != nil {
		t.Fatalf("AppendHouseholdEvent: %v", err)
	}

	var got routineDetail
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet,
		"/api/v1/household/routines/evening-mail", nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %s", resp.Status)
	}
	if len(got.Runs) != 2 {
		t.Fatalf("a rename lost this routine's history: got %d runs, want both", len(got.Runs))
	}
	// Merged from one query per name, so the order has to be restored.
	if got.Runs[0].EventID != "new-1" || got.Runs[1].EventID != "old-1" {
		t.Fatalf("runs are not newest first across names: %+v", got.Runs)
	}
	// Two names can point at overlapping history; an event is one event.
	seen := map[string]int{}
	for _, r := range got.Runs {
		seen[r.EventID]++
		if seen[r.EventID] > 1 {
			t.Fatalf("event %s served twice", r.EventID)
		}
	}
}
