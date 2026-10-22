package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func (s *Server) listPrincipals(_ *http.Request, _ principal.Identity) (any, int, error) {
	principals, err := s.DB.ListPrincipals()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return principals, http.StatusOK, nil
}

func (s *Server) createPrincipal(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Name      string `json:"name"`
		Role      string `json:"role"`
		ActingFor string `json:"actingFor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	if err := s.Principals.Create(req.Name, runtimedb.Role(req.Role), req.ActingFor); err != nil {
		return nil, http.StatusBadRequest, err
	}
	s.event("principal.created", "", id, req.Name+" ("+req.Role+")")
	p, err := s.DB.GetPrincipal(req.Name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return p, http.StatusCreated, nil
}

func (s *Server) setPrincipalDisabled(disabled bool) handlerFunc {
	return func(r *http.Request, id principal.Identity) (any, int, error) {
		name := r.PathValue("name")
		if disabled && name == id.Name {
			// An admin locking themself out is unrecoverable through the
			// API (bootstrap only runs on an empty principals table).
			return nil, http.StatusBadRequest, fmt.Errorf("cannot disable the calling principal")
		}
		if err := s.DB.SetPrincipalDisabled(name, disabled); err != nil {
			if errors.Is(err, runtimedb.ErrNotFound) {
				return nil, http.StatusNotFound, fmt.Errorf("principal not found")
			}
			return nil, http.StatusInternalServerError, err
		}
		verb := "principal.enabled"
		if disabled {
			verb = "principal.disabled"
		}
		s.event(verb, "", id, name)
		return nil, http.StatusOK, nil
	}
}

func (s *Server) mintToken(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		ActingFor  string `json:"actingFor"`
		TTLSeconds int64  `json:"ttlSeconds"`
	}
	// An empty body is a valid "defaults" mint (io.EOF from the decoder).
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	if req.TTLSeconds < 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("ttlSeconds must be >= 0")
	}
	secret, err := s.Principals.Mint(name, req.ActingFor, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("principal not found")
		}
		return nil, http.StatusBadRequest, err
	}
	s.event("token.minted", "", id, "for "+name)
	// mintResponse skips the redacting encoder — the ONE moment a secret
	// legitimately appears in a response (ADR-W007 A3/B3).
	return mintResponse{Token: secret}, http.StatusCreated, nil
}
