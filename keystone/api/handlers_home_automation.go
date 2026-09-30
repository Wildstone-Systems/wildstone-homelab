package api

// GET /api/v1/home-automation (#446): the one typed answer to "where does
// home automation stand here, and what may happen next".
//
// It JOINS truths other endpoints already own — catalog packages and
// installed origins (handlers_capabilities.go), desired and observed
// resources, node liveness (handlers_nodes.go), the radio inventory and
// readiness probes (handlers_probes.go), radio ownership
// (plan.RadioOwners), pending and failed plans, and backup coverage —
// and adds only the judgement that ties them together: per component a
// closed state, typed blockers with stable codes, evidence and the
// actions the Console may offer. The Console renders that and never
// re-derives protocol dependencies, suitability or remedies itself.
//
// A read in every sense: the probes it dispatches are parameterless
// reads, and it records no plan, event or setup state. `?select=`,
// `?topology=`, `?node=` and `?bluetooth=` describe a household's GOALS
// for this one answer — nothing about them is stored.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// homeAutomationReadinessProtocolMinor is the Wardstone protocol minor
// that first carried HomeAutomationReadiness (#436).
const homeAutomationReadinessProtocolMinor = 25

// haProbeTimeout bounds each probe the aggregate waits on. Both ops read
// a handful of files; a node that has not answered by then is reported
// as failed to answer rather than waited for.
const haProbeTimeout = 5 * time.Second

// haState is a component's closed state.
type haState string

const (
	haNotSelected      haState = "not_selected"
	haBlocked          haState = "blocked"
	haReadyToPropose   haState = "ready_to_propose"
	haAwaitingApproval haState = "awaiting_approval"
	haApplying         haState = "applying"
	haExternalStep     haState = "external_step"
	haReady            haState = "ready"
	haDegraded         haState = "degraded"
	haUnavailable      haState = "unavailable"
)

// Blocker severities: a blocking one stops a proposal (or degrades what
// is installed); a warning is shown and changes no state.
const (
	haBlocking = "blocking"
	haWarning  = "warning"
)

type haEvidence struct {
	// Source is where the fact came from: node, readiness, radios,
	// observed, plan, catalog, statefile, backup, principal.
	Source     string     `json:"source"`
	Subject    string     `json:"subject,omitempty"`
	State      string     `json:"state,omitempty"`
	Value      string     `json:"value,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`
}

// haAction is a permitted next action. Kind is closed: retry_probe,
// install_package, reconfigure_package, upgrade_package, open_plan,
// open_service, select_radio, manual_step, mint_publisher, open_backups,
// choose_node, pair_node, resolve_dependency. None of them is performed
// by this read; each names what the Console may offer and with what.
type haAction struct {
	Kind         string            `json:"kind"`
	Label        string            `json:"label"`
	Component    string            `json:"component,omitempty"`
	Package      string            `json:"package,omitempty"`
	Capability   string            `json:"capability,omitempty"`
	Node         string            `json:"node,omitempty"`
	PlanID       string            `json:"planId,omitempty"`
	RadioIDs     []string          `json:"radioIds,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	Instructions []string          `json:"instructions,omitempty"`
}

type haBlocker struct {
	Code     string       `json:"code"`
	Severity string       `json:"severity"`
	Message  string       `json:"message"`
	Evidence []haEvidence `json:"evidence"`
	Actions  []haAction   `json:"actions"`
}

type haStep struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Instructions []string `json:"instructions"`
	// Status: done or pending when Wildstone can observe the step,
	// unverified when it cannot (the Console offers a recheck/tick).
	Status   string       `json:"status"`
	Evidence []haEvidence `json:"evidence,omitempty"`
	Action   *haAction    `json:"action,omitempty"`
}

type haResource struct {
	Kind             string     `json:"kind"`
	Name             string     `json:"name"`
	Node             string     `json:"node,omitempty"`
	Observed         bool       `json:"observed"`
	Healthy          bool       `json:"healthy"`
	AwaitingApproval bool       `json:"awaitingApproval,omitempty"`
	AppHealth        string     `json:"appHealth,omitempty"`
	Detail           string     `json:"detail,omitempty"`
	ObservedAt       *time.Time `json:"observedAt,omitempty"`
}

type haPlanRef struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Summary string `json:"summary"`
	Package string `json:"package,omitempty"`
}

type haBackup struct {
	Plans             []string   `json:"plans"`
	Volumes           []string   `json:"volumes"`
	Uncovered         []string   `json:"uncovered"`
	LastSuccessAt     *time.Time `json:"lastSuccessAt,omitempty"`
	LastRunStatus     string     `json:"lastRunStatus,omitempty"`
	LastRunFinishedAt *time.Time `json:"lastRunFinishedAt,omitempty"`
}

type haRadioOwner struct {
	Device     string `json:"device"`
	RadioID    string `json:"radioId,omitempty"`
	Container  string `json:"container"`
	Package    string `json:"package,omitempty"`
	Capability string `json:"capability,omitempty"`
	// Component is the home-automation component owning it, empty for a
	// service outside this model (a camera app, a hand-written container).
	Component string `json:"component,omitempty"`
	Source    string `json:"source"`
}

type haComponent struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Capability string   `json:"capability"`
	Topology   string   `json:"topology"`
	Protocol   bool     `json:"protocol"`
	Requires   []string `json:"requires"`
	// Packages are the catalog packages that can provide this component.
	Packages []string `json:"packages"`
	// Package is the installed one, else the one a proposal would use.
	Package          string   `json:"package,omitempty"`
	Version          string   `json:"version,omitempty"`
	InstalledVersion string   `json:"installedVersion,omitempty"`
	Stale            bool     `json:"stale,omitempty"`
	Mode             string   `json:"mode,omitempty"`
	Modes            []string `json:"modes,omitempty"`
	// Selection is why this component is part of the answer: installed,
	// pending (a plan exists), requested (?select/?topology), dependency
	// (a selected component needs it) or none.
	Selection string  `json:"selection"`
	State     haState `json:"state"`
	Reason    string  `json:"reason"`
	Node      string  `json:"node,omitempty"`

	Radios          []haRadioOwner   `json:"radios,omitempty"`
	RadioCandidates []string         `json:"radioCandidates,omitempty"`
	Addresses       []serviceAddress `json:"addresses,omitempty"`
	Resources       []haResource     `json:"resources,omitempty"`
	PendingPlan     *haPlanRef       `json:"pendingPlan,omitempty"`
	FailedPlan      *haPlanRef       `json:"failedPlan,omitempty"`
	Backup          *haBackup        `json:"backup,omitempty"`
	Blockers        []haBlocker      `json:"blockers"`
	Actions         []haAction       `json:"actions"`
	ExternalSteps   []haStep         `json:"externalSteps"`

	installed bool
	spec      haComponentSpec
	pkg       *pkgspec.Package
}

type haReadiness struct {
	// Probe: ok, offline, agent_outdated, failed, not_wired.
	Probe             string               `json:"probe"`
	Detail            string               `json:"detail,omitempty"`
	PlatformSupported bool                 `json:"platformSupported"`
	Interfaces        []ifaceView          `json:"interfaces"`
	Checks            []readinessCheckView `json:"checks"`
}

type haSuitability struct {
	Component string `json:"component"`
	// Fit: detected (the inventory identified this protocol) or possible
	// (an unidentified serial radio the household may confirm).
	Fit string `json:"fit"`
}

type haRadio struct {
	radioView
	Owners      []haRadioOwner  `json:"owners"`
	SuitableFor []haSuitability `json:"suitableFor"`
}

type haRadioInventory struct {
	Probe             string    `json:"probe"`
	Detail            string    `json:"detail,omitempty"`
	PlatformSupported bool      `json:"platformSupported"`
	Radios            []haRadio `json:"radios"`
	// UnresolvedClaims are device claims on exclusive paths the
	// inventory does not list: a stick that was unplugged, or a path
	// written by hand.
	UnresolvedClaims []haRadioOwner `json:"unresolvedClaims"`
}

type haNode struct {
	ID            string            `json:"id"`
	Connected     bool              `json:"connected"`
	AgentVersion  string            `json:"agentVersion,omitempty"`
	ProtocolMinor uint32            `json:"protocolMinor,omitempty"`
	LastSeenAt    *time.Time        `json:"lastSeenAt,omitempty"`
	Selected      bool              `json:"selected"`
	HostAddress   string            `json:"hostAddress,omitempty"`
	Readiness     *haReadiness      `json:"readiness,omitempty"`
	RadioProbe    *haRadioInventory `json:"radioInventory,omitempty"`
	Blockers      []haBlocker       `json:"blockers"`

	liveKnown bool
	inventory []plan.Radio // nil: could not ask
}

type haView struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// Topology is what is installed or pending: none, external, managed
	// or both. RequestedTopology echoes ?topology.
	Topology          string        `json:"topology"`
	RequestedTopology string        `json:"requestedTopology,omitempty"`
	Node              string        `json:"node,omitempty"`
	Nodes             []haNode      `json:"nodes"`
	Components        []haComponent `json:"components"`
	// Blockers are deployment-level: no catalog, no node.
	Blockers []haBlocker `json:"blockers"`
}

type haPlan struct {
	rec    runtimedb.PlanRecord
	stored plan.StoredProposal
}

// haBuild is one evaluation's inputs, read once.
type haBuild struct {
	s          *Server
	current    []resource.Resource
	health     map[string]runtimedb.Observed
	versions   map[string]string
	params     map[string]map[string]string
	nodes      map[string]*haNode
	nodeOrder  []string
	pending    []haPlan
	backupRuns []runtimedb.BackupRun
	node       string
	selection  map[string]string
	modeHint   map[string]string
	requires   map[string][]string
	comps      map[string]*haComponent
}

func (s *Server) homeAutomationStatus(r *http.Request, _ principal.Identity) (any, int, error) {
	q := r.URL.Query()
	topology := q.Get("topology")
	if topology != "" && topology != haTopologyManaged && topology != haTopologyExternal {
		return nil, http.StatusBadRequest, fmt.Errorf("topology must be managed or external")
	}
	requested := map[string]bool{}
	for _, id := range splitCSV(q["select"]) {
		if _, ok := haSpec(id); !ok {
			return nil, http.StatusBadRequest, fmt.Errorf("unknown component %q in select", id)
		}
		requested[id] = true
	}
	switch topology {
	case haTopologyManaged:
		requested[haHomeAssistant] = true
	case haTopologyExternal:
		requested[haExternalHA] = true
	}
	modeHint := map[string]string{}
	if m := q.Get("bluetooth"); m != "" {
		spec, _ := haSpec(haBluetooth)
		if !haContains(spec.OnModes, m) {
			return nil, http.StatusBadRequest, fmt.Errorf("bluetooth must be one of %s", strings.Join(spec.OnModes, ", "))
		}
		modeHint[haBluetooth] = m
	}

	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	b := &haBuild{s: s, current: current, health: map[string]runtimedb.Observed{},
		modeHint: modeHint, comps: map[string]*haComponent{}, nodes: map[string]*haNode{}}
	if obs, err := s.DB.ListObserved(); err == nil {
		for _, o := range obs {
			b.health[o.Kind+"/"+o.Name] = o
		}
	}
	b.versions, b.params = s.installedFromPlans()
	for _, st := range []runtimedb.PlanState{runtimedb.PlanProposed, runtimedb.PlanApproved, runtimedb.PlanApplying} {
		recs, err := s.DB.PlansInState(st)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		for _, rec := range recs {
			var sp plan.StoredProposal
			if json.Unmarshal([]byte(rec.PlanJSON), &sp) == nil {
				b.pending = append(b.pending, haPlan{rec: rec, stored: sp})
			}
		}
	}
	if runs, err := s.DB.ListBackupRuns(500); err == nil {
		b.backupRuns = runs
	}
	if err := b.loadNodes(); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if n := q.Get("node"); n != "" {
		if _, ok := b.nodes[n]; !ok {
			return nil, http.StatusBadRequest, fmt.Errorf("unknown node %q", n)
		}
		b.node = n
	}

	b.resolveRequires()
	b.resolveSelection(requested)
	if b.node == "" {
		b.node = b.defaultNode()
	}
	b.probeNodes(r.Context())

	view := haView{GeneratedAt: time.Now().UTC(), RequestedTopology: topology, Node: b.node, Blockers: []haBlocker{}}
	if s.Catalog == nil {
		view.Blockers = append(view.Blockers, haBlocker{
			Code: "catalog_unavailable", Severity: haBlocking,
			Message:  "This control plane has no package catalog, so nothing can be installed from here.",
			Evidence: []haEvidence{{Source: "catalog", Detail: "keystone was started without -catalog-dir"}},
			Actions:  []haAction{},
		})
	}
	if len(b.nodeOrder) == 0 {
		view.Blockers = append(view.Blockers, b.noNodeBlocker())
	}
	for _, spec := range haComponents {
		c := b.evaluate(spec)
		b.comps[spec.ID] = c
	}
	for _, spec := range haComponents {
		view.Components = append(view.Components, *b.comps[spec.ID])
	}
	view.Topology = b.topology()
	for _, id := range b.nodeOrder {
		n := b.nodes[id]
		n.Selected = id == b.node
		view.Nodes = append(view.Nodes, *n)
	}
	if view.Nodes == nil {
		view.Nodes = []haNode{}
	}
	return view, http.StatusOK, nil
}

func haContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// loadNodes joins recorded nodes with live sessions, exactly as the node
// inventory does.
func (b *haBuild) loadNodes() error {
	recorded, err := b.s.DB.ListNodes()
	if err != nil {
		return err
	}
	var live map[string]LiveNode
	if b.s.LiveNodes != nil {
		live = map[string]LiveNode{}
		for _, l := range b.s.LiveNodes() {
			live[l.NodeID] = l
		}
	}
	for _, rn := range recorded {
		seen := rn.LastSeenAt
		n := &haNode{ID: rn.NodeID, AgentVersion: rn.AgentVersion, ProtocolMinor: rn.ProtocolMinor, LastSeenAt: &seen,
			liveKnown: live != nil, Blockers: []haBlocker{}}
		if l, ok := live[rn.NodeID]; ok {
			n.Connected, n.AgentVersion, n.ProtocolMinor = true, l.AgentVersion, l.ProtocolMinor
		}
		b.nodes[n.ID] = n
	}
	for id, l := range live {
		if _, ok := b.nodes[id]; !ok {
			since := l.Since
			b.nodes[id] = &haNode{ID: id, Connected: true, AgentVersion: l.AgentVersion, ProtocolMinor: l.ProtocolMinor,
				LastSeenAt: &since, liveKnown: true, Blockers: []haBlocker{}}
		}
	}
	for id := range b.nodes {
		b.nodeOrder = append(b.nodeOrder, id)
	}
	sort.Strings(b.nodeOrder)
	return nil
}

// catalogPackages are the catalog packages providing a capability,
// sorted by name so the proposed package is deterministic.
func (b *haBuild) catalogPackages(capName string) []*pkgspec.Package {
	if b.s.Catalog == nil {
		return nil
	}
	var out []*pkgspec.Package
	for _, p := range b.s.Catalog.List() {
		if p.Metadata.Capability == capName {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

// resolveRequires adds the catalog's trait dependencies to the table's:
// a package that `requires` a trait another component's package
// provides depends on that component.
func (b *haBuild) resolveRequires() {
	provider := map[string]string{}
	for _, spec := range haComponents {
		if len(spec.ModeParams) > 0 {
			continue
		}
		for _, p := range b.catalogPackages(spec.Capability) {
			for _, pr := range p.Provides {
				if _, taken := provider[pr.Trait]; !taken {
					provider[pr.Trait] = spec.ID
				}
			}
		}
	}
	b.requires = map[string][]string{}
	for _, spec := range haComponents {
		reqs := append([]string{}, spec.Requires...)
		if len(spec.ModeParams) == 0 {
			for _, p := range b.catalogPackages(spec.Capability) {
				for _, rq := range p.Requires {
					if !rq.AppliesTo(b.params[p.Metadata.Name]) {
						continue
					}
					if dep, ok := provider[rq.Trait]; ok && dep != spec.ID && !haContains(reqs, dep) {
						reqs = append(reqs, dep)
					}
				}
			}
		}
		b.requires[spec.ID] = reqs
	}
}

// installedPackage is the catalog package of this capability that is
// installed, and whether anything of the capability is installed at all
// (a capability can be present with no catalog package for it).
func (b *haBuild) installedPackage(spec haComponentSpec) (*pkgspec.Package, bool) {
	for _, p := range b.catalogPackages(spec.Capability) {
		if packageIsInstalled(p, b.current) {
			return p, true
		}
	}
	owner := "capability/" + spec.Capability
	for _, r := range b.current {
		if r.Metadata.Owner == owner {
			return nil, true
		}
	}
	return nil, false
}

// modeParam is the name of the mode param a package declares, if any.
func modeParam(spec haComponentSpec, p *pkgspec.Package) (pkgspec.ParamDecl, bool) {
	if p == nil {
		return pkgspec.ParamDecl{}, false
	}
	for _, name := range spec.ModeParams {
		for _, d := range p.Params {
			if d.Name == name {
				return d, true
			}
		}
	}
	return pkgspec.ParamDecl{}, false
}

// installedMode is a mode component's value on the installed package.
func (b *haBuild) installedMode(spec haComponentSpec, p *pkgspec.Package) string {
	d, ok := modeParam(spec, p)
	if !ok {
		return ""
	}
	if v := b.params[p.Metadata.Name][d.Name]; v != "" {
		return v
	}
	return d.Default
}

func (b *haBuild) isInstalled(spec haComponentSpec) bool {
	if len(spec.ModeParams) == 0 {
		_, ok := b.installedPackage(spec)
		return ok
	}
	p, ok := b.installedPackage(spec)
	return ok && haContains(spec.OnModes, b.installedMode(spec, p))
}

// pendingFor is the newest open plan touching this component: an install
// or upgrade whose origin names one of its packages, or a removal of
// resources it owns. For a mode component only a change of its mode
// counts.
func (b *haBuild) pendingFor(spec haComponentSpec) *haPlan {
	pkgs := map[string]bool{}
	for _, p := range b.catalogPackages(spec.Capability) {
		pkgs[p.Metadata.Name] = true
	}
	owned := map[[2]string]bool{}
	for _, r := range b.current {
		if r.Metadata.Owner == "capability/"+spec.Capability {
			owned[[2]string{string(r.Kind), r.Metadata.Name}] = true
		}
	}
	var found *haPlan
	for i := range b.pending {
		p := &b.pending[i]
		hit := false
		if o := p.stored.Origin; o != nil && pkgs[o.Package] {
			hit = true
			if len(spec.ModeParams) > 0 {
				hit = false
				inst, _ := b.installedPackage(spec)
				now := b.installedMode(spec, inst)
				for _, name := range spec.ModeParams {
					if v, ok := o.Params[name]; ok && v != now {
						hit = true
					}
				}
			}
		}
		if len(spec.ModeParams) == 0 {
			for _, d := range p.stored.Delete {
				if owned[[2]string{d.Kind, d.Name}] {
					hit = true
				}
			}
		}
		if hit && (found == nil || p.rec.Seq > found.rec.Seq) {
			found = p
		}
	}
	return found
}

// resolveSelection expands the household's goals with everything they
// need, transitively.
func (b *haBuild) resolveSelection(requested map[string]bool) {
	b.selection = map[string]string{}
	for _, spec := range haComponents {
		switch {
		case b.isInstalled(spec):
			b.selection[spec.ID] = "installed"
		case b.pendingFor(spec) != nil:
			b.selection[spec.ID] = "pending"
		case requested[spec.ID]:
			b.selection[spec.ID] = "requested"
		}
	}
	for changed := true; changed; {
		changed = false
		for _, spec := range haComponents {
			if b.selection[spec.ID] == "" {
				continue
			}
			for _, dep := range b.requires[spec.ID] {
				if b.selection[dep] == "" {
					b.selection[dep] = "dependency"
					changed = true
				}
			}
		}
	}
}

// componentNode is where a component runs: its installed resources'
// node, else a pending plan's node param, else the evaluated node.
func (b *haBuild) componentNode(spec haComponentSpec, pkg *pkgspec.Package) string {
	owner := "capability/" + spec.Capability
	node := ""
	for _, r := range b.current {
		if r.Metadata.Owner != owner || r.Metadata.Node == "" {
			continue
		}
		if pkg != nil && r.Metadata.Package != "" && r.Metadata.Package != pkg.Metadata.Name {
			continue
		}
		if r.Kind == resource.KindContainer {
			return r.Metadata.Node
		}
		if node == "" {
			node = r.Metadata.Node
		}
	}
	if node != "" {
		return node
	}
	if p := b.pendingFor(spec); p != nil && p.stored.Origin != nil {
		if n := p.stored.Origin.Params["node"]; n != "" {
			return n
		}
	}
	return b.node
}

// defaultNode is the node a household's next install belongs on: where
// home automation already runs, else the only node there is.
func (b *haBuild) defaultNode() string {
	for _, spec := range haComponents {
		if b.selection[spec.ID] == "installed" || b.selection[spec.ID] == "pending" {
			pkg, _ := b.installedPackage(spec)
			if n := b.componentNode(haComponentSpec{Capability: spec.Capability}, pkg); n != "" {
				return n
			}
		}
	}
	if len(b.nodeOrder) == 1 {
		return b.nodeOrder[0]
	}
	return ""
}

// probeNodes asks the evaluated node, and every node something is
// installed on, for its radios and readiness — concurrently, each op
// bounded by haProbeTimeout.
func (b *haBuild) probeNodes(ctx context.Context) {
	want := map[string]bool{}
	if b.node != "" {
		want[b.node] = true
	}
	for _, spec := range haComponents {
		if b.selection[spec.ID] == "installed" || b.selection[spec.ID] == "pending" {
			pkg, _ := b.installedPackage(spec)
			if n := b.componentNode(spec, pkg); n != "" {
				want[n] = true
			}
		}
	}
	var wg sync.WaitGroup
	for id := range want {
		n, ok := b.nodes[id]
		if !ok {
			continue // a resource naming a node that never joined
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.probe(ctx, n)
		}()
	}
	wg.Wait()
	for id := range want {
		if n, ok := b.nodes[id]; ok {
			n.Blockers = b.nodeBlockers(n)
			b.annotateRadios(n)
		}
	}
}

func (b *haBuild) probe(ctx context.Context, n *haNode) {
	n.Readiness = &haReadiness{Interfaces: []ifaceView{}, Checks: []readinessCheckView{}}
	n.RadioProbe = &haRadioInventory{Radios: []haRadio{}, UnresolvedClaims: []haRadioOwner{}}
	switch {
	case b.s.Agents == nil:
		n.Readiness.Probe, n.RadioProbe.Probe = "not_wired", "not_wired"
		return
	case n.liveKnown && !n.Connected:
		n.Readiness.Probe, n.RadioProbe.Probe = "offline", "offline"
		return
	}
	apply := func(op *wsv1.Operation) (*wsv1.OpResult, string, bool) {
		cctx, cancel := context.WithTimeout(ctx, haProbeTimeout)
		defer cancel()
		res, err := b.s.Agents.Apply(cctx, n.ID, op)
		if err != nil {
			return nil, err.Error(), false
		}
		if oe := res.GetError(); oe != nil {
			return nil, oe.GetMessage(), true
		}
		return res, "", true
	}
	var wg sync.WaitGroup
	var reached [2]bool
	if n.liveKnown && n.ProtocolMinor < homeAutomationReadinessProtocolMinor {
		n.Readiness.Probe = "agent_outdated"
		n.Readiness.Detail = fmt.Sprintf("agent speaks protocol minor %d; the readiness probe needs %d", n.ProtocolMinor, homeAutomationReadinessProtocolMinor)
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, detail, ok := apply(&wsv1.Operation{Op: &wsv1.Operation_HomeAutomationReadiness{HomeAutomationReadiness: &wsv1.HomeAutomationReadiness{}}})
			reached[0] = ok
			if res == nil || res.GetHomeAutomationReadiness() == nil {
				n.Readiness.Probe, n.Readiness.Detail = "failed", detail
				return
			}
			hr := res.GetHomeAutomationReadiness()
			n.Readiness.Probe, n.Readiness.PlatformSupported = "ok", hr.GetPlatformSupported()
			n.Readiness.Interfaces, n.Readiness.Checks = viewReadiness(hr)
		}()
	}
	if n.liveKnown && n.ProtocolMinor < plan.RadioInventoryProtocolMinor {
		n.RadioProbe.Probe = "agent_outdated"
		n.RadioProbe.Detail = fmt.Sprintf("agent speaks protocol minor %d; the radio inventory needs %d", n.ProtocolMinor, plan.RadioInventoryProtocolMinor)
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, detail, ok := apply(&wsv1.Operation{Op: &wsv1.Operation_ListRadios{ListRadios: &wsv1.ListRadios{}}})
			reached[1] = ok
			if res == nil || res.GetListRadios() == nil {
				n.RadioProbe.Probe, n.RadioProbe.Detail = "failed", detail
				return
			}
			lr := res.GetListRadios()
			n.RadioProbe.Probe, n.RadioProbe.PlatformSupported = "ok", lr.GetPlatformSupported()
			for _, rv := range viewRadios(lr) {
				n.RadioProbe.Radios = append(n.RadioProbe.Radios, haRadio{radioView: rv, Owners: []haRadioOwner{}, SuitableFor: []haSuitability{}})
			}
		}()
	}
	wg.Wait()
	if !n.liveKnown {
		// No session registry: the node is connected exactly when it
		// answered.
		n.Connected = reached[0] || reached[1]
		if !n.Connected {
			n.Readiness.Probe, n.RadioProbe.Probe = "offline", "offline"
		}
	}
	if n.RadioProbe.Probe == "ok" {
		rs := make([]radioView, 0, len(n.RadioProbe.Radios))
		for _, r := range n.RadioProbe.Radios {
			rs = append(rs, r.radioView)
		}
		n.inventory = planRadios(rs)
	}
	n.HostAddress = hostAddress(n.Readiness.Interfaces)
}

// hostAddress is the first IPv4 address on a default-route interface —
// the address a Home Assistant integration on the LAN dials.
func hostAddress(ifaces []ifaceView) string {
	for _, pass := range []bool{true, false} {
		for _, i := range ifaces {
			if pass && !i.DefaultRoute {
				continue
			}
			for _, a := range i.Addresses {
				ip, _, err := net.ParseCIDR(a)
				if err != nil {
					ip = net.ParseIP(a)
				}
				if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
					return ip.String()
				}
			}
		}
	}
	return ""
}

// componentForCapability maps a claim's owner back to a component.
func componentForOwner(owner string) string {
	capName := strings.TrimPrefix(owner, "capability/")
	for _, spec := range haComponents {
		if spec.Capability == capName && len(spec.ModeParams) == 0 {
			return spec.ID
		}
	}
	return ""
}

func ownerView(c plan.DeviceClaim) haRadioOwner {
	return haRadioOwner{
		Device: c.Device, RadioID: c.RadioID, Container: c.Container, Package: c.Package,
		Capability: strings.TrimPrefix(c.Owner, "capability/"), Component: componentForOwner(c.Owner), Source: c.Source,
	}
}

// annotateRadios attaches owners (plan.RadioOwners, the proposal
// pipeline's own ownership truth) and protocol suitability to a node's
// inventory.
func (b *haBuild) annotateRadios(n *haNode) {
	if n.RadioProbe == nil || n.RadioProbe.Probe != "ok" {
		return
	}
	owners := plan.RadioOwners(b.current, n.ID, n.inventory)
	listed := map[string]bool{}
	for i := range n.RadioProbe.Radios {
		r := &n.RadioProbe.Radios[i]
		listed[r.ID] = true
		for _, c := range owners[r.ID] {
			r.Owners = append(r.Owners, ownerView(c))
		}
		for _, spec := range haComponents {
			if spec.RadioKind == "" {
				continue
			}
			switch {
			case r.Kind == spec.RadioKind:
				r.SuitableFor = append(r.SuitableFor, haSuitability{Component: spec.ID, Fit: "detected"})
			case r.Kind == "unknown" && spec.RadioClaims && (r.DevPath != "" || len(r.ByIDPaths) > 0):
				r.SuitableFor = append(r.SuitableFor, haSuitability{Component: spec.ID, Fit: "possible"})
			}
		}
	}
	var devices []string
	for d := range owners {
		if !listed[d] {
			devices = append(devices, d)
		}
	}
	sort.Strings(devices)
	for _, d := range devices {
		for _, c := range owners[d] {
			n.RadioProbe.UnresolvedClaims = append(n.RadioProbe.UnresolvedClaims, ownerView(c))
		}
	}
}

func retryProbe(node string) haAction {
	return haAction{Kind: "retry_probe", Label: "Check again", Node: node}
}

func (b *haBuild) noNodeBlocker() haBlocker {
	return haBlocker{
		Code: "no_node", Severity: haBlocking,
		Message:  "No machine has joined Wildstone yet, so there is nowhere to run anything.",
		Evidence: []haEvidence{{Source: "node", Detail: "no recorded or connected nodes"}},
		Actions:  []haAction{{Kind: "pair_node", Label: "Add a machine"}},
	}
}

// nodeBlockers are what stands in the way on one node, independent of
// any component: connectivity, agent age, a probe that did not answer.
func (b *haBuild) nodeBlockers(n *haNode) []haBlocker {
	out := []haBlocker{}
	if n.Readiness == nil {
		return out
	}
	ev := haEvidence{Source: "node", Subject: n.ID, ObservedAt: n.LastSeenAt}
	if n.Readiness.Probe == "not_wired" {
		ev.State = "not_wired"
		return append(out, haBlocker{
			Code: "agent_plane_unavailable", Severity: haBlocking,
			Message:  "Keystone cannot reach any machine's agent right now, so nothing about " + n.ID + " can be checked.",
			Evidence: []haEvidence{ev}, Actions: []haAction{retryProbe(n.ID)},
		})
	}
	if !n.Connected {
		ev.State = "disconnected"
		return append(out, haBlocker{
			Code: "node_offline", Severity: haBlocking,
			Message:  n.ID + " is not connected, so its radios and prerequisites cannot be checked and nothing can change on it.",
			Evidence: []haEvidence{ev}, Actions: []haAction{retryProbe(n.ID)},
		})
	}
	for _, p := range []struct{ state, detail, what string }{
		{n.Readiness.Probe, n.Readiness.Detail, "readiness"},
		{n.RadioProbe.Probe, n.RadioProbe.Detail, "radio inventory"},
	} {
		switch p.state {
		case "agent_outdated":
			out = append(out, haBlocker{
				Code: "agent_outdated", Severity: haBlocking,
				Message: "The Wildstone agent on " + n.ID + " is too old to report its " + p.what + ".",
				Evidence: []haEvidence{{Source: "node", Subject: n.ID, State: "agent_outdated",
					Value: fmt.Sprintf("protocol minor %d", n.ProtocolMinor), Detail: p.detail}},
				Actions: []haAction{
					{Kind: "manual_step", Label: "Upgrade the agent", Node: n.ID,
						Instructions: []string{"Upgrade Wildstone on " + n.ID + " to the release this control plane runs, then check again."}},
					retryProbe(n.ID),
				},
			})
		case "failed":
			out = append(out, haBlocker{
				Code: "probe_failed", Severity: haBlocking,
				Message:  n.ID + " did not answer its " + p.what + " check.",
				Evidence: []haEvidence{{Source: "node", Subject: n.ID, State: "failed", Detail: p.detail}},
				Actions:  []haAction{retryProbe(n.ID)},
			})
		}
	}
	return out
}

// evaluate judges one component. The order of the state rules is the
// contract: unavailable, then an open plan, then (installed) offline or
// failed → degraded, a pending observable external step → external_step,
// any other blocking blocker → degraded, not yet observed → applying,
// else ready; (not installed) selected with a blocking blocker →
// blocked, selected → ready_to_propose, else not_selected.
func (b *haBuild) evaluate(spec haComponentSpec) *haComponent {
	c := &haComponent{
		ID: spec.ID, Title: spec.Title, Capability: spec.Capability, Topology: spec.Topology,
		Protocol: spec.Protocol, Requires: append([]string{}, b.requires[spec.ID]...), spec: spec,
		Packages: []string{}, Blockers: []haBlocker{}, Actions: []haAction{}, ExternalSteps: []haStep{},
		Selection: b.selection[spec.ID],
	}
	if c.Selection == "" {
		c.Selection = "none"
	}
	pkgs := b.catalogPackages(spec.Capability)
	for _, p := range pkgs {
		c.Packages = append(c.Packages, p.Metadata.Name)
	}
	installedPkg, anyInstalled := b.installedPackage(spec)
	c.pkg = installedPkg
	if c.pkg == nil && len(pkgs) > 0 {
		c.pkg = pkgs[0]
	}
	if c.pkg != nil {
		c.Package, c.Version = c.pkg.Metadata.Name, c.pkg.Metadata.Version
	}
	c.installed = b.isInstalled(spec)
	c.Node = b.componentNode(spec, installedPkg)

	// Mode components are a setting of their capability's package.
	if len(spec.ModeParams) > 0 {
		c.Modes = append([]string{}, spec.OnModes...)
		if d, ok := modeParam(spec, c.pkg); ok {
			if enum, found := strings.CutPrefix(d.Validate, "enum:"); found {
				c.Modes = strings.Split(enum, "|")
			}
			if anyInstalled {
				c.Mode = b.installedMode(spec, installedPkg)
			}
		} else {
			c.State = haUnavailable
			c.Reason = "The " + spec.Capability + " package in this catalog has no " + strings.ToLower(spec.Title) + " setting yet."
			if c.pkg == nil {
				c.Reason = "No package in this catalog provides " + spec.Capability + "."
			}
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "mode_unavailable", Severity: haBlocking, Message: c.Reason,
				Evidence: []haEvidence{{Source: "catalog", Subject: spec.Capability, Value: c.Package,
					Detail: "no param named " + strings.Join(spec.ModeParams, " or ")}},
				Actions: []haAction{},
			})
			return c
		}
	}
	if !anyInstalled && len(pkgs) == 0 {
		c.State = haUnavailable
		c.Reason = "No package in this catalog provides " + spec.Capability + " yet."
		c.Blockers = append(c.Blockers, haBlocker{
			Code: "package_unavailable", Severity: haBlocking, Message: c.Reason,
			Evidence: []haEvidence{{Source: "catalog", Subject: spec.Capability, State: "absent"}},
			Actions:  []haAction{},
		})
		return c
	}
	mode := c.Mode
	if !c.installed || mode == "" || !haContains(spec.OnModes, mode) {
		if h := b.modeHint[spec.ID]; h != "" {
			mode = h
		}
	}

	if installedPkg != nil && len(spec.ModeParams) == 0 {
		detail := b.s.describeCapability(installedPkg, b.current, b.health)
		detail = b.s.withVersion(installedPkg, detail, b.versions)
		c.InstalledVersion, c.Stale, c.Addresses = detail.InstalledVersion, detail.Stale, detail.Addresses
		for _, msg := range detail.Blockers {
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "service_blocker", Severity: haWarning, Message: msg,
				Evidence: []haEvidence{{Source: "catalog", Subject: installedPkg.Metadata.Name}}, Actions: []haAction{},
			})
		}
	}

	var awaitingObservation, offline bool
	var n *haNode
	if c.Node != "" {
		n = b.nodes[c.Node]
	}
	{
		// Everything here runs on, or writes its files to, a node.
		switch {
		case c.Node == "" && len(b.nodeOrder) == 0:
			c.Blockers = append(c.Blockers, b.noNodeBlocker())
		case c.Node == "":
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "choose_node", Severity: haBlocking,
				Message:  "Choose which machine this runs on.",
				Evidence: []haEvidence{{Source: "node", Value: strings.Join(b.nodeOrder, ", ")}},
				Actions:  []haAction{{Kind: "choose_node", Label: "Choose a machine", Component: spec.ID}},
			})
		case n == nil:
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "node_offline", Severity: haBlocking,
				Message:  c.Node + " has never joined Wildstone.",
				Evidence: []haEvidence{{Source: "node", Subject: c.Node, State: "unknown"}},
				Actions:  []haAction{{Kind: "choose_node", Label: "Choose another machine", Component: spec.ID}},
			})
			offline = true
		default:
			needsProbe := len(spec.Checks) > 0 || spec.RadioClaims
			for _, nb := range n.Blockers {
				if nb.Code == "node_offline" || nb.Code == "agent_plane_unavailable" {
					offline = offline || nb.Code == "node_offline"
					c.Blockers = append(c.Blockers, nb)
				} else if needsProbe {
					c.Blockers = append(c.Blockers, nb)
				}
			}
		}
	}

	c.Blockers = append(c.Blockers, b.checkBlockers(spec, mode, c, n)...)
	if spec.RadioClaims {
		c.Blockers = append(c.Blockers, b.radioBlockers(spec, c, n)...)
	}
	for _, dep := range c.Requires {
		d := b.comps[dep]
		if d == nil || d.State == haReady || d.State == haExternalStep {
			continue
		}
		code, msg := "dependency_not_ready", fmt.Sprintf("Needs %s first (it is %s).", d.Title, strings.ReplaceAll(string(d.State), "_", " "))
		if d.State == haUnavailable {
			code, msg = "dependency_unavailable", fmt.Sprintf("Needs %s, which this catalog cannot provide.", d.Title)
		}
		c.Blockers = append(c.Blockers, haBlocker{
			Code: code, Severity: haBlocking, Message: msg,
			Evidence: []haEvidence{{Source: "component", Subject: dep, State: string(d.State), Detail: d.Reason}},
			Actions:  []haAction{{Kind: "resolve_dependency", Label: "Set up " + d.Title, Component: dep}},
		})
	}

	// A Wildstone-run Home Assistant this component feeds must carry
	// the settings it needs; offered as a reconfigure of that package.
	if hass := b.comps[haHomeAssistant]; hass != nil && hass.installed && hass.pkg != nil && c.Selection != "none" {
		want := map[string]string{}
		for name, v := range spec.ManagedHAParams {
			for _, d := range hass.pkg.Params {
				if d.Name != name {
					continue
				}
				have := b.params[hass.pkg.Metadata.Name][name]
				if have == "" {
					have = d.Default
				}
				if have != v {
					want[name] = v
				}
			}
		}
		if len(want) > 0 {
			var names []string
			for n := range want {
				names = append(names, n+"="+want[n])
			}
			sort.Strings(names)
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "ha_setting_needed", Severity: haWarning,
				Message:  fmt.Sprintf("Home Assistant needs %s to use %s.", strings.Join(names, ", "), spec.Title),
				Evidence: []haEvidence{{Source: "plan", Subject: hass.pkg.Metadata.Name, Value: strings.Join(names, ", "), Detail: "installed params differ"}},
				Actions: []haAction{{Kind: "reconfigure_package", Label: "Preview the Home Assistant change", Component: haHomeAssistant,
					Package: hass.pkg.Metadata.Name, Capability: hass.Capability, Node: hass.Node, Params: want}},
			})
		}
	}

	// Installed: what is actually running, and whether it answers.
	var failed *haPlan
	if c.installed && len(spec.ModeParams) == 0 {
		c.Resources, awaitingObservation = b.resources(spec, installedPkg)
		for _, r := range c.Resources {
			if !r.Observed || r.Healthy || r.AwaitingApproval {
				if r.AppHealth != "unhealthy" {
					continue
				}
			}
			code, msg := "unhealthy", fmt.Sprintf("%s/%s is not in the state Wildstone asked for.", r.Kind, r.Name)
			if r.Healthy && r.AppHealth == "unhealthy" {
				code, msg = "app_unhealthy", fmt.Sprintf("%s/%s is running but reports itself unhealthy.", r.Kind, r.Name)
			}
			c.Blockers = append(c.Blockers, haBlocker{
				Code: code, Severity: haBlocking, Message: msg,
				Evidence: []haEvidence{{Source: "observed", Subject: r.Kind + "/" + r.Name, State: healthWord(r),
					Value: r.AppHealth, Detail: r.Detail, ObservedAt: r.ObservedAt}},
				Actions: []haAction{
					{Kind: "open_service", Label: "Open the service", Component: spec.ID, Capability: spec.Capability, Package: c.Package},
				},
			})
		}
		failed = b.failedPlan(installedPkg)
		if failed != nil {
			c.FailedPlan = planRef(failed)
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "plan_failed", Severity: haBlocking,
				Message: "The last change to " + c.Title + " did not finish applying.",
				Evidence: []haEvidence{{Source: "plan", Subject: failed.rec.ID, State: string(failed.rec.State),
					Detail: failed.rec.Summary, ObservedAt: timePtr(failed.rec.ResolvedAt)}},
				Actions: []haAction{
					{Kind: "open_plan", Label: "Open the plan", PlanID: failed.rec.ID},
					{Kind: "open_service", Label: "Open the service", Component: spec.ID, Capability: spec.Capability, Package: c.Package},
				},
			})
		}
		c.Backup = b.backup(spec, installedPkg)
		if c.Backup != nil && len(c.Backup.Uncovered) > 0 {
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "backup_missing", Severity: haWarning,
				Message:  c.Title + " keeps data no backup plan covers: " + strings.Join(c.Backup.Uncovered, ", ") + ".",
				Evidence: []haEvidence{{Source: "backup", Subject: strings.Join(c.Backup.Uncovered, ","), State: "uncovered"}},
				Actions:  []haAction{{Kind: "open_backups", Label: "Open backups"}},
			})
		} else if c.Backup != nil && c.Backup.LastRunStatus == string(runtimedb.BackupRunFailed) {
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "backup_failed", Severity: haWarning,
				Message:  "The last backup of " + c.Title + " failed.",
				Evidence: []haEvidence{{Source: "backup", Subject: strings.Join(c.Backup.Plans, ","), State: "failed", ObservedAt: c.Backup.LastRunFinishedAt}},
				Actions:  []haAction{{Kind: "open_backups", Label: "Open backups"}},
			})
		}
		if c.Stale {
			c.Blockers = append(c.Blockers, haBlocker{
				Code: "stale_package", Severity: haWarning,
				Message:  fmt.Sprintf("%s runs %s %s; the catalog now has %s.", c.Title, c.Package, c.InstalledVersion, c.Version),
				Evidence: []haEvidence{{Source: "catalog", Subject: c.Package, Value: c.Version, Detail: "installed " + c.InstalledVersion}},
				Actions:  []haAction{{Kind: "upgrade_package", Label: "Preview the upgrade", Package: c.Package, Capability: spec.Capability, Node: c.Node}},
			})
		}
	}

	// External steps, for anything selected.
	pendingStep := false
	if c.Selection != "none" {
		facts := b.facts(c.Node)
		for _, st := range spec.Steps {
			if st.Mode != "" && st.Mode != mode {
				continue
			}
			es := haStep{ID: st.ID, Title: st.Title, Instructions: st.Instructions(facts), Status: "unverified"}
			if st.Verify != nil {
				es.Status, es.Evidence = st.Verify(b, c)
			}
			if es.Status == "pending" {
				pendingStep = pendingStep || c.installed
				if st.Action != nil {
					es.Action = st.Action(c)
				}
			}
			c.ExternalSteps = append(c.ExternalSteps, es)
		}
	}

	blocking := false
	for _, bl := range c.Blockers {
		if bl.Severity == haBlocking {
			blocking = true
		}
	}
	openService := haAction{Kind: "open_service", Label: "Open the service", Component: spec.ID, Capability: spec.Capability, Package: c.Package}
	if p := b.pendingFor(spec); p != nil {
		c.PendingPlan = planRef(p)
		c.Actions = append(c.Actions, haAction{Kind: "open_plan", Label: "Review the plan", PlanID: p.rec.ID})
		if p.rec.State == runtimedb.PlanProposed {
			c.State, c.Reason = haAwaitingApproval, "A plan is waiting for your approval."
		} else {
			c.State, c.Reason = haApplying, "An approved plan is being applied."
		}
		return c
	}
	switch {
	case c.installed:
		c.Actions = append(c.Actions, openService)
		switch {
		case offline:
			c.State, c.Reason = haDegraded, c.Node+" is not connected."
		case failed != nil:
			c.State, c.Reason = haDegraded, "The last change did not finish applying."
		case pendingStep:
			c.State, c.Reason = haExternalStep, "Installed; a step outside Wildstone is still needed."
			for _, es := range c.ExternalSteps {
				if es.Action != nil {
					c.Actions = append(c.Actions, *es.Action)
				}
			}
		case blocking:
			c.State, c.Reason = haDegraded, firstBlocking(c.Blockers)
		case awaitingObservation:
			c.State, c.Reason = haApplying, "Installed; waiting for the first healthy observation."
		default:
			c.State, c.Reason = haReady, "Installed and healthy."
		}
		if c.Stale {
			c.Actions = append(c.Actions, haAction{Kind: "upgrade_package", Label: "Preview the upgrade", Package: c.Package, Capability: spec.Capability, Node: c.Node})
		}
	case c.Selection == "none":
		c.State, c.Reason = haNotSelected, "Not chosen."
	case blocking:
		c.State, c.Reason = haBlocked, firstBlocking(c.Blockers)
	default:
		c.State, c.Reason = haReadyToPropose, "Ready to preview a plan."
		c.Actions = append(c.Actions, b.proposeAction(spec, c, mode))
	}
	return c
}

func firstBlocking(bs []haBlocker) string {
	for _, b := range bs {
		if b.Severity == haBlocking {
			return b.Message
		}
	}
	return ""
}

func healthWord(r haResource) string {
	switch {
	case !r.Observed:
		return "unobserved"
	case r.AwaitingApproval:
		return "awaiting_approval"
	case r.Healthy:
		return "healthy"
	}
	return "unhealthy"
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func planRef(p *haPlan) *haPlanRef {
	ref := &haPlanRef{ID: p.rec.ID, State: string(p.rec.State), Summary: p.rec.Summary}
	if p.stored.Origin != nil {
		ref.Package = p.stored.Origin.Package
	}
	return ref
}

// proposeAction is the action a ready_to_propose component offers: an
// install, or for a mode component a reconfigure of its package.
func (b *haBuild) proposeAction(spec haComponentSpec, c *haComponent, mode string) haAction {
	if len(spec.ModeParams) > 0 {
		d, _ := modeParam(spec, c.pkg)
		a := haAction{Kind: "reconfigure_package", Label: "Preview turning on " + strings.ToLower(spec.Title),
			Component: spec.ID, Package: c.Package, Capability: spec.Capability, Node: c.Node}
		if mode != "" {
			a.Params = map[string]string{d.Name: mode}
		}
		return a
	}
	return haAction{Kind: "install_package", Label: "Preview installing " + spec.Title, Component: spec.ID,
		Package: c.Package, Capability: spec.Capability, Node: c.Node, RadioIDs: c.RadioCandidates}
}

// checkBlockers turns this component's readiness needs into blockers.
// When the mode is not yet known, mode-specific needs are warnings: the
// household can still pick the mode that does not need them.
func (b *haBuild) checkBlockers(spec haComponentSpec, mode string, c *haComponent, n *haNode) []haBlocker {
	var out []haBlocker
	if n == nil || n.Readiness == nil || n.Readiness.Probe != "ok" {
		return out
	}
	if !n.Readiness.PlatformSupported {
		if len(spec.Checks) == 0 {
			return out
		}
		return append(out, haBlocker{
			Code: "platform_unsupported", Severity: haBlocking,
			Message:  n.ID + " cannot report home-automation prerequisites (not a Linux host).",
			Evidence: []haEvidence{{Source: "readiness", Subject: n.ID, State: "unsupported"}},
			Actions:  []haAction{{Kind: "choose_node", Label: "Choose another machine", Component: spec.ID}},
		})
	}
	defaultIfaces := map[string]bool{}
	for _, i := range n.Readiness.Interfaces {
		if i.DefaultRoute {
			defaultIfaces[i.Name] = true
		}
	}
	for _, need := range spec.Checks {
		severity := haBlocking
		if need.Mode != "" {
			switch {
			case mode == "" || !haContains(spec.OnModes, mode):
				severity = haWarning
			case mode != need.Mode:
				continue
			}
		}
		var rows []readinessCheckView
		for _, row := range n.Readiness.Checks {
			if row.Check != need.Check {
				continue
			}
			if interfaceScopedChecks[need.Check] && len(defaultIfaces) > 0 && !defaultIfaces[row.Subject] {
				continue
			}
			rows = append(rows, row)
		}
		title := readinessCheckTitle[need.Check]
		if len(rows) == 0 {
			out = append(out, haBlocker{
				Code: need.Check + "_unknown", Severity: haWarning,
				Message:  n.ID + " did not report " + title + ".",
				Evidence: []haEvidence{{Source: "readiness", Subject: need.Check, State: "unknown"}},
				Actions:  []haAction{retryProbe(n.ID)},
			})
			continue
		}
		for _, row := range rows {
			if row.State == "ready" {
				continue
			}
			sev := severity
			if row.State == "unknown" {
				sev = haWarning
			}
			msg := fmt.Sprintf("%s: %s is %s on %s.", spec.Title, title, row.State, n.ID)
			if row.Subject != "" && interfaceScopedChecks[need.Check] {
				msg = fmt.Sprintf("%s: %s is %s on %s (%s).", spec.Title, title, row.State, n.ID, row.Subject)
			}
			actions := []haAction{}
			if row.State == "missing" {
				subject := row.Subject
				if !interfaceScopedChecks[need.Check] {
					subject = firstDefault(defaultIfaces)
				}
				actions = append(actions, haAction{Kind: "manual_step", Label: "How to fix this", Node: n.ID,
					Component: spec.ID, Instructions: readinessRemedy(need.Check, n.ID, subject)})
			}
			if row.State == "unsupported" {
				actions = append(actions, haAction{Kind: "choose_node", Label: "Choose another machine", Component: spec.ID})
			}
			actions = append(actions, retryProbe(n.ID))
			out = append(out, haBlocker{
				Code: need.Check + "_" + row.State, Severity: sev, Message: msg,
				Evidence: []haEvidence{{Source: "readiness", Subject: row.Subject, State: row.State, Value: row.Value, Detail: row.Detail}},
				Actions:  actions,
			})
		}
	}
	return out
}

func firstDefault(ifaces map[string]bool) string {
	var names []string
	for n := range ifaces {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// radioBlockers judges a radio-driven component against its node's
// inventory: the radio it holds (installed), or the radios it could take
// (not installed). Never guesses from names: suitability is the
// inventory's kind, or "possible" for an unidentified serial radio.
func (b *haBuild) radioBlockers(spec haComponentSpec, c *haComponent, n *haNode) []haBlocker {
	var out []haBlocker
	if n == nil || n.RadioProbe == nil {
		return out
	}
	known := n.RadioProbe.Probe == "ok"
	owners := plan.RadioOwners(b.current, n.ID, n.inventory)
	owner := "capability/" + spec.Capability
	if c.installed {
		var devices []string
		for d := range owners {
			devices = append(devices, d)
		}
		sort.Strings(devices)
		for _, d := range devices {
			claims := owners[d]
			var mine *plan.DeviceClaim
			for i := range claims {
				if claims[i].Owner == owner {
					mine = &claims[i]
				}
			}
			if mine == nil {
				continue
			}
			c.Radios = append(c.Radios, ownerView(*mine))
			if known && mine.RadioID == "" {
				out = append(out, haBlocker{
					Code: "radio_missing", Severity: haBlocking,
					Message: fmt.Sprintf("The radio %s was given (%s) is not plugged into %s.", spec.Title, mine.Source, n.ID),
					Evidence: []haEvidence{{Source: "radios", Subject: mine.Source, State: "absent",
						Detail: "not in the node's radio inventory"}},
					Actions: []haAction{
						{Kind: "select_radio", Label: "Choose a replacement radio", Component: spec.ID, Node: n.ID, RadioIDs: b.candidates(spec, n, owners, true)},
						retryProbe(n.ID),
					},
				})
			}
			if len(claims) > 1 {
				ev := []haEvidence{}
				actions := []haAction{}
				for _, cl := range claims {
					ev = append(ev, haEvidence{Source: "statefile", Subject: "Container/" + cl.Container, Value: cl.Source, Detail: cl.Package})
					if cl.Owner != owner {
						actions = append(actions, haAction{Kind: "open_service", Label: "Open " + cl.Container,
							Component: componentForOwner(cl.Owner), Capability: strings.TrimPrefix(cl.Owner, "capability/"), Package: cl.Package})
					}
				}
				actions = append(actions, haAction{Kind: "select_radio", Label: "Choose another radio", Component: spec.ID, Node: n.ID,
					RadioIDs: b.candidates(spec, n, owners, true)})
				out = append(out, haBlocker{
					Code: "radio_conflict", Severity: haBlocking,
					Message:  fmt.Sprintf("%s shares its radio with another service; a radio answers one program at a time.", spec.Title),
					Evidence: ev, Actions: actions,
				})
			}
		}
		return out
	}
	if !known {
		return out
	}
	c.RadioCandidates = b.candidates(spec, n, owners, true)
	if len(c.RadioCandidates) > 0 {
		return out
	}
	severity := haWarning
	if spec.RadioRequired {
		severity = haBlocking
	}
	if owned := b.candidates(spec, n, owners, false); len(owned) > 0 {
		ev := []haEvidence{}
		actions := []haAction{}
		for _, id := range owned {
			for _, cl := range owners[id] {
				ev = append(ev, haEvidence{Source: "radios", Subject: id, State: "owned", Value: "Container/" + cl.Container, Detail: cl.Package})
				actions = append(actions, haAction{Kind: "open_service", Label: "Open " + cl.Container,
					Component: componentForOwner(cl.Owner), Capability: strings.TrimPrefix(cl.Owner, "capability/"), Package: cl.Package})
			}
		}
		actions = append(actions, retryProbe(n.ID))
		return append(out, haBlocker{
			Code: "radio_in_use", Severity: severity,
			Message:  fmt.Sprintf("Every %s radio on %s is already used by another service.", spec.Title, n.ID),
			Evidence: ev, Actions: actions,
		})
	}
	msg := fmt.Sprintf("No %s radio is plugged into %s.", spec.Title, n.ID)
	if !spec.RadioRequired {
		msg += " A network coordinator can be used instead."
	}
	return append(out, haBlocker{
		Code: "no_radio", Severity: severity, Message: msg,
		Evidence: []haEvidence{{Source: "radios", Subject: n.ID, State: "none",
			Value: fmt.Sprintf("%d radio(s) inventoried", len(n.RadioProbe.Radios))}},
		Actions: []haAction{
			{Kind: "manual_step", Label: "Plug in a radio", Node: n.ID, Component: spec.ID,
				Instructions: []string{fmt.Sprintf("Plug a %s adapter into %s (a USB extension cable away from the machine helps reception), then check again.", spec.Title, n.ID)}},
			retryProbe(n.ID),
		},
	})
}

// candidates are radios suitable for a component: free ones when free is
// true, else the suitable ones someone else holds.
func (b *haBuild) candidates(spec haComponentSpec, n *haNode, owners map[string][]plan.DeviceClaim, free bool) []string {
	var detected, possible []string
	for _, r := range n.RadioProbe.Radios {
		if (len(owners[r.ID]) == 0) != free {
			continue
		}
		for _, s := range r.SuitableFor {
			if s.Component != spec.ID {
				continue
			}
			if s.Fit == "detected" {
				detected = append(detected, r.ID)
			} else {
				possible = append(possible, r.ID)
			}
		}
	}
	return append(detected, possible...)
}

// resources are this component's desired resources joined with their
// observations; awaiting reports whether anything that must be observed
// has not been yet.
func (b *haBuild) resources(spec haComponentSpec, pkg *pkgspec.Package) ([]haResource, bool) {
	owner := "capability/" + spec.Capability
	var out []haResource
	awaiting := false
	for _, r := range b.current {
		if r.Metadata.Owner != owner {
			continue
		}
		if pkg != nil && r.Metadata.Package != "" && r.Metadata.Package != pkg.Metadata.Name {
			continue
		}
		hr := haResource{Kind: string(r.Kind), Name: r.Metadata.Name, Node: r.Metadata.Node}
		if o, ok := b.health[string(r.Kind)+"/"+r.Metadata.Name]; ok {
			at := o.UpdatedAt
			hr.Observed, hr.Healthy, hr.AwaitingApproval, hr.AppHealth, hr.Detail, hr.ObservedAt =
				true, o.Healthy, o.AwaitingApproval, o.AppHealth, o.Detail, &at
		}
		mustObserve := r.Kind == resource.KindContainer || r.Kind == resource.KindConfigFile
		if mustObserve && (!hr.Observed || hr.AwaitingApproval || hr.AppHealth == "starting") {
			awaiting = true
		}
		out = append(out, hr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, awaiting
}

// failedPlan is the newest attempt at this package when that attempt
// failed. An older failure a later plan applied over is history, not a
// blocker.
func (b *haBuild) failedPlan(pkg *pkgspec.Package) *haPlan {
	if pkg == nil {
		return nil
	}
	recs, err := b.s.DB.PlansMentioning(`"package":"`+pkg.Metadata.Name+`"`, 50)
	if err != nil {
		return nil
	}
	for _, rec := range recs {
		var sp plan.StoredProposal
		if json.Unmarshal([]byte(rec.PlanJSON), &sp) != nil || sp.Origin == nil || sp.Origin.Package != pkg.Metadata.Name {
			continue
		}
		switch rec.State {
		case runtimedb.PlanFailed:
			return &haPlan{rec: rec, stored: sp}
		case runtimedb.PlanApplied, runtimedb.PlanApplying, runtimedb.PlanApproved:
			return nil
		}
	}
	return nil
}

// backup is a component's backup coverage: which of its volumes a
// BackupPlan covers, and how the newest run of those plans went. A
// volume that is itself a backup repository is not data to cover.
func (b *haBuild) backup(spec haComponentSpec, pkg *pkgspec.Package) *haBackup {
	owner := "capability/" + spec.Capability
	repos := map[string]bool{}
	covering := map[string][]string{}
	for _, r := range b.current {
		if r.Kind == resource.KindBackupPlan && r.BackupPlan != nil {
			repos[r.BackupPlan.Repository] = true
			for _, v := range r.BackupPlan.Volumes {
				covering[v] = append(covering[v], r.Metadata.Name)
			}
		}
	}
	bk := &haBackup{Plans: []string{}, Volumes: []string{}, Uncovered: []string{}}
	plans := map[string]bool{}
	for _, r := range b.current {
		if r.Kind != resource.KindVolume || r.Metadata.Owner != owner || repos[r.Metadata.Name] {
			continue
		}
		if pkg != nil && r.Metadata.Package != "" && r.Metadata.Package != pkg.Metadata.Name {
			continue
		}
		bk.Volumes = append(bk.Volumes, r.Metadata.Name)
		if len(covering[r.Metadata.Name]) == 0 {
			bk.Uncovered = append(bk.Uncovered, r.Metadata.Name)
		}
		for _, p := range covering[r.Metadata.Name] {
			plans[p] = true
		}
	}
	if len(bk.Volumes) == 0 {
		return nil
	}
	for p := range plans {
		bk.Plans = append(bk.Plans, p)
	}
	sort.Strings(bk.Plans)
	sort.Strings(bk.Volumes)
	sort.Strings(bk.Uncovered)
	for _, run := range b.backupRuns { // newest first
		if !plans[run.PlanName] || run.RunKind != runtimedb.BackupRunBackup || run.FinishedAt.IsZero() {
			continue
		}
		if bk.LastRunStatus == "" {
			bk.LastRunStatus, bk.LastRunFinishedAt = string(run.Status), timePtr(run.FinishedAt)
		}
		if run.Status == runtimedb.BackupRunSucceeded {
			bk.LastSuccessAt = timePtr(run.FinishedAt)
			break
		}
	}
	return bk
}

// facts are the values external-step instructions name for a node.
func (b *haBuild) facts(node string) haFacts {
	f := haFacts{Node: node}
	if n, ok := b.nodes[node]; ok {
		f.HostAddress = n.HostAddress
	}
	for pkg, params := range b.params {
		p, err := b.catalogGet(pkg)
		if err != nil {
			continue
		}
		switch p.Metadata.Capability {
		case "mqtt":
			f.MQTTHost, f.MQTTPort = params["listen_addr"], params["listen_port"]
		case "zwave":
			f.ZWaveHost, f.ZWavePort = params["listen_addr"], params["ws_port"]
		}
	}
	return f
}

func (b *haBuild) catalogGet(name string) (*pkgspec.Package, error) {
	if b.s.Catalog == nil {
		return nil, fmt.Errorf("no catalog")
	}
	return b.s.Catalog.Get(name, "")
}

// verifyPublisher: the external adapter's credential exists when its
// publisher principal does and is enabled.
func verifyPublisher(b *haBuild, c *haComponent) (string, []haEvidence) {
	name := publisherPrincipalPrefix + c.Capability
	p, err := b.s.DB.GetPrincipal(name)
	switch {
	case err != nil:
		return "pending", []haEvidence{{Source: "principal", Subject: name, State: "absent"}}
	case p.Disabled || p.Role != runtimedb.RolePublisher:
		return "pending", []haEvidence{{Source: "principal", Subject: name, State: "disabled", Value: string(p.Role)}}
	}
	return "done", []haEvidence{{Source: "principal", Subject: name, State: "present", ObservedAt: timePtr(p.CreatedAt)}}
}

// topology is what is installed or pending.
func (b *haBuild) topology() string {
	has := func(id string) bool {
		c := b.comps[id]
		return c != nil && (c.Selection == "installed" || c.Selection == "pending")
	}
	switch m, e := has(haHomeAssistant), has(haExternalHA); {
	case m && e:
		return "both"
	case m:
		return haTopologyManaged
	case e:
		return haTopologyExternal
	}
	return "none"
}
