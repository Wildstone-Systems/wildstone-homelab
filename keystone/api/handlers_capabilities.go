package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// packageView is what the Console browses. Params travel with their
// required/default, because that is the install form (ADR-W021 A5).
type packageView struct {
	Name       string                `json:"name"`
	Version    string                `json:"version"`
	Capability string                `json:"capability"`
	Profile    string                `json:"profile,omitempty"`
	Params     []pkgspec.ParamDecl   `json:"params,omitempty"`
	Provides   []pkgspec.Provide     `json:"provides,omitempty"`
	Requires   []pkgspec.Requirement `json:"requires,omitempty"`
	Enabled    bool                  `json:"enabled"`
}

// listPackages answers what this control plane can install.
func (s *Server) listPackages(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Catalog == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no catalog is configured (keystone -catalog-dir)")
	}
	enabled, err := s.enabledCapabilities()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := []packageView{}
	for _, p := range s.Catalog.List() {
		out = append(out, packageView{
			Name: p.Metadata.Name, Version: p.Metadata.Version,
			Capability: p.Metadata.Capability, Profile: p.Metadata.Profile,
			Params: p.Params, Provides: p.Provides, Requires: p.Requires,
			Enabled: enabled[p.Metadata.Capability],
		})
	}
	return out, http.StatusOK, nil
}

// enabledCapabilities reads which capabilities the deployment has, from
// the owners already recorded on desired resources.
//
// This is what makes `requires` resolve against the deployment instead of
// against directories someone passed on a command line (ADR-W021 A4,
// closing F40). The control plane always knew this; nothing had ever
// asked it.
func (s *Server) enabledCapabilities() (map[string]bool, error) {
	current, err := s.State.Load()
	if err != nil {
		return nil, fmt.Errorf("loading statefile: %w", err)
	}
	enabled := map[string]bool{}
	for _, r := range current {
		if name, ok := strings.CutPrefix(r.Metadata.Owner, "capability/"); ok && name != "" {
			enabled[name] = true
		}
	}
	return enabled, nil
}

type enableRequest struct {
	Package string            `json:"package"`
	Version string            `json:"version,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	DryRun  bool              `json:"dryRun,omitempty"`
}

// enableCapability expands a catalog package server-side and proposes the
// result as an ordinary plan (ADR-W021 A1).
//
// Expansion used to live in the CLI, which meant Keystone never saw a
// package: the Console had nothing to call, the stored plan recorded
// fourteen documents rather than what was installed, and `requires`
// resolved against whatever -catalog directories were passed. All three
// were the same defect wearing different clothes.
//
// It funnels into submitProposalOpts deliberately. That is the ONE
// proposal pipeline (ADR-W017 A3), so a capability install is held to
// exactly the validation, standing policy and audit a hand-written
// proposal is — the Console gaining a button must not gain an exemption.
func (s *Server) enableCapability(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Catalog == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no catalog is configured (keystone -catalog-dir)")
	}
	var req enableRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxProposalBytes)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	if req.Package == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("package required")
	}
	pkg, err := s.Catalog.Get(req.Package, req.Version)
	if err != nil {
		return nil, http.StatusNotFound, err
	}

	enabled, err := s.enabledCapabilities()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// The package being installed is in its own catalog, as the CLI's
	// expansion always did: a package may satisfy its own requirement.
	providers := append([]*pkgspec.Package{pkg}, s.Catalog.ProvidersFor(enabled)...)

	rendered, err := capability.NewEngine().Expand(pkg, req.Params, capability.NewCatalog(providers...))
	if err != nil {
		// Expansion failures are the operator's input being wrong — a
		// missing required param, an unsatisfied requirement — not a
		// server fault.
		return nil, http.StatusBadRequest, err
	}
	docs := make([]string, 0, len(rendered))
	for _, res := range rendered {
		data, err := resource.Marshal(res)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		docs = append(docs, string(data))
	}

	return s.submitProposalOpts(id, proposalRequest{
		Summary: fmt.Sprintf("enable capability %s (package %s %s)",
			pkg.Metadata.Capability, pkg.Metadata.Name, pkg.Metadata.Version),
		Put: docs,
	}, proposalOpts{
		DryRun: req.DryRun,
		Origin: &plan.CapabilityOrigin{
			Package: pkg.Metadata.Name,
			Version: pkg.Metadata.Version,
			Params:  req.Params,
		},
	})
}
