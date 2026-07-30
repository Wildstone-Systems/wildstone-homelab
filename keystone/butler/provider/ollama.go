package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DefaultOllamaModel is a reasonable default for a local Ollama daemon.
// Ollama model availability is host-local (whatever the operator has
// pulled), so this is a placeholder callers are expected to override via
// Model / ChatRequest.Model to match a model actually present on the host.
const DefaultOllamaModel = "llama3.1"

// OllamaProvider is a thin stdlib net/http client for Ollama's native
// /api/chat endpoint with tools. Host is configurable so tests can point it
// at an httptest fake; it defaults to the standard local Ollama daemon
// address.
type OllamaProvider struct {
	Host       string
	Model      string
	HTTPClient *http.Client
}

// NewOllamaProvider builds a provider against a local Ollama daemon at
// host (e.g. "http://localhost:11434"). Ollama has no API key by default —
// it is a local daemon, not a cloud service.
func NewOllamaProvider(host string) *OllamaProvider {
	return &OllamaProvider{
		Host:  host,
		Model: DefaultOllamaModel,
	}
}

func (p *OllamaProvider) Name() string { return "ollama" }

// --- Ollama /api/chat wire shapes ---

type ollamaFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ollamaTool struct {
	Type     string            `json:"type"`
	Function ollamaFunctionDef `json:"function"`
}

type ollamaToolCallFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
}

type ollamaResponse struct {
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason,omitempty"`
}

type ollamaErrorBody struct {
	Error string `json:"error"`
}

func (p *OllamaProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = p.Model
	}
	if model == "" {
		model = DefaultOllamaModel
	}

	oreq := ollamaRequest{
		Model:    model,
		Messages: toOllamaMessages(req.System, req.Messages),
		Stream:   false,
	}
	for _, t := range req.Tools {
		oreq.Tools = append(oreq.Tools, ollamaTool{
			Type: "function",
			Function: ollamaFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	body, err := json.Marshal(oreq)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var eb ollamaErrorBody
		_ = json.Unmarshal(respBody, &eb)
		msg := eb.Error
		if msg == "" {
			msg = "request failed"
		}
		return nil, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, msg)
	}

	var oresp ollamaResponse
	if err := json.Unmarshal(respBody, &oresp); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}

	out := &ChatResponse{
		Text:       oresp.Message.Content,
		StopReason: oresp.DoneReason,
	}
	for _, tc := range oresp.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
		})
	}
	return out, nil
}

// toOllamaMessages translates provider-neutral Messages into Ollama chat
// turns: system prompt becomes the leading system message, assistant tool
// calls become message.tool_calls, and a "tool" role turn becomes one
// role:"tool" message per ToolResult. Ollama's tool_calls carry no call ID
// on the wire, so ToolResult.CallID is not transmitted (it is only used by
// the caller to correlate results with the calls it made).
func toOllamaMessages(system string, msgs []Message) []ollamaMessage {
	var out []ollamaMessage
	if system != "" {
		out = append(out, ollamaMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			for _, tr := range m.ToolResults {
				out = append(out, ollamaMessage{Role: "tool", Content: tr.Content})
			}
		case "assistant":
			am := ollamaMessage{Role: "assistant", Content: m.Text}
			for _, tc := range m.ToolCalls {
				am.ToolCalls = append(am.ToolCalls, ollamaToolCall{
					Function: ollamaToolCallFunction{Name: tc.Name, Arguments: tc.Input},
				})
			}
			out = append(out, am)
		default: // "user"
			out = append(out, ollamaMessage{Role: "user", Content: m.Text})
		}
	}
	return out
}
