package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Offboarding through the API denies access, settles the work, and
// reports what it did per store.
func TestOffboardingReportsPerStore(t *testing.T) {
	h := freshnessHarness(t)
	if _, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "sandy", Owner: "sandy",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember,
		Audience: runtimedb.ButlerAudienceShared, Objective: "book it", CompletionCriteria: "booked",
	}); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Result struct {
			Stores        map[string]int `json:"stores"`
			TransferredTo string         `json:"transferredTo"`
		} `json:"result"`
		Records   []runtimedb.OffboardingRecord              `json:"records"`
		Resources []runtimedb.OffboardingResourceDisposition `json:"resources"`
		Note      string                                     `json:"note"`
	}
	session := h.s.StartConsoleSession("mika", time.Now())
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/sandy/offboarding", "mika", sessionCookie(session))
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("offboarding = %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Result.Stores["shared-tasks-transferred"] != 1 {
		t.Fatalf("shared work was not transferred: %+v", out.Result.Stores)
	}
	if len(out.Records) == 0 {
		t.Fatal("the offboarding left no per-store record")
	}
	if len(out.Resources) == 0 || out.Resources[0].Kind != "task" || out.Resources[0].Disposition != "transferred" {
		t.Fatalf("shared task has no recorded transfer decision: %+v", out.Resources)
	}
	if !strings.Contains(out.Note, "never restores access") {
		t.Fatalf("the note does not state the cleanup rule: %q", out.Note)
	}
	// An operator cannot do it.
	if res := h.do(runtimedb.RoleOperator, http.MethodPost,
		"/api/v1/butler/members/sam/offboarding", map[string]any{}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("an operator offboarded a member: %d", res.StatusCode)
	}
}

func TestOffboardingRequiresFreshOwnerAndKeepsLastOwner(t *testing.T) {
	h := freshnessHarness(t)
	now := time.Now()
	h.s.Now = func() time.Time { return now }
	stale := h.s.StartConsoleSession("mika", now.Add(-30*time.Minute))
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/sandy/offboarding", "mika", sessionCookie(stale))
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("stale Owner offboarded a Member: %d", res.StatusCode)
	}
	res = h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/members/sandy/offboard", map[string]any{}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("bearer admin offboarded a Member: %d", res.StatusCode)
	}
	if member, _ := h.s.DB.GetButlerMember("sandy"); !member.Active() {
		t.Fatal("unauthorized offboarding changed Member state")
	}
	fresh := h.s.StartConsoleSession("mika", now)
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/butler/members/sandy/offboarding", `{"administrator":"sandy"}`, "mika", sessionCookie(fresh)); code != http.StatusBadRequest {
		t.Fatalf("departing Person accepted as transfer destination: %d", code)
	}
	if member, _ := h.s.DB.GetButlerMember("sandy"); !member.Active() {
		t.Fatal("invalid transfer destination partially offboarded Member")
	}
	res = h.consoleRequest(http.MethodPost, "/api/v1/butler/members/mika/offboarding", "mika", sessionCookie(fresh))
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("last Owner offboarding = %d, want 400", res.StatusCode)
	}
	if member, _ := h.s.DB.GetButlerMember("mika"); !member.Active() {
		t.Fatal("last Owner was offboarded")
	}
}

func TestPurgeRequiresAFreshOwnerSession(t *testing.T) {
	h := freshnessHarness(t)
	const name = "purge-check"
	if _, err := h.s.DB.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{
		Name: name, Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: "private routine document",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.ChangeReviewedRoutine(name, "mika", "delete"); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/household/routines/" + name + "/purge"
	now := time.Now()
	h.s.Now = func() time.Time { return now }
	stale := sessionCookie(h.s.StartConsoleSession("mika", now.Add(-30*time.Minute)))
	if code, _ := h.sessionRequest(http.MethodPost, path, `{}`, "mika", stale); code != http.StatusForbidden {
		t.Fatalf("stale Owner purged routine: %d", code)
	}
	member := sessionCookie(h.s.StartConsoleSession("sandy", now))
	if code, _ := h.sessionRequest(http.MethodPost, path, `{}`, "sandy", member); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("Member purged routine: %d", code)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, path, map[string]any{}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("bearer admin purged routine: %d", res.StatusCode)
	}
	r, err := h.s.DB.GetHouseholdRoutine(name)
	if err != nil || r.Document != "private routine document" {
		t.Fatalf("refused purge changed payload: %+v %v", r, err)
	}
	fresh := sessionCookie(h.s.StartConsoleSession("mika", now))
	if code, out := h.sessionRequest(http.MethodPost, path, `{}`, "mika", fresh); code != http.StatusOK {
		t.Fatalf("fresh Owner purge = %d: %v", code, out)
	}
	r, err = h.s.DB.GetHouseholdRoutine(name)
	if err != nil || r.Document != "[purged]" {
		t.Fatalf("Owner purge left payload: %+v %v", r, err)
	}
}

// Quarantine cannot be released on an assertion alone, and the refusal
// explains why.
func TestRestoreReconciliationNeedsAReplay(t *testing.T) {
	h := newHarness(t)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/restore/quarantine",
		map[string]any{"reason": "restored from the nightly backup"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("entering quarantine = %d", res.StatusCode)
	}
	var state struct {
		State runtimedb.RestoreState `json:"state"`
		Note  string                 `json:"note"`
	}
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/restore", nil, &state)
	if !state.State.Quarantined {
		t.Fatal("a restored database is not quarantined")
	}
	if !strings.Contains(state.Note, "acknowledgement alone") {
		t.Fatalf("the state does not explain the release rule: %q", state.Note)
	}

	var refusal struct {
		Error string `json:"error"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/restore/reconcile",
		map[string]any{"journalCurrent": false, "release": true}, &refusal)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("reconciling without asserting the journal = %d", res.StatusCode)
	}
	if !strings.Contains(refusal.Error, "proves nothing") {
		t.Fatalf("the refusal does not say why: %q", refusal.Error)
	}

	var done struct {
		Replayed int  `json:"replayed"`
		Released bool `json:"released"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/restore/reconcile",
		map[string]any{"journalCurrent": true, "release": true}, &done); res.StatusCode != http.StatusOK {
		t.Fatalf("reconciling = %d", res.StatusCode)
	}
	if !done.Released {
		t.Fatal("a full reconciliation did not release quarantine")
	}
}

// F-155-07: offboarding a Person revokes the bearer tokens of the
// principals linked to them, so a token minted before offboarding stops
// authenticating.
func TestOffboardingRevokesLinkedPrincipalToken(t *testing.T) {
	h := freshnessHarness(t)
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/principals",
		map[string]string{"name": "sandy-cli", "role": "operator"}, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create principal = %d", resp.StatusCode)
	}
	var mint mintResponse
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/principals/sandy-cli/tokens",
		map[string]int{"ttlSeconds": 3600}, &mint); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint = %d", resp.StatusCode)
	}
	if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkAPIPrincipal, "sandy-cli", "sandy", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if resp := h.doToken(mint.Token, http.MethodGet, "/api/v1/butlers", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("precondition: token before offboarding = %d", resp.StatusCode)
	}
	session := h.s.StartConsoleSession("mika", time.Now())
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/sandy/offboarding", "mika", sessionCookie(session))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("offboarding = %d", res.StatusCode)
	}
	if resp := h.doToken(mint.Token, http.MethodGet, "/api/v1/butlers", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("offboarded Person's token = %d, want 401", resp.StatusCode)
	}
}
