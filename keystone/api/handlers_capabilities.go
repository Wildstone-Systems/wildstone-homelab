package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	Params     []paramView           `json:"params,omitempty"`
	Provides   []pkgspec.Provide     `json:"provides,omitempty"`
	Requires   []pkgspec.Requirement `json:"requires,omitempty"`
	Enabled    bool                  `json:"enabled"`
}

// listPackages answers what this control plane can install.

// paramView is a ParamDecl plus what this deployment already knows about
// it (ADR-W021 A5, extended).
//
// The form was generated from the package alone, so it asked for facts
// the control plane was already holding: which node — there is one — and
// which domain — every installed capability used the same one. Asking is
// the defect; a description only makes the question answerable.
type paramView struct {
	pkgspec.ParamDecl
	// Options, when non-empty, are the only sensible answers. The form
	// should offer a choice rather than a blank box.
	Options []string `json:"options,omitempty"`
	// Suggested is the value to prefill. Separate from Default because a
	// package's default is a property of the PACKAGE, and this is a
	// property of the deployment.
	Suggested string `json:"suggested,omitempty"`
}

// deploymentFacts are the answers Keystone can supply for ParamSources.
type deploymentFacts struct {
	nodes  []string
	domain string
}

// gatherFacts reads what the deployment already knows.
//
// The domain comes from the Routes that exist rather than from any
// stored answer: a Route host is "<service>.<domain>", so the deployment
// states its own domain by having any Route at all. That also means the
// answer is right even for a deployment nobody recorded params for —
// which is every deployment installed before capability origins were
// recorded.
func (s *Server) gatherFacts() deploymentFacts {
	var f deploymentFacts
	// Recorded nodes, not only live ones: a node that is briefly
	// disconnected is still where a capability belongs, and offering an
	// empty list because an agent is restarting would be worse than
	// offering a node that needs to come back.
	if recorded, err := s.DB.ListNodes(); err == nil {
		for _, n := range recorded {
			f.nodes = append(f.nodes, n.NodeID)
		}
	}
	sort.Strings(f.nodes)

	current, err := s.State.Load()
	if err != nil {
		return f
	}
	counts := map[string]int{}
	for _, r := range current {
		if r.Kind != resource.KindRoute || r.Route == nil || r.Route.Host == "" {
			continue
		}
		if _, rest, ok := strings.Cut(r.Route.Host, "."); ok && rest != "" {
			counts[rest]++
		}
	}
	best := 0
	for d, n := range counts {
		// Ties broken by name so the answer does not depend on map order.
		if n > best || (n == best && d < f.domain) {
			f.domain, best = d, n
		}
	}
	return f
}

func resolveParams(decls []pkgspec.ParamDecl, f deploymentFacts) []paramView {
	out := make([]paramView, 0, len(decls))
	for _, d := range decls {
		v := paramView{ParamDecl: d}
		switch d.Source {
		case "node":
			v.Options = f.nodes
			if len(f.nodes) == 1 {
				v.Suggested = f.nodes[0]
			}
		case "domain":
			if f.domain != "" {
				v.Suggested = f.domain
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) listPackages(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Catalog == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no catalog is configured (keystone -catalog-dir)")
	}
	enabled, err := s.enabledCapabilities()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	facts := s.gatherFacts()
	out := []packageView{}
	for _, p := range s.Catalog.List() {
		out = append(out, packageView{
			Name: p.Metadata.Name, Version: p.Metadata.Version,
			Capability: p.Metadata.Capability, Profile: p.Metadata.Profile,
			Params: resolveParams(p.Params, facts), Provides: p.Provides, Requires: p.Requires,
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

	// The catalog is told the node's desired state, because addressing
	// lives in the Ingress declaration rather than in any package
	// (ADR-W019 A10). Without it, a .Resolved origin would describe the
	// host a Route declares instead of the one it is served at, and an
	// OIDC issuer that disagrees with the browser is a login loop.
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	catalog := capability.NewCatalog(providers...).WithCurrent(current)

	rendered, err := capability.NewEngine().Expand(pkg, req.Params, catalog)
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
