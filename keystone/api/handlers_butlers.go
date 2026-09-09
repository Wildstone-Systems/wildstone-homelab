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

func (s *Server) listButlers(_ *http.Request, _ principal.Identity) (any, int, error) {
	all, err := s.DB.ListButlers()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]map[string]any, 0, len(all))
	for _, b := range all {
		out = append(out, butlerView(b))
	}
	return map[string]any{"butlers": out}, http.StatusOK, nil
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
	seq, err := s.DB.CreateButler(runtimedb.Butler{
		Name: req.Name, Directive: req.Directive,
		Surface: runtimedb.ButlerSurface(req.Surface), Profile: req.Profile, Route: req.Route,
	}, actingMember(id))
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	b, err := s.DB.GetButler(req.Name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	_ = seq
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
		err = s.DB.RevokeButlerAddress(req.Transport, req.ExternalID, actingMember(id))
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
