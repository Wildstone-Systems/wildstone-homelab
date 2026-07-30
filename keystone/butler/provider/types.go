// Package provider defines a provider-neutral abstraction for chatting with
// an LLM and driving tool calls, plus stdlib-only clients for Anthropic,
// OpenAI-compatible, and Ollama backends.
//
// This package is the S4a slice of ADR-W006 (Spike S4): the Provider
// interface, the three backend clients, and the tool-reliability probe. It
// does not build the tool gateway, agent loop, or read tools over Keystone
// state — that is keystone/butler (S4b).
package provider

import "context"

// Tool is a provider-neutral tool declaration: a name, a human-readable
// description, and a JSON-schema-shaped parameter spec (the same shape as
// Anthropic's input_schema / OpenAI's function.parameters / Ollama's
// function.parameters).
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolCall is a request from the model to invoke a tool with a given input.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResult is the result of executing a ToolCall, keyed back to the call
// that produced it.
type ToolResult struct {
	CallID  string
	Content string
}

// Message is one turn in a conversation. Role is one of "user", "assistant",
// or "tool". An "assistant" message may carry ToolCalls (the model asking to
// invoke tools). A "tool" message carries ToolResults (the caller's
// responses to a prior assistant turn's tool calls) and is translated by
// each provider into that API's tool-result shape.
type Message struct {
	Role        string
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// ChatRequest is a provider-neutral chat request.
type ChatRequest struct {
	System    string
	Messages  []Message
	Tools     []Tool
	Model     string
	MaxTokens int
}

// ChatResponse is a provider-neutral chat response: either assistant text,
// or one or more tool calls the caller must execute and feed back as a
// "tool" Message.
type ChatResponse struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
}

// Provider is the interface every backend (Anthropic, OpenAI-compatible,
// Ollama) implements. Chat is stateless — the caller (the S4b agent loop)
// owns conversation history and re-sends it on every call.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	Name() string
}
