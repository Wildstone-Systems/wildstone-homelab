package api

// Configuring the Butler from the Console (ADR-W017 A4, F92).
//
// The provider, model and endpoint were startup flags only. An operator
// who wanted to point the Butler at their own Ollama had to edit
// /etc/wildstone/keystone.env and restart Keystone — and had no way to
// discover what models that endpoint offered, so the model name was
// typed from memory or from a terminal on another machine.
//
// Two endpoints close that: one lists what a backend actually has, and
// one records the choice. Neither invents a provider Keystone cannot
// already build.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
)

// butlerModel is one model a backend reports.
type butlerModel struct {
	Name string `json:"name"`
	// SizeBytes is 0 for a cloud-backed model, which is a real
	// distinction an operator wants: it is the difference between
	// "runs on the machine in the cupboard" and "leaves the house".
	SizeBytes  int64  `json:"sizeBytes,omitempty"`
	Parameters string `json:"parameters,omitempty"`
	Local      bool   `json:"local"`
}

// listButlerModels asks a backend what it can actually run.
//
// The endpoint is a query parameter rather than the configured one, so
// an operator can see a candidate's models BEFORE committing to it —
// choosing a model you cannot see is how a typo becomes a support
// question.
func (s *Server) listButlerModels(r *http.Request, _ principal.Identity) (any, int, error) {
	endpoint := r.URL.Query().Get("endpoint")
	if endpoint == "" && s.ButlerHost != nil {
		endpoint = s.ButlerHost.Endpoint
	}
	if endpoint == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("endpoint required (no Butler endpoint is configured to fall back to)")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, http.StatusBadRequest, fmt.Errorf("endpoint must be an http(s) URL, got %q", endpoint)
	}

	// Bounded: an operator typing an address should get an answer or a
	// clear failure, not a hung Console tab.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.String(), "/")+"/api/tags", nil)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// The operator's most likely mistake is an address this host
		// cannot reach, so say that rather than "failed".
		return nil, http.StatusBadGateway,
			fmt.Errorf("could not reach %s from this machine: %w", u.Host, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, http.StatusBadGateway,
			fmt.Errorf("%s answered %s — is it an Ollama endpoint?", u.Host, res.Status)
	}

	var body struct {
		Models []struct {
			Name    string `json:"name"`
			Size    int64  `json:"size"`
			Details struct {
				ParameterSize string `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("%s did not answer with a model list: %w", u.Host, err)
	}

	out := make([]butlerModel, 0, len(body.Models))
	for _, m := range body.Models {
		out = append(out, butlerModel{
			Name:       m.Name,
			SizeBytes:  m.Size,
			Parameters: m.Details.ParameterSize,
			Local:      looksLocal(m.Name, m.Size),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return map[string]any{"endpoint": u.String(), "models": out}, http.StatusOK, nil
}

// looksLocal distinguishes a model that runs on this hardware from one
// Ollama proxies to a hosted backend.
//
// A household running its own machine may care a great deal which of
// these leaves the house, so the distinction is worth drawing — but it
// has to be drawn honestly, because Ollama does not state it outright.
//
// Size alone is not the signal, which is what the first attempt got
// wrong: a cloud entry is not zero bytes, it is a stub manifest of a few
// hundred (317-384 observed on a live endpoint), so "size > 0" reported
// every cloud model as local. Both signals are used, and neither is
// authoritative on its own: Ollama's own ":cloud"/"-cloud" naming, and a
// size no real weights file could be.
//
// Wrong in the safe direction by design. An unrecognised naming scheme
// gets reported as NOT local, so an operator is warned about a model
// that turns out to be local rather than reassured about one that is
// not.
func looksLocal(name string, size int64) bool {
	if strings.Contains(strings.ToLower(name), "cloud") {
		return false
	}
	const smallestPlausibleWeights = 1 << 20 // 1 MiB
	return size >= smallestPlausibleWeights
}

// getButlerConfig serves the stored choice, if any.
//
// Absent is a normal state, not an error: a deployment running on its
// startup flags has never stored anything, and A9 keeps those flags as
// the bootstrap.
func (s *Server) getButlerConfig(_ *http.Request, _ principal.Identity) (any, int, error) {
	c, ok, err := s.DB.GetButlerConfig()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"stored": ok, "config": c}, http.StatusOK, nil
}

// setButlerConfig records a provider choice and applies it without
// restarting Keystone (ADR-W017 A9).
func (s *Server) setButlerConfig(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	if !butlerProviders[req.Provider] {
		return nil, http.StatusBadRequest,
			fmt.Errorf("%q is not a provider (want off, anthropic, openai or ollama)", req.Provider)
	}
	// An API key is never accepted here (A4, unrelaxed by A9). Refusing
	// loudly beats ignoring quietly: an operator who tried to send one
	// should learn it belongs in the environment, not discover later
	// that the field they filled in did nothing.
	if looksLikeCredential(r) {
		return nil, http.StatusBadRequest,
			fmt.Errorf("API keys are not accepted here and are never stored; set ANTHROPIC_API_KEY or OPENAI_API_KEY in keystone's environment (ADR-W017 A4)")
	}
	if s.ApplyButlerConfig == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this control plane cannot change the Butler at runtime")
	}

	stored := runtimedb.ButlerConfig{
		Provider: req.Provider, Model: req.Model, Endpoint: req.Endpoint, UpdatedBy: id.Name,
	}
	if err := s.DB.SetButlerConfig(stored); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Applied AFTER the write, so a control plane that restarts between
	// the two comes back with the choice the operator made rather than
	// the one it was running.
	mode, detail, err := s.ApplyButlerConfig(r.Context(), stored)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("stored, but could not be applied: %w", err)
	}
	if err := s.DB.RecordButlerProbe(string(mode), detail); err != nil {
		s.Log.Error("recording butler probe", "err", err)
	}
	out, _, _ := s.DB.GetButlerConfig()
	return map[string]any{"stored": true, "config": out}, http.StatusOK, nil
}

var butlerProviders = map[string]bool{"off": true, "anthropic": true, "openai": true, "ollama": true}

// looksLikeCredential reports a body carrying something that should
// never travel to this endpoint.
func looksLikeCredential(r *http.Request) bool {
	for _, k := range []string{"apiKey", "api_key", "token", "secret", "password"} {
		if r.URL.Query().Has(k) {
			return true
		}
	}
	return false
}
