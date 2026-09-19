package api

// The household's timezone as a setting an Agent can propose and an
// Owner approves (ADR-W033 E2).
//
// It used to be only a startup flag defaulting to the host's clock. On a
// VM running UTC that told the household's Agent it was UTC, and "3pm"
// landed five hours early on the calendar. It is ButlerSettings.Timezone
// now — the zone quiet hours already kept — seeded from
// -household-timezone, changed by an Owner in the Console or by
// approving an Agent's proposal, and read live by everything that keeps
// the household's time.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// householdLoc is the household's zone now: the live setting when there
// is one, the startup value otherwise.
func (s *Server) householdLoc() *time.Location {
	return s.HouseholdZone.Or(s.HouseholdLoc)
}

// householdProposer is the principal a household-setting proposal is
// recorded under. The Person who asked is in acting_for; the Agent that
// proposed it is named in the summary.
const householdProposer = "butler"

// ProposeHouseholdTimezone records an Agent's proposal to change the
// household's timezone, for an Owner to approve. It changes nothing.
func (s *Server) ProposeHouseholdTimezone(_ context.Context, zone, reason, proposedBy string) (string, string, error) {
	if _, err := time.LoadLocation(zone); err != nil || zone == "Local" {
		return "", "", fmt.Errorf("%q is not a time zone name", zone)
	}
	current := "UTC"
	if l := s.householdLoc(); l != nil {
		current = l.String()
	}
	if zone == current {
		return "", current, fmt.Errorf("the household already keeps %s", zone)
	}
	summary := fmt.Sprintf("change the household timezone from %s to %s", current, zone)
	if proposedBy != "" {
		summary += fmt.Sprintf(" (asked by %s", proposedBy)
		if reason != "" {
			summary += ": " + reason
		}
		summary += ")"
	}
	stored := storedProposal{
		Summary: summary,
		Setting: &plan.SettingChange{Timezone: zone, Reason: reason},
	}
	if head, err := s.State.Head(); err == nil {
		stored.BasedOnHead = head
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return "", current, err
	}
	id, err := s.DB.InsertPlan(householdProposer, proposedBy, summary, "low", string(raw))
	if err != nil {
		return "", current, err
	}
	s.recordEvent("household.timezone_proposed", id, summary, map[string]string{"zone": zone, "by": proposedBy})
	return id, current, nil
}

// applySettingPlan approves a setting-only proposal: the setting changes
// and everything reading the household's clock follows.
func (s *Server) applySettingPlan(planID string, stored storedProposal, id principal.Identity) (any, int, error) {
	settings, _, err := s.DB.GetButlerSettings()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if tz := stored.Setting.Timezone; tz != "" {
		settings.Timezone = tz
	}
	settings.UpdatedBy = actingMember(id)
	if err := s.DB.SetButlerSettings(settings); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if s.HouseholdZone != nil {
		s.HouseholdZone.Set(settings.Location())
	}
	if err := s.DB.TransitionPlan(planID, runtimedb.PlanApproved, id.Name); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("plan.approved", planID, id, stored.Summary)
	rec, err := s.DB.GetPlan(planID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return viewPlan(rec), http.StatusOK, nil
}

// recordEvent appends one event to the stream, saying what happened in
// its own words (appendEvent is the secrets code's, and names a secret).
func (s *Server) recordEvent(typ, name, message string, attrs map[string]string) {
	if _, err := s.DB.AppendEvent(runtimedb.Event{Type: typ, Name: name, Message: message, Attrs: attrs}); err != nil {
		s.Log.Error("event append failed", "type", typ, "err", err)
	}
}
