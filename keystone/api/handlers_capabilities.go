package api

import (
	"encoding/json"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
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
	Name       string                 `json:"name"`
	Version    string                 `json:"version"`
	Capability string                 `json:"capability"`
	Profile    string                 `json:"profile,omitempty"`
	Params     []paramView            `json:"params,omitempty"`
	Provides   []pkgspec.Provide      `json:"provides,omitempty"`
	Requires   []pkgspec.Requirement  `json:"requires,omitempty"`
	Publishes  *pkgspec.PublishesSpec `json:"publishes,omitempty"`
	Enabled    bool                   `json:"enabled"`
	capabilityDetail
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

// serviceAddress is where an installed capability's Route is served, and
// whether it answers.
type serviceAddress struct {
	Route string `json:"route"`
	// URL is what a BROWSER should use — derived from the ingress
	// posture, so the ported form under a tailnet rather than the
	// hostname the package declared (ADR-W022 A1). This is the answer to
	// "which port is this on", which previously existed only in
	// <prefix>/routes/ on the node.
	URL string `json:"url"`
	// Healthy reflects observed state. Since the Route health gate
	// probes the front door (F84), this means the address answers rather
	// than that a file was written.
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

type resourceRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// capabilityDetail is what the Services page could not previously say
// about a capability: where it is, whether it answers, what it created,
// and what stands in the way. All of it was derivable and none shown.
type capabilityDetail struct {
	Addresses []serviceAddress `json:"addresses,omitempty"`
	Resources []resourceRef    `json:"resources,omitempty"`
	// Blockers are reasons this will not give a WORKING service.
	// Distinct from providedSecrets, which the form already collects:
	// these cannot be answered by filling in a field.
	Blockers []string `json:"blockers,omitempty"`
}

// packageIsTheInstalledOne reports whether THIS package is the profile
// providing its capability here, rather than a sibling that could.
//
// A package declaring no containers is treated as the installed one:
// there is nothing to distinguish it by, and showing its capability's
// resources is closer to right than showing none.
func packageIsTheInstalledOne(pkg *pkgspec.Package, current []resource.Resource) bool {
	names := pkgspec.ContainerNames(pkg)
	if len(names) == 0 {
		return true
	}
	present := map[string]bool{}
	for _, r := range current {
		if r.Kind == resource.KindContainer {
			present[r.Metadata.Name] = true
		}
	}
	for _, n := range names {
		if present[n] {
			return true
		}
	}
	return false
}

func (s *Server) describeCapability(pkg *pkgspec.Package, current []resource.Resource, health map[string]runtimedb.Observed) capabilityDetail {
	var d capabilityDetail
	owner := "capability/" + pkg.Metadata.Capability
	posture := ingressorigin.Of(current)

	// Resources are owned by the CAPABILITY, not the package, so several
	// profiles of one capability all match the owner and every one of
	// them looks installed. Attribute only to the profile whose own
	// containers are actually present.
	if !packageIsTheInstalledOne(pkg, current) {
		d.Blockers = capabilityBlockers(pkg, posture)
		return d
	}

	for _, r := range current {
		if r.Metadata.Owner != owner {
			continue
		}
		d.Resources = append(d.Resources, resourceRef{Kind: string(r.Kind), Name: r.Metadata.Name})
		if r.Kind != resource.KindRoute || r.Route == nil || r.Route.Host == "" {
			continue
		}
		a := serviceAddress{Route: r.Metadata.Name, URL: posture.OriginFor(r.Route.Host)}
		if o, ok := health[string(r.Kind)+"/"+r.Metadata.Name]; ok {
			a.Healthy, a.Detail = o.Healthy, o.Detail
		}
		d.Addresses = append(d.Addresses, a)
	}
	sort.Slice(d.Resources, func(i, j int) bool {
		if d.Resources[i].Kind != d.Resources[j].Kind {
			return d.Resources[i].Kind < d.Resources[j].Kind
		}
		return d.Resources[i].Name < d.Resources[j].Name
	})
	sort.Slice(d.Addresses, func(i, j int) bool { return d.Addresses[i].Route < d.Addresses[j].Route })
	d.Blockers = capabilityBlockers(pkg, posture)
	return d
}

// capabilityBlockers are reasons this will not work HERE, as opposed to
// values the install form can collect.
func capabilityBlockers(pkg *pkgspec.Package, posture ingressorigin.Posture) []string {
	var out []string
	if posture.TLS != "tailscale" {
		return out
	}
	// F79: under a shared hostname an unpinned service cannot know its
	// own origin when it is expanded, so any URL it builds for a browser
	// — an OIDC redirect, its own base URL — will not match where it is
	// served. It converges, reports healthy, and cannot be signed into.
	for _, label := range pkgspec.RouteLabels(pkg) {
		if _, pinned := ingressorigin.WellKnownPortFor(label); !pinned {
			out = append(out, "under a tailnet this service's address is not fixed, so the links it builds for "+
				"itself (an OIDC redirect, its own base URL) will not match where it is served — it will install, "+
				"report healthy, and you will not be able to sign in")
			break
		}
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
	current, _ := s.State.Load()
	health := map[string]runtimedb.Observed{}
	if obs, err := s.DB.ListObserved(); err == nil {
		for _, o := range obs {
			health[string(o.Kind)+"/"+o.Name] = o
		}
	}
	out := []packageView{}
	for _, p := range s.Catalog.List() {
		out = append(out, packageView{
			Name: p.Metadata.Name, Version: p.Metadata.Version,
			Capability: p.Metadata.Capability, Profile: p.Metadata.Profile,
			Params: resolveParams(p.Params, facts), Provides: p.Provides, Requires: p.Requires,
			Publishes:        p.Publishes,
			Enabled:          enabled[p.Metadata.Capability],
			capabilityDetail: s.describeCapability(p, current, health),
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
