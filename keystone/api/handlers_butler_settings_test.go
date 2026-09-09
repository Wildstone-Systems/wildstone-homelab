package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// The settings endpoint hands back A4's defaults until a deployment
// stores its own, and says which it is doing.
func TestButlerSettingsDefaultsAreReportedAsUnstored(t *testing.T) {
	h := newHarness(t)
	var body struct {
		Stored   bool `json:"stored"`
		Settings struct {
			HouseholdRounds int    `json:"householdRounds"`
			OperatorRounds  int    `json:"operatorRounds"`
			Timezone        string `json:"timezone"`
		} `json:"settings"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/settings", nil, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET settings = %d", res.StatusCode)
	}
	if body.Stored {
		t.Fatal("defaults were reported as stored settings")
	}
	if body.Settings.HouseholdRounds != 16 || body.Settings.OperatorRounds != 24 {
		t.Fatalf("defaults = %+v, want A4's 16 and 24", body.Settings)
	}
}

// A partial update changes only what it names, and an out-of-range value
// is refused with the field and its range named.
func TestButlerSettingsValidateAndMerge(t *testing.T) {
	h := newHarness(t)

	var body struct {
		Settings struct {
			HouseholdRounds int    `json:"householdRounds"`
			OperatorRounds  int    `json:"operatorRounds"`
			UpdatedBy       string `json:"updatedBy"`
		} `json:"settings"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butler/settings",
		map[string]any{"operatorRounds": 32}, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT settings = %d", res.StatusCode)
	}
	if body.Settings.OperatorRounds != 32 {
		t.Fatalf("operatorRounds = %d, want 32", body.Settings.OperatorRounds)
	}
	if body.Settings.HouseholdRounds != 16 {
		t.Fatalf("a partial update reset householdRounds to %d", body.Settings.HouseholdRounds)
	}
	if body.Settings.UpdatedBy == "" {
		t.Fatal("the change was recorded with no acting identity")
	}

	var refusal struct {
		Error string `json:"error"`
	}
	bad := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butler/settings",
		map[string]any{"operatorRounds": 500}, &refusal)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("an out-of-range setting = %d, want 400", bad.StatusCode)
	}
	if !strings.Contains(refusal.Error, "operatorRounds") {
		t.Fatalf("the refusal does not name the field: %q", refusal.Error)
	}

	unknown := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butler/settings",
		map[string]any{"apiKey": "secret"}, nil)
	if unknown.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d, want 400", unknown.StatusCode)
	}

	// The refused writes changed nothing.
	body.Settings.OperatorRounds = 0
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/settings", nil, &body)
	if body.Settings.OperatorRounds != 32 {
		t.Fatalf("stored operatorRounds after refused writes = %d", body.Settings.OperatorRounds)
	}
}
