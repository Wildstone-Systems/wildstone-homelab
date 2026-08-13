package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// DefaultAnthropicModel is the default Anthropic model id, per the
// claude-api skill's Current Models table / "always use claude-opus-5
// unless the user explicitly names a different model" default guidance.
const DefaultAnthropicModel = "claude-opus-5"

const anthropicAPIVersion = "2023-06-01"

// AnthropicProvider is a thin stdlib net/http client for the Anthropic
// Messages API (POST /v1/messages). BaseURL is configurable so tests can
// point it at an httptest fake; it defaults to the real Anthropic host.
type AnthropicProvider struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// NewAnthropicProvider builds a provider against the real Anthropic API.
// apiKey should come from config/env — never hardcode it, and it is never
// logged or included in returned errors.
func NewAnthropicProvider(apiKey string) *AnthropicProvider {
	return &AnthropicProvider{
		BaseURL: "https://api.anthropic.com",
		APIKey:  apiKey,
		Model:   DefaultAnthropicModel,
	}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

// --- Anthropic Messages API wire shapes ---

type anthropicToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicContentBlock is a union of the block shapes we need: text,
// tool_use (assistant call), and tool_result (our reply to a call).
type anthropicContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Tools     []anthropicToolDef `json:"tools,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicResponse struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
}

type anthropicErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *AnthropicProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = p.Model
	}
	if model == "" {
		model = DefaultAnthropicModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	areq := anthropicRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  toAnthropicMessages(req.Messages),
	}
	for _, t := range req.Tools {
		areq.Tools = append(areq.Tools, anthropicToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	body, err := json.Marshal(areq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	client := httpClientOrDefault(p.HTTPClient)
	status, respBody, err := doWithRetry(ctx, client, func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("anthropic: build request: %w", err)
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("x-api-key", p.APIKey)
		httpReq.Header.Set("anthropic-version", anthropicAPIVersion)
		return httpReq, nil
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed: %w", err)
	}

	if status != http.StatusOK {
		var eb anthropicErrorBody
		_ = json.Unmarshal(respBody, &eb)
		msg := eb.Error.Message
		if msg == "" {
			msg = "request failed"
		}
		return nil, fmt.Errorf("anthropic: status %d: %s", status, msg)
	}

	var aresp anthropicResponse
	if err := json.Unmarshal(respBody, &aresp); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	out := &ChatResponse{
		StopReason: aresp.StopReason,
		Truncated:  aresp.StopReason == "max_tokens",
	}
	for _, b := range aresp.Content {
		switch b.Type {
		case "text":
			out.Text += b.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return out, nil
}

// toAnthropicMessages translates provider-neutral Messages into the
// Messages API shape: assistant tool calls become tool_use blocks, and a
// "tool" role turn becomes a user message carrying tool_result blocks (the
// Messages API has no dedicated "tool" role — results ride back as user
// content).
func toAnthropicMessages(msgs []Message) []anthropicMessage {
	var out []anthropicMessage
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			var blocks []anthropicContentBlock
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: tr.CallID,
					Content:   tr.Content,
				})
			}
			out = append(out, anthropicMessage{Role: "user", Content: blocks})
		case "assistant":
			var blocks []anthropicContentBlock
			if m.Text != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: tc.Input,
				})
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
		default: // "user"
			out = append(out, anthropicMessage{
				Role:    "user",
				Content: []anthropicContentBlock{{Type: "text", Text: m.Text}},
			})
		}
	}
	return out
}
