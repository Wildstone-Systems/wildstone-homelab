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
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// packageIcon is the app's own logo slug, or the generic fallback for a
// package that serves no Route (an adapter, a model runner) and for one
// whose container has no row in the dashboard's table.
//
// pkgspec.Routes parses the templates as YAML with the params' defaults
// substituted, so this reads the upstream STRUCTURALLY. A regex over the
// raw text was the first cut and was wrong twice over: it required
// `upstream:` to begin a line, so an equally valid `spec: {upstream:
// "container/x:80"}` silently fell back to the generic icon, and a
// templated upstream never matched at all.
//
// The first route whose container has a row wins, rather than simply the
// first route: photos-immich serves both photos and photos-api, and only
// one of the two is the app you would recognise.
func packageIcon(pkg *pkgspec.Package) string {
	fallback := reconcile.IconFor("")
	for _, r := range pkgspec.Routes(pkg) {
		if icon := reconcile.IconForUpstream(r.Upstream); icon != fallback {
			return icon
		}
	}
	return fallback
}

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
	// ProvidedSecrets are the secrets this package declares that only an
	// operator can supply. A form that collects them must ask for THESE
	// names: they differ per package (a Telegram bot token, a Matrix
	// access token) and a guessed name writes into a secret nothing
	// reads.
	ProvidedSecrets []string `json:"providedSecrets,omitempty"`
	// Icon is the dashboard-icons slug for this app's own logo, resolved
	// from the SAME table the household dashboard renders from
	// (reconcile.IconFor). The Console shows a logo beside every package
	// name; without this it would need its own container-name-to-slug
	// map, which is a second copy of that table and a copy that drifts.
	Icon string `json:"icon,omitempty"`
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
	// Installed is what this capability is ACTUALLY running with, read
	// from the origin of the plan that installed it.
	//
	// It exists because reconfiguring silently reset every param the
	// operator did not retype. Home Assistant was installed with
	// timezone=CST; the package default is UTC; the form offered UTC
	// because that is all it knew, and an upgrade would have moved a
	// household's clock with nothing anywhere saying so. A default is
	// what a package suggests to someone installing it for the first
	// time, and it is the wrong answer for everyone else.
	Installed string `json:"installed,omitempty"`
}

// deploymentFacts are the answers Keystone can supply for ParamSources.
type deploymentFacts struct {
	nodes  []string
	domain string
	// keystoneURL is the public API as something on a node reaches it.
	// It is empty when the deployment cannot answer, which the form
	// renders as an ordinary blank rather than a guess.
	keystoneURL string
	// intakeURL is the household event endpoint, base plus path.
	intakeURL string
	// timezone is the household's clock, as Keystone was started with.
	// Empty when it is the default "Local", which is not a zone anybody
	// means and not an answer worth suggesting.
	timezone string
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
	f.keystoneURL = s.APIURL
	if f.keystoneURL == "" {
		// Keystone advertises an address only when started with -hosts,
		// which the installer does not pass. But it does give the Console
		// route the address the proxy — a container — reaches the API
		// at, and anything else in a container reaches it the same way.
		// Without this a chat bot could not be connected on a stock
		// install (ADR-W033 B7.1).
		f.keystoneURL = consoleUpstreamURL(current)
	}
	if f.keystoneURL != "" {
		f.intakeURL = strings.TrimSuffix(f.keystoneURL, "/") + "/api/v1/household/events"
	}
	if s.HouseholdTimezone != "Local" {
		f.timezone = s.HouseholdTimezone
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

func resolveParams(decls []pkgspec.ParamDecl, f deploymentFacts, installed map[string]string) []paramView {
	out := make([]paramView, 0, len(decls))
	for _, d := range decls {
		v := paramView{ParamDecl: d, Installed: installed[d.Name]}
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
		case "keystone_url":
			v.Suggested = f.keystoneURL
		case "household_intake_url":
			v.Suggested = f.intakeURL
		case "timezone":
			v.Suggested = f.timezone
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
	// hostname the package declared (ADR-W024 A1). This is the answer to
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
	// Purpose is a Secret's one-line explanation, carried here so the
	// Secrets screen can say what a name means. Joined from the package
	// rather than from the node: the node holds values and knows
	// nothing about what they are for.
	Purpose string `json:"purpose,omitempty"`
}

// capabilityDetail is what the Services page could not previously say
// about a capability: where it is, whether it answers, what it created,
// and what stands in the way. All of it was derivable and none shown.
type capabilityDetail struct {
	// InstalledVersion is the package version this capability was
	// expanded from, when a plan recorded it (ADR-W021 A2). Empty for
	// anything installed before origins existed.
	InstalledVersion string `json:"installedVersion,omitempty"`
	// Stale means the catalog now holds a different version than the one
	// running. An operator who upgrades otherwise sees no change and
	// cannot tell "already current" from "needs re-installing" from "the
	// upgrade failed" — and reasonably concludes the last (F91).
	Stale bool `json:"stale,omitempty"`

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
		ref := resourceRef{Kind: string(r.Kind), Name: r.Metadata.Name}
		if r.Kind == resource.KindSecret && r.Secret != nil {
			ref.Purpose = r.Secret.Purpose
		}
		d.Resources = append(d.Resources, ref)
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

// installedVersions maps capability name to the package version its most
// recent install was expanded from.
//
// Read from plan origins rather than from the statefile, because a
// resource records its owning CAPABILITY and never the package version
// that produced it — so the plan is the only place the answer exists.
func (s *Server) installedVersions() map[string]string {
	versions, _ := s.installedFromPlans()
	return versions
}

// installedFromPlans reads what each package was last APPLIED with: its
// version, and the params it was given.
//
// The plan origin is the only record of either. Nothing else stores the
// answers an operator typed, which is why reconfiguring used to lose
// them: the form had the package's defaults and the deployment's facts,
// and no memory of the install itself.
func (s *Server) installedFromPlans() (versions map[string]string, params map[string]map[string]string) {
	versions, params = map[string]string{}, map[string]map[string]string{}
	// AppliedPlanJSON, not ListPlans: the listing deliberately blanks
	// plan_json, so reading origins from it silently found nothing on
	// every deployment — which is how F91's "you are behind the catalog"
	// came to report nothing at all.
	docs, err := s.DB.AppliedPlanJSON(200)
	if err != nil {
		return versions, params
	}
	// Oldest first, so the newest install for a capability wins.
	for i := len(docs) - 1; i >= 0; i-- {
		var stored plan.StoredProposal
		if json.Unmarshal([]byte(docs[i]), &stored) != nil || stored.Origin == nil {
			continue
		}
		versions[stored.Origin.Package] = stored.Origin.Version
		// One entry per install: an instance's params are its own, not
		// its package's, and must not overwrite another instance's
		// (#246).
		key := stored.Origin.Package
		if inst := stored.Origin.Params[pkgspec.InstanceParam]; inst != "" {
			key += "#" + inst
		}
		if len(stored.Origin.Params) > 0 {
			params[key] = stored.Origin.Params
		}
	}
	return versions, params
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
	facts := s.gatherFacts()
	versions, installedParams := s.installedFromPlans()
	// Checked, now that Enabled is derived from it. Discarding this error
	// used to cost nothing — the field came from enabledCapabilities(),
	// which returned one. Left discarded it turns an unreadable statefile
	// into a 200 saying every service is absent, and a Console offering
	// to install things that are already running.
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
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
			Params: resolveParams(p.Params, facts, installedParams[p.Metadata.Name]), Provides: p.Provides, Requires: p.Requires,
			Publishes:       p.Publishes,
			ProvidedSecrets: pkgspec.ProvidedSecrets(p),
			Icon:            packageIcon(p),
			// Whether THIS PACKAGE is installed, not whether its
			// capability is. Two profiles can run side by side now, and
			// keyed on the capability both of them read as installed —
			// so once Calibre-Web was in, Kavita showed as installed
			// with no Install action and the side-by-side case could not
			// be reached from the Console at all.
			Enabled:          packageIsInstalled(p, current),
			capabilityDetail: s.withVersion(p, s.describeCapability(p, current, health), versions),
		})
	}
	return out, http.StatusOK, nil
}

// claimedByAnotherCapability refuses an install that would re-own a
// resource another capability already holds.
//
// A resource is identified by kind and name alone, so a Put of a name
// that exists is an update, whoever declared it. calendar-radicale
// shipped Route/calendar, the name mail-roundcube's Davis already used:
// enabling the calendar rewrote Davis's Route to point at Radicale, the
// plan called it "Update Route/calendar", and the household's existing
// calendar address was on its way out with nothing saying so.
//
// Siblings within ONE capability share names deliberately (the book
// packages' Secret/restic-books), so only a different capability's
// claim is refused. Resources with no capability owner — written by
// hand — are left to the operator, as they always were.
func claimedByAnotherCapability(pkg *pkgspec.Package, current, rendered []resource.Resource) error {
	mine := installOwner(pkg, rendered)
	type key struct{ kind, name string }
	held := map[key]string{}
	for _, r := range current {
		if strings.HasPrefix(r.Metadata.Owner, "capability/") && r.Metadata.Owner != mine {
			held[key{string(r.Kind), r.Metadata.Name}] = r.Metadata.Owner
		}
	}
	var clashes []string
	for _, r := range rendered {
		if owner, ok := held[key{string(r.Kind), r.Metadata.Name}]; ok {
			clashes = append(clashes, fmt.Sprintf("%s/%s (held by %s)", r.Kind, r.Metadata.Name, owner))
		}
	}
	if len(clashes) == 0 {
		return nil
	}
	sort.Strings(clashes)
	return fmt.Errorf("package %s would take over resources another capability owns: %s — installing it would re-point them rather than add its own",
		pkg.Metadata.Name, strings.Join(clashes, ", "))
}

// droppedByUpgrade is what this capability owns today that its new
// expansion no longer produces.
//
// Installing over an existing capability only ever added: the proposal
// carried Put and nothing else, so a resource the previous version
// declared and the new one does not simply stayed. home-assistant 1.1.0
// replaced a mounted ConfigFile with seeded files, and after the upgrade
// the statefile still held configfile/home-assistant-wildstone —
// reconciled forever, written to disk forever, mounted by nothing.
//
// Harmless there, and not harmless in general: the same gap leaves a
// Route serving a container the package has dropped, or a Job running
// against a service that no longer exists. An upgrade that does not
// converge to the new package's definition is not an upgrade.
//
// DATA IS NEVER PROPOSED FOR DELETION. Volumes and seeded files belong
// to the household from the moment they land (ADR-W004 A4, ADR-W029 A2)
// — and a Volume delete cannot apply at all, so proposing one would fail
// the whole plan every tick until F60 set it aside, taking the rest of
// the upgrade with it. A package that stops declaring a volume leaves
// the data exactly where it is.
//
// NEITHER IS A SIBLING PROFILE'S. This is scoped to the package being
// installed, not to its capability, and the difference is the whole
// point of the Package field on a resource's metadata. Owner names the
// CAPABILITY, so books-calibre-web and books-kavita both render
// resources owned by "capability/books"; keyed on owner alone, enabling
// one read the other's container and Route as leftovers of an upgrade
// and proposed deleting them. Installing one book app uninstalled the
// other, and a household that wanted to compare them, or simply wanted
// both, had no way to say so (ADR-W029 A7).
//
// Replacing a sibling is now something a capability must ask for —
// `exclusive: true`, for the ones where two really cannot coexist — and
// when it does, the deletions are named in the plan rather than
// happening as a side effect of an install nobody read as a removal.
func droppedByUpgrade(pkg *pkgspec.Package, current, rendered []resource.Resource) []plan.DeletionRef {
	owner := installOwner(pkg, rendered)
	type key struct{ kind, name string }
	produced := map[key]bool{}
	for _, r := range rendered {
		produced[key{string(r.Kind), r.Metadata.Name}] = true
	}
	// Resources written before Metadata.Package existed carry no
	// provenance. Attributing them to this package is right exactly when
	// this package is the profile actually installed — the same question
	// packageIsTheInstalledOne already answers for the Console, and the
	// same heuristic, so a legacy deployment upgrades as it always did
	// and a legacy SIBLING is still left alone.
	// An instanced package postdates unstamped resources, so it has no
	// legacy to claim.
	legacyIsMine := !pkg.Metadata.Instanced && packageIsTheInstalledOne(pkg, current)
	var out []plan.DeletionRef
	for _, r := range current {
		if r.Metadata.Owner != owner {
			continue
		}
		if r.Kind == resource.KindVolume || r.Kind == resource.KindSeedFile {
			continue
		}
		switch r.Metadata.Package {
		case pkg.Metadata.Name:
			// Mine: an earlier version of this same package.
		case "":
			// An exclusive capability holds ONE profile, so an unstamped
			// resource it owns belongs to the profile being replaced
			// whether or not this package is the one already installed.
			//
			// Scoping this to legacyIsMine alone was a regression on the
			// behaviour it replaced: switching dns-pihole to dns-adguard
			// on a deployment written before provenance existed skipped
			// every unstamped container and route, so AdGuard installed
			// while Pi-hole kept running and the two fought over :53.
			// The old owner-keyed sweep removed it; the new one has to
			// as well, or "exclusive" means nothing where it matters
			// most.
			if !legacyIsMine && !pkg.Metadata.Exclusive {
				continue
			}
		default:
			if !pkg.Metadata.Exclusive {
				continue // a sibling profile, and both may run
			}
		}
		if produced[key{string(r.Kind), r.Metadata.Name}] {
			continue
		}
		out = append(out, plan.DeletionRef{Kind: string(r.Kind), Name: r.Metadata.Name})
	}
	// Deterministic, so the same upgrade proposes the same plan twice.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
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

// CapabilityEnabled reports whether one capability is installed on this
// deployment, right now. Exported so cmd/keystone can gate a household
// action on it without a second copy of the "owner says capability/x"
// rule — two copies is how one of them drifts.
func (s *Server) CapabilityEnabled(name string) (bool, error) {
	enabled, err := s.enabledCapabilities()
	if err != nil {
		return false, err
	}
	if enabled[name] {
		return true, nil
	}
	// An instanced package is installed as "<capability>-<instance>"
	// (#246), never under its own name, so it is installed when any
	// instance of it is. Only for a package the catalog says is
	// instanced: "chat" must not count as installed because "chat-x" is.
	if s.Catalog != nil {
		if pkg, err := s.Catalog.Get(name, ""); err == nil && pkg.Metadata.Instanced {
			for c := range enabled {
				if strings.HasPrefix(c, name+"-") {
					return true, nil
				}
			}
		}
	}
	return false, nil
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
	if err := claimedByAnotherCapability(pkg, current, rendered); err != nil {
		return nil, http.StatusConflict, err
	}
	docs := make([]string, 0, len(rendered))
	for _, res := range rendered {
		data, err := resource.Marshal(res)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		docs = append(docs, string(data))
	}

	capName, _ := pkg.CapabilityFor(req.Params) // Expand has already refused a bad instance
	return s.submitProposalOpts(id, proposalRequest{
		Summary: fmt.Sprintf("enable capability %s (package %s %s)",
			capName, pkg.Metadata.Name, pkg.Metadata.Version),
		Put:    docs,
		Delete: droppedByUpgrade(pkg, current, rendered),
	}, proposalOpts{
		DryRun: req.DryRun,
		Origin: &plan.CapabilityOrigin{
			Package: pkg.Metadata.Name,
			Version: pkg.Metadata.Version,
			Params:  req.Params,
		},
	})
}

// withVersion marks a capability whose running version is not the one
// the catalog now holds.
func (s *Server) withVersion(pkg *pkgspec.Package, d capabilityDetail, versions map[string]string) capabilityDetail {
	v, known := versions[pkg.Metadata.Name]
	if !known {
		return d
	}
	d.InstalledVersion = v
	d.Stale = v != pkg.Metadata.Version
	return d
}

type disableRequest struct {
	Package string `json:"package"`
	// Instance names which install of an instanced package to remove
	// (#246). Required for one, refused for any other.
	Instance string `json:"instance,omitempty"`
	DryRun   bool   `json:"dryRun,omitempty"`
}

// disableCapability removes what ONE PACKAGE produced.
//
// It names a package rather than a capability, everywhere, including for
// the capabilities that only ever have one profile. Two profiles can now
// run side by side (ADR-W029 A7), which makes "disable books" a sentence
// with three possible meanings — drop Calibre-Web, drop Kavita, drop
// both — and the reading it used to take was the most destructive one.
//
// It lives here rather than in the CLI because answering it correctly
// needs the catalog and the recorded install params, and the CLI has
// neither. Computing it client-side from /api/v1/resources produced two
// bugs in a row, and the second is the one that matters: both book
// packages declare Secret/restic-books, the restic repository password.
// Whichever installed last owns the stamp on it, so disabling that one
// proposed deleting a secret the surviving app still needs — and losing
// a repository password is not a broken backup, it is an unreadable one.
//
// So a resource a STILL-INSTALLED sibling would render is never deleted,
// which is exactly the shared-name case the catalog blesses: identical
// specs, one household repository, whichever app is reading the books.
//
// DATA IS NEVER PROPOSED FOR DELETION, the rule droppedByUpgrade has and
// this never got. Volumes and SeedFiles belong to the household
// (ADR-W004 A4), and the reconciler refuses a Volume Delete outright —
// so proposing one never removed data, it only guaranteed the plan ended
// in a failing step retried every tick until F60 set it aside. Removing
// an app is not removing the library it was reading.
func (s *Server) disableCapability(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Catalog == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no catalog is configured (keystone -catalog-dir)")
	}
	var req disableRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxProposalBytes)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	if req.Package == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("package required")
	}
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}

	pkg, err := s.Catalog.Get(req.Package, "")
	if err != nil {
		// A capability name is the likely mistake, and it has a good
		// answer: the packages actually installed under it.
		if installed := packagesUnder(req.Package, current); len(installed) > 0 {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"%q is a capability, not a package — installed under it: %s. Name the one to remove",
				req.Package, strings.Join(installed, ", "))
		}
		return nil, http.StatusNotFound, err
	}
	if req.Instance != "" && !pkg.Metadata.Instanced {
		return nil, http.StatusBadRequest, fmt.Errorf("%s is installed once, not per instance: remove it without naming one", pkg.Metadata.Name)
	}
	capName, err := pkg.CapabilityFor(map[string]string{pkgspec.InstanceParam: req.Instance})
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("%v — name the instance to remove", err)
	}
	owner := "capability/" + capName

	// What a surviving sibling still needs. Expanded from its own
	// recorded params, so this is what it WOULD render today, not what
	// happened to be stamped with its name.
	_, installedParams := s.installedFromPlans()
	enabled, err := s.enabledCapabilities()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	survives := map[[2]string]bool{}
	for _, sib := range s.Catalog.List() {
		// Another instance is another capability, with its own owner, so
		// deletesForDisable already leaves it alone.
		if pkg.Metadata.Instanced {
			break
		}
		if sib.Metadata.Name == pkg.Metadata.Name || sib.Metadata.Capability != pkg.Metadata.Capability {
			continue
		}
		if !packageIsInstalled(sib, current) {
			continue
		}
		rendered, err := capability.NewEngine().Expand(sib, installedParams[sib.Metadata.Name],
			capability.NewCatalog(append([]*pkgspec.Package{sib}, s.Catalog.ProvidersFor(enabled)...)...).WithCurrent(current))
		if err != nil {
			// A sibling that cannot be expanded is a sibling whose needs
			// are unknown, and deleting what it might need on that basis
			// is the wrong way to be wrong.
			return nil, http.StatusConflict, fmt.Errorf(
				"cannot determine what %s still needs (%v) — refusing to propose deletions that might belong to it",
				sib.Metadata.Name, err)
		}
		for _, rr := range rendered {
			survives[[2]string{string(rr.Kind), rr.Metadata.Name}] = true
		}
	}

	deletes := deletesForDisable(pkg.Metadata.Name, owner, current, survives)
	if len(deletes) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("%s has nothing installed to remove", capName)
	}

	return s.submitProposalOpts(id, proposalRequest{
		Summary: fmt.Sprintf("disable %s (%d resource(s) removed)", capName, len(deletes)),
		Delete:  deletes,
	}, proposalOpts{DryRun: req.DryRun})
}

// packagesUnder lists the packages with resources under a capability.
func packagesUnder(capName string, current []resource.Resource) []string {
	owner := "capability/" + capName
	set := map[string]bool{}
	for _, r := range current {
		if r.Metadata.Owner == owner && r.Metadata.Package != "" {
			set[r.Metadata.Package] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// packageIsInstalled reports whether this package's own containers are
// present — the same question packageIsTheInstalledOne answers, asked of
// a sibling rather than of the package being acted on.
func packageIsInstalled(pkg *pkgspec.Package, current []resource.Resource) bool {
	// An instanced package names its containers per instance, so there
	// is no fixed name to look for; any resource it stamped is an
	// instance of it.
	if pkg.Metadata.Instanced {
		for _, r := range current {
			if r.Metadata.Package == pkg.Metadata.Name {
				return true
			}
		}
		return false
	}
	names := pkgspec.ContainerNames(pkg)
	if len(names) > 0 {
		// A package that RUNS something is installed when that thing is
		// present, and by nothing else.
		//
		// "Any resource carrying this package's stamp" was the obvious
		// rule and the wrong one, because a removal deliberately RETAINS
		// what a surviving sibling still needs. Remove books-kavita and
		// Secret/restic-books stays, still stamped books-kavita — so the
		// package read as installed for ever: it kept a tile in
		// Installed, offered no way to put it back, and a second removal
		// answered "nothing installed to remove".
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
	// A package that runs nothing — the config-shim shape, ADR-W024 A10
	// — has no container to look for, so its own stamp is the only
	// evidence there is.
	for _, r := range current {
		if r.Metadata.Package == pkg.Metadata.Name {
			return true
		}
	}
	return false
}

// deletesForDisable picks the resources a disable may remove.
//
// Pure, and separate, because this is the part that was wrong twice.
// Both mistakes were about BOUNDARIES rather than about the rule:
//
//   - Scoped per capability instead of to the named package's own
//     capability, the legacy rule below applied to every capability with
//     no stamped resources — so disabling one book app selected
//     Authelia, LLDAP, Caddy, the dashboard and the chat adapters: 34
//     resources, from a command naming one app.
//   - Scoped correctly but with no notion of what a sibling still needs,
//     it selected Secret/restic-books, the restic repository password
//     that the SURVIVING book app also uses. A lost repository password
//     is not a broken backup, it is an unreadable one.
//
// survives is keyed {kind, name} and holds what a still-installed
// sibling would render today.
func deletesForDisable(pkgName, owner string, current []resource.Resource, survives map[[2]string]bool) []deletionRef {
	// A legacy resource carries no provenance, and belongs to this
	// package only when nothing else has claimed the capability.
	claimed := map[string]bool{}
	for _, rr := range current {
		if rr.Metadata.Owner == owner && rr.Metadata.Package != "" {
			claimed[rr.Metadata.Package] = true
		}
	}
	legacyIsMine := len(claimed) == 0 || (len(claimed) == 1 && claimed[pkgName])

	var out []deletionRef
	for _, rr := range current {
		if rr.Metadata.Owner != owner {
			continue // never another capability's, whatever its provenance
		}
		switch {
		case rr.Metadata.Package == pkgName:
		case rr.Metadata.Package == "":
			if !legacyIsMine {
				continue
			}
		default:
			continue // a sibling profile's
		}
		if rr.Kind == resource.KindVolume || rr.Kind == resource.KindSeedFile {
			continue // the household's data, never ours to propose removing
		}
		if survives[[2]string{string(rr.Kind), rr.Metadata.Name}] {
			continue // a sibling still needs it
		}
		out = append(out, deletionRef{Kind: string(rr.Kind), Name: rr.Metadata.Name})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// installOwner is the owner an install's resources carry: taken from
// what it rendered, which already names the instance for an instanced
// package, or the package's capability when it rendered nothing.
func installOwner(pkg *pkgspec.Package, rendered []resource.Resource) string {
	for _, r := range rendered {
		if r.Metadata.Owner != "" {
			return r.Metadata.Owner
		}
	}
	return "capability/" + pkg.Metadata.Capability
}

// consoleUpstreamURL is the API as the identity proxy's container
// reaches it, from the Console route's upstream, or "".
func consoleUpstreamURL(current []resource.Resource) string {
	for _, r := range current {
		if r.Kind == resource.KindRoute && r.Route != nil && r.Metadata.Owner == "capability/console" {
			up := r.Route.Upstream
			if up == "" || strings.Contains(up, "/") {
				return "" // a container/<name> upstream is not an address
			}
			return "http://" + up
		}
	}
	return ""
}
