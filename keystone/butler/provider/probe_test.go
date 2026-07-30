package provider

import (
	"context"
	"testing"
)

// scriptedProvider is a tiny in-test fake Provider that returns whatever
// ChatResponse is queued, in order. It doesn't touch the network — it
// exists purely to test Probe's verdict logic.
type scriptedProvider struct {
	responses []*ChatResponse
	i         int
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Chat(_ context.Context, _ ChatRequest) (*ChatResponse, error) {
	if s.i >= len(s.responses) {
		return &ChatResponse{Text: "(no more scripted responses)"}, nil
	}
	resp := s.responses[s.i]
	s.i++
	return resp, nil
}

func TestProbe_CorrectToolCall_Full(t *testing.T) {
	sp := &scriptedProvider{
		responses: []*ChatResponse{
			{
				ToolCalls: []ToolCall{
					{ID: "1", Name: "get_health", Input: map[string]any{"service": "widget-api"}},
				},
				StopReason: "tool_use",
			},
		},
	}

	verdict, err := Probe(context.Background(), sp)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if verdict != Full {
		t.Fatalf("Probe() = %v, want Full", verdict)
	}
}

func TestProbe_RamblingText_QAOnly(t *testing.T) {
	sp := &scriptedProvider{
		responses: []*ChatResponse{
			{
				Text:       "I think the service might be having some kind of issue, hard to say without more context, maybe check the logs?",
				StopReason: "end_turn",
			},
		},
	}

	verdict, err := Probe(context.Background(), sp)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if verdict != QAOnly {
		t.Fatalf("Probe() = %v, want QAOnly", verdict)
	}
}

func TestProbe_WrongTool_QAOnly(t *testing.T) {
	sp := &scriptedProvider{
		responses: []*ChatResponse{
			{
				ToolCalls: []ToolCall{
					{ID: "1", Name: "restart_service", Input: map[string]any{"service": "widget-api"}},
				},
				StopReason: "tool_use",
			},
		},
	}

	verdict, err := Probe(context.Background(), sp)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if verdict != QAOnly {
		t.Fatalf("Probe() = %v, want QAOnly (wrong tool name)", verdict)
	}
}

func TestVerdict_String(t *testing.T) {
	if Full.String() != "Full" {
		t.Errorf("Full.String() = %q", Full.String())
	}
	if QAOnly.String() != "QAOnly" {
		t.Errorf("QAOnly.String() = %q", QAOnly.String())
	}
}
