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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
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
		"leavesTheHouse": b.RouteKind == runtimedb.RouteCloud,
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
		key := fmt.Sprintf("%s/%t", b.Profile, readOnly)
		set, ok := available[key]
		if !ok {
			set = s.availableTools(b.Profile, readOnly)
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

// availableTools is the set of tool names a profile can actually use on
// this deployment, before any one Butler's narrowings.
//
// It asks the gateway rather than a list, with no Butler authority
// bound — the per-Butler half is subtracted by the caller, from a read
// it already has.
func (s *Server) availableTools(profile string, readOnly bool) map[string]bool {
	var manifest butler.CapabilityManifest
	switch profile {
	case runtimedb.ProfileHousehold:
		manifest = butler.NewHouseholdGateway("", s.ButlerHost.householdSeams()).Capabilities(readOnly)
	case runtimedb.ProfileOperator:
		_, live, _, _, _ := s.ButlerHost.Current()
		if live == nil {
			live = butler.NewToolGateway(nil, nil)
		}
		manifest = live.Scoped(nil).Capabilities(readOnly)
	default:
		return map[string]bool{}
	}
	set := map[string]bool{}
	for _, tool := range manifest.Tools {
		if tool.Available {
			set[tool.Name] = true
		}
	}
	return set
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
		Surface   string `json:"surface"`
		Profile   string `json:"profile"`
		Route     string `json:"route"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	// CreateButler trims the name, so the read-back has to use the
	// trimmed one. Re-reading what the caller sent returns 500 for a
	// Butler that was in fact created.
	name := strings.TrimSpace(req.Name)
	if _, err := s.DB.CreateButler(runtimedb.Butler{
		Name: name, Directive: req.Directive,
		Surface: runtimedb.ButlerSurface(req.Surface), Profile: req.Profile, Route: req.Route,
	}, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, err
		}
		return nil, http.StatusBadRequest, err
	}
	b, err := s.DB.GetButler(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return butlerView(b), http.StatusCreated, nil
}

// setButlerRoute changes which model a Butler runs on, immediately.
//
// The response carries the crossing rather than a confirmation prompt.
// A local-to-cloud move with a conversation already open sends that
// conversation's history to the new model on the next message, and the
// person making the choice is owed that fact in the moment they make
// it — as a statement of what happens, not a dialog standing between
// them and a control they own.
func (s *Server) setButlerRoute(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Route string `json:"route"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	from, to, err := s.DB.SetButlerRoute(name, req.Route, actingMember(id))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	out := butlerView(to)
	out["previousModel"] = from.Route
	if runtimedb.CrossesToCloud(from, to) {
		out["crossesToCloud"] = true
		out["warning"] = fmt.Sprintf(
			"%s now runs on %s, which is outside the house. An open conversation's history goes to it with the next message.",
			to.Name, to.Route)
	}
	return out, http.StatusOK, nil
}

func (s *Server) retireButler(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	if err := s.DB.RetireButler(name, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no butler named %q, or it is already retired", name)
		}
		return nil, http.StatusBadRequest, err
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
	switch b.Profile {
	case runtimedb.ProfileHousehold:
		gw := butler.NewHouseholdGateway("", s.ButlerHost.householdSeams())
		gw.SetAuthorizer(authority)
		manifest = gw.Capabilities(readOnly)
	case runtimedb.ProfileOperator:
		// The host's gateway, because its seams are what this
		// deployment actually wired — a bare one would report every
		// tool as unavailable and call it structure. Scoped, never
		// mutated: it is shared with every session in flight.
		_, live, _, _, _ := s.ButlerHost.Current()
		if live == nil {
			live = butler.NewToolGateway(nil, nil)
		}
		manifest = live.Scoped(authority).Capabilities(readOnly)
	default:
		return nil, http.StatusBadRequest, fmt.Errorf(
			"%s runs on the %s profile, which has no tool surface", b.Name, b.Profile)
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
		"profile":       b.Profile,
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
