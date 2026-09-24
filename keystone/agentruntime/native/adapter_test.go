package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
)

// --- test doubles ---------------------------------------------------------

// auditLog is a Recorder that keeps everything and can be told to fail.
type auditLog struct {
	mu     sync.Mutex
	events []agentruntime.AuditEvent
	failOn string
}

func (l *auditLog) Record(_ context.Context, e agentruntime.AuditEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failOn != "" && e.Type == l.failOn {
		return errors.New("ledger offline")
	}
	l.events = append(l.events, e)
	return nil
}

func (l *auditLog) types() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		t := e.Type
		if e.Type == agentruntime.AuditJobState {
			t += ":" + string(e.State)
		}
		out = append(out, t)
	}
	return out
}

func (l *auditLog) has(typ string) bool {
	for _, t := range l.types() {
		if t == typ {
			return true
		}
	}
	return false
}

// toolHost is a ToolHost whose tools are functions.
type toolHost struct {
	mu    sync.Mutex
	tools map[string]func(ctx context.Context, in map[string]any) (agentruntime.ToolResult, error)
	calls []string
}

func (h *toolHost) Specs() []agentruntime.ToolSpec {
	var out []agentruntime.ToolSpec
	for name := range h.tools {
		out = append(out, agentruntime.ToolSpec{Name: name, Description: name, Parameters: map[string]any{"type": "object"}})
	}
	return out
}

func (h *toolHost) Call(ctx context.Context, name string, in map[string]any) (agentruntime.ToolResult, error) {
	h.mu.Lock()
	h.calls = append(h.calls, name)
	fn := h.tools[name]
	h.mu.Unlock()
	return fn(ctx, in)
}

func (h *toolHost) called() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func standardTools() *toolHost {
	return &toolHost{tools: map[string]func(context.Context, map[string]any) (agentruntime.ToolResult, error){
		"lookup": func(_ context.Context, in map[string]any) (agentruntime.ToolResult, error) {
			return agentruntime.ToolResult{Content: "lights are on"}, nil
		},
		"broken": func(context.Context, map[string]any) (agentruntime.ToolResult, error) {
			return agentruntime.ToolResult{}, errors.New("backend down")
		},
		"forbidden": func(context.Context, map[string]any) (agentruntime.ToolResult, error) {
			return agentruntime.ToolResult{Content: "not permitted for you", Denied: true}, nil
		},
		"slow": func(ctx context.Context, _ map[string]any) (agentruntime.ToolResult, error) {
			<-ctx.Done()
			return agentruntime.ToolResult{}, ctx.Err()
		},
		"stubborn": func(context.Context, map[string]any) (agentruntime.ToolResult, error) {
			// Ignores its context entirely.
			time.Sleep(10 * time.Second)
			return agentruntime.ToolResult{Content: "late"}, nil
		},
	}}
}

// fastLimits keep every guardrail test well under a second.
var fastLimits = agentruntime.Limits{
	JobTimeout:        5 * time.Second,
	TurnTimeout:       2 * time.Second,
	ToolTimeout:       300 * time.Millisecond,
	StallTimeout:      2 * time.Second,
	HeartbeatInterval: 20 * time.Millisecond,
	MaxTurns:          6,
	MaxToolCalls:      10,
	MaxRepeatedCalls:  3,
	MaxToolFailures:   3,
	MaxInvalidActions: 2,
}

func fakeAdapter(t *testing.T, script []step, extraEnv ...string) *Adapter {
	t.Helper()
	b, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	env := append([]string{fakeSidecarEnv + "=1", fakeScriptEnv + "=" + string(b)}, extraEnv...)
	a, err := New(Config{
		Command: []string{os.Args[0], "-test.run=^TestFakeSidecar$"},
		Model:   Model{Name: "test-model", APIKey: "sk-test-secret"},
		Limits:  fastLimits,
		Env:     env,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func startJob(t *testing.T, a *Adapter, id string, tools agentruntime.ToolHost, rec agentruntime.Recorder, lim agentruntime.Limits) agentruntime.Status {
	t.Helper()
	st, err := a.Start(context.Background(), agentruntime.Job{
		ID: id, Instructions: "You are the Butler.", Input: "are the lights on?", Actor: "alice",
		Tools: tools, Recorder: rec, Limits: lim,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != agentruntime.StateQueued {
		t.Fatalf("Start returned %s, want QUEUED", st.State)
	}
	return st
}

func waitFor(t *testing.T, a *Adapter, id string) agentruntime.Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := a.Wait(ctx, id)
	if err != nil {
		t.Fatalf("Wait: %v (the job never settled: state %s) — this is the hang the contract forbids", err, st.State)
	}
	return st
}

func expectState(t *testing.T, st agentruntime.Status, want agentruntime.State, reasonHas string) {
	t.Helper()
	if st.State != want {
		t.Fatalf("state %s (reason %q), want %s", st.State, st.Reason, want)
	}
	if reasonHas != "" && !strings.Contains(st.Reason, reasonHas) {
		t.Fatalf("reason %q, want it to mention %q", st.Reason, reasonHas)
	}
}

func call(name, args string) *message { return &message{Name: name, Arguments: args} }
func send(m message) step             { return step{Send: &m} }
func exitWith(code int, stderr string) step {
	return step{Exit: &code, Stderr: stderr}
}

// --- scenarios ------------------------------------------------------------

func TestCompletesThroughWildstoneTools(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture.json")
	a := fakeAdapter(t, []step{
		send(message{Type: msgTurn, Turn: 1}),
		{Call: call("lookup", `{"room":"kitchen"}`)},
		send(message{Type: msgTurn, Turn: 2}),
		{FinalWithLastResult: true},
	}, fakeCaptureEnv+"="+capture)
	tools, log := standardTools(), &auditLog{}
	startJob(t, a, "j1", tools, log, agentruntime.Limits{})

	st := waitFor(t, a, "j1")
	expectState(t, st, agentruntime.StateCompleted, "")
	if st.Output != "result: lights are on" {
		t.Fatalf("output %q: the tool result did not travel back to the runtime", st.Output)
	}
	if st.ToolCalls != 1 || st.Turns != 2 || st.FinishedAt.IsZero() {
		t.Fatalf("counters: %+v", st)
	}
	if got := tools.called(); len(got) != 1 || got[0] != "lookup" {
		t.Fatalf("tools called %v", got)
	}
	want := []string{"job.state:QUEUED", "job.state:RUNNING", "tool.requested", "tool.result", "job.state:COMPLETED"}
	if got := log.types(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit trail\n got %v\nwant %v", got, want)
	}
	for _, e := range log.events {
		if e.Actor != "alice" || e.JobID != "j1" || e.Runtime != RuntimeName {
			t.Fatalf("audit event missing identity: %+v", e)
		}
	}

	// What the sidecar was given: the tools, the guidance, the model —
	// and the API key only in its environment, never in the stream.
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Start message  `json:"start"`
		Env   []string `json:"env"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Start.Instructions, "report_blocked") || got.Start.Model.Name != "test-model" {
		t.Fatalf("start message: %+v", got.Start)
	}
	if strings.Contains(string(raw[:strings.Index(string(raw), `"env"`)]), "sk-test-secret") {
		t.Fatal("the API key was written into the protocol stream")
	}
	var sawKey bool
	for _, kv := range got.Env {
		if kv == APIKeyEnv+"=sk-test-secret" {
			sawKey = true
		}
		if strings.HasPrefix(kv, "GOFLAGS=") || strings.HasPrefix(kv, "GOPATH=") {
			t.Fatalf("sidecar inherited keystone's environment: %s", kv)
		}
	}
	if !sawKey {
		t.Fatal("the sidecar did not receive its API key")
	}
}

func TestToolFailureIsAnObservationThenRepeatedFailuresBlock(t *testing.T) {
	t.Run("one failure, then recovery", func(t *testing.T) {
		a := fakeAdapter(t, []step{
			{Call: call("broken", `{}`)},
			{Call: call("lookup", `{}`)},
			{FinalWithLastResult: true},
		})
		log := &auditLog{}
		startJob(t, a, "j", standardTools(), log, agentruntime.Limits{})
		expectState(t, waitFor(t, a, "j"), agentruntime.StateCompleted, "")
		if !log.has(agentruntime.AuditToolFailed) {
			t.Fatalf("failure not audited: %v", log.types())
		}
	})
	t.Run("three in a row", func(t *testing.T) {
		a := fakeAdapter(t, []step{
			{Call: call("broken", `{"n":1}`)},
			{Call: call("broken", `{"n":2}`)},
			{Call: call("broken", `{"n":3}`)},
			{Call: call("lookup", `{}`)},
		})
		tools := standardTools()
		startJob(t, a, "j", tools, &auditLog{}, agentruntime.Limits{})
		expectState(t, waitFor(t, a, "j"), agentruntime.StateBlocked, "repeated tool failures")
		if n := len(tools.called()); n != 3 {
			t.Fatalf("%d calls ran, want 3", n)
		}
	})
}

func TestDeniedToolIsAuditedAndNeverRuns(t *testing.T) {
	a := fakeAdapter(t, []step{
		{Call: call("forbidden", `{}`)},
		{FinalWithLastResult: true},
	})
	log := &auditLog{}
	startJob(t, a, "j", standardTools(), log, agentruntime.Limits{})
	st := waitFor(t, a, "j")
	expectState(t, st, agentruntime.StateCompleted, "")
	if st.Output != "result: not permitted for you" {
		t.Fatalf("the model did not see the refusal: %q", st.Output)
	}
	if !log.has(agentruntime.AuditToolDenied) || log.has(agentruntime.AuditToolResult) {
		t.Fatalf("audit: %v", log.types())
	}
}

func TestMalformedActions(t *testing.T) {
	t.Run("bad arguments are refused before dispatch", func(t *testing.T) {
		a := fakeAdapter(t, []step{
			{Call: call("lookup", `[1,2]`)},
			{FinalWithLastResult: true},
		})
		tools, log := standardTools(), &auditLog{}
		startJob(t, a, "j", tools, log, agentruntime.Limits{})
		st := waitFor(t, a, "j")
		expectState(t, st, agentruntime.StateCompleted, "")
		if len(tools.called()) != 0 || !strings.Contains(st.Output, "JSON object") || !log.has(agentruntime.AuditInvalidAction) {
			t.Fatalf("calls %v output %q audit %v", tools.called(), st.Output, log.types())
		}
	})
	t.Run("a tool that does not exist is refused", func(t *testing.T) {
		a := fakeAdapter(t, []step{
			{Call: call("format_disk", `{}`)},
			{FinalWithLastResult: true},
		})
		tools := standardTools()
		startJob(t, a, "j", tools, &auditLog{}, agentruntime.Limits{})
		st := waitFor(t, a, "j")
		if len(tools.called()) != 0 || !strings.Contains(st.Output, "not an available tool") {
			t.Fatalf("calls %v output %q", tools.called(), st.Output)
		}
	})
	t.Run("a model that keeps producing them fails", func(t *testing.T) {
		bad := message{Type: msgInvalid, Name: "lookup", Reason: "arguments are not valid JSON"}
		a := fakeAdapter(t, []step{send(bad), send(bad), send(bad)})
		startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
		expectState(t, waitFor(t, a, "j"), agentruntime.StateFailed, "invalid actions")
	})
	t.Run("the SDK rejecting the model's action fails the job", func(t *testing.T) {
		a := fakeAdapter(t, []step{send(message{Type: msgError, Kind: "model_behavior", Message: "Tool nosuch not found"})})
		startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
		expectState(t, waitFor(t, a, "j"), agentruntime.StateFailed, "model_behavior")
	})
}

func TestNoNextActionBlocksInsteadOfHanging(t *testing.T) {
	a := fakeAdapter(t, []step{send(message{Type: msgFinal, Output: "  "})})
	startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
	expectState(t, waitFor(t, a, "j"), agentruntime.StateBlocked, "no answer and no next action")
}

func TestAgentReportsBlocked(t *testing.T) {
	a := fakeAdapter(t, []step{send(message{Type: msgBlocked, Reason: "I do not have access to the garage"})})
	startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
	expectState(t, waitFor(t, a, "j"), agentruntime.StateBlocked, "garage")
}

func TestRepeatedActionBlocks(t *testing.T) {
	a := fakeAdapter(t, []step{{Call: call("lookup", `{"room":"kitchen"}`), CallForever: true}})
	tools, log := standardTools(), &auditLog{}
	startJob(t, a, "j", tools, log, agentruntime.Limits{})
	st := waitFor(t, a, "j")
	expectState(t, st, agentruntime.StateBlocked, "repeating the same action")
	if n := len(tools.called()); n != fastLimits.MaxRepeatedCalls {
		t.Fatalf("%d identical calls ran, want %d", n, fastLimits.MaxRepeatedCalls)
	}
}

func TestToolBudgetBlocks(t *testing.T) {
	// Distinct inputs, so only the budget can stop it.
	var script []step
	for i := 0; i < 20; i++ {
		script = append(script, step{Call: call("lookup", fmt.Sprintf(`{"n":%d}`, i))})
	}
	a := fakeAdapter(t, script)
	lim := fastLimits
	lim.MaxToolCalls = 4
	startJob(t, a, "j", standardTools(), &auditLog{}, lim)
	st := waitFor(t, a, "j")
	expectState(t, st, agentruntime.StateBlocked, "tool call budget")
	if st.ToolCalls != 4 {
		t.Fatalf("ToolCalls %d, want 4", st.ToolCalls)
	}
}

func TestTurnBudget(t *testing.T) {
	t.Run("the SDK reports it", func(t *testing.T) {
		a := fakeAdapter(t, []step{send(message{Type: msgError, Kind: errKindMaxTurns, Message: "Max turns (6) exceeded"})})
		startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
		expectState(t, waitFor(t, a, "j"), agentruntime.StateBlocked, "turn budget")
	})
	t.Run("keystone enforces it even when the SDK does not", func(t *testing.T) {
		a := fakeAdapter(t, []step{{Turns: true}})
		startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
		expectState(t, waitFor(t, a, "j"), agentruntime.StateBlocked, "turn budget")
	})
}

func TestTimeouts(t *testing.T) {
	t.Run("a model turn that never returns", func(t *testing.T) {
		// Alive (heartbeating) but making no progress: the classic stall.
		a := fakeAdapter(t, []step{{Heartbeats: true}, send(message{Type: msgTurn, Turn: 1})})
		lim := fastLimits
		lim.TurnTimeout = 200 * time.Millisecond
		startJob(t, a, "j", standardTools(), &auditLog{}, lim)
		st := waitFor(t, a, "j")
		expectState(t, st, agentruntime.StateFailed, "no progress")
		if st.LastHeartbeat.IsZero() {
			t.Fatal("heartbeats were not visible in status")
		}
	})
	t.Run("a frozen runtime", func(t *testing.T) {
		a := fakeAdapter(t, []step{send(message{Type: msgTurn, Turn: 1})})
		lim := fastLimits
		lim.StallTimeout = 200 * time.Millisecond
		startJob(t, a, "j", standardTools(), &auditLog{}, lim)
		expectState(t, waitFor(t, a, "j"), agentruntime.StateFailed, "stopped responding")
	})
	t.Run("the hard job timeout", func(t *testing.T) {
		// Busy forever — turns keep coming, so neither the turn nor the
		// stall timer fires. Only the wall clock can stop it.
		a := fakeAdapter(t, []step{{Heartbeats: true}, {Turns: true}})
		lim := fastLimits
		lim.MaxTurns = 1_000_000
		lim.JobTimeout = 300 * time.Millisecond
		startJob(t, a, "j", standardTools(), &auditLog{}, lim)
		expectState(t, waitFor(t, a, "j"), agentruntime.StateFailed, "timed out")
	})
	t.Run("a tool that honours its deadline", func(t *testing.T) {
		a := fakeAdapter(t, []step{{Call: call("slow", `{}`)}, {FinalWithLastResult: true}})
		log := &auditLog{}
		startJob(t, a, "j", standardTools(), log, agentruntime.Limits{})
		st := waitFor(t, a, "j")
		expectState(t, st, agentruntime.StateCompleted, "")
		if !strings.Contains(st.Output, "timed out") || !log.has(agentruntime.AuditToolTimeout) {
			t.Fatalf("output %q audit %v", st.Output, log.types())
		}
	})
	t.Run("a tool that ignores its deadline is abandoned", func(t *testing.T) {
		a := fakeAdapter(t, []step{{Call: call("stubborn", `{}`)}, {FinalWithLastResult: true}})
		startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
		began := time.Now()
		st := waitFor(t, a, "j")
		expectState(t, st, agentruntime.StateCompleted, "")
		if time.Since(began) > 3*time.Second {
			t.Fatal("the adapter waited on a tool past its timeout")
		}
	})
}

func TestCancellation(t *testing.T) {
	t.Run("a running job", func(t *testing.T) {
		a := fakeAdapter(t, []step{{Heartbeats: true}})
		startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
		time.Sleep(50 * time.Millisecond)
		if _, err := a.Cancel(context.Background(), "j"); err != nil {
			t.Fatal(err)
		}
		expectState(t, waitFor(t, a, "j"), agentruntime.StateCancelled, "")
	})
	t.Run("mid tool call", func(t *testing.T) {
		a := fakeAdapter(t, []step{{Call: call("slow", `{}`)}})
		lim := fastLimits
		lim.ToolTimeout = 5 * time.Second
		startJob(t, a, "j", standardTools(), &auditLog{}, lim)
		deadline := time.Now().Add(3 * time.Second)
		for {
			st, _ := a.Status(context.Background(), "j")
			if st.State == agentruntime.StateWaitingForTool {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("never reached WAITING_FOR_TOOL: %s", st.State)
			}
			time.Sleep(5 * time.Millisecond)
		}
		_, _ = a.Cancel(context.Background(), "j")
		expectState(t, waitFor(t, a, "j"), agentruntime.StateCancelled, "")
	})
	t.Run("a parked job", func(t *testing.T) {
		a := fakeAdapter(t, []step{send(message{Type: msgWaiting, Question: "Which room?"})})
		startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
		expectState(t, waitFor(t, a, "j"), agentruntime.StateWaitingForUser, "")
		st, err := a.Cancel(context.Background(), "j")
		if err != nil {
			t.Fatal(err)
		}
		expectState(t, st, agentruntime.StateCancelled, "")
		if _, err := a.Resume(context.Background(), "j", agentruntime.Event{Kind: agentruntime.EventUserReply, Text: "kitchen"}); !errors.Is(err, agentruntime.ErrNotResumable) {
			t.Fatalf("resumed a cancelled job: %v", err)
		}
	})
}

func TestWaitingForUserThenResume(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture.json")
	a := fakeAdapter(t, []step{
		send(message{Type: msgWaiting, Question: "Which room?", History: json.RawMessage(`[{"turn":"first"}]`)}),
	}, fakeCaptureEnv+"="+capture)
	startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
	st := waitFor(t, a, "j")
	expectState(t, st, agentruntime.StateWaitingForUser, "")
	if st.Question != "Which room?" {
		t.Fatalf("question %q", st.Question)
	}

	if _, err := a.Resume(context.Background(), "j", agentruntime.Event{Kind: agentruntime.EventUserReply, Text: " "}); !errors.Is(err, agentruntime.ErrInvalidJob) {
		t.Fatalf("empty reply accepted: %v", err)
	}
	// The same fake plays again; it asks again, which is fine — what is
	// under test is what the resumed run was given.
	if _, err := a.Resume(context.Background(), "j", agentruntime.Event{Kind: agentruntime.EventUserReply, Text: "the kitchen"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "j")
	raw, _ := os.ReadFile(capture)
	var got struct {
		Start message `json:"start"`
	}
	_ = json.Unmarshal(raw, &got)
	if got.Start.Input != "the kitchen" || string(got.Start.History) != `[{"turn":"first"}]` {
		t.Fatalf("resumed run got input %q history %s", got.Start.Input, got.Start.History)
	}
}

func TestRuntimeCrash(t *testing.T) {
	a := fakeAdapter(t, []step{exitWith(1, "TypeError: boom")})
	startJob(t, a, "j", standardTools(), &auditLog{}, agentruntime.Limits{})
	expectState(t, waitFor(t, a, "j"), agentruntime.StateFailed, "boom")
}

func TestMissingRuntimeFails(t *testing.T) {
	a, err := New(Config{Command: []string{filepath.Join(t.TempDir(), "no-such-node")}, Limits: fastLimits})
	if err != nil {
		t.Fatal(err)
	}
	startJob(t, a, "j", nil, &auditLog{}, agentruntime.Limits{})
	expectState(t, waitFor(t, a, "j"), agentruntime.StateFailed, "could not start")
	if h := a.Health(context.Background()); h.OK {
		t.Fatal("health reported OK for a missing runtime")
	}
}

func TestAuditFailsClosed(t *testing.T) {
	a := fakeAdapter(t, []step{{Call: call("lookup", `{}`)}, {FinalWithLastResult: true}})
	tools := standardTools()
	log := &auditLog{failOn: agentruntime.AuditToolRequested}
	startJob(t, a, "j", tools, log, agentruntime.Limits{})
	expectState(t, waitFor(t, a, "j"), agentruntime.StateFailed, "audit unavailable")
	if len(tools.called()) != 0 {
		t.Fatal("a tool ran without an audit record")
	}

	b := fakeAdapter(t, nil)
	_, err := b.Start(context.Background(), agentruntime.Job{ID: "k", Input: "hi", Recorder: &auditLog{failOn: agentruntime.AuditJobState}})
	if err == nil {
		t.Fatal("a job started without an audit record")
	}
	if _, err := b.Status(context.Background(), "k"); !errors.Is(err, agentruntime.ErrUnknownJob) {
		t.Fatal("an unaudited job was left behind")
	}
}

func TestJobValidation(t *testing.T) {
	a := fakeAdapter(t, nil)
	ctx := context.Background()
	cases := map[string]agentruntime.Job{
		"no id":       {Input: "x", Recorder: &auditLog{}},
		"no recorder": {ID: "a", Input: "x"},
		"no input":    {ID: "a", Recorder: &auditLog{}},
		"reserved tool": {ID: "a", Input: "x", Recorder: &auditLog{}, Tools: &toolHost{tools: map[string]func(context.Context, map[string]any) (agentruntime.ToolResult, error){
			toolAskUser: nil,
		}}},
	}
	for name, job := range cases {
		if _, err := a.Start(ctx, job); !errors.Is(err, agentruntime.ErrInvalidJob) {
			t.Errorf("%s: %v", name, err)
		}
	}
	startJob(t, a, "dup", nil, &auditLog{}, agentruntime.Limits{})
	if _, err := a.Start(ctx, agentruntime.Job{ID: "dup", Input: "x", Recorder: &auditLog{}}); !errors.Is(err, agentruntime.ErrDuplicateJob) {
		t.Fatalf("duplicate id: %v", err)
	}
	_, _ = a.Cancel(ctx, "dup")
	for _, f := range []func() error{
		func() error { _, err := a.Status(ctx, "nope"); return err },
		func() error { _, err := a.Cancel(ctx, "nope"); return err },
		func() error { _, err := a.Wait(ctx, "nope"); return err },
		func() error {
			_, err := a.Resume(ctx, "nope", agentruntime.Event{Kind: agentruntime.EventUserReply, Text: "x"})
			return err
		},
	} {
		if err := f(); !errors.Is(err, agentruntime.ErrUnknownJob) {
			t.Errorf("unknown job: %v", err)
		}
	}
}
