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
