package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func isDeployment(raw string) bool {
	var doc struct {
		Deployment json.RawMessage `json:"deployment"`
	}
	return json.Unmarshal([]byte(raw), &doc) == nil && len(doc.Deployment) > 0 && string(doc.Deployment) != "null"
}
func (s *Server) deploymentReader(id principal.Identity) error {
	if id.Role != runtimedb.RoleAdmin {
		return fmt.Errorf("only an active Owner may inspect deployment review")
	}
	member, err := s.DB.GetButlerMember(actingMember(id))
	if err != nil || !member.Active() || member.Role != runtimedb.RoleOwner {
		return fmt.Errorf("only an active Owner may inspect deployment review")
	}
	return nil
}
func (s *Server) deploymentSlot(_ *http.Request, id principal.Identity) (any, int, error) {
	if err := s.deploymentReader(id); err != nil {
		return nil, http.StatusForbidden, err
	}
	if s.Deployments == nil {
		return map[string]any{"configured": false}, http.StatusOK, nil
	}
	slot, err := s.Deployments.Slot()
	if err != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("local deployment inventory unavailable")
	}
	return slot, http.StatusOK, nil
}
func (s *Server) proposeDeployment(r *http.Request, id principal.Identity, req proposalRequest) (any, int, error) {
	if body, code, err := s.freshOwnerReview(r, id); err != nil {
		return body, code, err
	}
	if len(req.Put) > 0 || len(req.Delete) > 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("deployment Plans cannot include other changes")
	}
	if s.Deployments == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("local Helm publication is not configured")
	}
	rec, err := s.Deployments.Propose(actingMember(id), req.Deployment.Instance, req.Deployment.Timezone)
	if err != nil {
		return nil, http.StatusConflict, err
	}
	s.event("plan.proposed", rec.ID, id, "Exact local deployment projection prepared for Owner review")
	return viewPlan(rec), http.StatusCreated, nil
}
func (s *Server) approveDeployment(r *http.Request, id principal.Identity, rec runtimedb.PlanRecord) (any, int, error) {
	if body, code, err := s.freshOwnerReview(r, id); err != nil {
		return body, code, err
	}
	if s.Deployments == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("local Helm publication is not configured")
	}
	if err := s.Deployments.Approve(rec.ID, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrRevisionMoved) {
			_ = s.DB.TransitionPlan(rec.ID, runtimedb.PlanSuperseded, "")
		}
		return nil, http.StatusConflict, err
	}
	s.event("plan.approved", rec.ID, id, "Product input accepted; local promotion queued. Flux and application outcomes are unobserved.")
	latest, err := s.DB.GetPlan(rec.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	view := viewPlan(latest)
	status := s.Deployments.Status(rec.ID)
	view.DeploymentStatus = &status
	return view, http.StatusOK, nil
}
