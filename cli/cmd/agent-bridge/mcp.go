package main

// A minimal, hand-rolled JSON-RPC 2.0 "Streamable HTTP" MCP endpoint
// (ADR-W040 §6): one POST endpoint, no new Go dependency. It translates
// Hermes's tools/list and tools/call into the identical Manifest/Invoke
// calls this bridge already makes over agentapi/httpclient — the same
// authority, the same audit row, no new gate (§6: "no new authority in
// the translation").
//
// Scope, stated plainly: this implements request/response over a single
// POST per JSON-RPC call. It does not implement session resumption,
// server-initiated SSE push, or notifications/tools/list_changed (not
// required by the H3 brief; ADR-W040 §8 leaves mid-session grant changes
// to "the runtime calls Manifest again"). That is enough for Hermes's own
// use of an MCP server: synchronous tool calls during a run.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// mcpProtocolVersion is the MCP spec revision this server speaks.
const mcpProtocolVersion = "2025-06-18"

// handleHeader is the ADR-W040 §6-recommended shape: the task handle as
// an explicit, request-scoped value. runIDHeader is the fallback runs.go
// documents for a runtime (Hermes) whose own MCP config cannot carry a
// per-call value.
const (
	handleHeader = "X-Wildstone-Handle"
	runIDHeader  = "X-Hermes-Run-Id"
)

// jsonrpcRequest is one JSON-RPC 2.0 request. A notification (no id) is
// legal and gets no response body (see ServeHTTP).
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// jsonrpcError codes follow the JSON-RPC 2.0 spec's reserved range.
type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParseError     = -32700
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// mcpServer is the MCP endpoint. It never sees the Wildstone bearer:
// keystone here is the same agentapi.Client the delivery loop uses, and
// every call it makes still authenticates to Wildstone with that bearer
// underneath — Hermes never learns it.
type mcpServer struct {
	keystone agentapi.Client
	runs     *runRegistry
	// secret is MCP_SHARED_SECRET: the one thing that authenticates
	// Hermes to THIS server (H3 brief: "a shared secret header
	// MCP_SHARED_SECRET between Hermes and the bridge").
	secret string
	log    *slog.Logger
}

func (m *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req jsonrpcRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeJSONRPC(w, jsonrpcResponse{JSONRPC: "2.0", Error: &jsonrpcError{Code: codeParseError, Message: "parse error: " + err.Error()}})
		return
	}
	resp := m.dispatch(r.Context(), r, req)
	// A notification (no id) gets no response body at all, per JSON-RPC
	// 2.0 — only a request that carried an id is answered.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSONRPC(w, resp)
}

// authorized checks the shared secret in a constant-time compare, the
// same discipline a bearer check anywhere else in this codebase uses.
func (m *mcpServer) authorized(r *http.Request) bool {
	if m.secret == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(m.secret)) == 1
}

func (m *mcpServer) dispatch(ctx context.Context, r *http.Request, req jsonrpcRequest) jsonrpcResponse {
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = mcpInitializeResult{
			ProtocolVersion: mcpProtocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      mcpServerInfo{Name: "wildstone-agent-bridge", Version: version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "notifications/initialized":
		resp.Result = map[string]any{}
	case "tools/list":
		handle, err := m.handleFor(r)
		if err != nil {
			// No run in flight and none named: Hermes lists its tools
			// once, when it connects, before any task exists. It is
			// offered the Agent's own tool set; each tools/call is still
			// authorized against the run's task (agentapi.ToolsClient).
			// With runs in flight but none identifiable it still refuses,
			// rather than answer for one of them.
			tc, ok := m.keystone.(agentapi.ToolsClient)
			if ok && m.runs.idle() && r.Header.Get(handleHeader) == "" && r.Header.Get(runIDHeader) == "" {
				specs, terr := tc.Tools(ctx)
				if terr != nil {
					resp.Error = &jsonrpcError{Code: codeInternalError, Message: terr.Error()}
					return resp
				}
				resp.Result = mcpToolsListResult{Tools: toolsFor(agentapi.Manifest{Tools: specs})}
				return resp
			}
			resp.Error = &jsonrpcError{Code: codeInvalidParams, Message: err.Error()}
			return resp
		}
		manifest, err := m.keystone.Manifest(ctx, handle)
		if err != nil {
			resp.Error = &jsonrpcError{Code: codeInternalError, Message: err.Error()}
			return resp
		}
		resp.Result = mcpToolsListResult{Tools: toolsFor(manifest)}
	case "tools/call":
		handle, err := m.handleFor(r)
		if err != nil {
			resp.Error = &jsonrpcError{Code: codeInvalidParams, Message: err.Error()}
			return resp
		}
		var params mcpToolsCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &jsonrpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
			return resp
		}
		if params.Name == "" {
			resp.Error = &jsonrpcError{Code: codeInvalidParams, Message: "tools/call requires a tool name"}
			return resp
		}
		result, err := m.keystone.Invoke(ctx, agentapi.InvokeRequest{Handle: handle, Tool: params.Name, Args: params.Arguments})
		if err != nil {
			resp.Error = &jsonrpcError{Code: codeInternalError, Message: err.Error()}
			return resp
		}
		resp.Result = toolResultFor(result)
	default:
		resp.Error = &jsonrpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
	return resp
}

// handleFor recovers the Wildstone task handle for one MCP call, trying
// three things in order:
//
//  1. An explicit X-Wildstone-Handle header — ADR-W040 §6's own
//     recommended shape, for any runtime that can be configured to send
//     one.
//  2. The run-id map runs.go documents, keyed by X-Hermes-Run-Id —
//     the shape Hermes's own static per-server identity_header forces
//     today, if Hermes turns out to attach a per-run correlation header
//     to its own outbound MCP calls (undocumented upstream; TODO(spike
//     S3)).
//  3. runs.solePending(): when exactly one Hermes run is in flight —
//     the packaged config's own max_concurrent_runs: 1, matching this
//     bridge's own one-delivery-at-a-time loop — there is nothing to
//     disambiguate, so this is not a guess. It is what makes the
//     packaged shape work AT ALL today, since neither of the first two
//     headers has a documented way to reach Hermes's own outbound MCP
//     request yet.
func (m *mcpServer) handleFor(r *http.Request) (agentapi.Handle, error) {
	if h := r.Header.Get(handleHeader); h != "" {
		return agentapi.Handle(h), nil
	}
	if rid := r.Header.Get(runIDHeader); rid != "" {
		if h, ok := m.runs.get(rid); ok {
			return h, nil
		}
		return "", fmt.Errorf("no handle is registered for run %q", rid)
	}
	if h, ok := m.runs.solePending(); ok {
		return h, nil
	}
	return "", fmt.Errorf("missing handle: neither %s nor %s was sent, and no single run is unambiguously in flight",
		handleHeader, runIDHeader)
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpInitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      mcpServerInfo  `json:"serverInfo"`
}

type mcpToolAnnotations struct {
	ReadOnlyHint bool `json:"readOnlyHint,omitempty"`
}

// mcpTool is one manifest ToolSpec, mapped onto an MCP tool declaration.
type mcpTool struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	InputSchema json.RawMessage     `json:"inputSchema"`
	Annotations *mcpToolAnnotations `json:"annotations,omitempty"`
}

type mcpToolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

// defaultInputSchema is what a tool with no declared schema gets: MCP's
// own inputSchema is required, and "no parameters" is itself a schema.
var defaultInputSchema = json.RawMessage(`{"type":"object"}`)

func toolsFor(m agentapi.Manifest) []mcpTool {
	out := make([]mcpTool, 0, len(m.Tools))
	for _, t := range m.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = defaultInputSchema
		}
		out = append(out, mcpTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
			Annotations: &mcpToolAnnotations{ReadOnlyHint: t.ReadOnlyHint},
		})
	}
	return out
}

type mcpToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// toolResultFor maps one agentapi.InvokeResult onto an MCP tool result
// (H3 brief): ok is an ordinary result; waiting is ALSO an ordinary,
// non-error result — a wait is not a failure, it is "not yet", and a
// runtime that has no synchronous way to sit and wait should be told so
// in words, not handed an error it might retry as one; denied, rejected
// and error all set isError so the model sees a call that did not
// succeed and does not represent it as having happened.
func toolResultFor(res agentapi.InvokeResult) mcpToolCallResult {
	switch res.Outcome {
	case agentapi.OutcomeOK:
		return mcpToolCallResult{Content: []mcpContent{{Type: "text", Text: res.Content}}}
	case agentapi.OutcomeWaiting:
		text := "waiting: " + string(res.WaitReason)
		if res.Reason != "" {
			text += " (" + res.Reason + ")"
		}
		return mcpToolCallResult{Content: []mcpContent{{Type: "text", Text: text}}}
	default: // denied, rejected, error
		text := string(res.Outcome)
		if res.Reason != "" {
			text += ": " + res.Reason
		}
		return mcpToolCallResult{Content: []mcpContent{{Type: "text", Text: text}}, IsError: true}
	}
}

func writeJSONRPC(w http.ResponseWriter, resp jsonrpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
