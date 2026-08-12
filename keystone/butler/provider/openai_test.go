package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newOpenAITestProvider(t *testing.T, handler http.HandlerFunc) *OpenAIProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &OpenAIProvider{
		BaseURL: srv.URL,
		APIKey:  "sk-openai-test-secret-key",
		Model:   "gpt-4o",
	}
}

func TestOpenAIProvider_Name(t *testing.T) {
	p := NewOpenAIProvider("http://localhost", "k")
	if p.Name() != "openai" {
		t.Fatalf("Name() = %q, want %q", p.Name(), "openai")
	}
}

func TestOpenAIProvider_RequestShape_ToolCallResponse(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]any

	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{
			"choices": [{
				"message": {
					"role": "assistant",
					"tool_calls": [{
						"id": "call_abc",
						"type": "function",
						"function": {"name": "get_health", "arguments": "{\"service\":\"widget-api\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}]
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

	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-openai-test-secret-key" {
		t.Errorf("authorization header = %q", gotAuth)
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

func TestOpenAIProvider_TextResponse(t *testing.T) {
	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "All nominal."}, "finish_reason": "stop"}]}`))
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

func TestOpenAIProvider_MultiTurn_ToolResultShape(t *testing.T) {
	var gotBody map[string]any

	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "unhealthy"}, "finish_reason": "stop"}]}`))
	})

	_, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{
			{Role: "user", Text: "Check widget-api"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "call_abc", Name: "get_health", Input: map[string]any{"service": "widget-api"}},
				},
			},
			{
				Role: "tool",
				ToolResults: []ToolResult{
					{CallID: "call_abc", Content: "unhealthy: connection refused"},
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
	if tc["id"] != "call_abc" {
		t.Errorf("tool_calls[0].id = %v", tc["id"])
	}

	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Fatalf("messages[2].role = %v, want tool", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "call_abc" {
		t.Errorf("tool_call_id = %v", toolMsg["tool_call_id"])
	}
	if toolMsg["content"] != "unhealthy: connection refused" {
		t.Errorf("content = %v", toolMsg["content"])
	}
}

func TestOpenAIProvider_LengthFinishReasonSetsTruncated(t *testing.T) {
	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "cut off"}, "finish_reason": "length"}]}`))
	})

	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "status?"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.StopReason != "length" {
		t.Errorf("StopReason = %q, want length", resp.StopReason)
	}
	if !resp.Truncated {
		t.Error("Truncated = false, want true for finish_reason length")
	}
}

func TestOpenAIProvider_StopFinishReasonNotTruncated(t *testing.T) {
	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "done"}, "finish_reason": "stop"}]}`))
	})

	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "status?"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Truncated {
		t.Error("Truncated = true, want false for finish_reason stop")
	}
}

func TestOpenAIProvider_Retries429ThenSucceeds(t *testing.T) {
	var attempts int32
	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": {"type": "rate_limit", "message": "slow down"}}`))
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}]}`))
	})

	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 (2 failures + 1 success)", got)
	}
}

func TestOpenAIProvider_Retries5xxExhaustsAttempts(t *testing.T) {
	var attempts int32
	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error": {"type": "server_error", "message": "boom"}}`))
	})

	_, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries on persistent 502")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to surface the server message", err)
	}
	if got := atomic.LoadInt32(&attempts); got != maxChatAttempts {
		t.Errorf("attempts = %d, want %d (bounded retry)", got, maxChatAttempts)
	}
}

func TestOpenAIProvider_NoRetryOn404(t *testing.T) {
	var attempts int32
	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": {"type": "not_found", "message": "no such model"}}`))
	})

	_, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error on 404 response")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (a non-429 4xx must not be retried)", got)
	}
}

func TestOpenAIProvider_RetryAfterHonored(t *testing.T) {
	withInstantRetrySleep(t, func(ctx context.Context, d time.Duration) error {
		if d != 4*time.Second {
			t.Errorf("backoff duration = %v, want 4s (from Retry-After header)", d)
		}
		return nil
	})

	var attempts int32
	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": {"type": "rate_limit", "message": "slow down"}}`))
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}]}`))
	})

	if _, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "hi"}},
	}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
}

func TestOpenAIProvider_KeyNeverLeaksInError(t *testing.T) {
	p := newOpenAITestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": {"type": "server_error", "message": "internal error"}}`))
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
