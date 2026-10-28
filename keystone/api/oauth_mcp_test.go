package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentmcp"
)

type oauthMCPTestClient struct {
	agentapi.Client
	manifestHandle agentapi.Handle
	invokeRequest  agentapi.InvokeRequest
}

func (c *oauthMCPTestClient) Tools(context.Context) ([]agentapi.ToolSpec, error) {
	return []agentapi.ToolSpec{{Name: "peek", Description: "read synthetic state", ReadOnlyHint: true}}, nil
}

func (c *oauthMCPTestClient) Manifest(_ context.Context, handle agentapi.Handle) (agentapi.Manifest, error) {
	c.manifestHandle = handle
	return agentapi.Manifest{Tools: []agentapi.ToolSpec{{Name: "peek", ReadOnlyHint: true}}}, nil
}

func (c *oauthMCPTestClient) Invoke(_ context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
	c.invokeRequest = req
	return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: "synthetic result"}, nil
}

func doOAuthMCP(t *testing.T, h *harness, session, origin, version, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer opaque-oauth-access-token")
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set(mcpSessionHeader, session)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if version != "" {
		req.Header.Set(mcpProtocolHeader, version)
	}
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestOAuthMCPTaskSessionDispatchesThroughCanonicalClient(t *testing.T) {
	h, introspector := activeOAuthHarness(t)
	client := &oauthMCPTestClient{}
	h.s.AgentGateway = client
	_, admitted := admitOAuthTask(t, h,
		`{"objective":"check synthetic state","clientSessionId":"mcp-session-1","idempotencyKey":"turn-1"}`)
	if admitted.MCPSession == "" || admitted.SessionExpiresAt.IsZero() {
		t.Fatalf("admission omitted MCP session: %+v", admitted)
	}

	introspector.calls = 0
	idle := doOAuthMCP(t, h, "", "https://keystone.example.internal", agentmcp.ProtocolVersion,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if idle.Code != http.StatusOK || !bytes.Contains(idle.Body.Bytes(), []byte(`"name":"peek"`)) {
		t.Fatalf("idle tools/list = %d %s", idle.Code, idle.Body.String())
	}
	if introspector.calls != 2 {
		t.Fatalf("idle discovery introspections = %d, want auth + recheck", introspector.calls)
	}

	listed := doOAuthMCP(t, h, admitted.MCPSession, "", agentmcp.ProtocolVersion,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if listed.Code != http.StatusOK || client.manifestHandle != admitted.Handle {
		t.Fatalf("task tools/list = %d %s, handle %q", listed.Code, listed.Body.String(), client.manifestHandle)
	}
	called := doOAuthMCP(t, h, admitted.MCPSession, "", agentmcp.ProtocolVersion,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"peek","arguments":{"safe":true}}}`)
	if called.Code != http.StatusOK || client.invokeRequest.Handle != admitted.Handle || client.invokeRequest.Tool != "peek" ||
		!bytes.Contains(called.Body.Bytes(), []byte("synthetic result")) || called.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("tools/call = %d %s, request %+v", called.Code, called.Body.String(), client.invokeRequest)
	}
}

func TestOAuthMCPFramingAndSessionFailuresFailClosed(t *testing.T) {
	h, _ := activeOAuthHarness(t)
	h.s.AgentGateway = &oauthMCPTestClient{}
	_, admitted := admitOAuthTask(t, h,
		`{"objective":"check synthetic state","clientSessionId":"mcp-session-2","idempotencyKey":"turn-2"}`)
	cases := []struct {
		name, session, origin, version, body string
		status                               int
	}{
		{"wrong origin", admitted.MCPSession, "https://attacker.example", agentmcp.ProtocolVersion, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, http.StatusForbidden},
		{"missing version", admitted.MCPSession, "", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, http.StatusBadRequest},
		{"wrong version", admitted.MCPSession, "", "2024-11-05", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, http.StatusBadRequest},
		{"unbound call", "", "", agentmcp.ProtocolVersion, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"peek"}}`, http.StatusForbidden},
		{"unknown session", "unguessable-but-wrong", "", agentmcp.ProtocolVersion, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"peek"}}`, http.StatusForbidden},
		{"unsupported method", "", "", agentmcp.ProtocolVersion, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doOAuthMCP(t, h, tc.session, tc.origin, tc.version, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), tc.status)
			}
		})
	}

	h.s.Now = func() time.Time { return admitted.SessionExpiresAt }
	expired := doOAuthMCP(t, h, admitted.MCPSession, "", agentmcp.ProtocolVersion,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"peek"}}`)
	if expired.Code != http.StatusForbidden {
		t.Fatalf("expired session = %d %s", expired.Code, expired.Body.String())
	}
}

func TestOAuthMCPAuthenticationAndRouteDenialPrecedeBodyDecode(t *testing.T) {
	h, introspector := activeOAuthHarness(t)
	body := &observedReader{r: bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)}
	req := httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.Header.Set("Authorization", "Bearer opaque-oauth-access-token")
	req.Header.Set("Content-Type", "application/json")
	introspector.err = io.ErrUnexpectedEOF
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || body.read {
		t.Fatalf("auth failure = %d body-read=%v", rec.Code, body.read)
	}

	staticBody := &observedReader{r: bytes.NewBufferString(`{"protected":"content"}`)}
	staticReq := httptest.NewRequest(http.MethodPost, "/mcp", staticBody)
	staticReq.Header.Set("Authorization", "Bearer wst_not-a-valid-token")
	staticReq.Header.Set("Content-Type", "application/json")
	staticRec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(staticRec, staticReq)
	if staticRec.Code != http.StatusUnauthorized || staticBody.read {
		t.Fatalf("static failure = %d body-read=%v", staticRec.Code, staticBody.read)
	}
}

func TestOAuthMCPInitializeAndNotification(t *testing.T) {
	h, _ := activeOAuthHarness(t)
	init := doOAuthMCP(t, h, "", "", "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if init.Code != http.StatusOK || init.Header().Get(mcpProtocolHeader) != agentmcp.ProtocolVersion {
		t.Fatalf("initialize = %d %s", init.Code, init.Body.String())
	}
	var response oauthMCPResponse
	if err := json.Unmarshal(init.Body.Bytes(), &response); err != nil || response.Error != nil {
		t.Fatalf("initialize response = %+v, %v", response, err)
	}
	notification := doOAuthMCP(t, h, "", "", agentmcp.ProtocolVersion,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if notification.Code != http.StatusAccepted || notification.Body.Len() != 0 {
		t.Fatalf("notification = %d %q", notification.Code, notification.Body.String())
	}
}
