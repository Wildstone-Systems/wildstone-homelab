package api

// Household routines (ADR-W030 A6 as changed by ADR-W033 E2/E3):
// native and Wildstone ownership stay distinct, trial and disable are
// exact, and no route other than an Owner's decision in the Console
// approves a routine Plan — not chat, and not a household review link.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// reviewHarness sets up an admitted Member (sam) linked through the
// trusted ingress, reachable from a known peer.
func reviewHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	// Identity-header paths are only reachable from the identity proxy,
	// and httptest speaks from loopback.
	h.s.SetTrustedIngress([]string{"127.0.0.1", "::1"})
	if err := h.s.DB.RecordButlerMember("sam", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	// Authelia asserts "sam@example.com"; the link says that is sam.
	if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkTrustedIngress,
		"sam@example.com", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	return h
}

// storePlan records a plan with the given step kinds and returns its id.
func storePlan(t *testing.T, h *harness, kinds ...resource.Kind) string {
	t.Helper()
	p := plan.Plan{Summary: "test"}
	for _, k := range kinds {
		p.Steps = append(p.Steps, plan.PlanStep{Kind: k, Name: "x", Description: "step"})
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	id, err := h.s.DB.InsertPlan("butler", "sam", "test", "low", string(raw))
	if err != nil {
		t.Fatalf("InsertPlan: %v", err)
	}
	return id
}

// Native and Wildstone routines stay distinct, and the distinction
// cannot be edited away.
func TestRoutineOwnershipStaysDistinct(t *testing.T) {
	h := reviewHarness(t)

	// A Wildstone routine has no external id: Wildstone runs it.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/routines",
		map[string]any{
			"name": "bad", "ownership": "wildstone", "externalId": "automation.morning",
			"connection": "home", "document": "steps: []",
		}, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a Wildstone routine took an external id: %d", res.StatusCode)
	}
	// A native one needs both the application and its id.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/routines",
		map[string]any{"name": "bad2", "ownership": "native", "document": "note: theirs"},
		nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a native routine was recorded with no application: %d", res.StatusCode)
	}
	for _, r := range []map[string]any{
		{"name": "ha-morning", "ownership": "native", "externalId": "automation.morning",
			"connection": "home", "document": "note: runs inside Home Assistant"},
		{"name": "breakfast", "ownership": "wildstone", "document": "steps: []"},
	} {
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/routines", r, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("creating %v = %d", r["name"], res.StatusCode)
		}
	}
	// Ownership cannot be edited: that would move responsibility for
	// running it without anyone deciding to.
	if _, err := h.s.DB.GetHouseholdRoutine("ha-morning"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.ExecForTest(
		`UPDATE household_routines SET ownership = 'wildstone' WHERE name = 'ha-morning'`); err == nil {
		t.Fatal("a native routine became a Wildstone one")
	}
}

// Trial runs but performs nothing; disable is exact and immediate.
func TestTrialRunsAndDisableIsExact(t *testing.T) {
	h := freshnessHarness(t)
	for _, name := range []string{"breakfast", "bedtime"} {
		if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
			Name: name, Ownership: runtimedb.RoutineWildstone, Author: "sam", Document: "steps: []",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var state struct {
		Routine         runtimedb.HouseholdRoutine `json:"routine"`
		PerformsActions bool                       `json:"performsActions"`
	}
	approveRoutineFixture(t, h, "breakfast")
	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	for _, target := range []string{"trial", "enabled"} {
		code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/routines/breakfast/state", `{"state":"`+target+`"}`, "mika", session)
		if code != http.StatusOK {
			t.Fatalf("fresh Owner changing approved mode: %d %v", code, out)
		}
		routine, err := h.s.DB.GetHouseholdRoutine("breakfast")
		if err != nil {
			t.Fatal(err)
		}
		if !routine.Active() || routine.PerformsActions() != (target == "enabled") {
			t.Fatalf("wrong approved mode: %+v", routine)
		}
	}

	// Disabling one disables exactly one.
	code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/routines/breakfast/state", `{"state":"disabled"}`, "mika", session)
	if code != http.StatusOK {
		t.Fatalf("Owner disable: %d %v", code, out)
	}
	state.Routine, _ = h.s.DB.GetHouseholdRoutine("breakfast")
	if state.Routine.Active() || state.Routine.DisabledBy == "" {
		t.Fatalf("disabled routine = %+v", state.Routine)
	}
	other, err := h.s.DB.GetHouseholdRoutine("bedtime")
	if err != nil {
		t.Fatal(err)
	}
	if other.DisabledAt.IsZero() == false {
		t.Fatal("disabling one routine disabled another")
	}
}

// Chat cannot approve by typing, and the refusal explains where it can
// be done instead.
func TestChatCannotApproveAPlanByAnyRoute(t *testing.T) {
	h := reviewHarness(t)
	if err := h.s.DB.SetButlerMemberRole("sam", runtimedb.RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	planID := storePlan(t, h, resource.KindAutomation)

	t.Run("typing approval is refused on both surfaces", func(t *testing.T) {
		// One Agent of each class, because the refusal differs by where
		// approval actually happens: a household Agent points at an
		// Owner, an Owner Agent at the Console.
		for surface, want := range map[string]string{
			"family": "Owner",
			"admin":  "Console",
		} {
			body, _, handled := h.s.runChatCommand(
				chatCommandCtx{At: h.agentAt(t, surface, "telegram")}, "/approve "+planID)
			if !handled {
				t.Fatalf("/approve on %s was not handled; a person would be told the command does not exist", surface)
			}
			if !strings.Contains(body, want) {
				t.Fatalf("/approve on %s said %q, which does not point anywhere useful", surface, body)
			}
		}
		// And nothing happened to the plan.
		record, err := h.s.DB.GetPlan(planID)
		if err != nil {
			t.Fatal(err)
		}
		if record.State != runtimedb.PlanProposed {
			t.Fatalf("a chat message changed a plan's state to %q", record.State)
		}
	})

	// There is no chat callback that approves a Plan, and no household
	// review route either: an Owner decides in the Console (ADR-W033 E3).
	t.Run("there is no chat or household route that approves a plan", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/household/review/callback",
			"/api/v1/household/review/chat",
		} {
			res := h.do(runtimedb.RoleConcierge, http.MethodPost, path,
				map[string]any{"bindingId": 1, "approve": true}, nil)
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("%s answered %d; a chat route that approves a plan must not exist", path, res.StatusCode)
			}
		}
		record, err := h.s.DB.GetPlan(planID)
		if err != nil {
			t.Fatal(err)
		}
		if record.State != runtimedb.PlanProposed {
			t.Fatalf("a plan reached %q without an Owner deciding it", record.State)
		}
	})
}

func timeSoon() time.Time { return time.Now().Add(10 * time.Minute) }
