package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAnthropicTestProvider(t *testing.T, handler http.HandlerFunc) (*AnthropicProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := &AnthropicProvider{
		BaseURL: srv.URL,
		APIKey:  "sk-ant-test-secret-key",
		Model:   DefaultAnthropicModel,
	}
	return p, srv
}

func TestAnthropicProvider_Name(t *testing.T) {
	p := NewAnthropicProvider("k")
	if p.Name() != "anthropic" {
		t.Fatalf("Name() = %q, want %q", p.Name(), "anthropic")
	}
}

func TestAnthropicProvider_RequestShape_ToolCallResponse(t *testing.T) {
	var gotPath string
	var gotHeaders http.Header
	var gotBody map[string]any

	p, _ := newAnthropicTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{
			"content": [
				{"type": "tool_use", "id": "toolu_123", "name": "get_health", "input": {"service": "widget-api"}}
			],
			"stop_reason": "tool_use"
		}`))
	})

	resp, err := p.Chat(context.Background(), ChatRequest{
		System: "You are a diagnostic assistant.",
		Messages: []Message{
			{Role: "user", Text: "Check widget-api"},
		},
		Tools: []Tool{
			{
				Name:        "get_health",
				Description: "Fetch health",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"service": map[string]any{"type": "string"},
					},
				},
			},
		},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	// (a) well-formed request: right endpoint, headers, system, tools shape
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotHeaders.Get("x-api-key") != "sk-ant-test-secret-key" {
		t.Errorf("x-api-key header = %q", gotHeaders.Get("x-api-key"))
	}
	if gotHeaders.Get("anthropic-version") != anthropicAPIVersion {
		t.Errorf("anthropic-version header = %q", gotHeaders.Get("anthropic-version"))
	}
	if gotBody["system"] != "You are a diagnostic assistant." {
		t.Errorf("system field = %v", gotBody["system"])
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", gotBody["tools"])
	}
	toolDef := tools[0].(map[string]any)
	if toolDef["name"] != "get_health" {
		t.Errorf("tool name = %v", toolDef["name"])
	}
	if _, ok := toolDef["input_schema"]; !ok {
		t.Errorf("tool def missing input_schema: %v", toolDef)
	}

	// (b) correctly parses a tool-call response
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %v, want 1", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Name != "get_health" {
		t.Errorf("ToolCalls[0].Name = %q", resp.ToolCalls[0].Name)
	}
	if resp.ToolCalls[0].Input["service"] != "widget-api" {
		t.Errorf("ToolCalls[0].Input = %v", resp.ToolCalls[0].Input)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
}

func TestAnthropicProvider_TextResponse(t *testing.T) {
	p, _ := newAnthropicTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"content": [{"type": "text", "text": "All systems nominal."}], "stop_reason": "end_turn"}`))
	})

	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "status?"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Text != "All systems nominal." {
		t.Errorf("Text = %q", resp.Text)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want none", resp.ToolCalls)
	}
}

func TestAnthropicProvider_MultiTurn_ToolResultShape(t *testing.T) {
	var gotBody map[string]any

	p, _ := newAnthropicTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"content": [{"type": "text", "text": "widget-api is unhealthy: connection refused."}], "stop_reason": "end_turn"}`))
	})

	_, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{
			{Role: "user", Text: "Check widget-api"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "toolu_123", Name: "get_health", Input: map[string]any{"service": "widget-api"}},
				},
			},
			{
				Role: "tool",
				ToolResults: []ToolResult{
					{CallID: "toolu_123", Content: "unhealthy: connection refused"},
				},
			},
		},
		Tools: []Tool{probeTool},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}

	// Turn 2: assistant with a tool_use block
	assistantMsg := messages[1].(map[string]any)
	if assistantMsg["role"] != "assistant" {
		t.Fatalf("messages[1].role = %v", assistantMsg["role"])
	}
	assistantContent := assistantMsg["content"].([]any)
	toolUseBlock := assistantContent[0].(map[string]any)
	if toolUseBlock["type"] != "tool_use" || toolUseBlock["id"] != "toolu_123" {
		t.Errorf("assistant tool_use block = %v", toolUseBlock)
	}

	// Turn 3: user message carrying a tool_result block (Anthropic shape)
	toolMsg := messages[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Fatalf("messages[2].role = %v, want user (tool_result rides on a user turn)", toolMsg["role"])
	}
	toolContent := toolMsg["content"].([]any)
	resultBlock := toolContent[0].(map[string]any)
	if resultBlock["type"] != "tool_result" {
		t.Errorf("result block type = %v", resultBlock["type"])
	}
	if resultBlock["tool_use_id"] != "toolu_123" {
		t.Errorf("result block tool_use_id = %v", resultBlock["tool_use_id"])
	}
	if resultBlock["content"] != "unhealthy: connection refused" {
		t.Errorf("result block content = %v", resultBlock["content"])
	}
}

func TestAnthropicProvider_KeyNeverLeaksInError(t *testing.T) {
	p, _ := newAnthropicTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": {"type": "api_error", "message": "internal error"}}`))
	})

	_, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if strings.Contains(err.Error(), p.APIKey) {
		t.Fatalf("error message leaked API key: %v", err)
	}
}
