package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/oauthresource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type fakeOAuthIntrospector struct {
	claims oauthresource.Claims
	err    error
	calls  int
}

func (f *fakeOAuthIntrospector) Introspect(_ context.Context, _ string) (oauthresource.Claims, error) {
	f.calls++
	return f.claims, f.err
}

func activeOAuthHarness(t *testing.T) (*harness, *fakeOAuthIntrospector) {
	t.Helper()
	h, registration, _, _ := externalEnrollmentHarness(t)
	issued, err := h.s.DB.IssueExternalEnrollmentChallenge(runtimedb.IssueExternalEnrollmentChallenge{
		RegistrationID: registration.ID, PersonID: registration.PersonID, IssuedBy: registration.PersonID,
	})
	if err != nil {
		t.Fatal(err)
	}
	registration, err = h.s.DB.ActivateExternalClientRegistration(runtimedb.ActivateExternalRegistration{
		RegistrationID: registration.ID, PersonID: registration.PersonID, Issuer: registration.Issuer,
		Subject: "byte-exact-subject", Proof: issued.Secret, ActivatedBy: registration.PersonID,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	introspector := &fakeOAuthIntrospector{claims: oauthresource.Claims{
		Active: true, Issuer: registration.Issuer, Subject: registration.Subject,
		ClientID: registration.ClientID, Audience: []string{"https://keystone.example.internal/mcp"},
		Scope: []string{"wildstone.agent"}, TokenType: "Bearer", TokenUse: "access_token",
		ExpiresAt: now.Add(time.Hour), CredentialEpoch: registration.CredentialEpoch,
	}}
	h.s.OAuthResource = &oauthresource.Resolver{DB: h.s.DB, Introspector: introspector,
		Issuer: registration.Issuer, Resource: "https://keystone.example.internal/mcp", Now: func() time.Time { return now }}
	return h, introspector
}

func TestOAuthProfileDeniesUnclassifiedRouteBeforeIntrospectionOrBody(t *testing.T) {
	h, introspector := activeOAuthHarness(t)
	body := &observedReader{r: strings.NewReader(`{"protected":"content"}`)}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/plans", body)
	req.Header.Set("Authorization", "Bearer opaque-oauth-access-token")
	rec := httptest.NewRecorder()
	h.s.handle(principal.VerbRead, func(*http.Request, principal.Identity) (any, int, error) {
		t.Fatal("unclassified handler reached")
		return nil, 0, nil
	}).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || body.read || introspector.calls != 0 {
		t.Fatalf("status=%d body-read=%v introspections=%d", rec.Code, body.read, introspector.calls)
	}
}

func TestOAuthProfileRejectsURLBearerBeforeIntrospection(t *testing.T) {
	h, introspector := activeOAuthHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/tools?access_token=opaque-url-bearer", nil)
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || introspector.calls != 0 {
		t.Fatalf("status=%d introspections=%d", rec.Code, introspector.calls)
	}
}

func TestOAuthProfileResolvesExistingRuntimePrincipalAndMetadata(t *testing.T) {
	h, introspector := activeOAuthHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/tools", nil)
	req.Header.Set("Authorization", "Bearer opaque-oauth-access-token")
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("valid OAuth identity was refused: %d %s", rec.Code, rec.Body.String())
	}
	if introspector.calls != 1 {
		t.Fatalf("introspection calls = %d", introspector.calls)
	}

	metadata := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"resource":"https://keystone.example.internal/mcp"`) ||
		!strings.Contains(metadata.Body.String(), `"wildstone.agent"`) {
		t.Fatalf("metadata = %d %s", metadata.Code, metadata.Body.String())
	}
}

func TestOAuthWrongClaimsAndIssuerFailureFailClosed(t *testing.T) {
	mutations := []struct {
		name string
		edit func(*oauthresource.Claims)
	}{
		{"inactive", func(c *oauthresource.Claims) { c.Active = false }},
		{"wrong issuer", func(c *oauthresource.Claims) { c.Issuer = "https://other.example" }},
		{"wrong subject", func(c *oauthresource.Claims) { c.Subject += "x" }},
		{"wrong client", func(c *oauthresource.Claims) { c.ClientID += "x" }},
		{"wrong resource", func(c *oauthresource.Claims) { c.Audience = []string{"https://other.example/mcp"} }},
		{"wrong scope", func(c *oauthresource.Claims) { c.Scope = []string{"openid"} }},
		{"id token", func(c *oauthresource.Claims) { c.TokenUse = "id_token" }},
		{"expired", func(c *oauthresource.Claims) { c.ExpiresAt = time.Unix(1, 0) }},
		{"wrong epoch", func(c *oauthresource.Claims) { c.CredentialEpoch++ }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			h, introspector := activeOAuthHarness(t)
			tc.edit(&introspector.claims)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/tools", nil)
			req.Header.Set("Authorization", "Bearer opaque-oauth-access-token")
			rec := httptest.NewRecorder()
			h.s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Fatalf("claim mutation authenticated: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestOAuthRequestLocalRecheckFailsClosedOnIssuerOutage(t *testing.T) {
	h, introspector := activeOAuthHarness(t)
	rechecked := false
	handler := h.s.handle(principal.VerbRead, func(r *http.Request, _ principal.Identity) (any, int, error) {
		auth, ok := oauthresource.FromContext(r.Context())
		if !ok {
			t.Fatal("OAuth request has no request-local recheck context")
		}
		introspector.err = errors.New("synthetic issuer outage")
		if _, err := auth.Recheck(r.Context()); err == nil {
			t.Fatal("issuer outage reused cached identity")
		}
		rechecked = true
		return map[string]bool{"ok": true}, http.StatusOK, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/tools", nil)
	req.Header.Set("Authorization", "Bearer opaque-oauth-access-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !rechecked || introspector.calls != 2 {
		t.Fatalf("status=%d rechecked=%v introspections=%d", rec.Code, rechecked, introspector.calls)
	}
}
