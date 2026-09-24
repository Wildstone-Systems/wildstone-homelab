package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

func testMCPServer(ks *fakeClient) *mcpServer {
	return &mcpServer{keystone: ks, runs: newRunRegistry(), secret: "sh4red", log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
}

func doMCP(t *testing.T, m *mcpServer, secret string, extraHeaders map[string]string, body string) (*httptest.ResponseRecorder, jsonrpcResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	for k, v := range extraHeaders {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	var resp jsonrpcResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding response: %v (body: %s)", err, w.Body.String())
		}
	}
	return w, resp
}

func TestMCPMissingSecretIsUnauthorized(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	w, _ := doMCP(t, m, "", nil, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMCPWrongSecretIsUnauthorized(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	w, _ := doMCP(t, m, "wrong", nil, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMCPInitializeAndPing(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	_, resp := doMCP(t, m, "sh4red", nil, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	raw, _ := json.Marshal(resp.Result)
	var init mcpInitializeResult
	if err := json.Unmarshal(raw, &init); err != nil {
		t.Fatalf("decoding initialize result: %v", err)
	}
	if init.ProtocolVersion == "" || init.ServerInfo.Name == "" {
		t.Fatalf("initialize result incomplete: %+v", init)
	}

	_, resp = doMCP(t, m, "sh4red", nil, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if resp.Error != nil {
		t.Fatalf("ping error: %+v", resp.Error)
	}
}

func TestMCPToolsListMapsManifestToolsWithReadOnlyHint(t *testing.T) {
	var seenHandle agentapi.Handle
	ks := &fakeClient{
		manifestFn: func(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
			seenHandle = h
			return agentapi.Manifest{Tools: []agentapi.ToolSpec{
				{Name: "get_household_events", Description: "read events", ReadOnlyHint: true, InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
				{Name: "do_household_action", Description: "act", ReadOnlyHint: false},
			}}, nil
		},
	}
	m := testMCPServer(ks)
	_, resp := doMCP(t, m, "sh4red", map[string]string{handleHeader: "task1.secret"},
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	if seenHandle != agentapi.Handle("task1.secret") {
		t.Fatalf("Manifest called with handle %q, want task1.secret", seenHandle)
	}
	raw, _ := json.Marshal(resp.Result)
	var list mcpToolsListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decoding tools/list result: %v", err)
	}
	if len(list.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(list.Tools))
	}
	if !list.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tool 0 readOnlyHint = false, want true")
	}
	if list.Tools[1].Annotations.ReadOnlyHint {
		t.Fatalf("tool 1 readOnlyHint = true, want false")
	}
	if len(list.Tools[1].InputSchema) == 0 {
		t.Fatalf("tool with no declared schema must still carry a schema")
	}
}

func TestMCPToolsListMissingHandleIsInvalidParams(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	_, resp := doMCP(t, m, "sh4red", nil, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Fatalf("resp.Error = %+v, want code %d", resp.Error, codeInvalidParams)
	}
}

func TestMCPToolsCallOKIsNotError(t *testing.T) {
	ks := &fakeClient{invokeFn: func(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
		if req.Tool != "get_household_events" {
			t.Errorf("invoked tool = %q", req.Tool)
		}
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: "3 events"}, nil
	}}
	m := testMCPServer(ks)
	_, resp := doMCP(t, m, "sh4red", map[string]string{handleHeader: "task1.secret"},
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_household_events","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	raw, _ := json.Marshal(resp.Result)
	var result mcpToolCallResult
	json.Unmarshal(raw, &result) //nolint:errcheck
	if result.IsError {
		t.Fatalf("result.IsError = true, want false")
	}
	if len(result.Content) != 1 || result.Content[0].Text != "3 events" {
		t.Fatalf("result.Content = %+v", result.Content)
	}
}

func TestMCPToolsCallDeniedIsError(t *testing.T) {
	ks := &fakeClient{invokeFn: func(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: "not authorised"}, nil
	}}
	m := testMCPServer(ks)
	_, resp := doMCP(t, m, "sh4red", map[string]string{handleHeader: "task1.secret"},
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"do_household_action"}}`)
	raw, _ := json.Marshal(resp.Result)
	var result mcpToolCallResult
	json.Unmarshal(raw, &result) //nolint:errcheck
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true for a denied outcome")
	}
	if len(result.Content) != 1 {
		t.Fatalf("result.Content = %+v", result.Content)
	}
}

func TestMCPToolsCallWaitingIsNotAnError(t *testing.T) {
	ks := &fakeClient{invokeFn: func(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeWaiting, WaitReason: agentapi.WaitConfirmPending}, nil
	}}
	m := testMCPServer(ks)
	_, resp := doMCP(t, m, "sh4red", map[string]string{handleHeader: "task1.secret"},
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"do_household_action"}}`)
	raw, _ := json.Marshal(resp.Result)
	var result mcpToolCallResult
	json.Unmarshal(raw, &result) //nolint:errcheck
	if result.IsError {
		t.Fatalf("result.IsError = true, want false: waiting is 'not yet', not a failure")
	}
	if len(result.Content) != 1 || result.Content[0].Text == "" {
		t.Fatalf("result.Content = %+v, want an explanation of the wait", result.Content)
	}
}

func TestMCPHandleFromRunIDFallback(t *testing.T) {
	var seenHandle agentapi.Handle
	ks := &fakeClient{manifestFn: func(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
		seenHandle = h
		return agentapi.Manifest{}, nil
	}}
	m := testMCPServer(ks)
	m.runs.set("run-42", agentapi.Handle("task9.secret"))
	_, resp := doMCP(t, m, "sh4red", map[string]string{runIDHeader: "run-42"}, `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	if seenHandle != agentapi.Handle("task9.secret") {
		t.Fatalf("seenHandle = %q, want task9.secret", seenHandle)
	}
}

func TestMCPUnknownRunIDIsInvalidParams(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	_, resp := doMCP(t, m, "sh4red", map[string]string{runIDHeader: "no-such-run"}, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Fatalf("resp.Error = %+v, want code %d", resp.Error, codeInvalidParams)
	}
}

func TestMCPExplicitHandleHeaderWinsOverRunID(t *testing.T) {
	var seenHandle agentapi.Handle
	ks := &fakeClient{manifestFn: func(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
		seenHandle = h
		return agentapi.Manifest{}, nil
	}}
	m := testMCPServer(ks)
	m.runs.set("run-1", agentapi.Handle("wrong.secret"))
	_, resp := doMCP(t, m, "sh4red", map[string]string{handleHeader: "right.secret", runIDHeader: "run-1"},
		`{"jsonrpc":"2.0","id":10,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	if seenHandle != agentapi.Handle("right.secret") {
		t.Fatalf("seenHandle = %q, want right.secret (the explicit header)", seenHandle)
	}
}

func TestMCPHandleFallsBackToSolePendingRun(t *testing.T) {
	var seenHandle agentapi.Handle
	ks := &fakeClient{manifestFn: func(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
		seenHandle = h
		return agentapi.Manifest{}, nil
	}}
	m := testMCPServer(ks)
	m.runs.set("run-1", agentapi.Handle("task1.secret"))
	// No X-Wildstone-Handle, no X-Hermes-Run-Id: the single in-flight run
	// is the only reasonable answer.
	_, resp := doMCP(t, m, "sh4red", nil, `{"jsonrpc":"2.0","id":13,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	if seenHandle != agentapi.Handle("task1.secret") {
		t.Fatalf("seenHandle = %q, want task1.secret", seenHandle)
	}
}

func TestMCPHandleAmbiguousWithTwoPendingRunsIsInvalidParams(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	m.runs.set("run-1", agentapi.Handle("task1.secret"))
	m.runs.set("run-2", agentapi.Handle("task2.secret"))
	_, resp := doMCP(t, m, "sh4red", nil, `{"jsonrpc":"2.0","id":14,"method":"tools/list"}`)
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Fatalf("resp.Error = %+v, want code %d (two pending runs must not guess)", resp.Error, codeInvalidParams)
	}
}

func TestMCPMethodNotFound(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	_, resp := doMCP(t, m, "sh4red", nil, `{"jsonrpc":"2.0","id":11,"method":"resources/list"}`)
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("resp.Error = %+v, want code %d", resp.Error, codeMethodNotFound)
	}
}

func TestMCPParseErrorOnMalformedBody(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	w, resp := doMCP(t, m, "sh4red", nil, `{not json`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the error travels in the JSON-RPC body)", w.Code)
	}
	if resp.Error == nil || resp.Error.Code != codeParseError {
		t.Fatalf("resp.Error = %+v, want code %d", resp.Error, codeParseError)
	}
}

func TestMCPNotificationGetsNoBody(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	r.Header.Set("Authorization", "Bearer sh4red")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty (a notification gets no response)", w.Body.String())
	}
}

func TestMCPGetIsMethodNotAllowed(t *testing.T) {
	m := testMCPServer(&fakeClient{})
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestMCPManifestErrorIsInternalError(t *testing.T) {
	ks := &fakeClient{manifestFn: func(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
		return agentapi.Manifest{}, errors.New("boom")
	}}
	m := testMCPServer(ks)
	_, resp := doMCP(t, m, "sh4red", map[string]string{handleHeader: "task1.secret"}, `{"jsonrpc":"2.0","id":12,"method":"tools/list"}`)
	if resp.Error == nil || resp.Error.Code != codeInternalError {
		t.Fatalf("resp.Error = %+v, want code %d", resp.Error, codeInternalError)
	}
}
