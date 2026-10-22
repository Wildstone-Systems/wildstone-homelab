package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/automation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/policy"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"strings"
)

func (s *Server) freshOwnerReview(r *http.Request, id principal.Identity) (any, int, error) {
	if id.Role != runtimedb.RoleAdmin || id.AuthTime == nil {
		return nil, http.StatusForbidden, fmt.Errorf("review this Plan in Console with a fresh Owner sign-in")
	}
	member, err := s.DB.GetButlerMember(actingMember(id))
	if err != nil || !member.Active() || member.Role != runtimedb.RoleOwner {
		return nil, http.StatusForbidden, fmt.Errorf("only an active Owner may review Plans")
	}
	return s.stepUpRefusal(id)
}
func (s *Server) submitRoutinePlan(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		OutputAudience string   `json:"outputAudience"`
		SecretIDs      []string `json:"secretIds"`
		MemoryIDs      []int64  `json:"memoryIds"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	routine, err := s.DB.GetHouseholdRoutine(r.PathValue("name"))
	if err != nil {
		return nil, http.StatusNotFound, err
	}
	document, err := automation.ParseDraft(routine.Document)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.checkRoutinePlanPolicy(document, id); err != nil {
		return nil, http.StatusUnprocessableEntity, err
	}
	delivery, err := json.Marshal(document.Automation.Action)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	idPlan, err := s.DB.SubmitRoutinePlan(routine.Name, actingMember(id), "", req.OutputAudience, string(delivery), routine.Document, runtimedb.RoutineResources{SecretIDs: req.SecretIDs, MemoryIDs: req.MemoryIDs})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"planId": idPlan, "state": "awaiting_review", "note": "Waiting for an Owner to review in Console. Nothing has been activated."}, http.StatusOK, nil
}

func (s *Server) ownerReviewQueue(_ *http.Request, id principal.Identity) (any, int, error) {
	if id.Role != runtimedb.RoleAdmin {
		return nil, http.StatusForbidden, fmt.Errorf("Owner review queue requires an Owner")
	}
	rows, err := s.DB.OwnerReviewQueue(actingMember(id))
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	out := make([]planView, 0, len(rows))
	for _, row := range rows {
		if err := s.DB.CheckPlanPrivacy(row.ID, runtimedb.ModelLocal, apiMemoryDestination(id)); err != nil {
			if errors.Is(err, runtimedb.ErrPrivacy) {
				continue
			}
			return nil, http.StatusInternalServerError, err
		}
		out = append(out, viewPlan(row))
	}
	return out, http.StatusOK, nil
}

// RoutineProposalSeam binds Agent identity outside model input. The gateway
// supplies its bound Person; neither an approval nor execution seam is exposed.
func (s *Server) RoutineProposalSeam(agent string) func(context.Context, butler.RoutineProposalCall) (string, error) {
	return func(_ context.Context, c butler.RoutineProposalCall) (string, error) {
		routine, err := s.DB.GetHouseholdRoutine(c.Name)
		if err != nil {
			return "", err
		}
		doc, err := automation.ParseDraft(routine.Document)
		if err != nil {
			return "", err
		}
		if err := s.checkRoutinePlanPolicy(doc, principal.Identity{Name: agent, Role: runtimedb.RoleButler, ActingFor: c.Member}); err != nil {
			return "", err
		}
		delivery, err := json.Marshal(doc.Automation.Action)
		if err != nil {
			return "", err
		}
		return s.DB.SubmitRoutinePlan(c.Name, c.Member, agent, c.OutputAudience, string(delivery), routine.Document)
	}
}

func (s *Server) ownerReviewPreference(r *http.Request, id principal.Identity) (any, int, error) {
	if id.Role != runtimedb.RoleAdmin {
		return nil, http.StatusForbidden, fmt.Errorf("Owner access required")
	}
	member, err := s.DB.GetButlerMember(actingMember(id))
	if err != nil || !member.Active() || member.Role != runtimedb.RoleOwner {
		return nil, http.StatusForbidden, fmt.Errorf("Owner access required")
	}
	if r.Method == http.MethodPut {
		if body, code, err := s.freshOwnerReview(r, id); err != nil {
			return body, code, err
		}
		var pref runtimedb.OwnerReviewPreference
		if code, err := decodeStrict(r, &pref); err != nil {
			return nil, code, err
		}
		if err := s.DB.SetOwnerReviewPreference(actingMember(id), pref); err != nil {
			return nil, http.StatusBadRequest, err
		}
	}
	pref, err := s.DB.GetOwnerReviewPreference()
	return pref, http.StatusOK, err
}
func (s *Server) routineAudiences(_ *http.Request, _ principal.Identity) (any, int, error) {
	all, err := s.DB.ListAudiences()
	out := []runtimedb.Audience{}
	for _, a := range all {
		if a.Kind != "owner" {
			out = append(out, a)
		}
	}
	return out, http.StatusOK, err
}

// Each snapshot is a new execution authorization, even if its document also
// exists in Statefile. Validate it independently so an untouched legacy rule
// cannot inherit an exemption from change detection.
func (s *Server) checkRoutinePlanPolicy(document resource.Resource, id principal.Identity) error {
	violations, exempted := s.Policy.Validate(policy.Change{Proposed: []resource.Resource{document}, Principal: id.Name, Role: string(id.Role)})
	for _, v := range exempted {
		s.event("policy.exempted", "", id, v.String())
	}
	if len(violations) == 0 {
		return nil
	}
	messages := make([]string, 0, len(violations))
	for _, v := range violations {
		messages = append(messages, v.String())
	}
	detail := strings.Join(messages, "; ")
	s.event("policy.rejected", "", id, detail)
	return fmt.Errorf("routine proposal violates standing policy: %s", detail)
}
