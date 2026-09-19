package api

import (
	"bytes"
	"encoding/json"
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
	if plans[0].State != runtimedb.PlanApproved {
		t.Errorf("plan state = %q, want it already applied", plans[0].State)
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
