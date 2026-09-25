package butlerlive

// A Butler conversation on the REAL native runtime: the TypeScript
// sidecar and the OpenAI Agents SDK, talking over HTTP to a fake
// Ollama-style OpenAI-compatible server. Nothing between Serve and the
// wire is faked.
//
// It needs a built sidecar (see keystone/agentruntime/native's E2E tests):
//
//	WILDSTONE_NATIVE_AGENT_SIDECAR=$PWD/runtimes/native-agent/dist/src/main.js \
//	  go test ./keystone/butlerlive -run E2E

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime/native"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// fakeOllama answers /v1/chat/completions: first with a tool call, then
// with an answer. It keeps every request body.
type fakeOllama struct {
	mu     sync.Mutex
	bodies []string
}

func (s *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, string(body))
	n := len(s.bodies)
	s.mu.Unlock()

	msg := map[string]any{"role": "assistant", "content": "n1 is healthy."}
	finish := "stop"
	if n == 1 {
		msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_health", "arguments": `{"node":"n1"}`},
		}}}
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "created": time.Now().Unix(), "model": "llama",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func (s *fakeOllama) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func TestE2EAButlerConversationOnTheRealNativeRuntime(t *testing.T) {
	sidecar := os.Getenv("WILDSTONE_NATIVE_AGENT_SIDECAR")
	if sidecar == "" {
		t.Skip("WILDSTONE_NATIVE_AGENT_SIDECAR not set")
	}
	node := os.Getenv("WILDSTONE_NATIVE_AGENT_NODE")
	if node == "" {
		var err error
		if node, err = exec.LookPath("node"); err != nil {
			t.Skip("node not installed")
		}
	}
	server := &fakeOllama{}
	srv := httptest.NewServer(server)
	defer srv.Close()

	f := newFixture(t)
	// Alfred's local route now points at the fake server.
	if err := f.db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Endpoint: srv.URL, Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	rt, err := native.New(native.Config{Command: []string{node, sidecar}})
	if err != nil {
		t.Fatal(err)
	}
	if h := rt.Health(context.Background()); !h.OK {
		t.Fatalf("sidecar unhealthy: %s", h.Detail)
	}
	f.butlers.Native = rt

	turn := f.turn("alfred", "is n1 healthy?")
	turn.History = nil
	ans, err := f.butlers.Serve(context.Background(), turn)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if ans.FinalText != "n1 is healthy." {
		t.Fatalf("answer %q", ans.FinalText)
	}
	if n := f.tools["alfred"].count("get_health/n1"); n != 1 {
		t.Fatalf("get_health dispatched %d times through the gateway, want 1", n)
	}
	task, err := f.db.GetButlerTask(ans.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task %s, want completed", task.State)
	}

	reqs := server.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model requests, want 2", len(reqs))
	}
	first := reqs[0]
	for _, want := range []string{`"model":"llama"`, "operator surface", "is n1 healthy?", "get_health", "report_blocked"} {
		if !strings.Contains(first, want) {
			t.Errorf("the model request lacks %q", want)
		}
	}
	if !strings.Contains(reqs[1], "n1") {
		t.Error("the tool result did not reach the model")
	}

	// The audit trail is in the event stream.
	events, err := f.db.EventsAfter(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	for _, want := range []string{"agent." + agentruntime.AuditToolRequested, "agent." + agentruntime.AuditToolResult, "agent." + agentruntime.AuditJobState} {
		if !seen[want] {
			t.Errorf("no %s event", want)
		}
	}
}
