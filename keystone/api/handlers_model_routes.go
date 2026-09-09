package api

// Model routes from the Console (ADR-W030 A9/A9.1, work package P14).
//
// Which model answers, and what may reach it, is a household decision
// with a boundary attached. The endpoints here let an operator name
// routes, record probe verdicts against them, and turn them off — and
// deliberately do not let anyone change a route's KIND, because that
// would move the data boundary for every policy referring to the route
// by name.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func (s *Server) listModelRoutes(_ *http.Request, _ principal.Identity) (any, int, error) {
	routes, err := s.DB.ListModelRoutes()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Report the authority each route carries rather than making a
	// reader derive it from readiness: "answers questions only" is the
	// thing an operator actually wants to see.
	//
	// A route is now the model a Butler is ASSIGNED to (ADR-W031 A2),
	// not one of several a router might pick between — so there is
	// nothing here about capacity or priority, because nothing selects.
	out := make([]map[string]any, 0, len(routes))
	for _, r := range routes {
		out = append(out, map[string]any{
			"route":          r,
			"carriesActions": r.CarriesActionAuthority(),
			"leavesTheHouse": r.Kind == runtimedb.RouteCloud,
		})
	}
	return map[string]any{"routes": out}, http.StatusOK, nil
}

// upsertModelRoute records a route. No API key here and never will be,
// for the reason ADR-W017 A4 gives about the Butler config: what became
// editable is which model to use, not the secret used to reach it.
func (s *Server) upsertModelRoute(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Endpoint string `json:"endpoint"`
		Enabled  bool   `json:"enabled"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		if f, isCred := credentialField(err); isCred {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"%q is not accepted here and is never stored: set the provider's key in keystone's "+
					"environment instead (ADR-W017 A4)", f)
		}
		return nil, code, err
	}
	if err := s.DB.UpsertModelRoute(runtimedb.ModelRoute{
		Name: req.Name, Kind: req.Kind, Provider: req.Provider, Model: req.Model,
		Endpoint: req.Endpoint, Enabled: req.Enabled,
	}, actingMember(id)); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"name": req.Name,
		"note": "a new route is unprobed, so it answers questions and carries no authority to use " +
			"tools that do things until something has actually checked it",
	}, http.StatusOK, nil
}

// recordRouteReadiness stores a probe verdict for one route.
func (s *Server) recordRouteReadiness(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Readiness string `json:"readiness"`
		Detail    string `json:"detail"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.RecordRouteReadiness(name, req.Readiness, req.Detail); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no route named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"name": name, "readiness": req.Readiness}, http.StatusOK, nil
}

// setModelRouteEnabled turns a route off without losing it.
func (s *Server) setModelRouteEnabled(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.SetModelRouteEnabled(name, req.Enabled, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no route named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"name": name, "enabled": req.Enabled}, http.StatusOK, nil
}
