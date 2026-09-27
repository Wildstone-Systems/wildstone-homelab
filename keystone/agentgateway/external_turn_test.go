package agentgateway

// #364: a Butler whose runtime is external and connected is answered by
// that runtime, never in-process. These drive butlerlive.Serve against
// this gateway with a stand-in runtime on the other side of it: it
// long-polls Deliveries under its own principal, works the task through
// its handle, and reports its answer with Complete{Reply}.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// judge is the Butler's own route, which on an external runtime only
// ever judges an answer: every call it sees must be the tool-free
// objective check, never a turn to answer.
type judge struct {
	mu      sync.Mutex
	verdict string
	calls   int
	tooled  int
}

func (j *judge) Name() string { return "ollama" }

func (j *judge) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls++
	if len(req.Tools) > 0 {
		j.tooled++
	}
	return &provider.ChatResponse{Text: j.verdict}, nil
}

func (j *judge) counts() (calls, tooled int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls, j.tooled
}

// externalFixture makes alfred a connected Hermes-backed Butler and
// wires a Butlers to this fixture's gateway, as cmd/keystone does.
func externalFixture(t *testing.T, verdict string) (*fixture, *butlerlive.Butlers, *judge) {
	t.Helper()
	f := newFixture(t)
	alfred, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetButlerRuntime(alfred.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeHermes}, "mika"); err != nil {
		t.Fatal(err)
	}
	f.bindRuntime(t, "rt-alfred", "alfred")
	if err := f.db.SetRuntimeHealth(alfred.Seq, agentapi.HealthConnected); err != nil {
		t.Fatal(err)
	}
	j := &judge{verdict: verdict}
	bs := &butlerlive.Butlers{
		DB: f.db, Ledger: f.ledger, Gateways: f.g.Gateways,
		Providers:       func(runtimedb.Butler) (provider.Provider, error) { return j, nil },
		UseAgentGateway: true,
		AgentClient:     InProcess(f.g),
		WithPrincipal: func(ctx context.Context, name string) context.Context {
			rt, err := f.db.GetButlerRuntimeByName(name)
			if err != nil || rt.Principal == "" {
				return ctx
			}
			return WithPrincipal(ctx, rt.Principal)
		},
	}
	f.g.Replies = bs
	return f, bs, j
}

// nextTurn is the runtime's long-poll: the first turn delivery for it.
func nextTurn(t *testing.T, g *Gateway) (agentapi.Delivery, butlerlive.ExternalTurn) {
	t.Helper()
	ctx := WithPrincipal(context.Background(), "rt-alfred")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ds, err := g.Deliveries(ctx, "", 200*time.Millisecond)
		if err != nil {
			t.Errorf("Deliveries: %v", err)
			return agentapi.Delivery{}, butlerlive.ExternalTurn{}
		}
		for _, d := range ds {
			if d.Kind == agentapi.DeliveryTurn {
				var et butlerlive.ExternalTurn
				if err := json.Unmarshal(d.Payload, &et); err != nil {
					t.Errorf("turn payload: %v", err)
				}
				return d, et
			}
		}
	}
	t.Error("the runtime was never handed the turn")
	return agentapi.Delivery{}, butlerlive.ExternalTurn{}
}

func TestAConnectedExternalRuntimeAnswersTheTurn(t *testing.T) {
	f, bs, j := externalFixture(t, `{"kind":"informational"}`)

	ran := make(chan error, 1)
	go func() {
		d, et := nextTurn(t, f.g)
		if et.Text != "is n1 healthy?" {
			t.Errorf("turn text = %q, want what the person said", et.Text)
		}
		if len(et.History) != 2 || et.History[0].Text != "hello" {
			t.Errorf("history = %+v, want the conversation so far", et.History)
		}
		ctx := WithPrincipal(context.Background(), "rt-alfred")
		if err := f.g.Start(ctx, agentapi.WorkStart{Handle: d.Handle, Reason: "delivery"}); err != nil {
			ran <- err
			return
		}
		ran <- f.g.Complete(ctx, agentapi.WorkComplete{Handle: d.Handle, Reply: "n1 is fine",
			// Ignored: a runtime reports an answer, it does not certify it.
			Evidence: []agentapi.Evidence{{Kind: "delivery", Description: "self-certified"}}})
	}()

	answer, err := bs.Serve(context.Background(), butlerlive.Turn{
		Butler: "alfred", Member: "mika", Text: "is n1 healthy?",
		Connection: butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection},
		History:    []provider.Message{{Role: "user", Text: "hello"}, {Role: "assistant", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if err := <-ran; err != nil {
		t.Fatalf("the runtime's Complete: %v", err)
	}
	if answer.FinalText != "n1 is fine" {
		t.Fatalf("FinalText = %q, want the runtime's answer", answer.FinalText)
	}
	calls, tooled := j.counts()
	if calls != 1 || tooled != 0 {
		t.Fatalf("the Butler's route saw %d calls (%d offering tools), want only the one tool-free objective check", calls, tooled)
	}
	task, err := f.db.GetButlerTask(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task state = %s, want completed", task.State)
	}
	evidence, err := f.db.ListButlerTaskEvidence(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evidence {
		if e.Description == "self-certified" {
			t.Fatal("the runtime's own evidence was written; Wildstone must draw it from the ledger")
		}
	}
}

// An answer claiming an effect nothing performed is held to the same
// rule as the native runtime's: the person is told it was not done.
func TestAnExternalAnswerClaimingAnUnperformedEffectIsNotDone(t *testing.T) {
	f, bs, j := externalFixture(t,
		`{"kind":"effectful","effects":[{"description":"restart n1","sourceKey":""}],"answerClaimsEffect":true}`)

	go func() {
		d, _ := nextTurn(t, f.g)
		ctx := WithPrincipal(context.Background(), "rt-alfred")
		if err := f.g.Complete(ctx, agentapi.WorkComplete{Handle: d.Handle, Reply: "I restarted n1."}); err != nil {
			t.Errorf("Complete: %v", err)
		}
	}()

	answer, err := bs.Serve(context.Background(), butlerlive.Turn{
		Butler: "alfred", Member: "mika", Text: "restart n1",
		Connection: butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if _, tooled := j.counts(); tooled != 0 {
		t.Fatalf("the Butler's route was offered tools %d times: the turn ran in-process", tooled)
	}
	if strings.Contains(answer.FinalText, "restarted") {
		t.Fatalf("FinalText = %q: an unproven claim reached the person", answer.FinalText)
	}
	task, err := f.db.GetButlerTask(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State == runtimedb.ButlerTaskCompleted {
		t.Fatal("a claimed effect with no receipt completed the task")
	}
}

func TestAFailedExternalRunIsReported(t *testing.T) {
	f, bs, j := externalFixture(t, `{"kind":"informational"}`)

	go func() {
		d, _ := nextTurn(t, f.g)
		ctx := WithPrincipal(context.Background(), "rt-alfred")
		if err := f.g.Start(ctx, agentapi.WorkStart{Handle: d.Handle, Reason: "delivery"}); err != nil {
			t.Errorf("Start: %v", err)
		}
		if err := f.g.Fail(ctx, agentapi.WorkFail{Handle: d.Handle, Reason: "failed: model quota"}); err != nil {
			t.Errorf("Fail: %v", err)
		}
	}()

	answer, err := bs.Serve(context.Background(), butlerlive.Turn{
		Butler: "alfred", Member: "mika", Text: "is n1 healthy?",
		Connection: butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !strings.Contains(answer.FinalText, "failed: model quota") {
		t.Fatalf("FinalText = %q, want the runtime's reason", answer.FinalText)
	}
	if calls, _ := j.counts(); calls != 0 {
		t.Fatalf("the Butler's route saw %d calls, want none: nothing was answered", calls)
	}
}

// Past the bounded wait the person is told it is still going; the
// runtime's later answer still settles the task under the same rule.
func TestALateExternalAnswerStillSettlesTheTask(t *testing.T) {
	f, bs, j := externalFixture(t, `{"kind":"informational"}`)
	bs.ExternalReplyWait = 50 * time.Millisecond

	answer, err := bs.Serve(context.Background(), butlerlive.Turn{
		Butler: "alfred", Member: "mika", Text: "is n1 healthy?",
		Connection: butlerlive.ConnectionScope{Name: butlerlive.ConsoleConnection},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !strings.Contains(answer.FinalText, "still working") {
		t.Fatalf("FinalText = %q, want a still-working notice", answer.FinalText)
	}
	if calls, _ := j.counts(); calls != 0 {
		t.Fatalf("the Butler's route saw %d calls before any answer, want none", calls)
	}
	task, err := f.db.GetButlerTask(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskRunning {
		t.Fatalf("task state = %s, want still running", task.State)
	}

	d, _ := nextTurn(t, f.g)
	ctx := WithPrincipal(context.Background(), "rt-alfred")
	if err := f.g.Complete(ctx, agentapi.WorkComplete{Handle: d.Handle, Reply: "n1 is fine"}); err != nil {
		t.Fatalf("late Complete: %v", err)
	}
	task, err = f.db.GetButlerTask(answer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task state = %s, want completed by the late answer", task.State)
	}
	if task.ResultSummary != "n1 is fine" {
		t.Fatalf("result summary = %q, want the late answer", task.ResultSummary)
	}
}
