package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DefaultOpenAIModel is a reasonable default for OpenAI-compatible backends
// (LiteLLM, vLLM, OpenRouter, LM Studio, ...). Unlike the Anthropic model
// id, this is not sourced from the claude-api skill; it is a placeholder
// callers are expected to override via Model / ChatRequest.Model to match
// whatever the target endpoint actually serves.
const DefaultOpenAIModel = "gpt-4o"

// OpenAIProvider is a thin stdlib net/http client for an OpenAI-compatible
// chat/completions endpoint with function calling. BaseURL is configurable
// so tests can point it at an httptest fake, and so it can be pointed at
// LiteLLM/vLLM/OpenRouter/LM Studio in real deployments.
type OpenAIProvider struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// NewOpenAIProvider builds a provider against the given OpenAI-compatible
// base URL (e.g. "https://api.openai.com/v1" or a LiteLLM gateway URL).
// apiKey should come from config/env — never hardcode it, and it is never
// logged or included in returned errors.
func NewOpenAIProvider(baseURL, apiKey string) *OpenAIProvider {
	return &OpenAIProvider{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   DefaultOpenAIModel,
	}
}

func (p *OpenAIProvider) Name() string { return "openai" }

// --- OpenAI chat/completions wire shapes ---

type openAIFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type openAITool struct {
	Type     string            `json:"type"`
	Function openAIFunctionDef `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	Tools     []openAITool    `json:"tools,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

type openAIChoice struct {
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
}

type openAIErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (p *OpenAIProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = p.Model
	}
	if model == "" {
		model = DefaultOpenAIModel
	}

	oreq := openAIRequest{
		Model:     model,
		MaxTokens: req.MaxTokens,
		Messages:  toOpenAIMessages(req.System, req.Messages),
	}
	for _, t := range req.Tools {
		oreq.Tools = append(oreq.Tools, openAITool{
			Type: "function",
			Function: openAIFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	body, err := json.Marshal(oreq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("authorization", "Bearer "+p.APIKey)
	}

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var eb openAIErrorBody
		_ = json.Unmarshal(respBody, &eb)
		msg := eb.Error.Message
		if msg == "" {
			msg = "request failed"
		}
		return nil, fmt.Errorf("openai: status %d: %s", resp.StatusCode, msg)
	}

	var oresp openAIResponse
	if err := json.Unmarshal(respBody, &oresp); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(oresp.Choices) == 0 {
		return nil, fmt.Errorf("openai: response had no choices")
	}

	choice := oresp.Choices[0]
	out := &ChatResponse{
		Text:       choice.Message.Content,
		StopReason: choice.FinishReason,
	}
	for _, tc := range choice.Message.ToolCalls {
		var input map[string]any
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				return nil, fmt.Errorf("openai: decode tool call arguments: %w", err)
			}
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}
	return out, nil
}

// toOpenAIMessages translates provider-neutral Messages into
// chat/completions turns: system prompt becomes the leading system
// message, assistant tool calls become message.tool_calls, and a "tool"
// role turn becomes one role:"tool" message per ToolResult (chat/completions
// requires one tool message per tool_call_id, not a batched block).
func toOpenAIMessages(system string, msgs []Message) []openAIMessage {
	var out []openAIMessage
	if system != "" {
		out = append(out, openAIMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			for _, tr := range m.ToolResults {
				out = append(out, openAIMessage{
					Role:       "tool",
					Content:    tr.Content,
					ToolCallID: tr.CallID,
				})
			}
		case "assistant":
			am := openAIMessage{Role: "assistant", Content: m.Text}
			for _, tc := range m.ToolCalls {
				args, _ := json.Marshal(tc.Input)
				am.ToolCalls = append(am.ToolCalls, openAIToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openAIFunctionCall{
						Name:      tc.Name,
						Arguments: string(args),
					},
				})
			}
			out = append(out, am)
		default: // "user"
			out = append(out, openAIMessage{Role: "user", Content: m.Text})
		}
	}
	return out
}
