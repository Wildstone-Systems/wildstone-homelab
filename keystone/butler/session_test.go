package butler

import (
	"context"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
)

func TestSession_ReadFlow_GetHealth(t *testing.T) {
	h := startButlerGateway(t, []string{"tok"})
	agent := newButlerTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	agent.setHealth(true, "all services nominal")

	store := newTestStore(t)
	gw := newTestToolGateway(store, &AgentReader{Gateway: h.gw})
	gw.DefaultNode = "node-a"
	sess := &Session{Gateway: gw}

	sp := &scriptedProvider{responses: []*provider.ChatResponse{
		{
			ToolCalls: []provider.ToolCall{
				{ID: "1", Name: "get_health", Input: map[string]any{"node": "node-a"}},
			},
			StopReason: "tool_use",
		},
		{
			Text:       "node-a is healthy: all services nominal.",
			StopReason: "end_turn",
		},
	}}

	tr, err := sess.Run(context.Background(), sp, "Is node-a healthy?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tr.CappedIterations {
		t.Fatalf("Run hit the iteration cap unexpectedly")
	}
	if tr.FinalText != "node-a is healthy: all services nominal." {
		t.Fatalf("FinalText = %q", tr.FinalText)
	}
	if len(tr.Audit.Calls) != 1 {
		t.Fatalf("Audit.Calls = %d, want 1: %+v", len(tr.Audit.Calls), tr.Audit.Calls)
	}
	call := tr.Audit.Calls[0]
	if call.Name != "get_health" {
		t.Fatalf("Audit.Calls[0].Name = %q", call.Name)
	}
	if !contains(call.Result, "all services nominal") {
		t.Fatalf("Audit.Calls[0].Result missing health detail: %q", call.Result)
	}
	if tr.Audit.Provider != "scripted" {
		t.Fatalf("Audit.Provider = %q", tr.Audit.Provider)
	}

	// The second Chat call must have received the tool result as a "tool"
	// message so the model actually saw it.
	if len(sp.requests) != 2 {
		t.Fatalf("provider received %d requests, want 2", len(sp.requests))
	}
	secondReq := sp.requests[1]
	found := false
	for _, m := range secondReq.Messages {
		if m.Role == "tool" {
			for _, r := range m.ToolResults {
				if contains(r.Content, "all services nominal") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("second Chat request did not carry the get_health tool result back to the model")
	}
}

func TestSession_BoundedLoop_TerminatesAtCap(t *testing.T) {
	store := newTestStore(t)
	gw := newTestToolGateway(store, nil)
	sess := &Session{Gateway: gw, MaxIterations: 3}

	p := &alwaysToolCallProvider{toolName: "get_system_overview"}

	done := make(chan struct{})
	var tr *Transcript
	var err error
	go func() {
		tr, err = sess.Run(context.Background(), p, "loop forever please")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Session.Run did not terminate — the iteration cap did not bound it")
	}

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !tr.CappedIterations {
		t.Fatalf("Transcript.CappedIterations = false, want true")
	}
	if p.calls != 3 {
		t.Fatalf("provider was called %d times, want exactly MaxIterations=3", p.calls)
	}
}
