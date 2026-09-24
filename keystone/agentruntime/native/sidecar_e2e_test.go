package native

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
)

// These tests run the REAL TypeScript sidecar — the real OpenAI Agents
// SDK loop — with a scripted model in place of a provider. They catch
// what the fake sidecar cannot: drift between protocol.go and
// protocol.ts, and the SDK behaving differently from what the adapter
// assumes.
//
// They need a built sidecar:
//
//	(cd runtimes/native-agent && npm ci && npm run build)
//	WILDSTONE_NATIVE_AGENT_SIDECAR=$PWD/runtimes/native-agent/dist/src/main.js \
//	  go test ./keystone/agentruntime/native -run E2E
//
// WILDSTONE_NATIVE_AGENT_NODE picks the Node to run it with (default:
// node on PATH). CI sets it to the Node inside the release package, so
// the package itself is what gets tested.
const (
	sidecarPathEnv = "WILDSTONE_NATIVE_AGENT_SIDECAR"
	sidecarNodeEnv = "WILDSTONE_NATIVE_AGENT_NODE"
)

func realAdapter(t *testing.T, script string) *Adapter {
	t.Helper()
	path := os.Getenv(sidecarPathEnv)
	if path == "" {
		t.Skip(sidecarPathEnv + " not set; see the comment above")
	}
	node := os.Getenv(sidecarNodeEnv)
	if node == "" {
		var err error
		if node, err = exec.LookPath("node"); err != nil {
			t.Skip("node not installed")
		}
	}
	a, err := New(Config{
		Command: []string{node, path},
		Limits: agentruntime.Limits{
			JobTimeout: 30 * time.Second, TurnTimeout: 10 * time.Second, StallTimeout: 10 * time.Second,
			HeartbeatInterval: 250 * time.Millisecond, MaxTurns: 6,
		},
		Env: []string{"WILDSTONE_AGENT_ALLOW_SCRIPTED_MODEL=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(script)) {
		t.Fatalf("bad script: %s", script)
	}
	a.cfg.scriptedModel = json.RawMessage(script)
	return a
}

func TestE2EHealth(t *testing.T) {
	a := realAdapter(t, `[{"text":"x"}]`)
	h := a.Health(context.Background())
	if !h.OK || !strings.Contains(h.Detail, "openai-agents-sdk") {
		t.Fatalf("health: %+v", h)
	}
	caps, _ := a.Capabilities(context.Background())
	if caps.EngineVersion == "" || caps.EngineVersion == "unknown" {
		t.Fatalf("capabilities: %+v", caps)
	}
}

func TestE2ECompletesThroughWildstoneTools(t *testing.T) {
	a := realAdapter(t, `[
		{"tool_calls":[{"name":"lookup","arguments":"{\"room\":\"kitchen\"}"}]},
		{"text":"The kitchen lights are on."}
	]`)
	tools, log := standardTools(), &auditLog{}
	startJob(t, a, "e1", tools, log, agentruntime.Limits{})
	st := waitFor(t, a, "e1")
	expectState(t, st, agentruntime.StateCompleted, "")
	if st.Output != "The kitchen lights are on." || st.ToolCalls != 1 {
		t.Fatalf("status %+v", st)
	}
	if got := tools.called(); len(got) != 1 || got[0] != "lookup" {
		t.Fatalf("tools called %v", got)
	}
	if !log.has(agentruntime.AuditToolRequested) || !log.has(agentruntime.AuditToolResult) {
		t.Fatalf("audit %v", log.types())
	}
}

func TestE2EAskThenResume(t *testing.T) {
	a := realAdapter(t, `[
		{"tool_calls":[{"name":"ask_user","arguments":"{\"question\":\"Which room?\"}"}]},
		{"text":"Done."}
	]`)
	startJob(t, a, "e2", standardTools(), &auditLog{}, agentruntime.Limits{})
	st := waitFor(t, a, "e2")
	expectState(t, st, agentruntime.StateWaitingForUser, "")
	if st.Question != "Which room?" {
		t.Fatalf("question %q", st.Question)
	}
	// A resumed run starts a fresh sidecar, and so a fresh script: it
	// asks again. That the history is accepted and a definite state
	// follows is what matters here; the sidecar's own tests cover the
	// history's content.
	if _, err := a.Resume(context.Background(), "e2", agentruntime.Event{Kind: agentruntime.EventUserReply, Text: "kitchen"}); err != nil {
		t.Fatal(err)
	}
	if st := waitFor(t, a, "e2"); !st.State.Settled() {
		t.Fatalf("resumed job did not settle: %s", st.State)
	}
}

// The request that used to stall: a model that keeps calling the same
// tool, never answering. It now ends in a definite state.
func TestE2EFormerlyStallingRequestTerminates(t *testing.T) {
	a := realAdapter(t, `[{"tool_calls":[{"name":"lookup","arguments":"{\"room\":\"kitchen\"}"}]}]`)
	startJob(t, a, "e3", standardTools(), &auditLog{}, agentruntime.Limits{})
	st := waitFor(t, a, "e3")
	expectState(t, st, agentruntime.StateBlocked, "")
}

func TestE2EMalformedArgumentsReachKeystone(t *testing.T) {
	a := realAdapter(t, `[
		{"tool_calls":[{"name":"lookup","arguments":"not json"}]},
		{"text":"Sorry, done."}
	]`)
	tools, log := standardTools(), &auditLog{}
	startJob(t, a, "e4", tools, log, agentruntime.Limits{})
	expectState(t, waitFor(t, a, "e4"), agentruntime.StateCompleted, "")
	if !log.has(agentruntime.AuditInvalidAction) || len(tools.called()) != 0 {
		t.Fatalf("audit %v calls %v", log.types(), tools.called())
	}
}

func TestE2ECancel(t *testing.T) {
	a := realAdapter(t, `[{"sleep_ms":20000,"text":"too late"}]`)
	startJob(t, a, "e5", standardTools(), &auditLog{}, agentruntime.Limits{})
	time.Sleep(500 * time.Millisecond)
	if _, err := a.Cancel(context.Background(), "e5"); err != nil {
		t.Fatal(err)
	}
	expectState(t, waitFor(t, a, "e5"), agentruntime.StateCancelled, "")
}
