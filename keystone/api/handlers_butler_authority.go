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
	controls, err := s.DB.ListToolControls(r.URL.Query().Get("agentClass"))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"controls": controls}, http.StatusOK, nil
}

// setButlerToolControl turns a tool on or off for one agent class.
//
// It cannot widen a class's structural ceiling and does not pretend to:
// enabling a tool the surface does not register stores an intent that
// the resolver will keep refusing. The response says so rather than
// letting an administrator believe they granted something.
func (s *Server) setButlerToolControl(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		AgentClass string `json:"agentClass"`
		Tool       string `json:"tool"`
		State      string `json:"state"`
		Reason     string `json:"reason"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.State == "" {
		if err := s.DB.ClearToolControl(req.AgentClass, req.Tool); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return map[string]any{"cleared": true}, http.StatusOK, nil
	}
	if err := s.DB.SetToolControl(req.AgentClass, req.Tool, req.State, actingMember(id), req.Reason); err != nil {
		return nil, http.StatusBadRequest, err
	}
	out := map[string]any{"agentClass": req.AgentClass, "tool": req.Tool, "state": req.State}
	if req.State == runtimedb.ToolEnabled && !s.toolExistsOnClass(req.AgentClass, req.Tool) {
		out["note"] = fmt.Sprintf(
			"%s is not a tool a %s agent has, so this switch does nothing: a control cannot add a capability, only turn an existing one off and on",
			req.Tool, req.AgentClass)
	}
	return out, http.StatusOK, nil
}

// toolExistsOnClass asks the gateways themselves rather than a list, so
// the answer cannot drift from what is actually registered.
//
// The gateways are built bare: the question is which tools a class HAS
// registered, not which are wired on this deployment or permitted right
// now, and a bare gateway answers exactly that.
//
// This is where "a Member Agent cannot be configured with an Owner tool"
// (design §18.3) is answered: the member gateway does not register an
// owner tool, so the question comes back false and the control is stored
// as the inert intent it is. The other direction holds too: an Owner
// Agent's set is both gateways' (ADR-W033 B3), so a Member tool exists on
// it. The refusal is structural — the tool is
// absent, not switched off — which is why no list here has to be kept in
// step with anything.
func (s *Server) toolExistsOnClass(class, tool string) bool {
	var manifest butler.CapabilityManifest
	switch class {
	case runtimedb.AgentClassOwner:
		// Owner and Member tools both: "Member tools may be granted to
		// either Agent class" (ADR-W033 B3).
		manifest = butler.NewOwnerToolset(butler.NewToolGateway(nil, nil).ForAdministration(),
			butler.NewHouseholdGateway("", butler.HouseholdSeams{Memory: butler.InventoryMemory()}).ForAdministration()).Capabilities(false)
	case runtimedb.AgentClassMember:
		manifest = butler.NewHouseholdGateway("", butler.HouseholdSeams{Memory: butler.InventoryMemory()}).
			ForAdministration().Capabilities(false)
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
		AgentClass  string `json:"agentClass"`
		Butler      string `json:"butler"`
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
	// A grant names the Agent it belongs to, by name at the edge and by
	// row id underneath. Naming it is what makes a later downgrade able
	// to strip THIS Agent's grants without touching another's (§18.3).
	// Omitting it is still allowed, and still means the class-level
	// shape — a deployment with no Butlers has no Agent to name.
	grant := runtimedb.ToolGrant{
		Scope: req.Scope, ScopeID: req.ScopeID, AgentClass: req.AgentClass, Tool: req.Tool,
		GrantedBy: actingMember(id), Reason: req.Reason, CleanupPlan: req.CleanupPlan,
	}
	if req.Butler != "" {
		b, err := s.DB.GetButler(req.Butler)
		if err != nil {
			return nil, http.StatusNotFound, fmt.Errorf("no butler called %q", req.Butler)
		}
		grant.Butler = b.Seq
		// The class follows the Agent rather than the request. A caller
		// who names both could otherwise issue an owner grant against a
		// Member Agent, and the row would belong to nobody: invisible to
		// that Agent's resolution and untouched by its downgrade.
		grant.AgentClass = b.AgentClass
	}
	seq, err := s.DB.GrantTool(grant, time.Duration(req.Minutes)*time.Minute)
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
		AgentClass  string `json:"agentClass"`
		Tool        string `json:"tool"`
		Description string `json:"description"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	seq, err := s.DB.FileCapabilityRequest(runtimedb.CapabilityRequest{
		Kind: req.Kind, RequestedBy: actingMember(id), AgentClass: req.AgentClass,
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
