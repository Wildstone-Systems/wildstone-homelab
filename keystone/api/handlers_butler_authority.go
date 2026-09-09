package api

// The admin surface for tool controls, grants, and capability requests
// (ADR-W030 A7/A8, work package P7).
//
// Everything that grants requires manage; everything that ASKS requires
// only read, because asking is not having. That split is the whole
// permission model of this file, and it is why filing a request and
// deciding one are different endpoints with different verbs.

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (s *Server) listButlerToolControls(r *http.Request, _ principal.Identity) (any, int, error) {
	controls, err := s.DB.ListToolControls(r.URL.Query().Get("profile"))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"controls": controls}, http.StatusOK, nil
}

// setButlerToolControl turns a tool on or off for one profile.
//
// It cannot widen a profile's structural ceiling and does not pretend
// to: enabling a tool the surface does not register stores an intent
// that the resolver will keep refusing. The response says so rather than
// letting an administrator believe they granted something.
func (s *Server) setButlerToolControl(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Profile string `json:"profile"`
		Tool    string `json:"tool"`
		State   string `json:"state"`
		Reason  string `json:"reason"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.State == "" {
		if err := s.DB.ClearToolControl(req.Profile, req.Tool); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return map[string]any{"cleared": true}, http.StatusOK, nil
	}
	if err := s.DB.SetToolControl(req.Profile, req.Tool, req.State, actingMember(id), req.Reason); err != nil {
		return nil, http.StatusBadRequest, err
	}
	out := map[string]any{"profile": req.Profile, "tool": req.Tool, "state": req.State}
	if req.State == runtimedb.ToolEnabled && !s.toolExistsOnProfile(req.Profile, req.Tool) {
		out["note"] = fmt.Sprintf(
			"%s is not a tool the %s Butler has, so this switch does nothing: a control cannot add a capability, only turn an existing one off and on",
			req.Tool, req.Profile)
	}
	return out, http.StatusOK, nil
}

// toolExistsOnProfile asks the gateways themselves rather than a list,
// so the answer cannot drift from what is actually registered.
//
// The gateways are built bare: the question is which tools a profile HAS
// registered, not which are wired on this deployment or permitted right
// now, and a bare gateway answers exactly that.
func (s *Server) toolExistsOnProfile(profile, tool string) bool {
	var manifest butler.CapabilityManifest
	switch profile {
	case runtimedb.ProfileOperator:
		manifest = butler.NewToolGateway(nil, nil).Capabilities(false)
	case runtimedb.ProfileHousehold:
		manifest = butler.NewHouseholdGateway("", butler.HouseholdSeams{}).Capabilities(false)
	default:
		return false
	}
	for _, t := range manifest.Tools {
		if t.Name == tool {
			return true
		}
	}
	return false
}

func (s *Server) listButlerGrants(r *http.Request, _ principal.Identity) (any, int, error) {
	scope := r.URL.Query().Get("scope")
	scopeID := r.URL.Query().Get("scopeId")
	if scope == "" || scopeID == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("scope and scopeId are required")
	}
	grants, err := s.DB.ListToolGrants(scope, scopeID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"grants": grants}, http.StatusOK, nil
}

// grantButlerTool issues a temporary, work-bound grant.
func (s *Server) grantButlerTool(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Scope       string `json:"scope"`
		ScopeID     string `json:"scopeId"`
		Profile     string `json:"profile"`
		Tool        string `json:"tool"`
		Minutes     int    `json:"minutes"`
		Reason      string `json:"reason"`
		CleanupPlan string `json:"cleanupPlan"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Minutes <= 0 {
		req.Minutes = 60
	}
	seq, err := s.DB.GrantTool(runtimedb.ToolGrant{
		Scope: req.Scope, ScopeID: req.ScopeID, Profile: req.Profile, Tool: req.Tool,
		GrantedBy: actingMember(id), Reason: req.Reason, CleanupPlan: req.CleanupPlan,
	}, time.Duration(req.Minutes)*time.Minute)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"seq": seq}, http.StatusOK, nil
}

func (s *Server) revokeButlerGrant(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Seq int64 `json:"seq"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.RevokeToolGrant(req.Seq, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no live grant with that id")
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"revoked": true}, http.StatusOK, nil
}

func (s *Server) listCapabilityRequests(r *http.Request, _ principal.Identity) (any, int, error) {
	requests, err := s.DB.ListCapabilityRequests(r.URL.Query().Get("state"))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"requests": requests}, http.StatusOK, nil
}

// fileCapabilityRequest records an ask. Read authority is enough,
// because a request grants nothing — and requiring manage to ASK would
// mean only the people who can already enable a tool could request one.
func (s *Server) fileCapabilityRequest(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Kind        string `json:"kind"`
		Profile     string `json:"profile"`
		Tool        string `json:"tool"`
		Description string `json:"description"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	seq, err := s.DB.FileCapabilityRequest(runtimedb.CapabilityRequest{
		Kind: req.Kind, RequestedBy: actingMember(id), Profile: req.Profile,
		Tool: req.Tool, Description: req.Description,
	})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"seq": seq, "state": runtimedb.RequestOpen}, http.StatusOK, nil
}

// decideCapabilityRequest records a person's answer. Approving does not
// enable anything: that is setButlerToolControl, deliberately a separate
// call, so approving a request and turning something on cannot become
// one click by accident.
func (s *Server) decideCapabilityRequest(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Seq   int64  `json:"seq"`
		State string `json:"state"`
		Note  string `json:"note"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.DecideCapabilityRequest(req.Seq, req.State, actingMember(id), req.Note); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no request with that id")
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"seq":   req.Seq,
		"state": req.State,
		"note": "approving records a decision; it does not enable the tool — " +
			"use the tool control for that, so the two acts stay visible separately",
	}, http.StatusOK, nil
}
