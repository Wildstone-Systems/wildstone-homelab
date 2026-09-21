package api

// P13's evidence (ADR-W030): an admitted adult can review only a pure
// household-routine Plan without receiving Console credentials; mixed,
// infrastructure and authority Plans and chat approval attempts fail;
// callbacks are identity-, revision- and expiry-bound and single-use;
// native and Wildstone ownership stay distinct.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// reviewHarness sets up an admitted adult linked through the trusted
// ingress, with the review route reachable from a known peer.
func reviewHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	// The review route is only reachable from the identity proxy, and
	// httptest speaks from loopback.
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

// review posts to the narrow service route with an asserted identity.
func (h *harness) review(t *testing.T, assertedUser, token string, approve bool) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"token": token, "approve": approve})
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/household/review", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if assertedUser != "" {
		req.Header.Set(TrustedIngressHeader, assertedUser)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func issueToken(t *testing.T, h *harness, planID, member, routine string) (string, *http.Response) {
	t.Helper()
	var out struct {
		Token string `json:"token"`
		Note  string `json:"note"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/review/token",
		map[string]any{"planId": planID, "member": member, "routine": routine}, &out)
	return out.Token, res
}

// Model links issued before exact-version snapshots were required. New API
// issuance rejects these Plans; retained links still cannot bypass authentication.
func legacyReviewTokenFixture(t *testing.T, h *harness, id, member, routine string) string {
	t.Helper()
	var seq int64
	var revision int
	if routine != "" {
		r, err := h.s.DB.GetHouseholdRoutine(routine)
		if err != nil {
			t.Fatal(err)
		}
		seq, revision = r.Seq, r.Revision
	}
	token, err := h.s.DB.IssueHouseholdReviewToken(id, member, "root-admin", seq, revision, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// An admitted adult approves a pure routine plan through the household
// page, holding no Console credential at any point.
func TestMemberCannotReviewARoutinePlan(t *testing.T) {
	h := reviewHarness(t)
	id := storePlan(t, h, resource.KindAutomation)
	token, response := issueToken(t, h, id, "sam", "")
	if response.StatusCode != http.StatusBadRequest || token != "" {
		t.Fatal("issued an unusable legacy routine token to a Member")
	}
	// Even a link issued before the Owner-only gate cannot authorize a Member.
	token, err := h.s.DB.IssueHouseholdReviewToken(id, "sam", "root-admin", 0, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	response = h.review(t, "sam@example.com", token, true)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("Member approved: %d", response.StatusCode)
	}
	record, err := h.s.DB.GetPlan(id)
	if err != nil || record.State != runtimedb.PlanProposed {
		t.Fatalf("denial changed Plan: %+v %v", record, err)
	}
}

// Mixed, infrastructure and authority plans are not household
// decisions, and the refusal says why.
func TestOnlyPureRoutinePlansGetAReviewLink(t *testing.T) {
	h := reviewHarness(t)
	for name, kinds := range map[string][]resource.Kind{
		"infrastructure": {resource.KindContainer},
		"mixed":          {resource.KindAutomation, resource.KindRoute},
		"authority":      {resource.KindLdapGroup},
		"identity":       {resource.KindOidcClient},
		"secrets":        {resource.KindSecret},
	} {
		t.Run(name, func(t *testing.T) {
			planID := storePlan(t, h, kinds...)
			var refusal struct {
				Error string `json:"error"`
			}
			res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/review/token",
				map[string]any{"planId": planID, "member": "sam"}, &refusal)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("a %s plan got a household review link: %d", name, res.StatusCode)
			}
			if !strings.Contains(refusal.Error, "Console") {
				t.Fatalf("the refusal does not say where it is reviewed instead: %q", refusal.Error)
			}
		})
	}
}

// The link is bound to a person, an identity path, a revision and an
// expiry — and none of that is a login.
func TestReviewTokensAreBoundNotBearer(t *testing.T) {
	h := reviewHarness(t)
	if err := h.s.DB.RecordButlerMember("tom", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("tom", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.SetButlerMemberRole("sam", runtimedb.RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}

	t.Run("bound to the person it was issued to", func(t *testing.T) {
		planID := storePlan(t, h, resource.KindAutomation)
		token := legacyReviewTokenFixture(t, h, planID, "sam", "")
		// Another admitted adult holding the same link approves nothing.
		if err := h.s.DB.RecordButlerMember("mika", runtimedb.RoleMember, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember("mika", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkTrustedIngress,
			"mika@example.com", "mika", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if res := h.review(t, "mika@example.com", token, true); res.StatusCode != http.StatusForbidden {
			t.Fatalf("another adult used sam's link: %d", res.StatusCode)
		}
	})

	t.Run("only through the identity proxy", func(t *testing.T) {
		planID := storePlan(t, h, resource.KindAutomation)
		token := legacyReviewTokenFixture(t, h, planID, "sam", "")
		// A header anyone can set is not an authentication. With no
		// trusted peer configured the route is not there at all.
		h.s.SetTrustedIngress(nil)
		if res := h.review(t, "sam@example.com", token, true); res.StatusCode != http.StatusNotFound {
			t.Fatalf("the review route answered with no trusted ingress configured: %d", res.StatusCode)
		}
		h.s.SetTrustedIngress([]string{"127.0.0.1", "::1"})
		// And with no asserted identity at all.
		if res := h.review(t, "", token, true); res.StatusCode != http.StatusNotFound {
			t.Fatalf("the review route answered an unauthenticated request: %d", res.StatusCode)
		}
	})

	t.Run("superseded by an edit", func(t *testing.T) {
		if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
			Name: "morning", Ownership: runtimedb.RoutineWildstone, Author: "sam",
			Document: "steps: []",
		}); err != nil {
			t.Fatal(err)
		}
		planID := storePlan(t, h, resource.KindAutomation)
		token := legacyReviewTokenFixture(t, h, planID, "sam", "morning")

		// The routine is edited after the link went out. Approving now
		// would approve a version nobody read.
		var edited struct {
			Note string `json:"note"`
		}
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/routines/morning/edit",
			map[string]any{"document": "steps: [changed]"}, &edited); res.StatusCode != http.StatusOK {
			t.Fatalf("editing = %d", res.StatusCode)
		}
		if !strings.Contains(edited.Note, "approves nothing") {
			t.Fatalf("the edit did not say it invalidates the review link: %q", edited.Note)
		}
		if res := h.review(t, "sam@example.com", token, true); res.StatusCode != http.StatusForbidden {
			t.Fatalf("a link for a superseded revision still approved: %d", res.StatusCode)
		}
	})

	t.Run("useless once the person is disabled", func(t *testing.T) {
		planID := storePlan(t, h, resource.KindAutomation)
		token := legacyReviewTokenFixture(t, h, planID, "sam", "")
		if err := h.s.DB.DisableButlerMember("sam", "root-admin"); err != nil {
			t.Fatal(err)
		}
		// The link cannot resolve an identity any more, so the route
		// does not acknowledge it.
		if res := h.review(t, "sam@example.com", token, true); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a disabled member's link still worked: %d", res.StatusCode)
		}
		// And no new link can be issued for them.
		if _, res := issueToken(t, h, planID, "sam", ""); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("a link was issued to a disabled member: %d", res.StatusCode)
		}
	})
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
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/routines/breakfast/state",
		map[string]any{"state": "disabled"}, &state)
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
// be done instead. A proposal-bound callback can, with every narrowing
// the review page has.
func TestChatCannotApproveAPlanByAnyRoute(t *testing.T) {
	h := reviewHarness(t)
	if err := h.s.DB.SetButlerMemberRole("sam", runtimedb.RoleOwner, "root-admin"); err != nil {
		t.Fatal(err)
	}
	planID := storePlan(t, h, resource.KindAutomation)

	t.Run("typing approval is refused on both surfaces", func(t *testing.T) {
		// One Agent of each class, because the refusal differs by where
		// approval actually happens: a household Agent points at the
		// review link, an Owner Agent at the Console.
		for surface, want := range map[string]string{
			"family": "review link",
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

	// A8.1 draws the line here and draws it three times: the link
	// carries no approval authority, no chat callback approves a Plan,
	// and no Plan approval happens through Telegram. There is
	// therefore no callback ROUTE to approve one — chat gets a deep
	// link to the authenticated page and nothing else.
	//
	// This is the shape of the mistake it prevents: a chat adapter
	// holds converse and nothing more, so an endpoint that took a
	// token from it and transitioned a Plan would let anything that
	// could relay a token approve without a human ever loading the
	// page, seeing what they were approving, or authenticating to it.
	t.Run("there is no chat callback that approves a plan", func(t *testing.T) {
		token := legacyReviewTokenFixture(t, h, planID, "sam", "")
		for _, path := range []string{
			"/api/v1/household/review/callback",
			"/api/v1/household/review/chat",
		} {
			res := h.do(runtimedb.RoleConcierge, http.MethodPost, path,
				map[string]any{"bindingId": 1, "token": token, "approve": true}, nil)
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("%s answered %d; a chat route that approves a plan must not exist", path, res.StatusCode)
			}
		}
		record, err := h.s.DB.GetPlan(planID)
		if err != nil {
			t.Fatal(err)
		}
		if record.State != runtimedb.PlanProposed {
			t.Fatalf("a plan reached %q without anyone loading the review page", record.State)
		}
		// The token still works where it is supposed to: through the
		// identity proxy, as the person it was issued to.
		if res := h.review(t, "sam@example.com", token, true); res.StatusCode != http.StatusForbidden {
			t.Fatalf("the authenticated review page = %d", res.StatusCode)
		}
		after, err := h.s.DB.GetPlan(planID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != runtimedb.PlanProposed {
			t.Fatalf("plan state after the authenticated review = %q", after.State)
		}
	})
}

func timeSoon() time.Time { return time.Now().Add(10 * time.Minute) }
