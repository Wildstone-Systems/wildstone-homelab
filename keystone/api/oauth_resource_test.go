package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi/httpclient"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentgateway"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/oauthresource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

type fakeOAuthIntrospector struct {
	claims     oauthresource.Claims
	err        error
	calls      int
	beforeCall func(int)
}

func (f *fakeOAuthIntrospector) Introspect(_ context.Context, _ string) (oauthresource.Claims, error) {
	f.calls++
	if f.beforeCall != nil {
		f.beforeCall(f.calls)
	}
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
	h.s.SecretVault, err = h.s.DB.OpenSecretVault(filepath.Join(t.TempDir(), "household-secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	return h, introspector
}

func admitOAuthTask(t *testing.T, h *harness, body string) (*httptest.ResponseRecorder, agentapi.TaskAdmission) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer opaque-oauth-access-token")
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	var out agentapi.TaskAdmission
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode admission: %v", err)
		}
	}
	return rec, out
}

func TestOAuthTaskAdmissionIsExactAndConflictSafe(t *testing.T) {
	h, introspector := activeOAuthHarness(t)
	body := `{"objective":"check the greenhouse","clientSessionId":"session-1","idempotencyKey":"turn-1"}`
	firstRec, first := admitOAuthTask(t, h, body)
	if firstRec.Code != http.StatusCreated || first.TaskID == "" || first.Handle.IsZero() {
		t.Fatalf("first admission = %d %s", firstRec.Code, firstRec.Body.String())
	}
	secondRec, second := admitOAuthTask(t, h, body)
	if secondRec.Code != http.StatusCreated || second != first {
		t.Fatalf("exact retry = %d %+v, want %+v", secondRec.Code, second, first)
	}
	conflict, _ := admitOAuthTask(t, h,
		`{"objective":"change the greenhouse","clientSessionId":"session-1","idempotencyKey":"turn-1"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("changed retry = %d %s", conflict.Code, conflict.Body.String())
	}
	if introspector.calls != 6 { // initial authentication plus final recheck per request
		t.Fatalf("introspection calls = %d", introspector.calls)
	}
	if got := firstRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if _, err := h.s.DB.GetButlerTask(first.TaskID); err != nil {
		t.Fatalf("ordinary task was not created: %v", err)
	}
	if agent, err := h.s.DB.TaskButler(first.TaskID); err != nil || agent == "" {
		t.Fatalf("ordinary task was not bound to its Agent: %q %v", agent, err)
	}
	if digest, err := h.s.DB.ButlerTaskHandleHash(first.TaskID); err != nil || digest == "" {
		t.Fatalf("issued handle digest was not recorded: %q %v", digest, err)
	}
}

func TestOAuthTaskAdmissionRejectsAuthorityFieldsAndBadBounds(t *testing.T) {
	tests := []string{
		`{"objective":"hello","clientSessionId":"session-1","idempotencyKey":"turn-1","personId":"someone"}`,
		`{"objective":"hello","clientSessionId":"contains space","idempotencyKey":"turn-1"}`,
		`{"objective":"   ","clientSessionId":"session-1","idempotencyKey":"turn-1"}`,
	}
	for _, body := range tests {
		h, _ := activeOAuthHarness(t)
		rec, _ := admitOAuthTask(t, h, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestOAuthTaskAdmissionPublicClientRoundTrip(t *testing.T) {
	h, _ := activeOAuthHarness(t)
	client := &httpclient.Client{BaseURL: h.srv.URL, Bearer: "opaque-oauth-access-token", HTTP: h.srv.Client()}
	got, err := client.AdmitTask(context.Background(), agentapi.TaskAdmissionRequest{
		Objective: "check the greenhouse", ClientSessionID: "session-client", IdempotencyKey: "turn-client",
	})
	if err != nil || got.TaskID == "" || got.Handle.IsZero() || got.Handle.TaskID() != got.TaskID {
		t.Fatalf("AdmitTask = %+v, %v", got, err)
	}
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

func TestOAuthDeliveryPollAndAckUseFreshExactProvenance(t *testing.T) {
	h, introspector := activeOAuthHarness(t)
	h.s.AgentGateway = agentgateway.InProcess(&agentgateway.Gateway{DB: h.s.DB})
	rec, admitted := admitOAuthTask(t, h,
		`{"objective":"check the greenhouse","clientSessionId":"delivery-session","idempotencyKey":"delivery-turn"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admission = %d %s", rec.Code, rec.Body.String())
	}
	agentName, err := h.s.DB.TaskButler(admitted.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := h.s.DB.GetButler(agentName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.RecordAgentDeliveryForTask(agent.Seq, agentapi.DeliveryTurn, admitted.TaskID,
		`{"handle":"`+string(admitted.Handle)+`","text":"synthetic delivery"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.RecordAgentDelivery(agent.Seq, agentapi.DeliveryEscalation, `{"instruction":"static only"}`); err != nil {
		t.Fatal(err)
	}

	introspector.calls = 0
	client := &httpclient.Client{BaseURL: h.srv.URL, Bearer: "opaque-oauth-access-token", HTTP: h.srv.Client()}
	deliveries, err := client.Deliveries(context.Background(), "", 0)
	if err != nil || len(deliveries) != 1 || deliveries[0].Kind != agentapi.DeliveryTurn {
		t.Fatalf("deliveries = %+v, %v", deliveries, err)
	}
	if introspector.calls != 3 { // request authentication, pre-selection, pre-return
		t.Fatalf("poll introspections = %d", introspector.calls)
	}

	introspector.calls = 0
	if err := client.Ack(context.Background(), deliveries[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := client.Ack(context.Background(), deliveries[0].ID); err != nil {
		t.Fatalf("duplicate ack = %v", err)
	}
	if introspector.calls != 4 { // request authentication and pre-write recheck per ack
		t.Fatalf("ack introspections = %d", introspector.calls)
	}
}
