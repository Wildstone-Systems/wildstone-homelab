package api

// Only a human Owner decides a Plan (ADR-W033 E2/E3, ADR-W007 A2).
// Agents and Members may propose; an Owner approves the exact version,
// in the Console or with the host credential. A machine credential that
// holds the approve verb — `operator` — still decides nothing.

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// A Member cannot approve a routine Plan by any route: not with a
// credential acting for them, not through the Console door, and not
// through the household review route, which no longer exists.
func TestNonOwnerCannotApproveARoutinePlanByAnyRoute(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkTrustedIngress, "sandy", "sandy", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: "member-routine", Ownership: runtimedb.RoutineWildstone, Author: "sandy",
		Document: routineDoc("member-routine"),
	}); err != nil {
		t.Fatal(err)
	}
	owner := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/routines/member-routine/submit",
		`{"outputAudience":"aud_household"}`, "mika", owner)
	if code != http.StatusOK {
		t.Fatalf("submit: %d %v", code, out)
	}
	id := out["planId"].(string)
	approve := "/api/v1/plans/" + id + "/approve"

	for _, principalName := range []string{"op", "butler", "root-admin"} {
		tok, err := h.s.Principals.Mint(principalName, "sandy", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res := h.doToken(tok, http.MethodPost, approve, nil, nil); res.StatusCode == http.StatusOK {
			t.Fatalf("%s acting for a Member approved a routine Plan", principalName)
		}
	}
	member := sessionCookie(h.s.StartConsoleSession("sandy", time.Now()))
	if code, _ := h.sessionRequest(http.MethodPost, approve, "", "sandy", member); code == http.StatusOK {
		t.Fatal("a Member's Console request approved a routine Plan")
	}
	for _, path := range []string{"/api/v1/household/review", "/api/v1/household/review/token"} {
		body := `{"planId":"` + id + `","member":"sandy","token":"x","approve":true}`
		if code, _ := h.sessionRequest(http.MethodPost, path, body, "sandy", member); code != http.StatusNotFound {
			t.Fatalf("%s answered %d; the household review route must not exist", path, code)
		}
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, path, map[string]any{"planId": id}, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s answered %d to the host credential", path, res.StatusCode)
		}
	}
	if r, _ := h.s.DB.GetHouseholdRoutine("member-routine"); r.Active() {
		t.Fatal("a non-Owner activated the routine")
	}
	if rec, err := h.s.DB.GetPlan(id); err != nil || rec.State != runtimedb.PlanProposed {
		t.Fatalf("a refused approval changed the Plan: %+v %v", rec, err)
	}

	// The Owner, in the Console, at the exact version.
	if code, out := h.sessionRequest(http.MethodPost, approve, "", "mika", owner); code != http.StatusOK {
		t.Fatalf("the Owner could not approve: %d %v", code, out)
	}
	if r, _ := h.s.DB.GetHouseholdRoutine("member-routine"); !r.Active() {
		t.Fatal("the Owner's approval did not activate the routine")
	}
}

// The operator role holds the approve verb for machine operations, and
// still cannot approve or reject a Plan. The host credential can: it is
// how the installer and `wild plan approve` act for the Owner at the host.
func TestOperatorCannotDecideAPlanAndTheOwnerCan(t *testing.T) {
	h := newHarness(t)
	var rec runtimedb.PlanRecord
	if res := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/plans",
		proposalRequest{Summary: "deploy immich", Put: []string{containerYAML(t, "immich", "ghcr.io/immich-app/immich-server@"+testDigest, nil)}},
		&rec); res.StatusCode != http.StatusCreated {
		t.Fatalf("propose = %d", res.StatusCode)
	}
	for _, verb := range []string{"approve", "reject"} {
		for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler, runtimedb.RoleConcierge} {
			if res := h.do(role, http.MethodPost, "/api/v1/plans/"+rec.ID+"/"+verb, nil, nil); res.StatusCode != http.StatusForbidden {
				t.Fatalf("%s could %s a Plan: %d", role, verb, res.StatusCode)
			}
		}
	}
	if got, err := h.s.DB.GetPlan(rec.ID); err != nil || got.State != runtimedb.PlanProposed {
		t.Fatalf("a refused decision changed the Plan: %+v %v", got, err)
	}
	var approved runtimedb.PlanRecord
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+rec.ID+"/approve", nil, &approved); res.StatusCode != http.StatusOK {
		t.Fatalf("the host credential could not approve: %d", res.StatusCode)
	}
	if approved.State != runtimedb.PlanApproved || approved.ResolvedBy != "root-admin" {
		t.Fatalf("approved record = %+v", approved)
	}
}
