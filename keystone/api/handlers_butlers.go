package api

// Butlers from the Console (ADR-W031, work package B6).
//
// A household has Butlers, plural, each with its own model, directive
// and scope. These endpoints are the Console holder's: A10 gives them
// every Butler, and they decide who may talk to which.
//
// Two things about this surface are load-bearing rather than shape.
//
// Every response that names a Butler names its MODEL and whether that
// model leaves the house. A person deciding to tell a Butler something
// sensitive is relying on which model sits behind it, and an API that
// makes a Console fetch that separately is one where some screen
// eventually forgets to.
//
// Changing a Butler's model applies immediately and returns the
// crossing. A8: the Console holder owns this Butler, so a proposal they
// would then approve themselves is ceremony with no refusing party.
// What they are owed instead is the consequence — and on a local-to-
// cloud move, a warning naming what happens to the conversation that
// is already open.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"

	providerpkg "github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// butlerView is one Butler as a Console renders it.
//
// leavesTheHouse is computed here rather than left to the client
// because it is the single most consequential fact about a Butler, and
// a client deriving it from a route kind is a client that can derive it
// wrongly.
func butlerView(b runtimedb.Butler) map[string]any {
	return map[string]any{
		"butler":         b,
		"leavesTheHouse": b.ModelKind == runtimedb.ModelCloud,
		"carriesActions": b.CarriesActionAuthority(),
		"reachable":      b.Reachable(),
	}
}

// listButlers serves the roster, with each Butler's reach counted.
//
// The counts are here rather than in the client because the roster
// shows one line per Butler saying who can reach it and what it may do,
// and a Console assembling that would be making three calls per row —
// thirty on a household of ten, every time the screen reloads, with
// nothing to stop one of them failing and a row silently reading "no
// people" for a Butler two people talk to.
//
// The tool ceiling is resolved ONCE per (profile, verdict) pair rather
// than once per Butler. Every Butler on a profile has the same
// structural ceiling and the same deployment wiring; what differs is
// its own narrowings, which is a single cheap read. Building a gateway
// per row would be the same answer computed ten times.
func (s *Server) listButlers(_ *http.Request, _ principal.Identity) (any, int, error) {
	all, err := s.DB.ListButlers()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	available := map[string]map[string]bool{}
	out := make([]map[string]any, 0, len(all))
	for _, b := range all {
		view := butlerView(b)

		people, err := s.DB.ListButlerAccess(b.Name)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		conns, err := s.DB.ButlerConnections(b.Name)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		narrowed, err := s.DB.ButlerToolNarrowings(b.Name)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		view["people"] = len(people)
		view["connections"] = len(conns)

		readOnly := !b.CarriesActionAuthority()
		key := fmt.Sprintf("%s/%t", b.AgentClass, readOnly)
		set, ok := available[key]
		if !ok {
			set = s.availableTools(b.AgentClass, readOnly)
			available[key] = set
		}
		// Allowed is available minus this Butler's own narrowings, and
		// the denominator is AVAILABLE rather than the whole ceiling.
		// "5 of 6" is a sentence about what this Butler can do here;
		// "5 of 13" counts tools no deployment ever wired and reads as
		// somebody having switched eight things off.
		allowed := 0
		for tool := range set {
			if _, off := narrowed[tool]; !off {
				allowed++
			}
		}
		view["toolsAllowed"] = allowed
		view["toolsAvailable"] = len(set)
		out = append(out, view)
	}
	return map[string]any{"butlers": out}, http.StatusOK, nil
}

// availableTools is the set of tool names an agent class can actually
// use on this deployment, before any one Butler's narrowings.
//
// It asks the gateway rather than a list, with no Butler authority
// bound — the per-Butler half is subtracted by the caller, from a read
// it already has.
func (s *Server) availableTools(class string, readOnly bool) map[string]bool {
	set := map[string]bool{}
	for _, t := range s.availableToolDetails(class, readOnly) {
		if t.Available {
			set[t.Name] = true
		}
	}
	return set
}

// availableToolDetails is the same inventory with each tool's metadata
// kept — which is what §6.4's default set needs, because "except opt-in
// tools" cannot be answered from a list of names.
func (s *Server) availableToolDetails(class string, readOnly bool) []butler.CapabilityTool {
	var manifest butler.CapabilityManifest
	switch class {
	case runtimedb.AgentClassMember:
		manifest = butler.NewHouseholdGateway("", s.ButlerHost.householdSeams()).
			ForAdministration().Capabilities(readOnly)
	case runtimedb.AgentClassOwner:
		_, live, _, _, _ := s.ButlerHost.Current()
		if live == nil {
			live = butler.NewToolGateway(nil, nil)
		}
		manifest = live.Scoped(nil).ForAdministration().Capabilities(readOnly)
	default:
		return nil
	}
	return manifest.Tools
}

// BackfillAgentTools brings every Agent that predates 0059 onto the
// assigned-tools model (design §6.3, §6.4).
//
// Run once at startup, and idempotent: an Agent already enrolled is
// skipped entirely, so an Owner who has since removed a tool does not
// find it back the next time the process restarts. That skip is the
// whole reason enrollment is its own record rather than something
// inferred from having assignments.
//
// WHY A BACKFILL AND NOT A MIGRATION. The default set is "all currently
// available tools", and what is available is a fact about the gateways
// this deployment wired — which SQL cannot see. A migration could only
// have guessed, and guessing in the widening direction is how an Agent
// ends up holding something nobody chose.
//
// The window between the migration and this call is the one moment the
// old model is still in force. That is the safe direction to fail in: an
// unenrolled Agent reaches what it always did, rather than nothing.
func (s *Server) BackfillAgentTools() error {
	if s.DB == nil {
		return nil
	}
	butlers, err := s.DB.ListButlers()
	if err != nil {
		return err
	}
	for _, b := range butlers {
		governed, err := s.DB.AgentIsGoverned(b.Seq)
		if err != nil {
			return err
		}
		if governed {
			continue
		}
		if err := s.seedAgentTools(b, "system"); err != nil {
			return err
		}
	}
	return nil
}

// seedAgentTools gives an Agent its default tools and enrolls it in the
// assigned-tools model (design §6.4).
//
// "A newly created Owner Agent receives all currently available Member
// and Owner tools by default except opt-in tools. A Member Agent can
// receive only Member tools." Both halves are read off the gateways, so
// the default set is what this deployment actually wired rather than a
// list somebody keeps in step.
//
// ASSIGNMENTS FIRST, GOVERNANCE LAST, and the order is the safety
// argument: an Agent governed before its tools were written would be
// default-deny with nothing assigned, so a failure between the two steps
// would leave an Agent that can do nothing. The other way round, the
// same failure leaves an ungoverned Agent with some assignments, which
// behaves exactly as every pre-0059 Agent does and is harmless.
func (s *Server) seedAgentTools(b runtimedb.Butler, by string) error {
	classes := []string{runtimedb.AgentClassMember}
	if b.AgentClass == runtimedb.AgentClassOwner {
		classes = append(classes, runtimedb.AgentClassOwner)
	}
	inventory := s.ToolInventory
	if inventory == nil {
		inventory = s.availableToolDetails
	}
	for _, class := range classes {
		for _, tool := range inventory(class, false) {
			if !tool.Available || tool.OptIn {
				continue
			}
			if err := s.DB.AssignTool(b.Seq, tool.Name, by); err != nil {
				return err
			}
		}
	}
	return s.DB.GovernAgent(b.Seq, by)
}

// createButler makes a new Butler.
//
// Creating one grants a new scope rather than changing an existing
// thing, and it is the act A8 must keep a Butler out of — so it is the
// Console holder's, with manage authority, and never the Butler's.
func (s *Server) createButler(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Name      string `json:"name"`
		Directive string `json:"directive"`
		// AgentClass is the one the caller picks. The surface is DERIVED
		// from it rather than accepted alongside it: the two are one
		// fact (design §5.2), and a request carrying both could carry a
		// contradiction — at which point the API has to decide which of
		// the caller's two answers it meant.
		AgentClass string `json:"agentClass"`
		Model      string `json:"model"`
		// The same pair the assignment path takes, for the same reason:
		// a person making a Butler picks from what the provider has, not
		// from what somebody configured here first.
		ProviderModel string `json:"providerModel"`
		Endpoint      string `json:"endpoint"`
		Provider      string `json:"provider"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Model == "" && req.ProviderModel != "" {
		configured, err := s.ensureModel(r.Context(), req.ProviderModel, req.Provider, req.Endpoint)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		req.Model = configured
	}
	// CreateButler trims the name, so the read-back has to use the
	// trimmed one. Re-reading what the caller sent returns 500 for a
	// Butler that was in fact created.
	name := strings.TrimSpace(req.Name)
	if _, err := s.DB.CreateButler(runtimedb.Butler{
		Name: name, Directive: req.Directive,
		Surface:    runtimedb.SurfaceForAgentClass(req.AgentClass),
		AgentClass: req.AgentClass, Model: req.Model,
	}, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, err
		}
		return nil, http.StatusBadRequest, err
	}
	created, err := s.DB.GetButler(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := s.seedAgentTools(created, actingMember(id)); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Every Agent runs on a runtime and authenticates as a principal
	// bound to it (ADR-W039 §3.3). A new one is native, with its
	// credential minted here rather than synthesised later.
	if err := s.ensureNativeRuntime(created, id); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	b, err := s.DB.GetButler(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return butlerView(b), http.StatusCreated, nil
}

// setButlerModel changes which model a Butler runs on, immediately.
//
// The response carries the crossing rather than a confirmation prompt.
// A local-to-cloud move with a conversation already open sends that
// conversation's history to the new model on the next message, and the
// person making the choice is owed that fact in the moment they make
// it — as a statement of what happens, not a dialog standing between
// them and a control they own.
func (s *Server) setButlerModel(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		// Model names an already-configured model.
		Model string `json:"model"`
		// ProviderModel names one the provider offers, whether or not
		// this house has configured it yet — which is what the Console's
		// Model tab sends, because a person picking a model is picking
		// from what the provider actually has rather than from a list
		// somebody remembered to curate first.
		ProviderModel string `json:"providerModel"`
		Endpoint      string `json:"endpoint"`
		Provider      string `json:"provider"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Model == "" && req.ProviderModel != "" {
		// Find or create, here rather than in the client. Two calls —
		// configure the model, then assign it — can half-fail, and the
		// half that fails leaves a model row nothing is assigned to, or
		// worse, an assignment to a row whose kind was never checked.
		configured, err := s.ensureModel(r.Context(), req.ProviderModel, req.Provider, req.Endpoint)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		req.Model = configured
	}
	from, to, err := s.DB.SetButlerRoute(name, req.Model, actingMember(id))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	out := butlerView(to)
	out["previousModel"] = from.Model
	if runtimedb.CrossesToCloud(from, to) {
		out["crossesToCloud"] = true
		out["warning"] = fmt.Sprintf(
			"%s now runs on %s, which is outside the house. An open conversation's history goes to it with the next message.",
			to.Name, to.Model)
	}
	return out, http.StatusOK, nil
}

// ensureModel returns the configured model for a provider's model,
// creating it if this house has not used it before.
//
// The kind is decided HERE and never taken from the caller. It is the
// data boundary — the one fact this whole design exists to keep
// answerable — and a client that could name it is a client that could
// name it wrongly, or maliciously. looksLocal draws it from the
// provider's own listing and is deliberately wrong in the safe
// direction: an unrecognised naming scheme reports as NOT local, so
// somebody is warned about a model that turns out to stay home rather
// than reassured about one that does not.
//
// An existing row is returned untouched. Its kind is immutable at the
// schema level anyway, but the reason matters more than the mechanism:
// Butlers were assigned to it on the strength of where its words go.
func (s *Server) ensureModel(ctx context.Context, providerModel, provider, endpoint string) (string, error) {
	providerModel = strings.TrimSpace(providerModel)
	if providerModel == "" {
		return "", fmt.Errorf("naming a provider model needs the model")
	}
	existing, err := s.DB.ListModels()
	if err != nil {
		return "", err
	}
	for _, m := range existing {
		if m.ProviderModel == providerModel && (endpoint == "" || m.Endpoint == endpoint) {
			if !m.Enabled {
				return "", fmt.Errorf("%s is turned off; turn it back on before assigning a Butler to it", m.Name)
			}
			// Probe it if nobody ever has. A model configured but never
			// checked carries no authority to use tools that do things,
			// so a Butler moved onto one silently drops to answering
			// questions — and the person doing the moving reads that as
			// the model being poor rather than as nobody having asked
			// it yet. Assignment is the moment to find out.
			//
			// An already-probed model is left alone: re-checking on
			// every assignment would put a minute of provider latency
			// in front of a click, to re-answer a question that has an
			// answer. Re-check is a button for when it is stale.
			if m.Readiness == runtimedb.ModelUnprobed {
				s.probeAndRecord(ctx, m.Name)
			}
			return m.Name, nil
		}
	}
	if provider == "" {
		return "", fmt.Errorf(
			"%s is not configured here yet, so assigning it needs the provider and endpoint it comes from", providerModel)
	}
	// A hosted provider has a known address; an empty one means it.
	if kind, ok := providerpkg.LookupKind(provider); ok {
		endpoint = kind.Endpoint(endpoint)
	}
	if endpoint == "" {
		return "", fmt.Errorf("%s needs the endpoint of the %s server it comes from", providerModel, provider)
	}
	// The kind comes from the provider's own listing, with the real
	// size, never from the caller and never from the name alone. If the
	// endpoint cannot be reached, nothing is created: a model whose
	// destination could not be established is not one to assign a
	// Butler to on a guess.
	offered, _, _, err := offeredModels(ctx, provider, endpoint)
	if err != nil {
		return "", fmt.Errorf("could not ask %s what it serves, so %s cannot be assigned yet: %w",
			endpoint, providerModel, err)
	}
	kind := ""
	for _, m := range offered {
		if m.Name == providerModel {
			kind = runtimedb.ModelCloud
			if m.Local {
				kind = runtimedb.ModelLocal
			}
			break
		}
	}
	if kind == "" {
		return "", fmt.Errorf("%s does not serve a model called %s", endpoint, providerModel)
	}
	// The model's own name IS the configured name. The alternative is a
	// separate label, and a household that has to invent one ends up
	// with "kitchen-box" pointing at qwen3.5:4b — a screen that answers
	// "which model saw this" with a place.
	if err := s.DB.UpsertModel(runtimedb.Model{
		Name: providerModel, Kind: kind, Provider: provider,
		ProviderModel: providerModel, Endpoint: endpoint, Enabled: true,
	}, "console"); err != nil {
		return "", err
	}
	// Probe it now rather than leaving it unprobed.
	//
	// Unprobed is the safe default and the wrong outcome here: a Butler
	// moved onto a model nobody has checked loses the authority to use
	// tools that do anything (ADR-W017 A4), so it would silently drop to
	// answering questions only — which reads as the model being bad
	// rather than as nobody having asked it yet. The person assigning it
	// is standing right there; this is the moment to find out.
	s.probeAndRecord(ctx, providerModel)
	return providerModel, nil
}

// probeAndRecord asks a model whether it can operate a tool and stores
// the verdict. Failure to probe is recorded, never fatal: a model that
// could not be checked is left unprobed, which is what it is.
func (s *Server) probeAndRecord(ctx context.Context, name string) {
	if s.ProbeModel == nil {
		return
	}
	m, err := s.DB.GetModel(name)
	if err != nil {
		return
	}
	readiness, detail := s.ProbeModel(ctx, m.Provider, m.ProviderModel, m.Endpoint)
	if readiness == "" {
		return
	}
	if err := s.DB.RecordModelReadiness(name, readiness, detail); err != nil {
		s.Log.Warn("recording a model probe failed", "model", name, "err", err)
	}
}

// probeModel re-checks one model on demand — the Console's Re-check.
func (s *Server) probeModel(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	if _, err := s.DB.GetModel(name); err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no model named %q", name)
	}
	if s.ProbeModel == nil {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("this deployment cannot probe a model from here")
	}
	s.probeAndRecord(r.Context(), name)
	m, err := s.DB.GetModel(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"model": m}, http.StatusOK, nil
}

func (s *Server) retireButler(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var err error
	if destination := r.URL.Query().Get("transferMemoryTo"); destination != "" {
		err = s.DB.RetireButlerWithMemoryTransfer(name, destination, actingMember(id))
	} else {
		err = s.DB.RetireButler(name, actingMember(id))
	}
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler named %q, or it is already retired", name)
		}
		return nil, http.StatusBadRequest, err
	}
	// Nothing may act for a retired Agent: its runtime credential is
	// disabled and its runtime marked revoked (ADR-W039 §3.7).
	retired, err := s.DB.GetButler(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := s.revokeAgentRuntime(retired); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"retired": name}, http.StatusOK, nil
}

// setButlerDirective rewrites a Butler's purpose.
//
// It carries `manage` like every other Butler edit, and applies at once
// rather than through a Plan (A8): the Console holder owns every
// Butler, so asking them to propose a change and then approve it is
// self-approval with extra steps. A directive grants nothing (A9), so
// there is no authority here for an approval to be protecting.
func (s *Server) setButlerDirective(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Directive string `json:"directive"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	b, err := s.DB.SetButlerDirective(name, req.Directive, actingMember(id))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no butler named %q, or it is retired", name)
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	// The activity record A8 asks for: who changed what, and when.
	s.event("butler.directive", "", id, fmt.Sprintf("directive rewritten for butler %s", name))
	return butlerView(b), http.StatusOK, nil
}

// butlerScope answers "what does this Butler see" in one call, so a
// person does not assemble it from three screens.
func (s *Server) butlerScope(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	scope, err := s.DB.ButlerScope(name)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no butler named %q", name)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	people, err := s.DB.ListButlerAccess(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	addresses, err := s.DB.ListButlerAddresses(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"scope": scope, "people": people, "addresses": addresses,
	}, http.StatusOK, nil
}

// setButlerAccess grants or revokes one person's access to one Butler.
//
// One endpoint for both directions because they are one decision seen
// from two sides, and a Console toggling a checkbox should not have to
// pick a verb.
func (s *Server) setButlerAccess(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Member string `json:"member"`
		Allow  bool   `json:"allow"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var err error
	if req.Allow {
		err = s.DB.GrantButlerAccess(name, req.Member, actingMember(id))
	} else {
		err = s.DB.RevokeButlerAccess(name, req.Member, actingMember(id))
	}
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	people, err := s.DB.ListButlerAccess(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"butler": name, "people": people}, http.StatusOK, nil
}

// setButlerConnection grants or revokes one integration for one Butler.
func (s *Server) setButlerConnection(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Connection string `json:"connection"`
		Allow      bool   `json:"allow"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var err error
	if req.Allow {
		err = s.DB.GrantButlerConnection(name, req.Connection, actingMember(id))
	} else {
		err = s.DB.RevokeButlerConnection(name, req.Connection, actingMember(id))
	}
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	conns, err := s.DB.ButlerConnections(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"butler": name, "connections": conns}, http.StatusOK, nil
}

// setButlerTool narrows a tool for one Butler, or removes the
// narrowing.
//
// There is no widening here and there cannot be: allow:false narrows,
// allow:true removes a narrowing and returns the tool to whatever the
// profile says. A Butler narrows a surface and never widens one.
func (s *Server) setButlerTool(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Tool   string `json:"tool"`
		Allow  bool   `json:"allow"`
		Reason string `json:"reason"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var err error
	if req.Allow {
		err = s.DB.WidenButlerTool(name, req.Tool)
	} else {
		err = s.DB.NarrowButlerTool(name, req.Tool, req.Reason, actingMember(id))
	}
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	narrowed, err := s.DB.ButlerToolNarrowings(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"butler": name, "narrowedTools": narrowed,
		"note": "removing a narrowing returns the tool to what this butler's profile allows — it never widens past it",
	}, http.StatusOK, nil
}

// restoreButler brings a retired Butler back.
//
// Retiring is reversible by design — the whole reason it is not a
// delete — and until this existed the Console could do the one half of
// that and not the other, which made "you can bring it back" a sentence
// with no button behind it.
func (s *Server) restoreButler(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	if err := s.DB.RestoreButler(name, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	b, err := s.DB.GetButler(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Retiring revoked its runtime credential; a native runtime comes
	// back with it. An external one stays revoked until re-registered.
	if err := s.ensureNativeRuntime(b, id); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("butler.restored", "", id, fmt.Sprintf("butler %s brought back", name))
	return butlerView(b), http.StatusOK, nil
}

// duplicateButler copies a Butler's shape onto a new name.
//
// It is one call rather than six from the client for a reason the
// client cannot fix: creating, then granting each connection, then
// re-narrowing each tool is a sequence that can half-fail, and the
// half that fails is the narrowing — leaving a new Butler with its
// model, its connections, and every tool the profile allows. A copy
// that is quietly wider than its original is the worst possible
// outcome of a button labelled "Duplicate".
func (s *Server) duplicateButler(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Name string `json:"name"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	copyName := strings.TrimSpace(req.Name)
	if copyName == "" {
		// The server picks, so the Console does not have to invent a
		// naming rule the CLI would then invent differently.
		copyName = name + " copy"
		for n := 2; ; n++ {
			if _, err := s.DB.GetButler(copyName); errors.Is(err, runtimedb.ErrNotFound) {
				break
			} else if err != nil {
				return nil, http.StatusInternalServerError, err
			}
			copyName = fmt.Sprintf("%s copy %d", name, n)
		}
	}
	if _, err := s.DB.DuplicateButler(name, copyName, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, err
		}
		return nil, http.StatusBadRequest, err
	}
	b, err := s.DB.GetButler(copyName)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// The copy is its own Agent with its own runtime credential; the
	// original's is bound to the original for life (ADR-W039 §4.1).
	if err := s.ensureNativeRuntime(b, id); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("butler.duplicated", "", id, fmt.Sprintf("butler %s copied from %s", copyName, name))
	out := butlerView(b)
	out["note"] = "the copy has its model, connections and tool narrowings. Nobody can talk to it, it remembers nothing, and it has no conversations — those belong to " + name + "."
	return out, http.StatusCreated, nil
}

// butlerImpact counts what retiring this Butler would break.
//
// The counts are read rather than guessed because the dialog they feed
// is asking somebody to accept a consequence. "2 routines call it" is
// worth showing exactly when it is two; a screen that rounds, or that
// says "some routines may fail", has moved the work of finding out back
// onto the person it was supposed to inform.
func (s *Server) butlerImpact(r *http.Request, _ principal.Identity) (any, int, error) {
	impact, err := s.DB.ButlerImpactOf(r.PathValue("name"))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no butler named %q", r.PathValue("name"))
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return impact, http.StatusOK, nil
}

// butlerStatus answers "what is answering, and what may it do" for one
// named Butler.
//
// The Butler screen's backend chip used to read the DEPLOYMENT's status
// — one provider, one model, one tool list — which was true while there
// was one Butler and became a different Butler's answer the moment
// there were several. This is that question asked of a Butler.
//
// Mode is the route's probe verdict rather than a second opinion about
// it, and the tools are the ones this Butler would actually be offered:
// the same manifest the Tools tab renders, so a person cannot be told
// two things about one session.
func (s *Server) butlerStatus(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	b, err := s.DB.GetButler(name)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no butler named %q", name)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	mode := string(runtimedb.ButlerModeFull)
	if !b.CarriesActionAuthority() {
		mode = string(runtimedb.ButlerModeQAOnly)
	}
	out := map[string]any{
		"configured": true,
		"butler":     b.Name,
		"provider":   b.Provider,
		"model":      b.ProviderModel,
		"endpoint":   b.Endpoint,
		"mode":       mode,
	}
	// Whether the Console's mic works for this Butler (ADR-W042).
	if ok, max := s.dictationInfo(b.Name); ok {
		out["dictation"] = true
		out["dictationMaxSeconds"] = max
	}
	// A Butler that cannot answer says why, and says it from the model's
	// own readiness rather than from a sentence this handler invented.
	if !b.Reachable() {
		out["configured"] = false
		// The detail belongs to the MODEL, not the Butler: three
		// Butlers on one unreachable model fail for one reason, and it
		// is the model that knows what the provider said.
		why := ""
		if m, err := s.DB.GetModel(b.Model); err == nil {
			why = m.ReadinessDetail
		}
		if !b.ModelEnabled {
			why = fmt.Sprintf("%s is turned off", b.Model)
		}
		if why == "" {
			why = fmt.Sprintf("%s is not answering", b.Model)
		}
		out["reason"] = why
	}
	tools := []string{}
	for name := range s.availableTools(b.AgentClass, !b.CarriesActionAuthority()) {
		tools = append(tools, name)
	}
	sort.Strings(tools)
	out["tools"] = tools
	return out, http.StatusOK, nil
}

// butlerTools is what ONE Butler may actually do, answered by the same
// resolver a dispatch would consult.
//
// The Console cannot work this out for itself, and the attempts it
// would have to make are both wrong. A tool list written in the client
// is a second copy of the registry, and it drifts the first time a
// gateway gains or loses a tool — the screen then offers a switch for
// something that does not exist, or hides one that does. Deriving
// availability is worse: it means re-implementing the effective
// authority order (structural, disabled, enabled, grant, default) in
// TypeScript, and a screen that gets that order wrong shows a tool as
// on that the engine refuses, which is the one lie this surface must
// not tell.
//
// So the answer comes from the gateway itself, bound to this Butler's
// authority, and each unavailable tool carries the resolver's own
// words rather than a sentence a screen invented.
func (s *Server) butlerTools(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	b, err := s.DB.GetButler(name)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no butler named %q", name)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// A Butler whose route failed its tool probe is handed the
	// read-only set (ADR-W017 A4), and the screen has to show THAT set
	// rather than the one it would have on a working model. Otherwise
	// somebody looks at a Butler that answers questions only and sees a
	// list of things it will refuse.
	readOnly := !b.CarriesActionAuthority()
	authority := &butlerstore.Authority{DB: s.DB, Butler: b.Name}

	var manifest butler.CapabilityManifest
	switch b.AgentClass {
	case runtimedb.AgentClassMember:
		gw := butler.NewHouseholdGateway("", s.ButlerHost.householdSeams()).ForAdministration()
		gw.SetAuthorizer(authority)
		manifest = gw.Capabilities(readOnly)
	case runtimedb.AgentClassOwner:
		// The host's gateway, because its seams are what this
		// deployment actually wired — a bare one would report every
		// tool as unavailable and call it structure. Scoped, never
		// mutated: it is shared with every session in flight.
		_, live, _, _, _ := s.ButlerHost.Current()
		if live == nil {
			live = butler.NewToolGateway(nil, nil)
		}
		// And the household's tools, which an Owner Agent holds too
		// (ADR-W033 B3) — the same union its sessions are built from
		// (butler.OwnerToolset), so this screen lists what it can
		// actually reach and an Owner can narrow a household tool on it.
		member := butler.NewHouseholdGateway("", s.ButlerHost.householdSeams()).ForAdministration()
		member.SetAuthorizer(authority)
		manifest = butler.NewOwnerToolset(live.Scoped(authority).ForAdministration(), member).Capabilities(readOnly)
	default:
		return nil, http.StatusBadRequest, fmt.Errorf(
			"%s is a %s agent, which has no tool surface", b.Name, b.AgentClass)
	}

	// The narrowings come back alongside the manifest rather than being
	// inferred from it. "Unavailable because this Butler is narrowed out
	// of it" and "unavailable because nothing here provides it" are
	// different facts, and only the first is something a person can
	// undo from this screen.
	narrowed, err := s.DB.ButlerToolNarrowings(b.Name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{
		"butler":        b.Name,
		"agentClass":    b.AgentClass,
		"surface":       b.Surface,
		"modelVerdict":  manifest.ModelVerdict,
		"tools":         manifest.Tools,
		"actions":       manifest.Actions,
		"narrowedTools": narrowed,
	}, http.StatusOK, nil
}

// setButlerAddress adds or revokes somewhere a Butler answers.
func (s *Server) setButlerAddress(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Transport  string `json:"transport"`
		ExternalID string `json:"externalId"`
		Label      string `json:"label"`
		Allow      bool   `json:"allow"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var err error
	if req.Allow {
		_, err = s.DB.AddButlerAddress(name, req.Transport, req.ExternalID, req.Label, actingMember(id))
	} else {
		// Scoped to the named Butler. Revoking by (transport, id) alone
		// would let a request naming one Butler revoke a DIFFERENT
		// one's address and then report the named Butler's list back
		// unchanged — a revocation that appears not to have happened,
		// on a Butler nobody was editing.
		err = s.DB.RevokeButlerAddressFor(name, req.Transport, req.ExternalID, actingMember(id))
	}
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	addresses, err := s.DB.ListButlerAddresses(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"butler": name, "addresses": addresses}, http.StatusOK, nil
}

// setButlerAgentClass moves an Agent between the two classes (design
// §5.2, §18.3).
//
// CONSEQUENTIAL, under A4.1, in both directions. A downgrade withdraws
// authority — it revokes this Agent's live owner-class grants — and an
// upgrade widens the gateway the Agent reaches. Neither is a preference,
// and the entitlement writes slice 8 put on the same list are the same
// shape: a change to who may reach what, made from the Console.
//
// Not Plan-mediated, for the reason handlers_entitlements.go gives: a
// plan changes a NODE, and an Agent's class changes no node. The
// reconcile loop needs to hear nothing about it.
func (s *Server) setButlerAgentClass(r *http.Request, id principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant database is configured")
	}
	name := r.PathValue("name")
	var req struct {
		AgentClass string `json:"agentClass"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	revoked, err := s.DB.SetAgentClass(name, req.AgentClass, actingMember(id))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	b, err := s.DB.GetButler(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// The count is returned rather than logged, because "this took four
	// permissions away" is the part of a downgrade an Owner needs to see
	// at the moment they do it. Zero is a real answer too, and a
	// different one from "nothing happened".
	return map[string]any{
		"butler":        b.Name,
		"agentClass":    b.AgentClass,
		"surface":       b.Surface,
		"grantsRevoked": revoked,
	}, http.StatusOK, nil
}
