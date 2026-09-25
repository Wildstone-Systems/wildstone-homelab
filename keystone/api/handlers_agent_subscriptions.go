package api

// Owner approval of an agent_subscriptions proposal (ADR-W039 §3.2
// Events & subscriptions, §3.6; Phase 4, #341): the moment — and the
// ONLY moment — a subscription becomes enabled.
//
// A subscription is not desired state (like plan.SettingChange, its
// Plan JSON carries no Put/Delete and does not wait on the Statefile
// head), so nothing exists in agent_subscriptions until this runs: the
// row is created and approved in the same call, never left sitting
// around unapproved while a Plan is merely proposed.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// applySubscriptionPlan approves a subscription-only proposal.
func (s *Server) applySubscriptionPlan(planID string, stored storedProposal, id principal.Identity) (any, int, error) {
	sub := stored.Subscription
	b, err := s.DB.GetButler(sub.Butler)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("subscription names an unknown agent %q: %w", sub.Butler, err)
	}
	var capabilities []string
	if sub.CapabilitySet != "" {
		if err := json.Unmarshal([]byte(sub.CapabilitySet), &capabilities); err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("stored subscription capability set corrupt: %w", err)
		}
	}
	subID, err := s.DB.InsertAgentSubscription(b.Seq, sub.MatchKind, sub.MatchJSON, sub.Audience, capabilities, sub.BudgetJSON)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.DB.ApproveAgentSubscription(subID, actingMember(id)); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := s.DB.TransitionPlan(planID, runtimedb.PlanApproved, id.Name); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("plan.approved", planID, id, stored.Summary)
	rec, err := s.DB.GetPlan(planID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.recordProposalOutcomeDelivery(planID, rec.Principal, "approved", stored.Summary)
	return viewPlan(rec), http.StatusOK, nil
}
