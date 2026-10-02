package mailmonitor

import (
	"context"
	"fmt"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// ModelRoute is one model the monitor's Agent may extract with.
type ModelRoute struct {
	// Name is the route's name, for health and audit (never content).
	Name string
	// Location is runtimedb.ModelLocal or runtimedb.ModelCloud. Anything
	// else is treated as cloud: only an explicit "local" is local.
	Location string
	// Model is the provider's own model name, sent as ChatRequest.Model.
	Model    string
	Provider provider.Provider
}

// ModelRouter lists the Agent's model routes in its own order (ADR-W030
// A9: fallback moves only along the Agent's own chain). The extractor
// filters them by the source's processing location BEFORE any call, so
// a router cannot widen where mail goes.
type ModelRouter interface {
	ExtractionRoutes(ctx context.Context, agent int64) ([]ModelRoute, error)
}

// eligibleRoutes is the processing-location boundary. A local-only
// source (the default, and anything unrecognised) reaches local routes
// and nothing else — on every path, including fallback after an error.
// A cloud-authorized source may use both, local first.
func eligibleRoutes(routes []ModelRoute, processing string) []ModelRoute {
	var local, cloud []ModelRoute
	for _, r := range routes {
		if r.Provider == nil {
			continue
		}
		if r.Location == runtimedb.ModelLocal {
			local = append(local, r)
		} else {
			cloud = append(cloud, r)
		}
	}
	if processing == runtimedb.ProcessingCloud {
		return append(local, cloud...)
	}
	return local
}

// ButlerModelRouter routes to the monitor Agent's assigned model, the
// only chain entry an Agent has today. A route is local only when the
// model is recorded local AND its provider is not a hosted service: a
// hosted provider is cloud whatever the record says.
type ButlerModelRouter struct {
	DB *runtimedb.DB
	// Build constructs the provider client; nil uses NewProvider.
	Build func(b runtimedb.Butler) (provider.Provider, error)
}

// ExtractionRoutes returns the Agent's route, or none when its model is
// disabled or unavailable.
func (r ButlerModelRouter) ExtractionRoutes(_ context.Context, agent int64) ([]ModelRoute, error) {
	b, err := r.DB.GetButlerBySeq(agent)
	if err != nil {
		return nil, err
	}
	if b.Model == "" || !b.ModelEnabled {
		return nil, nil
	}
	if m, err := r.DB.GetModel(b.Model); err == nil && m.Readiness == runtimedb.ModelUnavailable {
		return nil, nil
	}
	build := r.Build
	if build == nil {
		build = NewProvider
	}
	p, err := build(b)
	if err != nil {
		return nil, err
	}
	location := runtimedb.ModelCloud
	if kind, ok := provider.LookupKind(b.Provider); ok && !kind.Hosted && b.ModelKind == runtimedb.ModelLocal {
		location = runtimedb.ModelLocal
	}
	return []ModelRoute{{Name: b.Model, Location: location, Model: b.ProviderModel, Provider: p}}, nil
}

// NewProvider builds a provider client for an Agent's model, the same
// way Keystone builds the Butler's.
func NewProvider(b runtimedb.Butler) (provider.Provider, error) {
	kind, ok := provider.LookupKind(b.Provider)
	if !ok {
		return nil, fmt.Errorf("mailmonitor: unknown provider %q", b.Provider)
	}
	key := kind.Key()
	if kind.KeyRequired && key == "" {
		return nil, fmt.Errorf("mailmonitor: provider %s has no key", kind.Name)
	}
	endpoint := kind.Endpoint(b.Endpoint)
	switch kind.Client {
	case "anthropic":
		p := provider.NewAnthropicProvider(key)
		if endpoint != "" {
			p.BaseURL = endpoint
		}
		p.Model = b.ProviderModel
		return p, nil
	case "openai":
		if endpoint == "" {
			return nil, fmt.Errorf("mailmonitor: provider %s needs an endpoint", kind.Name)
		}
		p := provider.NewOpenAIProvider(endpoint, key)
		p.Model = b.ProviderModel
		return p, nil
	case "ollama":
		p := provider.NewOllamaProvider(strings.TrimRight(endpoint, "/"))
		p.Model = b.ProviderModel
		return p, nil
	}
	return nil, fmt.Errorf("mailmonitor: provider %s has no client", kind.Name)
}
