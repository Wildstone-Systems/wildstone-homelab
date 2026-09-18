package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Offboarding through the API denies access, settles the work, and
// reports what it did per store.
func TestOffboardingReportsPerStore(t *testing.T) {
	h := newHarness(t)
	for _, m := range []struct{ name, role string }{
		{"root-admin", runtimedb.RoleOwner}, {"sam", runtimedb.RoleMember},
	} {
		if err := h.s.DB.RecordButlerMember(m.name, m.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(m.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: runtimedb.ButlerSurfaceFamily, Profile: runtimedb.ProfileHousehold,
		Audience: runtimedb.ButlerAudienceShared, Objective: "book it", CompletionCriteria: "booked",
	}); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Result struct {
			Stores        map[string]int `json:"stores"`
			TransferredTo string         `json:"transferredTo"`
		} `json:"result"`
		Records []runtimedb.OffboardingRecord `json:"records"`
		Note    string                        `json:"note"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/members/sam/offboarding",
		map[string]any{}, &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("offboarding = %d", res.StatusCode)
	}
	if out.Result.Stores["shared-tasks-transferred"] != 1 {
		t.Fatalf("shared work was not transferred: %+v", out.Result.Stores)
	}
	if len(out.Records) == 0 {
		t.Fatal("the offboarding left no per-store record")
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
