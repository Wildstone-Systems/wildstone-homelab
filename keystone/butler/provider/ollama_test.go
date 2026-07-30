package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newOllamaTestProvider(t *testing.T, handler http.HandlerFunc) *OllamaProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &OllamaProvider{
		Host:  srv.URL,
		Model: "llama3.1",
	}
}

func TestOllamaProvider_Name(t *testing.T) {
	p := NewOllamaProvider("http://localhost:11434")
	if p.Name() != "ollama" {
		t.Fatalf("Name() = %q, want %q", p.Name(), "ollama")
	}
}

func TestOllamaProvider_RequestShape_ToolCallResponse(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	p := newOllamaTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{
			"message": {
				"role": "assistant",
				"tool_calls": [{"function": {"name": "get_health", "arguments": {"service": "widget-api"}}}]
			},
			"done": true,
			"done_reason": "stop"
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
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if gotPath != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", gotPath)
	}
	if gotBody["stream"] != false {
		t.Errorf("stream = %v, want false", gotBody["stream"])
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	sysMsg := msgs[0].(map[string]any)
	if sysMsg["role"] != "system" || sysMsg["content"] != "You are a diagnostic assistant." {
		t.Errorf("system message = %v", sysMsg)
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", gotBody["tools"])
	}
	toolDef := tools[0].(map[string]any)
	if toolDef["type"] != "function" {
		t.Errorf("tool type = %v", toolDef["type"])
	}
	fn := toolDef["function"].(map[string]any)
	if fn["name"] != "get_health" {
		t.Errorf("function name = %v", fn["name"])
	}
	if _, ok := fn["parameters"]; !ok {
		t.Errorf("function missing parameters: %v", fn)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %v, want 1", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Name != "get_health" {
		t.Errorf("ToolCalls[0].Name = %q", resp.ToolCalls[0].Name)
	}
	if resp.ToolCalls[0].Input["service"] != "widget-api" {
		t.Errorf("ToolCalls[0].Input = %v", resp.ToolCalls[0].Input)
	}
}

func TestOllamaProvider_TextResponse(t *testing.T) {
	p := newOllamaTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"message": {"role": "assistant", "content": "All nominal."}, "done": true, "done_reason": "stop"}`))
	})

	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "status?"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Text != "All nominal." {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestOllamaProvider_MultiTurn_ToolResultShape(t *testing.T) {
	var gotBody map[string]any

	p := newOllamaTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"message": {"role": "assistant", "content": "unhealthy"}, "done": true, "done_reason": "stop"}`))
	})

	_, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{
			{Role: "user", Text: "Check widget-api"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "1", Name: "get_health", Input: map[string]any{"service": "widget-api"}},
				},
			},
			{
				Role: "tool",
				ToolResults: []ToolResult{
					{CallID: "1", Content: "unhealthy: connection refused"},
				},
			},
		},
		Tools: []Tool{probeTool},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	assistantMsg := msgs[1].(map[string]any)
	if assistantMsg["role"] != "assistant" {
		t.Fatalf("messages[1].role = %v", assistantMsg["role"])
	}
	tcs := assistantMsg["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_health" {
		t.Errorf("tool_calls[0].function.name = %v", fn["name"])
	}

	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Fatalf("messages[2].role = %v, want tool", toolMsg["role"])
	}
	if toolMsg["content"] != "unhealthy: connection refused" {
		t.Errorf("content = %v", toolMsg["content"])
	}
}

func TestOllamaProvider_KeyNeverLeaksInError(t *testing.T) {
	p := newOllamaTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal error"}`))
	})
	_, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if strings.Contains(err.Error(), "internal error") == false {
		t.Fatalf("expected error to surface server message, got: %v", err)
	}
}
