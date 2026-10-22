package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func (s *Server) whoami(_ *http.Request, id principal.Identity) (any, int, error) {
	return id, http.StatusOK, nil
}

// resourceView is how the API projects one Statefile resource: the
// identifying metadata as fields, the full spec as its canonical YAML
// document — one schema (the Statefile's), no parallel JSON codec.
type resourceView struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
	// Package is which package produced this resource. Owner names the
	// CAPABILITY, and two profiles of one capability share it, so this
	// is what lets a caller act on one profile without touching its
	// sibling. Empty on resources written before the field existed.
	Package string `json:"package,omitempty"`
	Node    string `json:"node,omitempty"`
	YAML    string `json:"yaml"`
}

func viewOf(r resource.Resource) (resourceView, error) {
	data, err := resource.Marshal(r)
	if err != nil {
		return resourceView{}, err
	}
	return resourceView{
		Kind:    string(r.Kind),
		Name:    r.Metadata.Name,
		Owner:   r.Metadata.Owner,
		Package: r.Metadata.Package,
		Node:    r.Metadata.Node,
		YAML:    string(data),
	}, nil
}

func (s *Server) listResources(_ *http.Request, _ principal.Identity) (any, int, error) {
	resources, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	views := make([]resourceView, 0, len(resources))
	for _, r := range resources {
		v, err := viewOf(r)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		views = append(views, v)
	}
	return views, http.StatusOK, nil
}

func (s *Server) getResource(r *http.Request, _ principal.Identity) (any, int, error) {
	kind := resource.Kind(r.PathValue("kind"))
	name := r.PathValue("name")
	if !resource.KnownKind(kind) {
		return nil, http.StatusNotFound, fmt.Errorf("unknown kind %q", kind)
	}
	resources, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	for _, res := range resources {
		if res.Kind == kind && res.Metadata.Name == name {
			v, err := viewOf(res)
			if err != nil {
				return nil, http.StatusInternalServerError, err
			}
			return v, http.StatusOK, nil
		}
	}
	return nil, http.StatusNotFound, fmt.Errorf("%s/%s not found", kind, name)
}

func (s *Server) listObserved(_ *http.Request, _ principal.Identity) (any, int, error) {
	obs, err := s.DB.ListObserved()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return obs, http.StatusOK, nil
}

// listEvents serves the event stream with an `after` cursor and an
// optional long-poll (`waitMs`, capped): with a wait, an empty result
// blocks until an event lands past the cursor or the wait elapses —
// enough for a live Console view without a push transport (ADR-W007 K3).
func (s *Server) listEvents(r *http.Request, _ principal.Identity) (any, int, error) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	waitMs, _ := strconv.Atoi(r.URL.Query().Get("waitMs"))
	events, err := longPoll(r, waitMs, func() ([]runtimedb.Event, error) {
		return s.DB.EventsAfter(after, limit)
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return events, http.StatusOK, nil
}

func (s *Server) listAudit(r *http.Request, _ principal.Identity) (any, int, error) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	records, err := s.DB.ListAudit(limit)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return records, http.StatusOK, nil
}
