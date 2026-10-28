// Package oauthresource validates an external Agent's OAuth resource token at
// Keystone's authentication boundary (ADR-W049). It does not grant task or
// capability authority; successful resolution produces the existing runtime
// principal context consumed by the canonical Agent API.
package oauthresource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

var (
	ErrUnauthenticated   = errors.New("oauth resource: unauthenticated")
	ErrInsufficientScope = errors.New("oauth resource: insufficient scope")
)

// Claims is the closed subset of RFC 7662 output accepted by ADR-W049.
type Claims struct {
	Active          bool
	Issuer          string
	Subject         string
	ClientID        string
	Audience        []string
	Scope           []string
	TokenType       string
	TokenUse        string
	ExpiresAt       time.Time
	CredentialEpoch int64
}

// Introspector is deliberately narrow so contract tests use a synthetic
// issuer without teaching the resource boundary another identity system.
type Introspector interface {
	Introspect(context.Context, string) (Claims, error)
}

// Provider consumes RFC 8414 metadata and RFC 7662 introspection. ClientSecret
// is runtime configuration only; it is never returned, logged, or persisted.
type Provider struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	Now          func() time.Time

	mu       sync.Mutex
	metadata authorizationServerMetadata
	readAt   time.Time
}

type authorizationServerMetadata struct {
	Issuer                string `json:"issuer"`
	IntrospectionEndpoint string `json:"introspection_endpoint"`
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (p *Provider) discover(ctx context.Context) (authorizationServerMetadata, error) {
	p.mu.Lock()
	if p.metadata.IntrospectionEndpoint != "" && p.now().Sub(p.readAt) < time.Hour {
		m := p.metadata
		p.mu.Unlock()
		return m, nil
	}
	p.mu.Unlock()

	issuer := strings.TrimRight(p.Issuer, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return authorizationServerMetadata{}, err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return authorizationServerMetadata{}, fmt.Errorf("reading authorization-server metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return authorizationServerMetadata{}, fmt.Errorf("reading authorization-server metadata: status %d", resp.StatusCode)
	}
	var m authorizationServerMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return authorizationServerMetadata{}, fmt.Errorf("decoding authorization-server metadata: %w", err)
	}
	if m.Issuer != p.Issuer || m.IntrospectionEndpoint == "" {
		return authorizationServerMetadata{}, errors.New("authorization-server metadata is incomplete or names another issuer")
	}
	u, err := url.Parse(m.IntrospectionEndpoint)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return authorizationServerMetadata{}, errors.New("authorization-server introspection endpoint is not an absolute URL")
	}
	p.mu.Lock()
	p.metadata, p.readAt = m, p.now()
	p.mu.Unlock()
	return m, nil
}

func (p *Provider) Introspect(ctx context.Context, token string) (Claims, error) {
	if token == "" {
		return Claims{}, ErrUnauthenticated
	}
	m, err := p.discover(ctx)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: issuer unavailable", ErrUnauthenticated)
	}
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.IntrospectionEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Claims{}, ErrUnauthenticated
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.ClientID != "" {
		req.SetBasicAuth(p.ClientID, p.ClientSecret)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: introspection unavailable", ErrUnauthenticated)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Claims{}, ErrUnauthenticated
	}
	var raw struct {
		Active          bool            `json:"active"`
		Issuer          string          `json:"iss"`
		Subject         string          `json:"sub"`
		ClientID        string          `json:"client_id"`
		Audience        json.RawMessage `json:"aud"`
		Scope           string          `json:"scope"`
		TokenType       string          `json:"token_type"`
		TokenUse        string          `json:"token_use"`
		ExpiresAt       json.Number     `json:"exp"`
		CredentialEpoch json.Number     `json:"wildstone_credential_epoch"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return Claims{}, ErrUnauthenticated
	}
	aud, ok := audiences(raw.Audience)
	if !ok {
		return Claims{}, ErrUnauthenticated
	}
	exp, err := strconv.ParseInt(string(raw.ExpiresAt), 10, 64)
	if err != nil {
		return Claims{}, ErrUnauthenticated
	}
	epoch, err := strconv.ParseInt(string(raw.CredentialEpoch), 10, 64)
	if err != nil {
		return Claims{}, ErrUnauthenticated
	}
	return Claims{Active: raw.Active, Issuer: raw.Issuer, Subject: raw.Subject,
		ClientID: raw.ClientID, Audience: aud, Scope: strings.Fields(raw.Scope),
		TokenType: raw.TokenType, TokenUse: raw.TokenUse,
		ExpiresAt: time.Unix(exp, 0), CredentialEpoch: epoch}, nil
}

func audiences(raw json.RawMessage) ([]string, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil || len(many) == 0 {
		return nil, false
	}
	for _, v := range many {
		if v == "" {
			return nil, false
		}
	}
	return many, true
}

// Resolver binds verified issuer claims to one immutable active registration.
type Resolver struct {
	DB           *runtimedb.DB
	Introspector Introspector
	Issuer       string
	Resource     string
	Now          func() time.Time
}

type Identity struct {
	RegistrationID   string
	PersonID         string
	AgentID          int64
	ConnectionID     int64
	RuntimePrincipal string
	ClientID         string
	CredentialEpoch  int64
}

// RequestAuth retains the bearer only in memory and only for the request. Its
// Recheck method is the sole seam later final-boundary callers may use.
type RequestAuth struct {
	resolver *Resolver
	bearer   string
	Identity Identity
}

type requestAuthKey struct{}

func WithRequestAuth(ctx context.Context, auth *RequestAuth) context.Context {
	return context.WithValue(ctx, requestAuthKey{}, auth)
}

func FromContext(ctx context.Context) (*RequestAuth, bool) {
	auth, ok := ctx.Value(requestAuthKey{}).(*RequestAuth)
	return auth, ok && auth != nil
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) Resolve(ctx context.Context, bearer string) (*RequestAuth, principal.Identity, error) {
	claims, err := r.Introspector.Introspect(ctx, bearer)
	if err != nil {
		return nil, principal.Identity{}, ErrUnauthenticated
	}
	id, err := r.resolveClaims(claims)
	if err != nil {
		return nil, principal.Identity{}, err
	}
	return &RequestAuth{resolver: r, bearer: bearer, Identity: id},
		principal.Identity{Name: id.RuntimePrincipal, Role: runtimedb.RoleButler}, nil
}

func (r *Resolver) resolveClaims(c Claims) (Identity, error) {
	if !c.Active || c.Issuer != r.Issuer || c.Subject == "" || c.ClientID == "" ||
		!strings.EqualFold(c.TokenType, "Bearer") || (c.TokenUse != "" && c.TokenUse != "access_token") ||
		c.ExpiresAt.IsZero() || !r.now().Before(c.ExpiresAt) || !contains(c.Audience, r.Resource) {
		return Identity{}, ErrUnauthenticated
	}
	if !contains(c.Scope, "wildstone.agent") {
		return Identity{}, ErrInsufficientScope
	}
	reg, err := r.DB.ResolveExternalClientRegistration(c.Issuer, c.Subject, c.ClientID, c.CredentialEpoch)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{RegistrationID: reg.ID, PersonID: reg.PersonID, AgentID: reg.Butler,
		ConnectionID: reg.Connection, RuntimePrincipal: reg.RuntimePrincipal,
		ClientID: reg.ClientID, CredentialEpoch: reg.CredentialEpoch}, nil
}

func (a *RequestAuth) Recheck(ctx context.Context) (Identity, error) {
	claims, err := a.resolver.Introspector.Introspect(ctx, a.bearer)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	id, err := a.resolver.resolveClaims(claims)
	if err != nil || id != a.Identity {
		return Identity{}, ErrUnauthenticated
	}
	return id, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
