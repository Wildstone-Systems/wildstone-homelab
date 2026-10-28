package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestRuntimeRouteClassifierIsClosed(t *testing.T) {
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/agent/tools"},
		{http.MethodPost, "/api/v1/agent/manifest"},
		{http.MethodPost, "/api/v1/agent/invoke"},
		{http.MethodPost, "/api/v1/agent/recall"},
		{http.MethodPost, "/api/v1/agent/remember"},
		{http.MethodPost, "/api/v1/agent/checkpoint"},
		{http.MethodPost, "/api/v1/agent/start"},
		{http.MethodPost, "/api/v1/agent/wait"},
		{http.MethodPost, "/api/v1/agent/complete"},
		{http.MethodPost, "/api/v1/agent/fail"},
		{http.MethodPost, "/api/v1/agent/propose"},
		{http.MethodPost, "/api/v1/agent/notify"},
		{http.MethodPost, "/api/v1/agent/heartbeat"},
		{http.MethodGet, "/api/v1/agent/deliveries"},
		{http.MethodPost, "/api/v1/agent/deliveries/delivery-1/ack"},
	}
	for _, tc := range allowed {
		if !runtimeRouteAllowed(tc.method, tc.path) {
			t.Errorf("%s %s denied", tc.method, tc.path)
		}
	}

	denied := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/resources"},
		{http.MethodGet, "/api/v1/events"},
		{http.MethodPost, "/api/v1/plans"},
		{http.MethodPost, "/api/v1/plans/plan-1/approve"},
		{http.MethodPost, "/api/v1/agent/tasks"},
		{http.MethodPost, "/mcp"},
		{http.MethodGet, "/mcp"},
		{http.MethodGet, "/api/v1/agent/manifest"},
		{http.MethodPost, "/api/v1/agent/tools"},
		{http.MethodPost, "/api/v1/agent/invoke/extra"},
		{http.MethodPost, "/api/v1/agent/invoker"},
		{http.MethodGet, "/api/v1/agent/deliveries/delivery-1/ack"},
		{http.MethodPost, "/api/v1/agent/deliveries//ack"},
		{http.MethodPost, "/api/v1/agent/deliveries/a/b/ack"},
		{http.MethodPost, "/api/v1/agent/deliveries/delivery-1/ack/extra"},
		{http.MethodGet, "/api/v1/synthetic-unclassified"},
	}
	for _, tc := range denied {
		if runtimeRouteAllowed(tc.method, tc.path) {
			t.Errorf("%s %s allowed", tc.method, tc.path)
		}
	}
}

func TestOAuthRouteClassifierIsClosedAndSeparateFromStatic(t *testing.T) {
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/agent/tools"},
		{http.MethodGet, "/api/v1/agent/deliveries"},
		{http.MethodPost, "/api/v1/agent/tasks"},
		{http.MethodPost, "/api/v1/agent/manifest"},
		{http.MethodPost, "/api/v1/agent/invoke"},
		{http.MethodPost, "/mcp"},
		{http.MethodPost, "/api/v1/agent/heartbeat"},
		{http.MethodPost, "/api/v1/agent/deliveries/delivery-1/ack"},
	}
	for _, tc := range allowed {
		if !oauthRouteAllowed(tc.method, tc.path) {
			t.Errorf("%s %s denied", tc.method, tc.path)
		}
	}
	denied := []struct{ method, path string }{
		{http.MethodGet, "/mcp"},
		{http.MethodGet, "/api/v1/resources"},
		{http.MethodPost, "/api/v1/plans"},
		{http.MethodGet, "/api/v1/synthetic-unclassified"},
		{http.MethodPost, "/api/v1/agent/deliveries//ack"},
		{http.MethodPost, "/api/v1/agent/deliveries/a/b/ack"},
	}
	for _, tc := range denied {
		if oauthRouteAllowed(tc.method, tc.path) {
			t.Errorf("%s %s allowed", tc.method, tc.path)
		}
	}
	if runtimeRouteAllowed(http.MethodPost, "/mcp") || runtimeRouteAllowed(http.MethodPost, "/api/v1/agent/tasks") {
		t.Fatal("OAuth-only route leaked into static classification")
	}
}

type observedReader struct {
	read bool
	r    io.Reader
}

func (r *observedReader) Read(p []byte) (int, error) {
	r.read = true
	return r.r.Read(p)
}

func TestRuntimeCredentialIsDeniedBeforeBodyOrHandler(t *testing.T) {
	h, _ := agentEnvHarness(t)
	createAgent(t, h, "route-gate")
	cred := rotate(t, h, "route-gate", nil)

	body := &observedReader{r: strings.NewReader(`{"protected":"content"}`)}
	effected := false
	handler := h.s.handle(principal.VerbRead, func(r *http.Request, _ principal.Identity) (any, int, error) {
		effected = true
		_, _ = io.ReadAll(r.Body)
		return map[string]bool{"reached": true}, http.StatusOK, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/synthetic-unclassified", body)
	req.Header.Set("Authorization", "Bearer "+cred.Secret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if body.read || effected {
		t.Fatalf("denied request reached protected input or effect: body read=%v effect=%v", body.read, effected)
	}
	audit, err := h.s.DB.ListAudit(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Principal != cred.CredentialRef || audit[0].Status != http.StatusForbidden || audit[0].Path != "/api/v1/synthetic-unclassified" {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestRouteGateDoesNotClassifyByRole(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name string
		role runtimedb.Role
		verb principal.Verb
		path string
	}{
		{"human admin", runtimedb.RoleAdmin, principal.VerbRead, "/api/v1/synthetic-admin-read"},
		{"node operator", runtimedb.RoleOperator, principal.VerbPropose, "/api/v1/synthetic-node-propose"},
		{"unbound butler", runtimedb.RoleButler, principal.VerbRead, "/api/v1/synthetic-butler-read"},
		{"chat adapter", runtimedb.RoleConcierge, principal.VerbConverse, "/api/v1/synthetic-adapter-call"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hit := false
			handler := h.s.handle(tc.verb, func(_ *http.Request, _ principal.Identity) (any, int, error) {
				hit = true
				return nil, http.StatusOK, nil
			})
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+h.tokens[tc.role])
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !hit {
				t.Fatalf("status = %d, handler reached = %v", rec.Code, hit)
			}
		})
	}
}
