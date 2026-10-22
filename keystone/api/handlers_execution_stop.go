package api

import (
	"fmt"
	"net/http"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
)

func (s *Server) getHouseholdExecutionStop(_ *http.Request, _ principal.Identity) (any, int, error) {
	state, err := s.DB.GetHouseholdExecutionStop()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return state, http.StatusOK, nil
}

func (s *Server) changeHouseholdExecutionStop(r *http.Request, id principal.Identity) (any, int, error) {
	if body, code, err := s.freshOwnerReview(r, id); err != nil {
		return body, code, err
	}
	var req struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var err error
	switch req.Action {
	case "stop":
		err = s.DB.StopHouseholdExecution(actingMember(id), req.Reason)
	case "resume":
		err = s.DB.ResumeHouseholdExecution(actingMember(id))
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("action must be stop or resume")
	}
	if err != nil {
		return nil, http.StatusConflict, err
	}
	return s.getHouseholdExecutionStop(r, id)
}

func (s *Server) controlReviewedRoutine(r *http.Request, id principal.Identity) (any, int, error) {
	if body, code, err := s.freshOwnerReview(r, id); err != nil {
		return body, code, err
	}
	var req struct {
		Action string `json:"action"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.ChangeReviewedRoutine(r.PathValue("name"), actingMember(id), req.Action); err != nil {
		return nil, http.StatusConflict, err
	}
	routine, err := s.DB.GetHouseholdRoutine(r.PathValue("name"))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return routine, http.StatusOK, nil
}

func (s *Server) purgeDeletedRoutine(r *http.Request, id principal.Identity) (any, int, error) {
	actor, err := s.activeOwnerConsoleActor(id)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	name := r.PathValue("name")
	if err := s.DB.PurgeDeletedRoutine(name, actor); err != nil {
		return nil, http.StatusConflict, err
	}
	return map[string]any{"purged": true, "name": name,
		"note": "Routine content erased; audit tombstone and effect receipts retained."}, http.StatusOK, nil
}

func (s *Server) reviewConversationWork(r *http.Request, id principal.Identity) (any, int, error) {
	if body, code, err := s.freshOwnerReview(r, id); err != nil {
		return body, code, err
	}
	var req struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.ReviewConversationWork(r.PathValue("id"), actingMember(id), req.Action, req.Note); err != nil {
		return nil, http.StatusConflict, err
	}
	work, err := s.DB.ConversationWorkForTask(r.PathValue("id"))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return work, http.StatusOK, nil
}
