package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (h *harness) authored(t *testing.T, method, path, user string, body any) (int, map[string]any) {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		r = bytes.NewReader(encoded)
	} else {
		r = bytes.NewReader(nil)
	}
	resp := h.member(method, path, user, "homepage_users", r, "application/json")
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// THE STRUCTURAL GUARANTEE: this door builds resources from typed
// fields, so it cannot write a kind it was not meant to.
//
// Every other check in this file is a rule somebody could get wrong.
// This one is a property of the code's shape: nothing a member sends
// becomes a Kind, so no JSON — however crafted — reaches a Container, a
// Secret or a Route through the family page. Asserted by reading the
// source rather than by probing inputs, because a probe only covers the
// shapes somebody thought to try.
func TestTheFamilyDoorCanOnlyEverWriteTwoKinds(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_portal_authoring.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	// "Kind" alone is the TYPE, which appears in signatures and in the
	// one string conversion a deletion needs. That conversion is why
	// familyMayWrite is checked at run time as well — a static rule
	// cannot see through `resource.Kind(someString)`, and the runtime
	// guard has its own test below.
	allowed := map[string]bool{"KindMeter": true, "KindCommitment": true, "Kind": true}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "resource" {
			return true
		}
		if !strings.HasPrefix(sel.Sel.Name, "Kind") {
			return true
		}
		if !allowed[sel.Sel.Name] {
			found = append(found, sel.Sel.Name)
		}
		return true
	})
	if len(found) > 0 {
		t.Errorf("the family authoring door names %v — it may write a Meter and a Commitment "+
			"and nothing else, and the guarantee is that no request body becomes a Kind", found)
	}
	// And it must not take a document: a handler that unmarshalled one
	// would let a member choose the kind whatever this test says.
	src, err := parser.ParseFile(token.NewFileSet(), "handlers_portal_authoring.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(src, func(n ast.Node) bool {
		call, ok := n.(*ast.SelectorExpr)
		if ok && call.Sel.Name == "Unmarshal" {
			if pkg, ok := call.X.(*ast.Ident); ok && pkg.Name == "resource" {
				t.Error("the family authoring door unmarshals a resource document; " +
					"a member would then choose the kind")
			}
		}
		return true
	})
}

// Thing first, then task — and in one act, so a failure halfway does
// not leave a meter nobody asked for.
func TestAMemberDeclaresTheTruckAndItsFirstJobInOneAct(t *testing.T) {
	h := commitmentHarness(t)
	code, out := h.authored(t, http.MethodPost, "/api/v1/home/tracked", "mika", map[string]any{
		"meter":   map[string]any{"description": "F-150 odometer", "unit": "miles"},
		"reading": 84210,
		"commitment": map[string]any{
			"description": "Oil and filter", "every": "5000", "lead": "300",
			"rollover": resource.RolloverFromCompletion,
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %+v", code, out)
	}
	meter, _ := out["meter"].(string)
	if meter != "f-150-odometer" {
		t.Errorf("meter name = %q, want one derived from what was typed", meter)
	}
	// It landed in the Statefile, not a proposal queue.
	current, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	var sawMeter, sawCommitment bool
	for _, res := range current {
		if res.Kind == resource.KindMeter && res.Metadata.Name == meter {
			sawMeter = true
			if res.Meter.Unit != "miles" {
				t.Errorf("unit = %q", res.Meter.Unit)
			}
		}
		if res.Kind == resource.KindCommitment && res.Metadata.Name == "oil-and-filter" {
			sawCommitment = true
			if res.Commitment.Due[0].Meter != meter {
				t.Errorf("the job is measured against %q, want the meter just made",
					res.Commitment.Due[0].Meter)
			}
		}
	}
	if !sawMeter || !sawCommitment {
		t.Fatalf("meter=%v commitment=%v — both should be desired state now", sawMeter, sawCommitment)
	}
	// And the reading came with it.
	latest, err := h.s.DB.LatestMeterReading(meter)
	if err != nil || latest.Value != 84210 {
		t.Errorf("latest = %+v, err = %v, want the number given at creation", latest, err)
	}
	// The household's history says who did it, with no approval step.
	plans, err := h.s.DB.ListPlans(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want one record", len(plans))
	}
	if plans[0].State != runtimedb.PlanApplied {
		t.Errorf("plan state = %q, want it already applied", plans[0].State)
	}
	approved, err := h.s.DB.PlansInState(runtimedb.PlanApproved)
	if err != nil || len(approved) != 0 {
		t.Fatalf("tracker edit granted host execution authority: %+v %v", approved, err)
	}
	if !strings.Contains(plans[0].Principal, "mika") {
		t.Errorf("proposer = %q, want the member", plans[0].Principal)
	}
}

// A second job on the same truck reuses the odometer. That is the
// payoff for the meter being its own thing.
func TestASecondJobReusesTheMeterAlreadyThere(t *testing.T) {
	h := commitmentHarness(t)
	code, out := h.authored(t, http.MethodPost, "/api/v1/home/tracked", "mika", map[string]any{
		"meter": map[string]any{"description": "F-150 odometer", "unit": "miles"},
	})
	if code != http.StatusCreated {
		t.Fatalf("status %d: %+v", code, out)
	}
	meter := out["meter"].(string)
	code, out = h.authored(t, http.MethodPost, "/api/v1/home/commitments", "mika", map[string]any{
		"description": "Tyre rotation", "meter": meter, "every": "7500", "lead": "300",
		"rollover": resource.RolloverFromCompletion,
	})
	if code != http.StatusCreated {
		t.Fatalf("status %d: %+v", code, out)
	}
	page := h.upkeep(t, "mika")
	if len(page.Tracked) != 1 || len(page.Tracked[0].Commitments) != 1 {
		t.Fatalf("tracked = %+v, want the rotation under the one odometer", page.Tracked)
	}
}

// Validation still applies. Not needing approval is not the same as not
// being checked.
func TestAuthoringIsStillValidated(t *testing.T) {
	h := commitmentHarness(t)
	for _, c := range []struct {
		what string
		body map[string]any
	}{
		{"no unit", map[string]any{"meter": map[string]any{"description": "The truck"}}},
		{"no description", map[string]any{"meter": map[string]any{"unit": "miles"}}},
		{"the calendar", map[string]any{"meter": map[string]any{"description": "clock", "unit": "days"}}},
	} {
		if code, out := h.authored(t, http.MethodPost, "/api/v1/home/tracked", "mika", c.body); code == http.StatusCreated {
			t.Errorf("%s was accepted: %+v", c.what, out)
		}
	}
	// A counter interval on the calendar is refused by the same
	// validation a plan faces — "every: 6mo" on an odometer and "every:
	// 5000" on the clock are both nonsense.
	if code, _ := h.authored(t, http.MethodPost, "/api/v1/home/commitments", "mika", map[string]any{
		"description": "Nonsense", "every": "5000", "rollover": resource.RolloverFromCompletion,
	}); code == http.StatusCreated {
		t.Error("a counter interval on the clock was accepted")
	}
}

// Two classes, and the same boundary visibleTo draws.
func TestHouseholdOnesAreAnybodysAndPersonalOnesAreNot(t *testing.T) {
	h := commitmentHarness(t)
	// Sandy adds a household commitment; Mika may change and remove it.
	code, out := h.authored(t, http.MethodPost, "/api/v1/home/commitments", "sandy", map[string]any{
		"description": "Furnace filter", "every": "3mo", "lead": "14d",
		"rollover": resource.RolloverFromCompletion,
	})
	if code != http.StatusCreated {
		t.Fatalf("status %d: %+v", code, out)
	}
	shared := out["commitment"].(string)
	if code, _ := h.authored(t, http.MethodPut, "/api/v1/home/commitments/"+shared, "mika", map[string]any{
		"description": "Furnace filter", "every": "6mo", "lead": "14d",
		"rollover": resource.RolloverFromCompletion,
	}); code != http.StatusOK {
		t.Errorf("a member could not fix a household commitment: %d", code)
	}

	// Mika's personal one is his alone — Sandy can neither change nor
	// remove it, and cannot even see that it is there.
	code, out = h.authored(t, http.MethodPost, "/api/v1/home/commitments", "mika", map[string]any{
		"description": "Repeat prescription", "every": "3mo", "lead": "14d",
		"rollover": resource.RolloverFromCompletion, "scope": resource.CommitmentScopePersonal,
	})
	if code != http.StatusCreated {
		t.Fatalf("status %d: %+v", code, out)
	}
	personal := out["commitment"].(string)
	if code, _ := h.authored(t, http.MethodPut, "/api/v1/home/commitments/"+personal, "sandy", map[string]any{
		"description": "Repeat prescription", "every": "1mo",
		"rollover": resource.RolloverFromCompletion, "scope": resource.CommitmentScopePersonal,
	}); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: a personal commitment should not be confirmed to exist", code)
	}
	if code, _ := h.authored(t, http.MethodDelete, "/api/v1/home/commitments/"+personal, "sandy", nil); code != http.StatusNotFound {
		t.Errorf("delete status = %d, want 404", code)
	}
	// Its owner still can.
	if code, _ := h.authored(t, http.MethodDelete, "/api/v1/home/commitments/"+personal, "mika", nil); code != http.StatusOK {
		t.Errorf("the owner could not remove their own: %d", code)
	}
}

// An edit that says nothing about the anchor does not move it. Silently
// re-basing every threshold is not what "fix the description" means.
func TestAnEditKeepsWhereTrackingStarted(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "filter", resource.CommitmentSpec{
		Description: "Furnace filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2024-01-01",
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
	}, "household")

	if code, out := h.authored(t, http.MethodPut, "/api/v1/home/commitments/filter", "mika", map[string]any{
		"description": "Furnace filter (the big one)", "every": "3mo", "lead": "14d",
		"rollover": resource.RolloverFromCompletion,
	}); code != http.StatusOK {
		t.Fatalf("status %d: %+v", code, out)
	}
	current, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range current {
		if res.Kind == resource.KindCommitment && res.Metadata.Name == "filter" {
			if res.Commitment.Anchor == "2024-01-01" {
				return
			}
			t.Fatalf("anchor = %q, want the original: an edit moved where tracking started",
				res.Commitment.Anchor)
		}
	}
	t.Fatal("the commitment vanished")
}

// Removing a meter is a member's to do, and not by accident. The
// commitments measured against it keep their thresholds and lose the
// number, so the confirmation names exactly what stops working.
func TestRemovingAMeterNamesWhatStopsWorking(t *testing.T) {
	h := commitmentHarness(t)
	h.truck(t)
	code, out := h.authored(t, http.MethodDelete, "/api/v1/home/meters/f150-odometer", "mika", nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want a confirmation first: %+v", code, out)
	}
	if out["consequence"] == nil || !strings.Contains(out["consequence"].(string), "measure against") {
		t.Errorf("consequence = %v, want it to name what stops working", out["consequence"])
	}
	deps, _ := out["dependants"].([]any)
	if len(deps) != 2 {
		t.Errorf("dependants = %v, want both truck jobs", deps)
	}
	// Confirmed, it goes — no Owner needed.
	if code, out := h.authored(t, http.MethodDelete,
		"/api/v1/home/meters/f150-odometer?confirm=yes", "mika", nil); code != http.StatusOK {
		t.Fatalf("status = %d: %+v", code, out)
	}
}

// A name collision is the system's problem.
func TestASecondThingWithTheSameNameGetsOneQuietly(t *testing.T) {
	h := commitmentHarness(t)
	for i, want := range []string{"the-truck", "the-truck-2"} {
		code, out := h.authored(t, http.MethodPost, "/api/v1/home/tracked", "mika", map[string]any{
			"meter": map[string]any{"description": "The truck", "unit": "miles"},
		})
		if code != http.StatusCreated {
			t.Fatalf("%d: status %d: %+v", i, code, out)
		}
		if out["meter"] != want {
			t.Errorf("name = %v, want %q", out["meter"], want)
		}
	}
}

// Removing a commitment stops the asking; it does not rewrite what the
// household actually did.
func TestRemovingACommitmentKeepsItsHistory(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "filter", resource.CommitmentSpec{
		Description: "Furnace filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2024-01-01",
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
	}, "household")
	if code, _ := h.authored(t, http.MethodPost, "/api/v1/home/commitments/filter/done", "mika", nil); code != http.StatusCreated {
		t.Fatalf("closing it: %d", code)
	}
	if code, _ := h.authored(t, http.MethodDelete, "/api/v1/home/commitments/filter", "mika", nil); code != http.StatusOK {
		t.Fatalf("removing it")
	}
	done, err := h.s.DB.ListCommitmentCompletions("filter")
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Errorf("completions = %d, want the record of what was done to survive", len(done))
	}
}

// The static guarantee stops at a string conversion, so the ceiling is
// enforced at run time too — at the one point every authored change
// passes through.
func TestTheFamilyDoorRefusesAKindItDoesNotOwn(t *testing.T) {
	h := commitmentHarness(t)
	for _, kind := range []resource.Kind{
		resource.KindAutomation, resource.KindSecret, resource.KindContainer,
	} {
		code, err := h.s.applyAuthored("mika", authored{
			Delete:  []plan.DeletionRef{{Kind: string(kind), Name: "anything"}},
			Summary: "should not happen",
		})
		if err == nil || code != http.StatusForbidden {
			t.Errorf("removing a %s through the family door gave (%d, %v), want 403", kind, code, err)
		}
	}
	code, err := h.s.applyAuthored("mika", authored{
		Put: []resource.Resource{{
			ApiVersion: resource.ApiVersion, Kind: resource.KindAutomation,
			Metadata: resource.Metadata{Name: "sneaky", Owner: "household"},
		}},
		Summary: "should not happen",
	})
	if err == nil || code != http.StatusForbidden {
		t.Errorf("writing an Automation through the family door gave (%d, %v), want 403", code, err)
	}
}

// Being allowed to ANSWER something personal is not being allowed to
// change it.
//
// `visibleTo` lets somebody named in `mayComplete` see a personal
// commitment — a parent can tick off a child's prescription. `mayAuthor`
// is narrower on purpose: they may answer it, and they may not rewrite
// the schedule or delete it. Those are different authorities and the
// gap between them is exactly where this rule lives.
func TestAnsweringSomebodysPersonalThingIsNotRewritingIt(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "kai-refill", resource.CommitmentSpec{
		Description: "Kai's prescription", Rollover: resource.RolloverFromCompletion,
		Anchor: "2026-01-01",
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
		Scope:  resource.CommitmentScopePersonal, MayComplete: []string{"household/mika"},
	}, "household/kai")

	// Mika can see it, because he may answer it.
	page := h.upkeep(t, "mika")
	var seen bool
	for _, row := range page.Calendar {
		if row.Name == "kai-refill" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("somebody who may answer a personal commitment cannot see it")
	}
	// And answer it.
	if code, _ := h.authored(t, http.MethodPost, "/api/v1/home/commitments/kai-refill/done", "mika", nil); code != http.StatusCreated {
		t.Errorf("answering gave %d, want 201", code)
	}
	// But not rewrite it.
	if code, _ := h.authored(t, http.MethodPut, "/api/v1/home/commitments/kai-refill", "mika", map[string]any{
		"description": "Kai's prescription", "every": "1mo",
		"rollover": resource.RolloverFromCompletion, "scope": resource.CommitmentScopePersonal,
	}); code != http.StatusForbidden {
		t.Errorf("editing gave %d, want 403: answering is not authoring", code)
	}
	// Nor delete it.
	if code, _ := h.authored(t, http.MethodDelete, "/api/v1/home/commitments/kai-refill", "mika", nil); code != http.StatusForbidden {
		t.Errorf("deleting gave %d, want 403", code)
	}
	// Its owner can do both.
	if code, _ := h.authored(t, http.MethodDelete, "/api/v1/home/commitments/kai-refill", "kai", nil); code != http.StatusOK {
		t.Errorf("the owner could not remove their own: %d", code)
	}
}

// An edit through the family page must not destroy what the page does
// not show.
//
// The first version rebuilt the spec from the body, which silently
// dropped every due rule after the first — so editing the F-150's oil
// discarded "or every six months". Earliest-wins is the centre of this
// design and the edit path was throwing half of it away.
func TestAnEditKeepsWhatThePageDoesNotShow(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "f150-odometer", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "F-150 odometer", Unit: "miles"},
	}); err != nil {
		t.Fatal(err)
	}
	h.declareCommitment(t, "f150-oil", resource.CommitmentSpec{
		Description: "Oil and filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2025-01-01", Disabled: true,
		AnchorAt: map[string]float64{"f150-odometer": 60000},
		Due: []resource.CommitmentDue{
			{Meter: "f150-odometer", Every: "5000", Lead: "300"},
			{Meter: resource.MeterClock, Every: "6mo", Lead: "14d"},
		},
		SatisfiedBy: &resource.CommitmentSatisfiedBy{EventType: "vehicle.serviced", Entity: "f150"},
		Scope:       resource.CommitmentScopePersonal,
	}, "household/mika")

	// An edit that says nothing about any of it.
	if code, out := h.authored(t, http.MethodPut, "/api/v1/home/commitments/f150-oil", "mika", map[string]any{
		"description": "Oil, filter and a look at the brakes",
		"meter":       "f150-odometer", "every": "6000", "lead": "300",
		"rollover": resource.RolloverFromCompletion,
	}); code != http.StatusOK {
		t.Fatalf("status %d: %+v", code, out)
	}
	current, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range current {
		if res.Kind != resource.KindCommitment || res.Metadata.Name != "f150-oil" {
			continue
		}
		spec := res.Commitment
		if spec.Description != "Oil, filter and a look at the brakes" {
			t.Errorf("description was not applied: %q", spec.Description)
		}
		if len(spec.Due) != 2 {
			t.Fatalf("due = %+v, want the edited rule AND the six-month one it never showed", spec.Due)
		}
		if spec.Due[0].Every != "6000" {
			t.Errorf("the edited rule did not land: %+v", spec.Due[0])
		}
		if spec.Due[1].Meter != resource.MeterClock || spec.Due[1].Every != "6mo" {
			t.Errorf("the second rule was lost or changed: %+v", spec.Due[1])
		}
		if spec.SatisfiedBy == nil || spec.SatisfiedBy.EventType != "vehicle.serviced" {
			t.Errorf("satisfiedBy was dropped: %+v", spec.SatisfiedBy)
		}
		if spec.AnchorAt["f150-odometer"] != 60000 {
			t.Errorf("anchorAt was dropped: %+v", spec.AnchorAt)
		}
		if !spec.Disabled {
			t.Error("the edit un-paused a paused commitment")
		}
		if spec.Anchor != "2025-01-01" {
			t.Errorf("anchor moved to %q — every threshold measured from it moved with it", spec.Anchor)
		}
		if spec.Scope != resource.CommitmentScopePersonal {
			t.Errorf("scope = %q: an edit that did not mention scope published a personal commitment",
				spec.Scope)
		}
		return
	}
	t.Fatal("the commitment vanished")
}

// Removing a meter must account for what the member cannot see.
//
// Naming somebody else's personal commitment would leak what personal
// scope exists to hide; leaving it out entirely lets a member break a
// thing they were never told about. Counting it does neither.
func TestRemovingAMeterCountsWhatItCannotName(t *testing.T) {
	h := commitmentHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
		Metadata: resource.Metadata{Name: "shared-meter", Owner: "household"},
		Meter:    &resource.MeterSpec{Description: "Shared meter", Unit: "units"},
	}); err != nil {
		t.Fatal(err)
	}
	h.declareCommitment(t, "kai-private", resource.CommitmentSpec{
		Description: "Kai's private thing", Rollover: resource.RolloverFromCompletion,
		Anchor: "2026-01-01", Scope: resource.CommitmentScopePersonal,
		Due: []resource.CommitmentDue{{Meter: "shared-meter", Every: "100", Lead: "10"}},
	}, "household/kai")

	code, out := h.authored(t, http.MethodDelete, "/api/v1/home/meters/shared-meter", "mika", nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want a confirmation: %+v", code, out)
	}
	if n, _ := out["hidden"].(float64); n != 1 {
		t.Errorf("hidden = %v, want the one it may not name", out["hidden"])
	}
	consequence, _ := out["consequence"].(string)
	if strings.Contains(consequence, "Kai") {
		t.Errorf("the confirmation names a personal commitment: %q", consequence)
	}
	if !strings.Contains(consequence, "not yours to see") {
		t.Errorf("consequence = %q, want it to account for what it cannot name", consequence)
	}
}

// A name with a past is taken. The ledger keeps history under the name,
// so re-adding the same thing must not inherit somebody else's cycles.
func TestReAddingSomethingDoesNotInheritItsOldHistory(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "furnace-filter", resource.CommitmentSpec{
		Description: "Furnace filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2024-01-01",
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
	}, "household")
	if code, _ := h.authored(t, http.MethodPost, "/api/v1/home/commitments/furnace-filter/done", "mika", nil); code != http.StatusCreated {
		t.Fatal("closing it")
	}
	if code, _ := h.authored(t, http.MethodDelete, "/api/v1/home/commitments/furnace-filter", "mika", nil); code != http.StatusOK {
		t.Fatal("removing it")
	}
	code, out := h.authored(t, http.MethodPost, "/api/v1/home/commitments", "mika", map[string]any{
		"description": "Furnace filter", "every": "3mo", "lead": "14d",
		"rollover": resource.RolloverFromCompletion,
	})
	if code != http.StatusCreated {
		t.Fatalf("status %d: %+v", code, out)
	}
	if out["commitment"] == "furnace-filter" {
		t.Error("the new one took the old name and inherited four months of somebody else's history")
	}
	page := h.history(t, "mika", out["commitment"].(string))
	if len(page.Cycles) != 0 {
		t.Errorf("a newly added thing starts with %d closed cycles", len(page.Cycles))
	}
}

// A paused commitment is shown, and shown as paused. Hiding it makes
// the tracker incomplete; showing it as overdue tells somebody to act
// on a thing the house was told to stop asking about.
func TestAPausedCommitmentReadsAsPausedAndOffersNothing(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "paused-thing", resource.CommitmentSpec{
		Description: "Something on hold", Rollover: resource.RolloverFromCompletion,
		Anchor: "2024-01-01", Disabled: true,
		Due: []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
	}, "household")

	page := h.upkeep(t, "mika")
	for _, row := range page.Calendar {
		if row.Name != "paused-thing" {
			continue
		}
		if !row.Paused || row.Standing != "paused" {
			t.Errorf("row = %+v, want it to read as paused rather than overdue", row)
		}
		if len(row.Asks) != 0 {
			t.Errorf("a paused commitment offers %+v to answer", row.Asks)
		}
		return
	}
	t.Error("a paused commitment vanished from the tracker entirely")
}

// The note is what makes a maintenance history worth keeping. It was
// stored, typed, rendered — and never written.
func TestANoteOnACompletionIsActuallyRecorded(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "filter", resource.CommitmentSpec{
		Description: "Furnace filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2024-01-01",
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
	}, "household")
	if code, _ := h.authored(t, http.MethodPost, "/api/v1/home/commitments/filter/done", "mika",
		map[string]any{"note": "the big one, $22 at the hardware shop"}); code != http.StatusCreated {
		t.Fatal("closing it")
	}
	page := h.history(t, "mika", "filter")
	if len(page.Cycles) != 1 {
		t.Fatalf("cycles = %d", len(page.Cycles))
	}
	if page.Cycles[0].Note != "the big one, $22 at the hardware shop" {
		t.Errorf("note = %q — stored, typed, rendered and never written", page.Cycles[0].Note)
	}
}

// A write that lost a race is refused, not silently applied.
//
// Dropping the approval step dropped the check that came with it: the
// Plans path refuses a plan whose Statefile moved since it was
// computed, and this path had nothing. Two members editing the same
// commitment both read, both wrote, and whichever landed last
// overwrote the other — with a 200 for the one whose change vanished.
func TestAWriteThatLostARaceIsRefusedRatherThanSilentlyWinning(t *testing.T) {
	h := commitmentHarness(t)
	h.declareCommitment(t, "filter", resource.CommitmentSpec{
		Description: "Furnace filter", Rollover: resource.RolloverFromCompletion,
		Anchor: "2024-01-01",
		Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo", Lead: "14d"}},
	}, "household")

	// Sandy's edit is computed against the head as it is now.
	basedOn, err := h.s.State.Head()
	if err != nil {
		t.Fatal(err)
	}
	// Mika's lands first, moving the head.
	if code, _ := h.authored(t, http.MethodPut, "/api/v1/home/commitments/filter", "mika", map[string]any{
		"description": "Furnace filter (Mika)", "every": "3mo", "lead": "14d",
		"rollover": resource.RolloverFromCompletion,
	}); code != http.StatusOK {
		t.Fatalf("the first edit did not land: %d", code)
	}
	moved, err := h.s.State.Head()
	if err != nil {
		t.Fatal(err)
	}
	if moved == basedOn {
		t.Fatal("the head did not move, so this test proves nothing")
	}
	// Sandy's now conflicts. It must be refused rather than overwrite.
	code, err := h.s.applyAuthored("sandy", authored{
		Put: []resource.Resource{{
			ApiVersion: resource.ApiVersion, Kind: resource.KindCommitment,
			Metadata: resource.Metadata{Name: "filter", Owner: "household"},
			Commitment: &resource.CommitmentSpec{
				Description: "Furnace filter (Sandy)", Rollover: resource.RolloverFromCompletion,
				Anchor: "2024-01-01",
				Due:    []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "6mo", Lead: "14d"}},
			},
		}},
		Summary: "sandy's edit",
		BasedOn: basedOn,
	})
	if code != http.StatusConflict || err == nil {
		t.Fatalf("a losing write gave (%d, %v), want 409 — the member whose change vanished "+
			"should not be told it worked", code, err)
	}
	// And Mika's is still what is there.
	current, lerr := h.s.State.Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, res := range current {
		if res.Kind == resource.KindCommitment && res.Metadata.Name == "filter" {
			if res.Commitment.Description != "Furnace filter (Mika)" {
				t.Errorf("description = %q, want the write that won", res.Commitment.Description)
			}
			return
		}
	}
	t.Fatal("the commitment vanished")
}

// "6m" is not six minutes on the family page.
//
// Validation reads Go's units, where m is minutes, so a household typing
// "every 6m" for six months got a reminder every six minutes, accepted
// silently. The page's door refuses anything shorter than a day on the
// calendar and says what m would have meant — on add and on edit.
func TestTheCalendarDoesNotCountInMinutes(t *testing.T) {
	h := commitmentHarness(t)
	for _, c := range []struct{ every, lead, want string }{
		{"6m", "", "for months, write 6mo"},
		{"1yr", "2m", "for months, write 2mo"},
		{"36h", "", "shorter than a day"},
	} {
		code, out := h.authored(t, http.MethodPost, "/api/v1/home/commitments", "mika", map[string]any{
			"description": "Filter", "every": c.every, "lead": c.lead, "rollover": resource.RolloverFromCompletion,
		})
		if code == http.StatusCreated {
			t.Errorf("every %q lead %q was accepted", c.every, c.lead)
			continue
		}
		if !strings.Contains(fmt.Sprint(out), c.want) {
			t.Errorf("every %q lead %q: refusal %v does not say %q", c.every, c.lead, out, c.want)
		}
	}
	// What the page actually sends still works.
	if code, out := h.authored(t, http.MethodPost, "/api/v1/home/commitments", "mika", map[string]any{
		"description": "Registration", "every": "1yr", "lead": "2w", "anchor": "2026-03-01",
		"rollover": resource.RolloverFromSchedule,
	}); code != http.StatusCreated {
		t.Fatalf("a calendar reminder in years was refused: %d %+v", code, out)
	}

	h.declareCommitment(t, "filter", resource.CommitmentSpec{
		Description: "Furnace filter", Rollover: resource.RolloverFromCompletion, Anchor: "2024-01-01",
		Due: []resource.CommitmentDue{{Meter: resource.MeterClock, Every: "3mo"}},
	}, "household")
	if code, _ := h.authored(t, http.MethodPut, "/api/v1/home/commitments/filter", "mika", map[string]any{
		"description": "Furnace filter", "every": "3m", "rollover": resource.RolloverFromCompletion,
	}); code == http.StatusOK {
		t.Error("an edit to every 3m (minutes) was accepted")
	}
}

// A one-off with a date is what the page sends for "just the once": an
// absolute due date and no interval.
func TestAOneOffOnTheCalendarTakesADate(t *testing.T) {
	h := commitmentHarness(t)
	if code, out := h.authored(t, http.MethodPost, "/api/v1/home/commitments", "mika", map[string]any{
		"description": "Renew passport", "at": "2027-03-01", "lead": "2mo", "rollover": resource.RolloverOnce,
	}); code != http.StatusCreated {
		t.Fatalf("a dated one-off was refused: %d %+v", code, out)
	}
}
