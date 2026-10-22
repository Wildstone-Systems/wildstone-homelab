package api

// POST /api/v1/home-automation/preview and /propose (#447): the next safe
// package step towards a household's home-automation goals.
//
// Both evaluate exactly what GET /api/v1/home-automation evaluates (the
// same haBuild, the same component table, the same states) and add only
// the ORDER: which component's package plan comes next, and what the rest
// of the sequence is. The step itself is a catalog package expanded and
// filed through proposeCapability — the capability endpoint's own path,
// and so the one proposal pipeline with its validation, policy, duplicate
// radio refusal and audit. There is no second plan format and nothing is
// approved here.
//
// Nothing about a household's choices is stored. Every call recomputes
// from installed resources, plan records, observations and a fresh probe,
// so a reload, a Console closed mid-way or a restarted control plane
// resumes from what is true rather than from what a wizard remembered.
//
// ONE PLAN AT A TIME. While any selected component has a plan open, the
// next step is waiting on that plan; while a component's install is not
// yet observed healthy, the next step is waiting on that. A dependent is
// never proposed before its provider is ready — and neither is anything
// else, since two open package plans would supersede each other at
// approval anyway.
//
// Home-Assistant-side steps Wildstone cannot observe never hold the
// sequence: once every package is in, they are returned as one stable
// external_step (the same answer on every call, filing nothing) until the
// caller says it has done them (confirmedSteps). Nothing loops.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
)

// maxHARequestBytes bounds an orchestration request: a handful of ids.
const maxHARequestBytes = 64 << 10

// Orchestration step actions. Closed vocabulary.
const (
	haStepInstall          = "install_package"
	haStepReconfigure      = "reconfigure_package"
	haStepAwaitApproval    = "await_approval"
	haStepAwaitApply       = "await_apply"
	haStepAwaitObservation = "await_observation"
	haStepResolve          = "resolve_blockers"
	haStepExternal         = "external_step"
	haStepDone             = "done"
)

// haOrchRequest is the household's goals for one call.
type haOrchRequest struct {
	Topology string   `json:"topology"`
	Node     string   `json:"node,omitempty"`
	Select   []string `json:"select,omitempty"`
	// Radios assigns a radio (a ListRadios id) to a radio-claiming
	// component. An empty id asks for no radio (Zigbee's network
	// coordinator), refused for a component that requires one. Only
	// components not yet installed or pending are judged; a choice for an
	// installed one is ignored.
	Radios map[string]string `json:"radios,omitempty"`
	// Modes picks a mode component's mode (bluetooth: host-bluez|proxy).
	Modes map[string]string `json:"modes,omitempty"`
	// Options set further params of a component's package — a closed
	// choice such as a coordinator adapter, or a listen address other
	// than the one the node reports. The node, radio, mode and
	// dependency-driven params are the orchestrator's and refused here.
	Options map[string]map[string]string `json:"options,omitempty"`
	// ConfirmedSteps are "<component>/<step id>" external steps the
	// household says it has done. Only honoured for steps Wildstone
	// cannot observe; a step it can observe is judged by observation.
	ConfirmedSteps []string `json:"confirmedSteps,omitempty"`
	// ExpectStep (propose only) is the step id the caller previewed. When
	// the next step is no longer that one, nothing is filed and the fresh
	// answer comes back with 409.
	ExpectStep string `json:"expectStep,omitempty"`
}

// haOrchExternal is one external step in the checklist.
type haOrchExternal struct {
	Component string `json:"component"`
	haStep
	// Due: the component is installed, so the step can be done now.
	Due       bool `json:"due"`
	Confirmed bool `json:"confirmed"`
}

// haOrchStep is one component's place in the sequence, or the next step.
type haOrchStep struct {
	// ID is stable for as long as the step is the same step: a preview's
	// id is what a propose passes as expectStep.
	ID        string  `json:"id"`
	Component string  `json:"component,omitempty"`
	Title     string  `json:"title"`
	Action    string  `json:"action"`
	Status    string  `json:"status"` // done | current | upcoming
	State     haState `json:"state,omitempty"`
	Reason    string  `json:"reason"`
	Package   string  `json:"package,omitempty"`
	Version   string  `json:"version,omitempty"`
	Node      string  `json:"node,omitempty"`
	// Params are the full params the package plan is (or would be)
	// expanded with — installed ones carried forward — for the current
	// package step only.
	Params        map[string]string `json:"params,omitempty"`
	RadioID       string            `json:"radioId,omitempty"`
	PlanID        string            `json:"planId,omitempty"`
	Blockers      []haBlocker       `json:"blockers"`
	Actions       []haAction        `json:"actions"`
	ExternalSteps []haOrchExternal  `json:"externalSteps,omitempty"`

	pkg *pkgspec.Package
}

type haOrchestration struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Topology    string    `json:"topology"`
	Node        string    `json:"node,omitempty"`
	// Complete: every package is in and healthy and every external step
	// is observed done or confirmed.
	Complete bool         `json:"complete"`
	Next     haOrchStep   `json:"next"`
	Sequence []haOrchStep `json:"sequence"`
	// ExternalSteps are every selected component's steps outside
	// Wildstone, in sequence order.
	ExternalSteps []haOrchExternal `json:"externalSteps"`
	// Rejected are choices refused (stale or conflicting radios); set
	// with a 409 whose body is still the full, fresh answer.
	Rejected []haBlocker `json:"rejected,omitempty"`
	// Preview is the dry-run plan for next (preview only): the capability
	// preview's own body.
	Preview any `json:"preview,omitempty"`
	// Plan is the plan record propose filed.
	Plan any `json:"plan,omitempty"`
}

func (s *Server) homeAutomationPreview(r *http.Request, id principal.Identity) (any, int, error) {
	return s.homeAutomationOrchestrate(r, id, false)
}

func (s *Server) homeAutomationPropose(r *http.Request, id principal.Identity) (any, int, error) {
	return s.homeAutomationOrchestrate(r, id, true)
}

func (s *Server) homeAutomationOrchestrate(r *http.Request, id principal.Identity, propose bool) (any, int, error) {
	if s.Catalog == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no catalog is configured (keystone -catalog-dir)")
	}
	var req haOrchRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxHARequestBytes)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	goals, err := req.goals()
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	o, code, err := s.orchestrateOnce(r, req, goals)
	if o == nil {
		return nil, code, err // a typed nil would be written as a body
	}
	if err != nil {
		return o, code, err
	}
	next := o.Next
	packageStep := next.Action == haStepInstall || next.Action == haStepReconfigure

	if !propose {
		if packageStep {
			body, st, err := s.proposeCapability(id, next.pkg, next.Params, true)
			if err != nil {
				o.refuse(st, err)
			} else {
				o.Preview = body
			}
		}
		return o, http.StatusOK, nil
	}

	if req.ExpectStep != "" && req.ExpectStep != next.ID {
		return o, http.StatusConflict, fmt.Errorf("the next step is now %s, not %s: nothing was proposed", next.ID, req.ExpectStep)
	}
	if !packageStep {
		return o, http.StatusConflict, fmt.Errorf("nothing to propose: %s", next.Reason)
	}
	body, st, err := s.proposeCapability(id, next.pkg, next.Params, false)
	if err != nil {
		o.refuse(st, err)
		return o, st, err
	}
	// Answer from the state the plan just created, not the one before it.
	// The plan exists whatever this says; a re-evaluation that cannot
	// answer still reports it.
	after, _, _ := s.orchestrateOnce(r, req, goals)
	if after == nil {
		return body, http.StatusCreated, nil
	}
	after.Plan = body
	return after, http.StatusCreated, nil
}

// orchestrateOnce evaluates and orders. A 409 for refused choices comes
// back with the full answer.
func (s *Server) orchestrateOnce(r *http.Request, req haOrchRequest, goals haGoals) (*haOrchestration, int, error) {
	b, view, code, err := s.evaluateHomeAutomation(r.Context(), goals)
	if err != nil {
		return nil, code, err
	}
	radios, rejected, err := b.checkChoices(req)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	o := b.orchestrate(req, radios)
	o.GeneratedAt = view.GeneratedAt
	if len(rejected) > 0 {
		o.Rejected = rejected
		msgs := make([]string, 0, len(rejected))
		for _, bl := range rejected {
			msgs = append(msgs, bl.Message)
		}
		return o, http.StatusConflict, fmt.Errorf("%s", strings.Join(msgs, " "))
	}
	return o, http.StatusOK, nil
}

// goals validates the request's shape and turns it into the evaluation's
// goals. Topology is required: the order depends on it.
func (req haOrchRequest) goals() (haGoals, error) {
	if req.Topology != haTopologyManaged && req.Topology != haTopologyExternal {
		return haGoals{}, fmt.Errorf("topology must be managed or external")
	}
	g := haGoals{topology: req.Topology, node: req.Node, requested: map[string]bool{}, modeHint: map[string]string{}}
	for _, id := range req.Select {
		spec, ok := haSpec(id)
		if !ok {
			return haGoals{}, fmt.Errorf("unknown component %q in select", id)
		}
		if spec.Topology != haTopologyShared && spec.Topology != req.Topology {
			return haGoals{}, fmt.Errorf("%s is part of a %s setup, not %s", id, spec.Topology, req.Topology)
		}
		g.requested[id] = true
	}
	for id, m := range req.Modes {
		g.modeHint[id] = m
	}
	for _, key := range req.ConfirmedSteps {
		if comp, step, ok := strings.Cut(key, "/"); !ok || comp == "" || step == "" {
			return haGoals{}, fmt.Errorf("confirmed step %q is not <component>/<step>", key)
		}
	}
	return g, nil
}

// reservedParam reports whether the orchestrator, not the caller, owns a
// param of this component's package.
func reservedParam(spec haComponentSpec, d pkgspec.ParamDecl) bool {
	if d.Source == "node" || d.Name == pkgspec.InstanceParam || haContains(haRadioParams, d.Name) || haContains(spec.ModeParams, d.Name) {
		return true
	}
	for _, other := range haComponents {
		if _, ok := other.ManagedHAParams[d.Name]; ok {
			return true
		}
	}
	return false
}

// checkChoices validates options (a 400) and radios. A radio that is no
// longer a free, suitable radio for its component on its node, or one
// chosen for two components, is rejected: the evaluation continues
// without it and the caller gets a 409 carrying the fresh answer.
func (b *haBuild) checkChoices(req haOrchRequest) (map[string]string, []haBlocker, error) {
	for comp, opts := range req.Options {
		spec, ok := haSpec(comp)
		if !ok {
			return nil, nil, fmt.Errorf("unknown component %q in options", comp)
		}
		c := b.comps[comp]
		for name := range opts {
			var decl *pkgspec.ParamDecl
			if c.pkg != nil {
				for i := range c.pkg.Params {
					if c.pkg.Params[i].Name == name {
						decl = &c.pkg.Params[i]
					}
				}
			}
			if decl == nil {
				return nil, nil, fmt.Errorf("%s: package %s has no param %q", comp, c.Package, name)
			}
			if reservedParam(spec, *decl) {
				return nil, nil, fmt.Errorf("%s: %s is chosen by the setup itself (node, radio, mode or a dependency), not as an option", comp, name)
			}
		}
	}
	radios := map[string]string{}
	var rejected []haBlocker
	var comps []string
	for comp := range req.Radios {
		comps = append(comps, comp)
	}
	sort.Strings(comps)
	taken := map[string]string{}
	for _, comp := range comps {
		rid := req.Radios[comp]
		spec, ok := haSpec(comp)
		if !ok || !spec.RadioClaims {
			return nil, nil, fmt.Errorf("%q does not take a radio", comp)
		}
		c := b.comps[comp]
		if rid == "" {
			if spec.RadioRequired {
				return nil, nil, fmt.Errorf("%s needs a radio", spec.Title)
			}
			radios[comp] = ""
			continue
		}
		if c.installed || c.PendingPlan != nil {
			continue
		}
		n := b.nodes[c.Node]
		if n == nil || n.RadioProbe == nil || n.RadioProbe.Probe != "ok" {
			// Unjudgeable now; the component's own node blockers say why.
			continue
		}
		if !haContains(c.RadioCandidates, rid) {
			rejected = append(rejected, haBlocker{
				Code: "radio_stale", Severity: haBlocking,
				Message: fmt.Sprintf("The radio chosen for %s is no longer a free %s radio on %s.", spec.Title, spec.Title, n.ID),
				Evidence: []haEvidence{{Source: "radios", Subject: rid, State: "unavailable",
					Value: strings.Join(c.RadioCandidates, ", "), Detail: "not among the component's free, suitable radios"}},
				Actions: []haAction{{Kind: "select_radio", Label: "Choose a radio", Component: comp, Node: n.ID, RadioIDs: c.RadioCandidates}},
			})
			continue
		}
		if other, dup := taken[rid]; dup {
			rejected = append(rejected, haBlocker{
				Code: "radio_conflict", Severity: haBlocking,
				Message:  fmt.Sprintf("The same radio was chosen for %s and %s; a radio answers one program at a time.", other, comp),
				Evidence: []haEvidence{{Source: "radios", Subject: rid, State: "chosen_twice", Value: other + ", " + comp}},
				Actions:  []haAction{{Kind: "select_radio", Label: "Choose another radio", Component: comp, Node: n.ID, RadioIDs: c.RadioCandidates}},
			})
			delete(radios, other)
			continue
		}
		taken[rid] = comp
		radios[comp] = rid
	}
	return radios, rejected, nil
}

// orchestrate orders the selected components and judges the next step.
func (b *haBuild) orchestrate(req haOrchRequest, radios map[string]string) *haOrchestration {
	o := &haOrchestration{Topology: req.Topology, Node: b.node, Sequence: []haOrchStep{}, ExternalSteps: []haOrchExternal{}}
	confirmed := map[string]bool{}
	for _, k := range req.ConfirmedSteps {
		confirmed[k] = true
	}
	order := b.planOrder()
	current := -1
	for i, c := range order {
		if c.PendingPlan != nil {
			current = i
			break
		}
	}
	for i, c := range order {
		st := b.orchStep(c)
		if current < 0 && st.Action != haStepDone {
			current = i
		}
		o.Sequence = append(o.Sequence, st)
		for _, es := range c.ExternalSteps {
			due := c.installed && (c.State == haReady || c.State == haExternalStep)
			o.ExternalSteps = append(o.ExternalSteps, haOrchExternal{Component: c.ID, haStep: es, Due: due,
				Confirmed: es.Status == "unverified" && confirmed[c.ID+"/"+es.ID]})
		}
	}
	for i := range o.Sequence {
		switch {
		case i == current:
			o.Sequence[i].Status = "current"
			b.prepare(&o.Sequence[i], order[i], req, radios)
		case o.Sequence[i].Action == haStepDone:
			o.Sequence[i].Status = "done"
		default:
			o.Sequence[i].Status = "upcoming"
		}
	}
	if current >= 0 {
		o.Next = o.Sequence[current]
		return o
	}

	// Every package is in. What is left is outside Wildstone: first what
	// it can observe (and so will see done), then what it cannot, which
	// ends when the household says so.
	var pending, unverified []haOrchExternal
	for _, es := range o.ExternalSteps {
		switch {
		case !es.Due:
		case es.Status == "pending":
			pending = append(pending, es)
		case es.Status == "unverified" && !es.Confirmed:
			unverified = append(unverified, es)
		}
	}
	for _, set := range [][]haOrchExternal{pending, unverified} {
		if len(set) == 0 {
			continue
		}
		ids := make([]string, 0, len(set))
		st := haOrchStep{Title: "Finish setup outside Wildstone", Action: haStepExternal, Status: "current",
			Blockers: []haBlocker{}, Actions: []haAction{}, ExternalSteps: set}
		for _, es := range set {
			ids = append(ids, es.Component+"/"+es.ID)
			if es.Action != nil {
				st.Actions = append(st.Actions, *es.Action)
			}
		}
		st.ID = haStepExternal + ":" + strings.Join(ids, ",")
		st.Reason = "Every package is installed; " + fmt.Sprint(len(set)) + " step(s) outside Wildstone remain."
		if set[0].Status == "unverified" {
			st.Reason += " Wildstone cannot see these; confirm each once it is done."
		}
		o.Next = st
		return o
	}
	o.Complete = true
	o.Next = haOrchStep{ID: haStepDone, Title: "Home automation is set up", Action: haStepDone, Status: "done",
		Reason:   "Every selected component is installed and healthy, and every step outside Wildstone is done.",
		Blockers: []haBlocker{}, Actions: []haAction{}}
	return o
}

// planOrder is the selected components in dependency order: the table's
// order, with any dependency the catalog adds under the params a step
// would use pulled ahead of its dependent.
func (b *haBuild) planOrder() []*haComponent {
	var sel []*haComponent
	index := map[string]int{}
	for _, spec := range haComponents {
		if c := b.comps[spec.ID]; c != nil && c.Selection != "none" {
			index[c.ID] = len(sel)
			sel = append(sel, c)
		}
	}
	placed := map[string]bool{}
	var out []*haComponent
	var visit func(c *haComponent, depth int)
	visit = func(c *haComponent, depth int) {
		if placed[c.ID] || depth > len(haComponents) {
			return
		}
		for _, dep := range b.stepDeps(c, b.intendedParams(c)) {
			if _, ok := index[dep]; ok {
				visit(sel[index[dep]], depth+1)
			}
		}
		if !placed[c.ID] {
			placed[c.ID] = true
			out = append(out, c)
		}
	}
	for _, c := range sel {
		visit(c, 0)
	}
	return out
}

// stepDeps are a component's dependencies under the params its package
// would be expanded with: the table's and the catalog's (b.requires),
// plus any `requires ... when:` a param turns on.
func (b *haBuild) stepDeps(c *haComponent, params map[string]string) []string {
	deps := append([]string{}, c.Requires...)
	if c.pkg == nil || len(c.spec.ModeParams) > 0 {
		return deps
	}
	for _, rq := range c.pkg.Requires {
		if !rq.AppliesTo(params) {
			continue
		}
		if dep, ok := b.provider[rq.Trait]; ok && dep != c.ID && !haContains(deps, dep) {
			deps = append(deps, dep)
		}
	}
	return deps
}

// intendedParams are the installed params plus what the dependency
// table demands of them — enough to order, not to propose.
func (b *haBuild) intendedParams(c *haComponent) map[string]string {
	out := map[string]string{}
	if c.pkg != nil {
		for k, v := range b.params[c.pkg.Metadata.Name] {
			out[k] = v
		}
	}
	if c.ID == haHomeAssistant {
		for k, v := range b.managedHADesired(c) {
			out[k] = v
		}
	}
	return out
}

// managedHADesired are the settings a Wildstone-run Home Assistant needs
// for the selected components (spec.ManagedHAParams), limited to params
// its package declares.
func (b *haBuild) managedHADesired(hass *haComponent) map[string]string {
	want := map[string]string{}
	if hass == nil || hass.pkg == nil {
		return want
	}
	for _, spec := range haComponents {
		if b.selection[spec.ID] == "" {
			continue
		}
		for name, v := range spec.ManagedHAParams {
			if declaresParam(hass.pkg, name) {
				want[name] = v
			}
		}
	}
	return want
}

func declaresParam(p *pkgspec.Package, name string) bool {
	_, ok := paramDecl(p, name)
	return ok
}

func paramDecl(p *pkgspec.Package, name string) (pkgspec.ParamDecl, bool) {
	if p == nil {
		return pkgspec.ParamDecl{}, false
	}
	for _, d := range p.Params {
		if d.Name == name {
			return d, true
		}
	}
	return pkgspec.ParamDecl{}, false
}

// haSettingsDrift is what an installed Home Assistant's params lack of
// managedHADesired.
func (b *haBuild) haSettingsDrift(hass *haComponent) map[string]string {
	drift := map[string]string{}
	if !hass.installed || hass.pkg == nil {
		return drift
	}
	for name, v := range b.managedHADesired(hass) {
		d, _ := paramDecl(hass.pkg, name)
		have := b.params[hass.pkg.Metadata.Name][name]
		if have == "" {
			have = d.Default
		}
		if have != v {
			drift[name] = v
		}
	}
	return drift
}

// orchStep maps a component's evaluated state onto its step.
func (b *haBuild) orchStep(c *haComponent) haOrchStep {
	st := haOrchStep{Component: c.ID, Title: c.Title, State: c.State, Reason: c.Reason, Package: c.Package,
		Version: c.Version, Node: c.Node, Blockers: append([]haBlocker{}, c.Blockers...), Actions: append([]haAction{}, c.Actions...)}
	intended := haStepInstall
	if len(c.spec.ModeParams) > 0 {
		intended = haStepReconfigure
	}
	switch c.State {
	case haReady, haExternalStep:
		st.Action = haStepDone
		if c.ID == haHomeAssistant {
			if drift := b.haSettingsDrift(c); len(drift) > 0 {
				st.Action = haStepReconfigure
				st.Reason = "Home Assistant needs " + joinParams(drift) + " for what you chose."
			}
		}
	case haAwaitingApproval:
		st.Action, st.PlanID = haStepAwaitApproval, c.PendingPlan.ID
	case haApplying:
		if c.PendingPlan != nil {
			st.Action, st.PlanID = haStepAwaitApply, c.PendingPlan.ID
		} else {
			st.Action = haStepAwaitObservation
		}
	case haReadyToPropose:
		st.Action = intended
	case haBlocked:
		st.Action = intended
		for _, bl := range c.Blockers {
			if bl.Severity == haBlocking && bl.Code != "dependency_not_ready" {
				st.Action = haStepResolve
			}
		}
	default: // degraded, unavailable
		st.Action = haStepResolve
	}
	st.ID = c.ID + ":" + st.Action
	switch {
	case st.PlanID != "":
		st.ID += ":" + st.PlanID
	case st.Action == haStepInstall || st.Action == haStepReconfigure:
		st.ID += ":" + c.Package
	}
	return st
}

func joinParams(m map[string]string) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// prepare fills the current step's package params — node, deployment
// facts, the node's address, the chosen radio, the mode, Home Assistant's
// dependency settings and the caller's options, over whatever the package
// is installed with — and turns it into resolve_blockers when something
// only the household can supply is missing.
func (b *haBuild) prepare(st *haOrchStep, c *haComponent, req haOrchRequest, radios map[string]string) {
	if st.Action != haStepInstall && st.Action != haStepReconfigure {
		return
	}
	pkg := c.pkg
	if pkg == nil {
		st.Action = haStepResolve
		return
	}
	st.pkg = pkg
	params := map[string]string{}
	if packageIsInstalled(pkg, b.current) {
		for k, v := range b.params[pkg.Metadata.Name] {
			params[k] = v
		}
	}
	var extra []haBlocker
	var n *haNode
	if c.Node != "" {
		n = b.nodes[c.Node]
	}
	for _, v := range resolveParams(pkg.Params, b.s.gatherFacts(), nil) {
		if params[v.Name] != "" {
			continue
		}
		switch {
		case v.Source == "node" && c.Node != "":
			params[v.Name] = c.Node
		case v.Suggested != "":
			params[v.Name] = v.Suggested
		case v.Validate == "private_ipv4" && v.Default == "" && n != nil && n.HostAddress != "":
			if pkgspec.ValidateParamValue(v.Validate, n.HostAddress) == nil {
				params[v.Name] = n.HostAddress
			}
		case v.Validate == "interface_name" && v.Default == "" && n != nil && n.Readiness != nil:
			// The LAN-facing interface (Thread's backbone, Matter's
			// primary interface): the one carrying the default route.
			for _, i := range n.Readiness.Interfaces {
				if i.DefaultRoute && pkgspec.ValidateParamValue(v.Validate, i.Name) == nil {
					params[v.Name] = i.Name
					break
				}
			}
		}
	}

	if len(c.spec.ModeParams) > 0 {
		d, _ := modeParam(c.spec, pkg)
		mode := req.Modes[c.ID]
		if mode == "" && haContains(c.spec.OnModes, c.Mode) {
			mode = c.Mode
		}
		if mode == "" {
			extra = append(extra, haBlocker{
				Code: "mode_needed", Severity: haBlocking,
				Message:  "Choose how " + c.Title + " should work: " + strings.Join(c.spec.OnModes, " or ") + ".",
				Evidence: []haEvidence{{Source: "catalog", Subject: pkg.Metadata.Name, Value: strings.Join(c.spec.OnModes, ", "), Detail: "param " + d.Name}},
				Actions:  []haAction{{Kind: "choose_mode", Label: "Choose a mode", Component: c.ID}},
			})
		} else {
			params[d.Name] = mode
		}
	}

	if c.spec.RadioClaims && !c.installed {
		b.assignRadio(st, c, radios, params, &extra)
	}
	for name, v := range req.Options[c.ID] {
		params[name] = v
	}
	if c.ID == haHomeAssistant {
		for name, v := range b.managedHADesired(c) {
			params[name] = v
		}
	}

	for _, dep := range b.stepDeps(c, params) {
		d := b.comps[dep]
		if d == nil {
			continue
		}
		if d.State != haReady && d.State != haExternalStep && c.blockerCode("dependency_not_ready", dep) == nil {
			extra = append(extra, haBlocker{
				Code: "dependency_not_ready", Severity: haBlocking,
				Message:  fmt.Sprintf("Needs %s first (it is %s).", d.Title, strings.ReplaceAll(string(d.State), "_", " ")),
				Evidence: []haEvidence{{Source: "component", Subject: dep, State: string(d.State), Detail: d.Reason}},
				Actions:  []haAction{{Kind: "resolve_dependency", Label: "Set up " + d.Title, Component: dep}},
			})
		}
		if d.installed && d.Node != "" && c.Node != "" && d.Node != c.Node {
			extra = append(extra, haBlocker{
				Code: "dependency_on_other_node", Severity: haBlocking,
				Message:  fmt.Sprintf("%s runs on %s, so %s must run there too.", d.Title, d.Node, c.Title),
				Evidence: []haEvidence{{Source: "component", Subject: dep, Value: d.Node, Detail: "chosen node " + c.Node}},
				Actions:  []haAction{{Kind: "choose_node", Label: "Use " + d.Node, Component: c.ID, Node: d.Node}},
			})
		}
	}

	for _, d := range pkg.Params {
		if d.Required && params[d.Name] == "" && d.Default == "" {
			extra = append(extra, haBlocker{
				Code: "param_needed", Severity: haBlocking,
				Message:  fmt.Sprintf("%s needs a value for %s.", c.Title, d.Name),
				Evidence: []haEvidence{{Source: "catalog", Subject: pkg.Metadata.Name, Value: d.Name, Detail: d.Description}},
				Actions: []haAction{{Kind: "provide_option", Label: "Provide " + d.Name, Component: c.ID, Package: pkg.Metadata.Name,
					Params: map[string]string{d.Name: d.Example}, Instructions: []string{d.Description}}},
			})
		}
	}

	st.Params = params
	st.Blockers = append(st.Blockers, extra...)
	for _, bl := range st.Blockers {
		if bl.Severity == haBlocking {
			st.Action = haStepResolve
			st.ID = c.ID + ":" + haStepResolve
			st.Reason = bl.Message
			return
		}
	}
}

func (c *haComponent) blockerCode(code, subject string) *haBlocker {
	for i := range c.Blockers {
		bl := &c.Blockers[i]
		if bl.Code == code && len(bl.Evidence) > 0 && bl.Evidence[0].Subject == subject {
			return bl
		}
	}
	return nil
}

// assignRadio puts the chosen radio's stable path into the package's
// radio param. With no choice made, the one radio the inventory
// identified for this protocol is taken; more than one, or only
// unidentified ones, is the household's choice.
func (b *haBuild) assignRadio(st *haOrchStep, c *haComponent, radios map[string]string, params map[string]string, extra *[]haBlocker) {
	rid, chosen := radios[c.ID]
	if !chosen {
		n := b.nodes[c.Node]
		var detected []string
		if n != nil && n.RadioProbe != nil {
			for _, r := range n.RadioProbe.Radios {
				for _, fit := range r.SuitableFor {
					if fit.Component == c.ID && fit.Fit == "detected" && haContains(c.RadioCandidates, r.ID) {
						detected = append(detected, r.ID)
					}
				}
			}
		}
		switch {
		case len(detected) == 1 && len(c.RadioCandidates) == 1:
			rid = detected[0]
		case len(c.RadioCandidates) > 0:
			*extra = append(*extra, haBlocker{
				Code: "radio_choice_needed", Severity: haBlocking,
				Message:  fmt.Sprintf("Choose which radio %s should use.", c.Title),
				Evidence: []haEvidence{{Source: "radios", Subject: c.Node, Value: strings.Join(c.RadioCandidates, ", ")}},
				Actions:  []haAction{{Kind: "select_radio", Label: "Choose a radio", Component: c.ID, Node: c.Node, RadioIDs: c.RadioCandidates}},
			})
			return
		}
	}
	if rid == "" {
		return // no radio: a network coordinator, or already blocked
	}
	var name string
	for _, p := range haRadioParams {
		if declaresParam(c.pkg, p) {
			name = p
			break
		}
	}
	path := ""
	if n := b.nodes[c.Node]; n != nil && n.RadioProbe != nil {
		for _, r := range n.RadioProbe.Radios {
			if r.ID == rid {
				path = r.DevPath
				if len(r.ByIDPaths) > 0 {
					path = r.ByIDPaths[0]
				}
			}
		}
	}
	if name == "" || path == "" {
		*extra = append(*extra, haBlocker{
			Code: "radio_unsupported", Severity: haBlocking,
			Message:  fmt.Sprintf("Package %s has no way to be given a radio.", c.Package),
			Evidence: []haEvidence{{Source: "catalog", Subject: c.Package, Detail: "no param named " + strings.Join(haRadioParams, " or ")}},
			Actions:  []haAction{},
		})
		return
	}
	st.RadioID = rid
	params[name] = path
}

// refuse turns a pipeline refusal of the next step into its blocker.
func (o *haOrchestration) refuse(status int, err error) {
	code := "plan_refused"
	if status == http.StatusConflict {
		code = "plan_conflict"
	}
	o.Next.Blockers = append(o.Next.Blockers, haBlocker{
		Code: code, Severity: haBlocking, Message: err.Error(),
		Evidence: []haEvidence{{Source: "plan", Subject: o.Next.Package, State: http.StatusText(status), Value: fmt.Sprint(status)}},
		Actions:  []haAction{},
	})
	o.Next.Action = haStepResolve
	o.Next.ID = o.Next.Component + ":" + haStepResolve
	o.Next.Reason = err.Error()
	for i := range o.Sequence {
		if o.Sequence[i].Status == "current" {
			o.Sequence[i] = o.Next
		}
	}
}
