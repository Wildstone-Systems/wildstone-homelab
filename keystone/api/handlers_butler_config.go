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
	"errors"
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
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
	prov := r.URL.Query().Get("provider")
	if endpoint == "" && prov == "" && s.ButlerHost != nil {
		endpoint = s.ButlerHost.Endpoint
	}
	if endpoint == "" && prov == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("endpoint required (no Butler endpoint is configured to fall back to)")
	}
	out, resolved, code, err := offeredModels(r.Context(), prov, endpoint)
	if err != nil {
		return nil, code, err
	}
	return map[string]any{"endpoint": resolved, "models": out}, http.StatusOK, nil
}

// providerKindView is one provider kind as the Console offers it:
// the catalog entry, and whether its key is present. Never the key.
type providerKindView struct {
	provider.Kind
	KeySet bool `json:"keySet"`
	// KeySource is where the key comes from: "environment", "console"
	// or "". The Console can replace a "console" key and must say that
	// an "environment" one is changed where it was set.
	KeySource string `json:"keySource,omitempty"`
}

// listProviderKinds answers which providers a Butler can think with and,
// for each hosted one, whether keystone has its key (ADR-W017 A9: a
// provider without its key reports that, and the key never leaves the
// environment). The Console's provider drawer is built from this, so it
// offers exactly what the factory can build.
func (s *Server) listProviderKinds(_ *http.Request, _ principal.Identity) (any, int, error) {
	out := make([]providerKindView, 0, len(provider.Kinds))
	for _, k := range provider.Kinds {
		out = append(out, providerKindView{Kind: k, KeySet: k.KeySet(), KeySource: k.KeySource()})
	}
	return map[string]any{"providers": out}, http.StatusOK, nil
}

// setProviderKey stores a hosted provider's key entered in the Console
// (ADR-W017 A10). Write-only: the answer says the key is set and from
// where, never what it is; the event names the provider and who, never
// the value; and the request body is the only place the value travels.
func (s *Server) setProviderKey(r *http.Request, id principal.Identity) (any, int, error) {
	kind, ok := provider.LookupKind(r.PathValue("name"))
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("no provider called %q", r.PathValue("name"))
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<10)).Decode(&req); err != nil {
		// Never the decoder's own message: it can quote the body.
		return nil, http.StatusBadRequest, fmt.Errorf("send the key as {\"key\": \"...\"}")
	}
	if err := provider.SetKey(kind, req.Key); err != nil {
		if errors.Is(err, provider.ErrNoKeyStore) {
			return nil, http.StatusServiceUnavailable, err
		}
		return nil, http.StatusBadRequest, err
	}
	s.appendEvent("provider.key_set", "", kind.Name, actingMember(id))
	out := map[string]any{"name": kind.Name, "keySet": true, "keySource": kind.KeySource()}
	if kind.KeySource() == "environment" {
		out["note"] = kind.KeyEnv + " is set in keystone's environment, and that one is used until it is removed there"
	}
	return out, http.StatusOK, nil
}

// removeProviderKey forgets a key entered in the Console. One set in the
// environment is not the Console's to remove, and stays.
func (s *Server) removeProviderKey(r *http.Request, id principal.Identity) (any, int, error) {
	kind, ok := provider.LookupKind(r.PathValue("name"))
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("no provider called %q", r.PathValue("name"))
	}
	if err := provider.RemoveKey(kind); err != nil {
		return nil, http.StatusServiceUnavailable, err
	}
	s.appendEvent("provider.key_removed", "", kind.Name, actingMember(id))
	return map[string]any{"name": kind.Name, "keySet": kind.KeySet(), "keySource": kind.KeySource()}, http.StatusOK, nil
}

// offeredModels asks one provider what it serves.
//
// Shared by the listing the Console renders and by the assignment path,
// so a model's local/cloud verdict is drawn from the same evidence in
// both — the provider's own listing. A second implementation is a
// second chance to be wrong about the one fact that decides whether
// material leaves the house.
//
// Each kind lists the way it speaks (provider.Kinds): Ollama's
// /api/tags, an OpenAI-compatible server's /models, Anthropic's
// /v1/models — the hosted ones with the key from the environment, which
// is sent to the provider and nowhere else. An empty provider is Ollama,
// which is what every caller meant before there was a choice.
func offeredModels(ctx context.Context, providerName, endpoint string) ([]butlerModel, string, int, error) {
	if strings.TrimSpace(providerName) == "" {
		providerName = "ollama"
	}
	kind, ok := provider.LookupKind(providerName)
	if !ok {
		return nil, "", http.StatusBadRequest, fmt.Errorf("unknown provider %q", providerName)
	}
	endpoint = kind.Endpoint(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", http.StatusBadRequest, fmt.Errorf("endpoint must be an http(s) URL, got %q", endpoint)
	}
	if kind.KeyRequired && !kind.KeySet() {
		return nil, "", http.StatusBadRequest, fmt.Errorf(
			"%s needs its key: set %s in keystone's environment (/etc/wildstone/keystone.env) and restart keystone", kind.Label, kind.KeyEnv)
	}
	switch kind.Client {
	case "openai":
		return listOpenAIModels(ctx, kind, u)
	case "anthropic":
		return listAnthropicModels(ctx, kind, u)
	default:
		return listOllamaModels(ctx, u)
	}
}

// listOllamaModels asks an Ollama server what it has pulled.
func listOllamaModels(ctx context.Context, u *url.URL) ([]butlerModel, string, int, error) {
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

// listOpenAIModels asks an OpenAI-compatible server (OpenAI, OpenRouter,
// LM Studio, vLLM…) what it serves: GET <base>/models.
//
// A hosted kind's models are all cloud. A self-hosted OpenAI-compatible
// server is local when its address is on this network — the one fact the
// listing cannot state, so it is drawn from where the server is.
func listOpenAIModels(ctx context.Context, kind provider.Kind, u *url.URL) ([]butlerModel, string, int, error) {
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	headers := map[string]string{}
	if key := kind.Key(); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	if code, err := getProviderJSON(ctx, kind, strings.TrimRight(u.String(), "/")+"/models", headers, &body); err != nil {
		return nil, "", code, err
	}
	local := !kind.Hosted && privateHost(u.Hostname())
	out := make([]butlerModel, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, butlerModel{Name: m.ID, Local: local})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, u.String(), http.StatusOK, nil
}

// listAnthropicModels asks Anthropic what it serves: GET /v1/models.
func listAnthropicModels(ctx context.Context, kind provider.Kind, u *url.URL) ([]butlerModel, string, int, error) {
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	headers := map[string]string{"x-api-key": kind.Key(), "anthropic-version": "2023-06-01"}
	if code, err := getProviderJSON(ctx, kind, strings.TrimRight(u.String(), "/")+"/v1/models?limit=1000", headers, &body); err != nil {
		return nil, "", code, err
	}
	out := make([]butlerModel, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, butlerModel{Name: m.ID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, u.String(), http.StatusOK, nil
}

// getProviderJSON is one bounded GET to a provider, decoded into into.
// The status it returns is the one this API should answer with: a
// provider that refuses the key is the operator's to fix (401 from the
// provider becomes a 400 naming the variable), anything else is a
// gateway failure.
func getProviderJSON(ctx context.Context, kind provider.Kind, target string, headers map[string]string, into any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return http.StatusBadGateway, fmt.Errorf("could not reach %s from this machine: %w", req.URL.Host, err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		if kind.KeyEnv != "" {
			return http.StatusBadRequest, fmt.Errorf("%s refused the key in %s (%s)", kind.Label, kind.KeyEnv, res.Status)
		}
		return http.StatusBadRequest, fmt.Errorf("%s wants a key (%s)", req.URL.Host, res.Status)
	case res.StatusCode != http.StatusOK:
		return http.StatusBadGateway, fmt.Errorf("%s answered %s when asked for its models", req.URL.Host, res.Status)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(into); err != nil {
		return http.StatusBadGateway, fmt.Errorf("%s did not answer with a model list: %w", req.URL.Host, err)
	}
	return http.StatusOK, nil
}

// privateHost reports whether a host is on this machine or this network:
// loopback, RFC 1918, link-local, the tailnet's CGNAT range, or a
// single-label or .local/.lan name. Wrong in the safe direction: an
// unrecognised public name is not private.
func privateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return true
		}
		_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
		return cgnat.Contains(ip)
	}
	h := strings.ToLower(host)
	return h == "localhost" || !strings.Contains(h, ".") || strings.HasSuffix(h, ".local") ||
		strings.HasSuffix(h, ".lan") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".ts.net")
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

// butlerProviders is every provider a choice may name: "off" and the
// catalog (provider.Kinds).
var butlerProviders = func() map[string]bool {
	m := map[string]bool{"off": true}
	for _, k := range provider.Kinds {
		m[k.Name] = true
	}
	return m
}()

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
