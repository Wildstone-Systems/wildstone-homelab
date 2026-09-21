package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestApprovalRequiresAFreshOwnerSession(t *testing.T) {
	h := freshnessHarness(t)
	id, err := h.s.DB.InsertPlan("butler", "sandy", "review test", "low", "{}")
	if err != nil {
		t.Fatal(err)
	}
	stale := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-20*time.Minute)))
	code, _ := h.sessionRequest(http.MethodPost, "/api/v1/plans/"+id+"/approve", "", "mika", stale)
	if code != http.StatusForbidden {
		t.Fatalf("stale approval reached mutation: %d", code)
	}
	p, err := h.s.DB.GetPlan(id)
	if err != nil || p.State != runtimedb.PlanProposed {
		t.Fatalf("stale approval changed plan: %+v %v", p, err)
	}
}

func approveRoutineFixture(t *testing.T, h *harness, name string) string {
	t.Helper()
	if _, err := h.s.DB.GetButlerMember("mika"); err != nil {
		if err = h.s.DB.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.DB.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	r, err := h.s.DB.GetHouseholdRoutine(name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := h.s.DB.SubmitRoutinePlan(name, "mika", "", "aud_household", "fixture delivery", r.Document)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRoutineApprovalOnlyThroughFreshOwnerConsole(t *testing.T) {
	h := freshnessHarness(t)
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "review-me", Ownership: runtimedb.RoutineWildstone, Author: "sandy", Document: routineDoc("review-me")}); err != nil {
		t.Fatal(err)
	}
	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/household/routines/review-me/submit", `{"outputAudience":"aud_household","agent":"someone-else"}`, "mika", session); code != http.StatusBadRequest {
		t.Fatalf("request could name a proposing Agent: %d", code)
	}
	code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/routines/review-me/submit", `{"outputAudience":"aud_household"}`, "mika", session)
	if code != http.StatusOK {
		t.Fatalf("submit: %d %v", code, out)
	}
	id := out["planId"].(string)
	for _, role := range []runtimedb.Role{runtimedb.RoleButler, runtimedb.RoleConcierge, runtimedb.RoleAdmin} {
		if res := h.do(role, http.MethodPost, "/api/v1/plans/"+id+"/approve", nil, nil); res.StatusCode != http.StatusForbidden {
			t.Fatalf("credential %s approved: %d", role, res.StatusCode)
		}
	}
	r, _ := h.s.DB.GetHouseholdRoutine("review-me")
	if r.Active() {
		t.Fatal("proposal or service activated routine")
	}
	code, out = h.sessionRequest(http.MethodPost, "/api/v1/plans/"+id+"/approve", "", "mika", session)
	if code != http.StatusOK {
		t.Fatalf("approve: %d %v", code, out)
	}
	r, _ = h.s.DB.GetHouseholdRoutine("review-me")
	if !r.Active() {
		t.Fatal("reviewed version not activated")
	}
}

func TestRoutineResourceRequestIsVisibleAndGrantedOnlyAfterReview(t *testing.T) {
	h := freshnessHarness(t)
	h.agentAt(t, "family", "telegram")
	memory := seedMemory(t, h, "family appointment", runtimedb.HouseholdOwner, runtimedb.ButlerAudienceShared)
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "resource-review", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: routineDoc("resource-review")}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AssignRoutineButler("resource-review", agentFor("family"), "mika"); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	request := fmt.Sprintf(`{"outputAudience":"aud_household","memoryIds":[%d]}`, memory)
	code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/routines/resource-review/submit", request, "mika", cookie)
	if code != http.StatusOK {
		t.Fatalf("submit: %d %v", code, out)
	}
	id := out["planId"].(string)
	if _, err := h.s.DB.BindRoutinePrincipal(id); err == nil {
		t.Fatal("submission granted unattended authority before approval")
	}
	code, out = h.sessionRequest(http.MethodGet, "/api/v1/plans/"+id, "", "mika", cookie)
	if code != http.StatusOK {
		t.Fatalf("review snapshot: %d %v", code, out)
	}
	routine, ok := out["routine"].(map[string]any)
	if !ok || len(routine["requiredMemory"].([]any)) != 1 {
		t.Fatalf("review omitted requested record: %v", out)
	}
	code, out = h.sessionRequest(http.MethodPost, "/api/v1/plans/"+id+"/approve", "", "mika", cookie)
	if code != http.StatusOK {
		t.Fatalf("approve: %d %v", code, out)
	}
	p, err := h.s.DB.BindRoutinePrincipal(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.CheckRoutineMemory(p, memory, runtimedb.MemoryDestination{BuiltIn: true}); err != nil {
		t.Fatalf("approved grant not usable: %v", err)
	}
}
func TestFreshOwnerRevisionTokenApprovesOnce(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkTrustedIngress, "mika", "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "morning", Ownership: runtimedb.RoutineWildstone, Author: "sandy", Document: routineDoc("morning")}); err != nil {
		t.Fatal(err)
	}
	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/routines/morning/submit", `{"outputAudience":"aud_household"}`, "mika", session)
	if code != 200 {
		t.Fatal(code, out)
	}
	tokenBody, _ := json.Marshal(map[string]any{"planId": out["planId"], "member": "mika", "routine": "morning"})
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/review/token", json.RawMessage(tokenBody), nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("bearer minted review token: %d", res.StatusCode)
	}
	stale := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-20*time.Minute)))
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/household/review/token", string(tokenBody), "mika", stale); code != http.StatusForbidden {
		t.Fatalf("stale session minted review token: %d", code)
	}
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/household/review/token", string(tokenBody), "mika", session); code != http.StatusOK {
		t.Fatalf("fresh Owner token: %d", code)
	}
	h.person("sam")
	memberBody, _ := json.Marshal(map[string]any{"planId": out["planId"], "member": "sam", "routine": "morning"})
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/household/review/token", string(memberBody), "mika", session); code != http.StatusBadRequest {
		t.Fatalf("issued unusable token to non-Owner: %d", code)
	}
	r, _ := h.s.DB.GetHouseholdRoutine("morning")
	token, err := h.s.DB.IssueHouseholdReviewToken(out["planId"].(string), "mika", "mika", r.Seq, r.Revision, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"token": token, "approve": true})
	for _, want := range []int{http.StatusOK, http.StatusForbidden} {
		code, out = h.sessionRequest(http.MethodPost, "/api/v1/household/review", string(body), "mika", session)
		if code != want {
			t.Fatalf("token: got %d want %d %v", code, want, out)
		}
	}
}

func TestLegacyRoutineReviewRequiresResubmission(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkTrustedIngress, "mika", "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	id := storePlan(t, h, resource.KindAutomation)
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	body, _ := json.Marshal(map[string]any{"planId": id, "member": "mika"})
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/household/review/token", string(body), "mika", cookie); code != http.StatusBadRequest {
		t.Fatalf("legacy Plan got unusable link: %d", code)
	}
	token := legacyReviewTokenFixture(t, h, id, "mika", "")
	body, _ = json.Marshal(map[string]any{"token": token, "approve": true})
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/household/review", string(body), "mika", cookie); code != http.StatusConflict {
		t.Fatalf("legacy token bypassed snapshot requirement: %d", code)
	}
	record, err := h.s.DB.GetPlan(id)
	if err != nil || record.State != runtimedb.PlanProposed {
		t.Fatalf("legacy review changed Plan: %+v %v", record, err)
	}
}

func TestRoutineReviewEnforcesStandingPolicy(t *testing.T) {
	for _, tc := range []struct{ name, rule, doc string }{
		{"tier", "automation-action-tier", strings.Replace(routineDoc("policy-test"), "tier: domestic", "tier: high", 1)},
		{"credential parameter", "secret-values-forbidden", strings.Replace(routineDoc("policy-test"), "params: { message:", "params: { api_key: fixture, message:", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := freshnessHarness(t)
			h.agentAt(t, "family", "telegram")
			if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "policy-test", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: tc.doc}); err != nil {
				t.Fatal(err)
			}
			cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
			code, out := h.sessionRequest(http.MethodPost, "/api/v1/household/routines/policy-test/submit", `{"outputAudience":"aud_household"}`, "mika", cookie)
			raw, _ := json.Marshal(out)
			if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), tc.rule) {
				t.Errorf("human proposal bypassed policy: %d %s", code, raw)
			}
			_, err := h.s.RoutineProposalSeam("sandy")(context.Background(), butler.RoutineProposalCall{Name: "policy-test", Member: "mika", OutputAudience: "aud_household"})
			if err == nil || !strings.Contains(err.Error(), tc.rule) {
				t.Errorf("Agent proposal bypassed policy: %v", err)
			}
			// A pre-existing stored proposal must be rechecked when the Owner approves.
			id, err := h.s.DB.SubmitRoutinePlan("policy-test", "mika", "", "aud_household", "fixture delivery", tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			code, out = h.sessionRequest(http.MethodPost, "/api/v1/plans/"+id+"/approve", "", "mika", cookie)
			raw, _ = json.Marshal(out)
			if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), tc.rule) {
				t.Errorf("approval bypassed policy: %d %s", code, raw)
			}
			routine, err := h.s.DB.GetHouseholdRoutine("policy-test")
			if err != nil || routine.Active() {
				t.Fatalf("policy-invalid routine authorized: %+v %v", routine, err)
			}
		})
	}
}

func TestAgentRoutinePlanStaysInConsoleWithoutDisclosureProvenance(t *testing.T) {
	h := freshnessHarness(t)
	h.agentAt(t, "family", "telegram")
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "private-routine", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: routineDoc("private-routine")}); err != nil {
		t.Fatal(err)
	}
	id, err := h.s.RoutineProposalSeam("sandy")(context.Background(), butler.RoutineProposalCall{Name: "private-routine", Member: "mika", OutputAudience: "aud_household"})
	if err != nil {
		t.Fatal(err)
	}
	token := h.console(runtimedb.RoleAdmin, "mika")
	for _, path := range []string{"/api/v1/plans/" + id, "/api/v1/plans", "/api/v1/owner-review"} {
		var body json.RawMessage
		res := h.doToken(token, http.MethodGet, path, nil, &body)
		if res.StatusCode >= 500 || strings.Contains(string(body), "private-routine") {
			t.Errorf("Agent Plan escaped through %s: %d %s", path, res.StatusCode, body)
		}
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	if code, out := h.sessionRequest(http.MethodGet, "/api/v1/plans/"+id, "", "mika", cookie); code != http.StatusOK {
		t.Fatalf("Console cannot review Plan: %d %v", code, out)
	}
}

func TestGenericPlansCannotAuthorizeAutomationWrites(t *testing.T) {
	h := freshnessHarness(t)
	doc := routineDoc("unreviewed-rule")
	if response := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans", map[string]any{"summary": "add routine", "put": []string{doc}}, nil); response.StatusCode != http.StatusBadRequest {
		t.Errorf("generic submission bypassed routine review: %d", response.StatusCode)
	}
	head, err := h.s.State.Head()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(storedProposal{BasedOnHead: head, Summary: "legacy routine write", Put: []string{doc}})
	id, err := h.s.DB.InsertPlan("mika", "mika", "legacy routine write", "low", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if response := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+id+"/approve", nil, nil); response.StatusCode != http.StatusConflict {
		t.Errorf("bearer activated generic Automation: %d", response.StatusCode)
	}
	cookie := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/plans/"+id+"/approve", "", "mika", cookie); code != http.StatusConflict {
		t.Errorf("Console skipped exact-version review: %d", code)
	}
	resources, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resources {
		if r.Kind == resource.KindAutomation && r.Metadata.Name == "unreviewed-rule" {
			t.Fatal("unreviewed Automation reached Statefile")
		}
	}
}
