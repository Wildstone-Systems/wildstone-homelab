package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Joan hears the household's clock is wrong and proposes the right zone.
// Nothing changes until an Owner approves; then the setting and every
// live reader follow at once (ADR-W033 E2).
func TestAProposedTimezoneChangesNothingUntilApproved(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata")
	}
	h := newHarness(t)
	h.s.HouseholdZone = householdtz.New(time.UTC)

	planID, from, err := h.s.ProposeHouseholdTimezone(t.Context(), "America/Chicago", "we live in Chicago", "mika")
	if err != nil || from != "UTC" {
		t.Fatalf("propose = %q %q %v", planID, from, err)
	}
	if h.s.householdLoc() != time.UTC {
		t.Fatal("proposing changed the household's clock")
	}
	rec, err := h.s.DB.GetPlan(planID)
	if err != nil || rec.State != runtimedb.PlanProposed || !strings.Contains(rec.Summary, "America/Chicago") {
		t.Fatalf("plan = %+v %v", rec, err)
	}

	var out map[string]any
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+planID+"/approve", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d %v", resp.StatusCode, out)
	}
	if h.s.householdLoc().String() != chicago.String() {
		t.Errorf("after approval the household keeps %v", h.s.householdLoc())
	}
	settings, _, _ := h.s.DB.GetButlerSettings()
	if settings.Timezone != "America/Chicago" {
		t.Errorf("stored timezone = %q", settings.Timezone)
	}
	// Proposing where the household already is says so.
	if _, _, err := h.s.ProposeHouseholdTimezone(t.Context(), "America/Chicago", "", "mika"); err == nil {
		t.Error("proposing the current zone was accepted")
	}
}

// An Owner changing it directly in the Console moves the live clock too.
func TestSavingTheSettingsMovesTheHouseholdClock(t *testing.T) {
	if _, err := time.LoadLocation("Europe/London"); err != nil {
		t.Skip("no tzdata")
	}
	h := newHarness(t)
	h.s.HouseholdZone = householdtz.New(time.UTC)
	settings := runtimedb.DefaultButlerSettings()
	settings.Timezone = "Europe/London"
	var out map[string]any
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butler/settings", settings, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("save = %d %v", resp.StatusCode, out)
	}
	if h.s.householdLoc().String() != "Europe/London" {
		t.Errorf("after saving, the household keeps %v", h.s.householdLoc())
	}
}
