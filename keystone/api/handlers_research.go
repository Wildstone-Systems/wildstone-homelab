package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/research"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (s *Server) researchOwner(id principal.Identity) error {
	if id.Role != runtimedb.RoleAdmin {
		return errors.New("Owner access required")
	}
	member, err := s.DB.GetButlerMember(actingMember(id))
	if err != nil || !member.Active() || member.Role != runtimedb.RoleOwner {
		return errors.New("active Owner access required")
	}
	return nil
}
func (s *Server) freshResearchOwner(r *http.Request, id principal.Identity) (any, int, error) {
	if err := sameSiteWrite(r); err != nil {
		return nil, http.StatusForbidden, errors.New("research configuration requires a same-origin request")
	}
	return s.freshOwnerReview(r, id)
}
func (s *Server) getResearchSelection(r *http.Request, id principal.Identity) (any, int, error) {
	if err := s.researchOwner(id); err != nil {
		return nil, http.StatusForbidden, err
	}
	b, err := s.DB.GetButler(r.PathValue("agent"))
	if err != nil {
		return researchError(err)
	}
	selection, err := s.DB.ResearchSelection(b.Seq)
	if err != nil {
		return researchError(err)
	}
	return selection, http.StatusOK, nil
}
func researchError(err error) (any, int, error) {
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, errors.New("research resource not found")
	}
	if errors.Is(err, research.ErrConflict) {
		return nil, http.StatusConflict, err
	}
	return nil, http.StatusInternalServerError, errors.New("research registry unavailable")
}
func (s *Server) listResearchProviders(_ *http.Request, id principal.Identity) (any, int, error) {
	if err := s.researchOwner(id); err != nil {
		return nil, http.StatusForbidden, err
	}
	providers, err := s.DB.SearchProviders()
	if err != nil {
		return researchError(err)
	}
	selection, err := s.DB.ResearchSelection(0)
	if err != nil {
		return researchError(err)
	}
	return map[string]any{"providers": providers, "defaultSelection": selection}, http.StatusOK, nil
}
func (s *Server) getResearchProvider(r *http.Request, id principal.Identity) (any, int, error) {
	if err := s.researchOwner(id); err != nil {
		return nil, http.StatusForbidden, err
	}
	p, err := s.DB.SearchProvider(r.PathValue("provider"))
	if err != nil {
		return researchError(err)
	}
	return p, http.StatusOK, nil
}

// PATCH accepts a complete replacement config so an omitted security setting
// cannot accidentally survive a change. Resource/service provisioning remains
// in S20's desired-state Plan; this Plan changes registry metadata only.
func (s *Server) proposeResearchProvider(r *http.Request, id principal.Identity) (any, int, error) {
	if body, code, err := s.freshResearchOwner(r, id); err != nil {
		return body, code, err
	}
	var req struct {
		Config   json.RawMessage `json:"config"`
		Expected string          `json:"expectedConfigRevision"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, errors.New("invalid research configuration request")
	}
	c := research.Change{ProviderID: r.PathValue("provider"), ExpectedRevision: req.Expected}
	if r.Method == http.MethodPost {
		c.Operation = "create"
		c.ProviderID = research.NewID()
		if req.Expected != "" {
			return nil, http.StatusBadRequest, errors.New("create cannot specify a prior revision")
		}
	} else {
		old, err := s.DB.SearchProvider(c.ProviderID)
		if err != nil {
			return researchError(err)
		}
		if req.Expected == "" || old.ConfigRevision != req.Expected {
			return researchError(research.ErrConflict)
		}
		c.Operation = "update"
		if r.Method == http.MethodDelete {
			c.Operation = "delete"
		}
	}
	if c.Operation == "delete" {
		if len(req.Config) != 0 {
			return nil, http.StatusBadRequest, errors.New("delete cannot include configuration")
		}
	} else {
		if len(req.Config) == 0 {
			return nil, http.StatusBadRequest, errors.New("configuration required")
		}
		normalized, err := research.DecodeConfig(req.Config)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		c.Config = &normalized
	}
	stored := storedProposal{Summary: "Review search provider " + c.Operation, Research: &c}
	raw, err := json.Marshal(stored)
	if err != nil {
		return researchError(err)
	}
	planID, err := s.DB.InsertPlan(id.Name, actingMember(id), stored.Summary, "medium", string(raw))
	if err != nil {
		return researchError(err)
	}
	return map[string]any{"planId": planID, "providerId": c.ProviderID, "state": "awaiting_review"}, http.StatusAccepted, nil
}
func (s *Server) testResearchProvider(r *http.Request, id principal.Identity) (any, int, error) {
	s.researchServiceMu.RLock()
	defer s.researchServiceMu.RUnlock()
	if body, code, err := s.freshResearchOwner(r, id); err != nil {
		return body, code, err
	}
	var req struct {
		Expected string `json:"expectedConfigRevision"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, errors.New("invalid provider test request")
	}
	p, err := s.DB.SearchProvider(r.PathValue("provider"))
	if err != nil {
		return researchError(err)
	}
	if req.Expected == "" || req.Expected != p.ConfigRevision {
		return researchError(research.ErrConflict)
	}
	if _, err = p.Config.Normalize(); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if p.EndpointRef == "" {
		return nil, http.StatusConflict, errors.New("provider_unavailable: endpoint reference not configured")
	}
	// Invalidate before testing: a failed/cancelled probe cannot leave an old
	// success usable. A later edit or test invalidates this probe's receipt.
	probe, err := s.DB.BeginSearchProviderTest(p.ID, p.ConfigRevision)
	if err != nil {
		return researchError(err)
	}
	ready := false
	if s.ResearchTest != nil {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(p.Limits.RequestTimeoutSeconds)*time.Second)
		err = s.ResearchTest(ctx, p)
		ready = err == nil && ctx.Err() == nil
		cancel()
	}
	if err = s.DB.RecordSearchProviderTest(p.ID, p.ConfigRevision, probe, ready); err != nil {
		return researchError(err)
	}
	p, err = s.DB.SearchProvider(p.ID)
	if err != nil {
		return researchError(err)
	}
	// Never return adapter errors: these can include URLs, queries or secrets.
	return p, http.StatusOK, nil
}
func (s *Server) selectResearchProvider(r *http.Request, id principal.Identity) (any, int, error) {
	if body, code, err := s.freshResearchOwner(r, id); err != nil {
		return body, code, err
	}
	var req struct {
		ProviderID json.RawMessage `json:"providerId"`
		Expected   string          `json:"expectedConfigRevision"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, errors.New("invalid selection request")
	}
	var provider *string
	if len(req.ProviderID) == 0 || json.Unmarshal(req.ProviderID, &provider) != nil {
		return nil, http.StatusBadRequest, errors.New("providerId must be an ID or null")
	}
	var agent int64
	if name := r.PathValue("agent"); name != "" {
		b, err := s.DB.GetButler(name)
		if err != nil {
			return researchError(err)
		}
		agent = b.Seq
	}
	if err := s.DB.SetResearchSelection(agent, provider, req.Expected); err != nil {
		return researchError(err)
	}
	selection, err := s.DB.ResearchSelection(agent)
	if err != nil {
		return researchError(err)
	}
	return selection, http.StatusOK, nil
}
