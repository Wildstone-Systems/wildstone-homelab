package oauthresource

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProviderConsumesMetadataAndIntrospectionWithoutLeakingBearer(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			json.NewEncoder(w).Encode(map[string]string{
				"issuer": issuer.URL, "introspection_endpoint": issuer.URL + "/introspect",
			})
		case "/introspect":
			if user, pass, ok := r.BasicAuth(); !ok || user != "resource-client" || pass != "synthetic-secret" {
				t.Fatalf("introspection client authentication = %q %q %v", user, pass, ok)
			}
			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("token") != "opaque-request-bearer" || strings.Contains(r.URL.String(), "opaque-request-bearer") {
				t.Fatalf("introspection request = %q %q", r.URL, body)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"active": true, "iss": issuer.URL, "sub": "subject-bytes", "client_id": "hermes-client",
				"aud": []string{"https://keystone.example.internal/mcp"}, "scope": "openid wildstone.agent",
				"token_type": "Bearer", "token_use": "access_token", "exp": now.Add(time.Minute).Unix(),
				"wildstone_credential_epoch": 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()

	p := &Provider{Issuer: issuer.URL, ClientID: "resource-client", ClientSecret: "synthetic-secret",
		HTTP: issuer.Client(), Now: func() time.Time { return now }}
	claims, err := p.Introspect(context.Background(), "opaque-request-bearer")
	if err != nil {
		t.Fatal(err)
	}
	if !claims.Active || claims.Issuer != issuer.URL || claims.Subject != "subject-bytes" ||
		claims.ClientID != "hermes-client" || claims.CredentialEpoch != 1 ||
		claims.TokenUse != "access_token" || !claims.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestProviderFailsClosedOnMetadataIssuerMismatchAndOutage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"issuer mismatch", func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"issuer": "https://other.example", "introspection_endpoint": "https://other.example/introspect"})
		}},
		{"issuer outage", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.handler)
			defer s.Close()
			p := &Provider{Issuer: s.URL, HTTP: s.Client()}
			if _, err := p.Introspect(context.Background(), "opaque"); err == nil {
				t.Fatal("provider failure authenticated a token")
			}
		})
	}
}
