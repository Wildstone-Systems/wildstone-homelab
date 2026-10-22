package research

import (
	"context"
	"time"
)

// Code is a bounded, non-sensitive adapter failure classification. Raw provider
// errors must never escape through Error, even for diagnostic callers.
type Code string

const (
	InvalidInput        Code = "invalid_input"
	Unsupported         Code = "unsupported"
	ProviderUnavailable Code = "provider_unavailable"
	RateLimited         Code = "rate_limited"
	Timeout             Code = "timeout"
	Cancelled           Code = "cancelled"
	InternalError       Code = "internal_error"
)

type Error struct{ Code Code }

func (e *Error) Error() string { return string(e.Code) + ": research request failed" }
func failure(code Code) error  { return &Error{Code: code} }
func contextError(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return failure(Timeout)
	}
	if ctx.Err() != nil {
		return failure(Cancelled)
	}
	return failure(ProviderUnavailable)
}

// Capabilities advertises only filters/formats actually implemented. Regions
// further narrows the region filter to the pinned worker's exact allowlist.
type Capabilities struct {
	SupportedFilters []string
	Regions          []string
	Formats          []string
}

type SearchFilters struct {
	Region      *string `json:"region,omitempty"`
	Language    *string `json:"language,omitempty"`
	RecencyDays *int    `json:"recencyDays,omitempty"`
}
type SearchRequest struct {
	Query   string
	Filters SearchFilters
	Limit   int // zero selects the contract default of five
}

// Source is untrusted evidence, not a persisted source reference or authority.
// The later evidence/tool layer assigns IDs and revisions after retaining it.
type Source struct {
	URL         string
	Title       string
	Provider    ProviderKind
	Backend     string
	RetrievedAt time.Time
	SourceDate  *time.Time
}
type SearchHit struct {
	Source
	Snippet string
}

// Attempt records every dispatched attempt, including failed/cancelled ones.
// Empty Code denotes success (including zero hits). No query or endpoint leaks.
type Attempt struct {
	Provider ProviderKind
	Backend  string
	Code     Code
}
type SearchResult struct {
	Items       []SearchHit
	Provider    ProviderKind
	Backend     string
	RetrievedAt time.Time
	Attempts    []Attempt
}

// SearchAdapter performs bounded cancellable search, never fetches result URLs.
// Callers own current authorization, egress provenance, attempt admission and
// persistence. This low-level seam neither registers tools nor grants authority.
type SearchAdapter interface {
	Capabilities() Capabilities
	Search(context.Context, SearchRequest) (SearchResult, error)
}

// ExtractionAdapter is deliberately separate from search and internal provider
// transport. Public-fetch/extraction implementations must enforce their own
// SSRF, byte, media-type and text limits; DDGS does not implement this interface.
type ExtractionRequest struct{ URL string }
type ExtractionResult struct {
	Source    Source
	MediaType string
	Text      string
	Truncated bool
}
type ExtractionAdapter interface {
	Capabilities() Capabilities
	Extract(context.Context, ExtractionRequest) (ExtractionResult, error)
}

// EndpointResolver resolves trusted Owner configuration references, never model
// URLs. Implementations must honor cancellation. Managed service wiring belongs
// to S20; absence of a resolver remains fail-closed.
type EndpointResolver func(context.Context, string) (string, error)

type Factory struct{ ResolveEndpoint EndpointResolver }

// NewSearch constructs an adapter for an already selected registry snapshot.
// A fresh registry selection is required for every call at the eventual gateway.
func (f Factory) NewSearch(ctx context.Context, p Provider) (SearchAdapter, error) {
	if p.State != "ready" || p.ConfigRevision == "" || p.TestedConfigRevision != p.ConfigRevision {
		return nil, failure(ProviderUnavailable)
	}
	return f.build(ctx, p)
}

// Test matches api.Server.ResearchTest. An explicit Owner test may probe untested
// configuration. It uses a fixed synthetic query, never treats /health as proof
// of upstream readiness, and does not mutate registry state itself.
func (f Factory) Test(ctx context.Context, p Provider) error {
	limits := DefaultLimits()
	if p.Limits != nil {
		limits = *p.Limits
	}
	if limits.RequestTimeoutSeconds < 1 || limits.RequestTimeoutSeconds > 30 {
		return failure(InvalidInput)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(limits.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	a, err := f.build(ctx, p)
	if err != nil {
		return err
	}
	_, err = a.Search(ctx, SearchRequest{Query: "example documentation", Limit: 1})
	return err
}

func (f Factory) build(ctx context.Context, p Provider) (SearchAdapter, error) {
	if p.Kind != DDGS {
		return nil, failure(Unsupported)
	}
	c, err := p.Config.Normalize()
	if err != nil {
		return nil, failure(Unsupported)
	}
	if c.EndpointRef == "" || f.ResolveEndpoint == nil {
		return nil, failure(ProviderUnavailable)
	}
	// The pinned worker accepts no credentials. Refuse rather than silently drop
	// an Owner-configured credential or load a secret into an unauthenticated wire.
	if c.CredentialRef != "" {
		return nil, failure(Unsupported)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Limits.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return nil, contextError(ctx)
	}
	endpoint, err := f.ResolveEndpoint(ctx, c.EndpointRef)
	if ctx.Err() != nil {
		return nil, contextError(ctx)
	}
	if err != nil {
		return nil, failure(ProviderUnavailable)
	}
	return newDDGS(endpoint, time.Duration(c.Limits.RequestTimeoutSeconds)*time.Second)
}
