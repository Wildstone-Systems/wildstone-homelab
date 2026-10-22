package api

// Editing a routine (#171). The first write path on this screen, and
// the one with no approval step in front of it, so what it refuses
// matters as much as what it accepts.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/automation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// seedRoutine records one Wildstone routine in the given state.
func seedRoutine(t *testing.T, h *harness, name, rule, state string) {
	t.Helper()
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: name, Ownership: runtimedb.RoutineWildstone,
		Document: routineDoc(rule), Author: "sam",
	}); err != nil {
		t.Fatalf("CreateHouseholdRoutine: %v", err)
	}
	if state == runtimedb.RoutineEnabled || state == runtimedb.RoutineTrial {
		approveRoutineFixture(t, h, name)
	}
	if state != runtimedb.RoutineDraft {
		if err := h.s.DB.SetHouseholdRoutineState(name, state, "sam"); err != nil {
			t.Fatalf("SetHouseholdRoutineState: %v", err)
		}
	}
}

// An enabled routine that is edited goes back to trial. Nothing else on
// this path would stop the house doing something new that nobody has
// watched.
func TestEditingAnEnabledRoutineRequiresOwnerReapproval(t *testing.T) {
	h := newHarness(t)
	seedRoutine(t, h, "evening-mail", "mail-check", runtimedb.RoutineEnabled)

	spec := editedSpec(t, "13:00")
	var got struct {
		Revision int    `json:"revision"`
		State    string `json:"state"`
		Note     string `json:"note"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/evening-mail/edit",
		map[string]any{"spec": spec, "note": "later"}, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("edit: %s", resp.Status)
	}
	if got.Revision != 2 {
		t.Fatalf("revision = %d, want 2", got.Revision)
	}
	if got.State != runtimedb.RoutineDraft {
		t.Fatalf("an edited routine is still acting: state = %q", got.State)
	}
	// And it is actually trial in the database, not just in the reply.
	after, err := h.s.DB.GetHouseholdRoutine("evening-mail")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != runtimedb.RoutineDraft || after.PerformsActions() {
		t.Fatalf("routine still performs actions after an edit: %+v", after)
	}
	// The reply says so rather than leaving someone to notice.
	if !strings.Contains(got.Note, "Owner approval") {
		t.Fatalf("the reply does not mention the state change: %q", got.Note)
	}
}

// A routine that was not acting is left where it is: there is nothing to
// take away, and silently moving a draft to trial would start running it.
func TestEditingANonActingRoutineLeavesItsState(t *testing.T) {
	h := newHarness(t)
	for _, state := range []string{runtimedb.RoutineDraft, runtimedb.RoutineTrial} {
		name := "r-" + state
		seedRoutine(t, h, name, "mail-"+state, state)
		var got struct {
			State string `json:"state"`
		}
		if resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
			"/api/v1/household/routines/"+name+"/edit",
			map[string]any{"spec": editedSpec(t, "14:00")}, &got); resp.StatusCode != http.StatusOK {
			t.Fatalf("edit %s: %s", state, resp.Status)
		}
		if got.State != runtimedb.RoutineDraft {
			t.Fatalf("editing a %s routine moved it to %q", state, got.State)
		}
	}
}

// An edit written against a revision someone else has already moved past
// is refused, not merged and not silently applied over the top.
func TestEditRefusesAStaleRevision(t *testing.T) {
	h := newHarness(t)
	seedRoutine(t, h, "evening-mail", "mail-check", runtimedb.RoutineTrial)
	// Someone else edits first.
	if _, err := h.s.DB.EditHouseholdRoutine("evening-mail", routineDoc("mail-check"),
		"alex", "got there first"); err != nil {
		t.Fatal(err)
	}

	resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/evening-mail/edit",
		map[string]any{"spec": editedSpec(t, "15:00"), "basedOnRevision": 1}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a stale edit was accepted: %s", resp.Status)
	}
	// The other person's revision stands.
	after, err := h.s.DB.GetHouseholdRoutine("evening-mail")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != 2 || after.UpdatedBy != "alex" {
		t.Fatalf("a refused edit still changed the routine: %+v", after)
	}

	// The same edit, written against what is actually there, applies.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/evening-mail/edit",
		map[string]any{"spec": editedSpec(t, "15:00"), "basedOnRevision": 2}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a current edit was refused: %s", resp.Status)
	}
}

// Renaming the rule through an edit would split its run history across
// two names, because events carry the name the rule had when it fired.
func TestEditCannotRenameTheRule(t *testing.T) {
	h := newHarness(t)
	seedRoutine(t, h, "evening-mail", "mail-check", runtimedb.RoutineTrial)

	resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/evening-mail/edit",
		map[string]any{"document": routineDoc("post-check")}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an edit renamed the rule: %s", resp.Status)
	}
	after, err := h.s.DB.GetHouseholdRoutine("evening-mail")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != 1 {
		t.Fatalf("a refused rename still recorded a revision: %+v", after)
	}
}

// A spec edit keeps the parts of the document the form never showed.
func TestEditKeepsWhatTheFormDoesNotRender(t *testing.T) {
	h := newHarness(t)
	const rich = `apiVersion: wildstone.dev/v1
kind: Automation
metadata: { name: mail-check, owner: household }
spec:
  description: Ask whether the mail was collected
  trigger: { eventType: person.arrived_home }
  conditions:
    - { kind: timeOfDay, after: "12:00" }
  context:
    - { kind: recentEvents, eventType: pantry.meal_logged, limit: 3 }
  escalation: { prompt: "Ask gently", into: message }
  action: { tool: notifications.notify, tier: domestic, params: { message: "Did you get the mail?" } }
`
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "evening-mail", Ownership: runtimedb.RoutineWildstone,
		Document: rich, Author: "sam",
	}); err != nil {
		t.Fatal(err)
	}

	// The form round-trips the whole spec, changing only the window.
	parsed, err := automation.ParseDraft(rich)
	if err != nil {
		t.Fatal(err)
	}
	spec := *parsed.Automation
	spec.Conditions[0].After = "13:30"

	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/evening-mail/edit",
		map[string]any{"spec": spec}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("edit: %s", resp.Status)
	}

	after, err := h.s.DB.GetHouseholdRoutine("evening-mail")
	if err != nil {
		t.Fatal(err)
	}
	out, err := automation.ParseDraft(after.Document)
	if err != nil {
		t.Fatalf("the stored document does not parse: %v\n%s", err, after.Document)
	}
	if out.Automation.Conditions[0].After != "13:30" {
		t.Fatalf("the change was lost: %+v", out.Automation.Conditions)
	}
	// The blocks the form never rendered have to survive it.
	if len(out.Automation.Context) != 1 || out.Automation.Context[0].EventType != "pantry.meal_logged" {
		t.Fatalf("context was dropped by an edit that never showed it: %+v", out.Automation.Context)
	}
	if out.Automation.Escalation == nil || out.Automation.Escalation.Prompt != "Ask gently" {
		t.Fatalf("escalation was dropped by an edit that never showed it: %+v", out.Automation.Escalation)
	}
	// And the rule keeps its identity.
	if out.Metadata.Name != "mail-check" || out.Metadata.Owner != "household" {
		t.Fatalf("the envelope was rewritten: %+v", out.Metadata)
	}
}

// Native routines are not editable, here as everywhere.
func TestEditRefusesANativeRoutine(t *testing.T) {
	h := newHarness(t)
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "ha-porch-light", Ownership: runtimedb.RoutineNative,
		ExternalID: "automation.porch", Connection: "home-assistant",
		Document: "opaque to us", Author: "sam",
	}); err != nil {
		t.Fatal(err)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/ha-porch-light/edit",
		map[string]any{"document": routineDoc("porch")}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a native routine was edited: %s", resp.Status)
	}
}

// editedSpec is the routineDoc spec with a different window.
func editedSpec(t *testing.T, after string) resource.AutomationSpec {
	t.Helper()
	parsed, err := automation.ParseDraft(routineDoc("x"))
	if err != nil {
		t.Fatal(err)
	}
	spec := *parsed.Automation
	spec.Conditions[0].After = after
	return spec
}

// A document sent whole is stored as sent, parseable or not.
//
// This route has always behaved that way and CreateHouseholdRoutine does
// not validate either, so refusing here would let a household write a
// broken routine and then be unable to replace it. The detail read is
// built to show an unparseable document for exactly this reason. A spec
// is the other case: this handler writes that document, so one that does
// not parse means the spec was not a rule.
func TestEditStoresAWholeDocumentAsSent(t *testing.T) {
	h := newHarness(t)
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "broken", Ownership: runtimedb.RoutineWildstone,
		Document: "not a rule at all", Author: "sam",
	}); err != nil {
		t.Fatal(err)
	}
	// Still broken, but replaceable.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/broken/edit",
		map[string]any{"document": "still not a rule"}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a whole document was refused: %s", resp.Status)
	}
	// And the edit that repairs it works, which is the point.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/broken/edit",
		map[string]any{"document": routineDoc("mail-check")}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the repairing edit was refused: %s", resp.Status)
	}
}

// A spec that is not a rule is refused, because this handler wrote the
// document it would have stored.
func TestEditRefusesASpecThatIsNotARule(t *testing.T) {
	h := newHarness(t)
	seedRoutine(t, h, "evening-mail", "mail-check", runtimedb.RoutineTrial)

	bad := editedSpec(t, "12:00")
	// The schema requires "<capability>.<tool>"; an unqualified tool
	// names nothing the engine can dispatch.
	//
	// Note this is a SCHEMA refusal. A tier above domestic is schema-valid
	// and refused by the automation-action-tier policy rule instead; see
	// TestEditRefusesAnActionTierAboveDomestic.
	bad.Action.Tool = "notify"
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/routines/evening-mail/edit",
		map[string]any{"spec": bad}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an invalid spec was stored: %s", resp.Status)
	}
	after, err := h.s.DB.GetHouseholdRoutine("evening-mail")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != 1 {
		t.Fatalf("a refused spec still recorded a revision: %+v", after)
	}
}

// A household routine acts without a per-firing approval, so the registry
// write is held to the automation-action-tier rule directly. These drive
// the API, not the form, whose dropdown is not a guard (ADR-W024 A5).
func TestEditRefusesAnActionTierAboveDomestic(t *testing.T) {
	h := newHarness(t)
	seedRoutine(t, h, "evening-mail", "mail-check", runtimedb.RoutineTrial)

	high := editedSpec(t, "12:00")
	high.Action.Tier = "high"
	whole := strings.Replace(routineDoc("mail-check"), "tier: domestic", "tier: high", 1)
	for name, body := range map[string]map[string]any{
		"spec":     {"spec": high},
		"document": {"document": whole},
	} {
		var got map[string]any
		resp := h.do(runtimedb.RoleAdmin, http.MethodPost,
			"/api/v1/household/routines/evening-mail/edit", body, &got)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("%s: a high-tier edit was not refused: %s", name, resp.Status)
		}
		if msg := fmt.Sprint(got); !strings.Contains(msg, "automation-action-tier") {
			t.Fatalf("%s: refusal does not name the policy rule: %s", name, msg)
		}
		after, err := h.s.DB.GetHouseholdRoutine("evening-mail")
		if err != nil {
			t.Fatal(err)
		}
		if after.Revision != 1 {
			t.Fatalf("%s: a refused edit still wrote revision %d", name, after.Revision)
		}
	}
}

func TestCreateRefusesAnActionTierAboveDomesticButNotForNative(t *testing.T) {
	h := newHarness(t)
	whole := strings.Replace(routineDoc("mail-check"), "tier: domestic", "tier: high", 1)

	resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/routines",
		map[string]any{"name": "sneaky", "ownership": "wildstone", "document": whole}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a high-tier Wildstone routine was stored: %s", resp.Status)
	}
	if _, err := h.s.DB.GetHouseholdRoutine("sneaky"); !errors.Is(err, runtimedb.ErrNotFound) {
		t.Fatalf("a refused create left a row behind: %v", err)
	}

	// A native routine's document is the application's, not Wildstone's.
	resp = h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/routines",
		map[string]any{"name": "ha-high", "ownership": "native", "externalId": "automation.x",
			"connection": "home", "document": whole}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a native routine was refused: %s", resp.Status)
	}
}
