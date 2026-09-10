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

// listOfferedModels asks a backend what it can actually run.
//
// The endpoint is a query parameter rather than the configured one, so
// an operator can see a candidate's models BEFORE committing to it —
// choosing a model you cannot see is how a typo becomes a support
// question.
func (s *Server) listOfferedModels(r *http.Request, _ principal.Identity) (any, int, error) {
	endpoint := r.URL.Query().Get("endpoint")
	if endpoint == "" && s.ButlerHost != nil {
		endpoint = s.ButlerHost.Endpoint
	}
	if endpoint == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("endpoint required (no Butler endpoint is configured to fall back to)")
	}
	out, resolved, code, err := offeredModels(r.Context(), endpoint)
	if err != nil {
		return nil, code, err
	}
	return map[string]any{"endpoint": resolved, "models": out}, http.StatusOK, nil
}

// offeredModels asks one provider endpoint what it serves.
//
// Shared by the listing the Console renders and by the assignment path,
// so a model's local/cloud verdict is drawn from the same evidence in
// both — the provider's own listing, with the real size. A second
// implementation is a second chance to be wrong about the one fact that
// decides whether material leaves the house.
func offeredModels(ctx context.Context, endpoint string) ([]butlerModel, string, int, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", http.StatusBadRequest, fmt.Errorf("endpoint must be an http(s) URL, got %q", endpoint)
	}

	// Bounded: an operator typing an address should get an answer or a
	// clear failure, not a hung Console tab.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.String(), "/")+"/api/tags", nil)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// The operator's most likely mistake is an address this host
		// cannot reach, so say that rather than "failed".
		return nil, "", http.StatusBadGateway,
			fmt.Errorf("could not reach %s from this machine: %w", u.Host, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", http.StatusBadGateway,
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
		return nil, "", http.StatusBadGateway, fmt.Errorf("%s did not answer with a model list: %w", u.Host, err)
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
	return out, u.String(), http.StatusOK, nil
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
	// DisallowUnknownFields, so a credential sent in the BODY is refused
	// rather than silently dropped (A4, unrelaxed by A9).
	//
	// The first version of this checked the URL's query parameters,
	// which is not where anybody would put one: a decoder that discards
	// unknown fields accepted {"provider":"openai","apiKey":"..."}
	// without a word, so the operator learned nothing and believed they
	// had configured a key. Refusing loudly is the whole claim; checking
	// the wrong place made it false.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if f, isCred := credentialField(err); isCred {
			return nil, http.StatusBadRequest, fmt.Errorf(
				"%q is not accepted here and is never stored: set ANTHROPIC_API_KEY or OPENAI_API_KEY "+
					"in keystone's environment instead (ADR-W017 A4)", f)
		}
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	if !butlerProviders[req.Provider] {
		return nil, http.StatusBadRequest,
			fmt.Errorf("%q is not a provider (want off, anthropic, openai or ollama)", req.Provider)
	}

	if s.ApplyButlerConfig == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this control plane cannot change the Butler at runtime")
	}

	stored := runtimedb.ButlerConfig{
		Provider: req.Provider, Model: req.Model, Endpoint: req.Endpoint, UpdatedBy: id.Name,
	}
	// APPLIED BEFORE IT IS PERSISTED, and the order is the whole point.
	//
	// Storing first looked safer — a control plane dying between the two
	// would come back with the operator's choice. It is much worse: a
	// choice that cannot be built (anthropic with no ANTHROPIC_API_KEY,
	// say) would be persisted, take precedence at startup, and
	// newButlerProvider's error would stop Keystone from starting AT
	// ALL. A misconfigured Butler must never cost the household its
	// control plane.
	//
	// So an unusable choice is refused here, while the previous provider
	// keeps answering and nothing durable has changed.
	mode, detail, err := s.ApplyButlerConfig(r.Context(), stored)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("not saved: %w", err)
	}
	if err := s.DB.SetButlerConfig(stored); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := s.DB.RecordButlerProbe(string(mode), detail); err != nil {
		s.Log.Error("recording butler probe", "err", err)
	}
	out, _, _ := s.DB.GetButlerConfig()
	return map[string]any{"stored": true, "config": out}, http.StatusOK, nil
}

var butlerProviders = map[string]bool{"off": true, "anthropic": true, "openai": true, "ollama": true}

// credentialField names the rejected field when an unknown-field error
// is about something credential-shaped, so the refusal can say what to
// do instead of only that the body was wrong.
func credentialField(err error) (string, bool) {
	msg := err.Error()
	const marker = `unknown field "`
	i := strings.Index(msg, marker)
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return "", false
	}
	field := rest[:j]
	lower := strings.ToLower(field)
	for _, k := range []string{"key", "token", "secret", "password", "credential"} {
		if strings.Contains(lower, k) {
			return field, true
		}
	}
	return field, false
}
