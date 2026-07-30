package butler

import (
	"context"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
)

// scriptedProvider is a tiny fake Provider that returns whatever
// ChatResponse is queued, in order, mirroring provider's own
// scriptedProvider test pattern (S4a). It never touches the network.
type scriptedProvider struct {
	responses []*provider.ChatResponse
	i         int
	// requests records every ChatRequest this provider was called with,
	// so tests can assert on what the Butler actually sent (tool results
	// fed back, etc).
	requests []provider.ChatRequest
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	s.requests = append(s.requests, req)
	if s.i >= len(s.responses) {
		return &provider.ChatResponse{Text: "(no more scripted responses)"}, nil
	}
	resp := s.responses[s.i]
	s.i++
	return resp, nil
}

// alwaysToolCallProvider always requests the same tool call, forever —
// used to test the agent loop's iteration cap.
type alwaysToolCallProvider struct {
	toolName string
	input    map[string]any
	calls    int
}

func (a *alwaysToolCallProvider) Name() string { return "always-tool-call" }

func (a *alwaysToolCallProvider) Chat(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	a.calls++
	return &provider.ChatResponse{
		ToolCalls:  []provider.ToolCall{{ID: "id", Name: a.toolName, Input: a.input}},
		StopReason: "tool_use",
	}, nil
}
