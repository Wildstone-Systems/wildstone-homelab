package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
)

func consoleHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := homeHarness(t)
	h.s.Upkeep = &upkeep.Engine{DB: h.s.DB, State: h.s.State, Loc: time.UTC}
	return h, h.console(runtimedb.RoleAdmin, "mika")
}

// The Console could read commitments and change nothing, so an Owner
// looking at a duplicate had no way to remove it without signing in to
// the family dashboard.
func TestTheConsoleCanAddEditAndRemoveACommitment(t *testing.T) {
	h, owner := consoleHarness(t)
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	body := `{"description":"Take out the trash","every":"1w","anchor":"` + yesterday + `",` +
		`"rollover":"fromSchedule"}`
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/commitments",
		json.RawMessage(body), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding from the Console: %d", res.StatusCode)
	}

	named := func() []string {
		// h.do closes the body and decodes into out; reading the
		// response afterwards is reading a closed body.
		var out []upkeep.Standing
		h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/commitments", nil, &out)
		var names []string
		for _, st := range out {
			names = append(names, st.Name)
		}
		return names
	}
	if !contains(named(), "take-out-the-trash") {
		t.Fatalf("not listed after adding: %v", named())
	}

	edit := `{"description":"Bins out","every":"1w","rollover":"fromSchedule"}`
	res = h.do(runtimedb.RoleAdmin, http.MethodPut,
		"/api/v1/household/commitments/take-out-the-trash", json.RawMessage(edit), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("editing from the Console: %d", res.StatusCode)
	}

	res = h.do(runtimedb.RoleAdmin, http.MethodDelete,
		"/api/v1/household/commitments/take-out-the-trash", nil, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		t.Fatalf("removing from the Console: %d", res.StatusCode)
	}
	if contains(named(), "take-out-the-trash") {
		t.Error("still listed after being removed from the Console")
	}
	_ = owner
}

// An Owner can act on somebody else's personal commitment, because the
// Console already lists it. A surface that shows a record and refuses
// to let the administrator touch it is the inconsistency.
func TestTheConsoleReachesAPersonalCommitmentItAlreadyShows(t *testing.T) {
	h, _ := consoleHarness(t)
	h.person("sandy")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	// Sandy's own, from the dashboard.
	body := `{"description":"Take the tablets","about":"sandy","every":"1d","anchor":"` + yesterday + `",` +
		`"rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		strings.NewReader(body), "application/json")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("sandy adding: %d", res.StatusCode)
	}

	res = h.do(runtimedb.RoleAdmin, http.MethodDelete,
		"/api/v1/household/commitments/take-the-tablets", nil, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		t.Errorf("the Console cannot remove a personal commitment it lists: %d", res.StatusCode)
	}
}

// And the operator standing cannot be reached from the household
// surface. It is a context value set by one function behind
// VerbManage, so no request a member can shape turns it on.
func TestAMemberCannotBecomeTheOperatorSurface(t *testing.T) {
	h, _ := consoleHarness(t)
	h.person("sandy", "bob")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	body := `{"description":"Take the tablets","about":"sandy","every":"1d","anchor":"` + yesterday + `",` +
		`"rollover":"fromSchedule","scope":"personal"}`
	res := h.member(http.MethodPost, "/api/v1/home/commitments", "sandy", "homepage_users",
		strings.NewReader(body), "application/json")
	res.Body.Close()

	// Bob, on the dashboard, with every header he can set.
	req, err := http.NewRequest(http.MethodDelete,
		h.srv.URL+"/api/v1/home/commitments/take-the-tablets", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TrustedIngressHeader, "bob")
	req.Header.Set(ConsoleGroupsHeader, "homepage_users,admins,root")
	req.Header.Set("X-Admin", "true")
	req.Header.Set("X-Wildstone-Admin-Actor", "true")
	out, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if out.StatusCode == http.StatusOK || out.StatusCode == http.StatusNoContent {
		t.Error("a household member removed somebody else's personal commitment")
	}
}

// An edit preserves every field it does not mention.
//
// The handler used to rebuild the spec from a flat struct, so a form
// that knows about four fields cleared the rest: correcting a
// description disabled appointment handling, emptied the mayComplete
// and notify lists, and turned a meter rule into a clock rule. Every
// surface then had to send every field — a rule each new form had to
// rediscover, and the Console's did not.
func TestEditingACommitmentPreservesWhatItDoesNotSend(t *testing.T) {
	h, _ := consoleHarness(t)
	h.person("sandy")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	// A commitment with everything the small forms know nothing about.
	full := `{"description":"Dentist","every":"6mo","anchor":"` + yesterday + `",` +
		`"rollover":"fromCompletion","requiresBooking":true,"lead":"14d",` +
		`"mayComplete":["household/sandy"],"notify":["household/sandy"]}`
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/commitments",
		json.RawMessage(full), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("adding: %d", res.StatusCode)
	}

	// The Console's form sends four fields, as it does.
	edit := `{"description":"Dentist checkup","every":"6mo","partOfDay":""}`
	res = h.do(runtimedb.RoleAdmin, http.MethodPut,
		"/api/v1/household/commitments/dentist", json.RawMessage(edit), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("editing: %d", res.StatusCode)
	}

	var after []upkeep.Standing
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/commitments", nil, &after)
	var got *upkeep.Standing
	for i := range after {
		if after[i].Name == "dentist" {
			got = &after[i]
		}
	}
	if got == nil {
		t.Fatal("gone after an edit")
	}
	if got.Description != "Dentist checkup" {
		t.Errorf("description = %q, the edit did not apply", got.Description)
	}
	if !got.RequiresBooking {
		t.Error("requiresBooking was cleared by an edit that never mentioned it")
	}
	if len(got.MayComplete) == 0 {
		t.Error("mayComplete was emptied by an edit that never mentioned it")
	}
	if len(got.Notify) == 0 {
		t.Error("notify was emptied by an edit that never mentioned it")
	}
	if got.Rollover != "fromCompletion" {
		t.Errorf("rollover = %q, want the fromCompletion it was created with", got.Rollover)
	}
}

// A shared commitment stays shared through an edit. Its owner is the
// bare word "household", which a form that stripped only the
// "household/" prefix read back as a person named household — so
// saving stored household/household and the commitment vanished from
// the household's view.
func TestEditingASharedCommitmentKeepsItShared(t *testing.T) {
	h, _ := consoleHarness(t)
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	body := `{"description":"Bins out","every":"1w","anchor":"` + yesterday + `",` +
		`"rollover":"fromSchedule"}`
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/commitments",
		json.RawMessage(body), nil)
	res.Body.Close()

	var before []upkeep.Standing
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/commitments", nil, &before)
	var wasScope, wasOwner string
	for _, st := range before {
		if st.Name == "bins-out" {
			wasScope, wasOwner = st.Scope, st.Owner
		}
	}
	if wasScope == "personal" {
		t.Fatalf("created personal, not shared: owner=%q", wasOwner)
	}

	// An edit that says nothing about who it is for.
	edit := `{"description":"Bins out on Thursday","every":"1w"}`
	res = h.do(runtimedb.RoleAdmin, http.MethodPut,
		"/api/v1/household/commitments/bins-out", json.RawMessage(edit), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("editing: %d", res.StatusCode)
	}

	var after []upkeep.Standing
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/commitments", nil, &after)
	for _, st := range after {
		if st.Name != "bins-out" {
			continue
		}
		if st.Scope == "personal" {
			t.Errorf("a shared commitment became personal on edit: owner=%q", st.Owner)
		}
		if strings.Contains(st.Owner, "household/household") {
			t.Errorf("owner = %q", st.Owner)
		}
	}
}
