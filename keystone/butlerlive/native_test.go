package butlerlive

// Conversations on the native agent runtime (ADR-W041): the route
// decides which loop runs, and the loop changes nothing about the
// ledger, the tools, authority or the answer's shape.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// scriptRuntime is an AgentRuntime whose "loop" is a Go function. It
// drives the job's hooks and tools the way a real runtime must, so what
// is under test is butlerlive's side of the contract.
type scriptRuntime struct {
	script func(ctx context.Context, j agentruntime.Job) agentruntime.Status

	mu        sync.Mutex
	started   []agentruntime.Job
	done      map[string]chan agentruntime.Status
	cancelled map[string]chan struct{}
}

// stopped is closed when the job is cancelled; a script may select on it.
func (r *scriptRuntime) stopped(id string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled[id]
}

func (r *scriptRuntime) Name() string { return "script" }

func (r *scriptRuntime) Start(ctx context.Context, j agentruntime.Job) (agentruntime.Status, error) {
	r.mu.Lock()
	r.started = append(r.started, j)
	if r.done == nil {
		r.done = map[string]chan agentruntime.Status{}
	}
	ch := make(chan agentruntime.Status, 1)
	r.done[j.ID] = ch
	if r.cancelled == nil {
		r.cancelled = map[string]chan struct{}{}
	}
	r.cancelled[j.ID] = make(chan struct{})
	r.mu.Unlock()
	go func() { ch <- r.script(context.Background(), j) }()
	return agentruntime.Status{JobID: j.ID, State: agentruntime.StateQueued}, nil
}

func (r *scriptRuntime) Wait(ctx context.Context, id string) (agentruntime.Status, error) {
	r.mu.Lock()
	ch := r.done[id]
	r.mu.Unlock()
	select {
	case st := <-ch:
		ch <- st // a job can be waited on more than once
		return st, nil
	case <-ctx.Done():
		return agentruntime.Status{}, ctx.Err()
	}
}

func (r *scriptRuntime) Resume(context.Context, string, agentruntime.Event) (agentruntime.Status, error) {
	return agentruntime.Status{}, errors.New("not used")
}
func (r *scriptRuntime) Cancel(_ context.Context, id string) (agentruntime.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.cancelled[id]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	return agentruntime.Status{}, nil
}
func (r *scriptRuntime) Status(context.Context, string) (agentruntime.Status, error) {
	return agentruntime.Status{}, nil
}
func (r *scriptRuntime) Capabilities(context.Context) (agentruntime.Capabilities, error) {
	return agentruntime.Capabilities{}, nil
}
func (r *scriptRuntime) Health(context.Context) agentruntime.Health {
	return agentruntime.Health{OK: true}
}

func (r *scriptRuntime) jobs() []agentruntime.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentruntime.Job(nil), r.started...)
}

// A model attempt, through the hooks, as a real runtime makes one.
func attempt(ctx context.Context, j agentruntime.Job, tools ...string) error {
	if err := j.Hooks.BeforeModel(ctx); err != nil {
		return err
	}
	return j.Hooks.AfterModel(ctx, true, tools)
}

func failed(err error) agentruntime.Status {
	return agentruntime.Status{State: agentruntime.StateFailed, Reason: err.Error()}
}

func answer(text string) func(context.Context, agentruntime.Job) agentruntime.Status {
	return func(ctx context.Context, j agentruntime.Job) agentruntime.Status {
		if err := attempt(ctx, j); err != nil {
			return failed(err)
		}
		if err := j.Hooks.BeforeOutcome(ctx, agentruntime.StateCompleted); err != nil {
			return failed(err)
		}
		return agentruntime.Status{State: agentruntime.StateCompleted, Output: text}
	}
}

func nativeFixture(t *testing.T, script func(context.Context, agentruntime.Job) agentruntime.Status) (*fixture, *scriptRuntime) {
	f := newFixture(t)
	rt := &scriptRuntime{script: script}
	f.butlers.Native = rt
	return f, rt
}

func taskOf(t *testing.T, f *fixture, id string) runtimedb.ButlerTask {
	t.Helper()
	task, err := f.db.GetButlerTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestAnOllamaButlerRunsOnTheNativeRuntime(t *testing.T) {
	f, rt := nativeFixture(t, answer("A byte contains eight bits."))
	ans, err := f.butlers.Serve(context.Background(), f.turn("alfred", "How many bits are in a byte?"))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if ans.FinalText != "A byte contains eight bits." {
		t.Fatalf("answer %q", ans.FinalText)
	}
	if task := taskOf(t, f, ans.TaskID); task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task %s, want completed", task.State)
	}
	jobs := rt.jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d native jobs, want 1", len(jobs))
	}
	j := jobs[0]
	want := agentruntime.ModelRoute{Protocol: agentruntime.ProtocolOpenAIChat, BaseURL: "http://kitchen:11434/v1", Model: "llama"}
	if j.Model != want {
		t.Fatalf("route %+v, want %+v — the assigned model, and only it", j.Model, want)
	}
	if !strings.Contains(j.Instructions, "operator surface") || !strings.Contains(j.Instructions, "look after the alfred things") {
		t.Fatalf("instructions are not the surface's prompt and directive: %q", j.Instructions)
	}
	if j.Actor != "mika" || j.Input != "How many bits are in a byte?" {
		t.Fatalf("job %+v", j)
	}
	// The answer was still verified against the objective, on the
	// Butler's own model, exactly as the Runner does.
	if !f.local.sawText("Classify both required and claimed effects") {
		t.Fatal("the objective check did not run")
	}
	if f.cloud.sawAnything() {
		t.Fatal("another Butler's model was called")
	}
}

// The runtime's audit trail reaches the operator's event stream as
// structure only: never what the model said.
func TestNativeAuditKeepsStructureAndDropsDetail(t *testing.T) {
	f := newFixture(t)
	err := auditRecorder{db: f.db}.Record(context.Background(), agentruntime.AuditEvent{
		JobID: "bt-1-n1", Runtime: "native", Type: agentruntime.AuditJobState,
		State: agentruntime.StateBlocked, Actor: "mika",
		Detail: "I could not find Sam's therapy appointment",
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.EventsAfter(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var got []runtimedb.Event
	for _, e := range events {
		if strings.HasPrefix(e.Type, "agent.") {
			got = append(got, e)
		}
	}
	if len(got) != 1 || got[0].Attrs["state"] != "BLOCKED" || got[0].Name != "bt-1-n1" {
		t.Fatalf("events %+v", got)
	}
	if got[0].Message != "" {
		t.Fatalf("the model's words reached the operator's stream: %q", got[0].Message)
	}
}

func TestAnAnthropicButlerStaysOnTheRunner(t *testing.T) {
	f, rt := nativeFixture(t, answer("unused"))
	f.cloud.reply = "robert answered on the runner"
	ans, err := f.butlers.Serve(context.Background(), f.turn("robert", "hello"))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(rt.jobs()) != 0 {
		t.Fatal("a route the native runtime cannot speak was sent to it")
	}
	if ans.FinalText != "robert answered on the runner" {
		t.Fatalf("answer %q", ans.FinalText)
	}
}

func TestNativeToolCallsGoThroughTheLedger(t *testing.T) {
	f, _ := nativeFixture(t, func(ctx context.Context, j agentruntime.Job) agentruntime.Status {
		if err := attempt(ctx, j, "get_health"); err != nil {
			return failed(err)
		}
		var last string
		for _, node := range []string{"n1", "n1"} {
			res, err := j.Tools.Call(ctx, "get_health", map[string]any{"node": node})
			if err != nil {
				return failed(err)
			}
			last = res.Content
		}
		if err := attempt(ctx, j); err != nil {
			return failed(err)
		}
		return agentruntime.Status{State: agentruntime.StateCompleted, Output: "health: " + last}
	})
	ans, err := f.butlers.Serve(context.Background(), f.turn("alfred", "is n1 healthy?"))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if n := f.tools["alfred"].count("get_health/n1"); n != 1 {
		t.Fatalf("n1 dispatched %d times; the repeat must be served from the ledger", n)
	}
	if len(ans.Calls) != 2 {
		t.Fatalf("answer lists %d calls, want 2", len(ans.Calls))
	}
	ev, err := f.db.ListButlerTaskEvidence(ans.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 2 {
		t.Fatalf("evidence rows = %d, want the verified read plus the delivery", len(ev))
	}
}

func TestNativeOutcomesParkTheTask(t *testing.T) {
	for name, c := range map[string]struct {
		status agentruntime.Status
		says   string
	}{
		"question": {agentruntime.Status{State: agentruntime.StateWaitingForUser, Question: "Which room?"}, "Which room?"},
		"blocked":  {agentruntime.Status{State: agentruntime.StateBlocked, Reason: "I have no access to the garage"}, "I have no access to the garage"},
		"paused":   {agentruntime.Status{State: agentruntime.StateBlocked, Reason: agentruntime.ReasonTurnBudget}, "say continue"},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := nativeFixture(t, func(ctx context.Context, j agentruntime.Job) agentruntime.Status {
				if err := attempt(ctx, j); err != nil {
					return failed(err)
				}
				return c.status
			})
			ans, err := f.butlers.Serve(context.Background(), f.turn("alfred", "turn on the lights"))
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if !strings.Contains(ans.FinalText, c.says) {
				t.Fatalf("answer %q, want it to say %q", ans.FinalText, c.says)
			}
			if task := taskOf(t, f, ans.TaskID); task.State != runtimedb.ButlerTaskWaiting {
				t.Fatalf("task %s, want waiting", task.State)
			}
		})
	}
}

func TestANativeFailureWaitsAndReports(t *testing.T) {
	f, _ := nativeFixture(t, func(context.Context, agentruntime.Job) agentruntime.Status {
		return agentruntime.Status{State: agentruntime.StateFailed, Reason: "model turn made no progress for 2m0s"}
	})
	ans, err := f.butlers.Serve(context.Background(), f.turn("alfred", "anything"))
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("err %v", err)
	}
	if task := taskOf(t, f, ans.TaskID); task.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("task %s, want waiting with everything verified kept", task.State)
	}
}

func TestWithdrawnAuthorityStopsANativeTurn(t *testing.T) {
	var f *fixture
	f, _ = nativeFixture(t, func(ctx context.Context, j agentruntime.Job) agentruntime.Status {
		task := strings.SplitN(j.ID, "-n", 2)[0]
		if err := f.db.TransitionButlerTask(task, runtimedb.ButlerTaskCancelled, "stopped", "mika"); err != nil {
			return failed(err)
		}
		if err := attempt(ctx, j); err != nil {
			return failed(err)
		}
		if _, err := j.Tools.Call(ctx, "get_health", map[string]any{"node": "n1"}); err != nil {
			return failed(err)
		}
		return agentruntime.Status{State: agentruntime.StateCompleted, Output: "done"}
	})
	ans, err := f.butlers.Serve(context.Background(), f.turn("alfred", "check n1"))
	if err == nil {
		t.Fatal("a cancelled task's turn was answered")
	}
	if ans.FinalText == "done" {
		t.Fatal("the answer was delivered")
	}
	if n := f.tools["alfred"].count("get_health/n1"); n != 0 {
		t.Fatal("a tool ran after authority was withdrawn")
	}
}

func TestNativeRoutes(t *testing.T) {
	for name, c := range map[string]struct {
		b    runtimedb.Butler
		ok   bool
		base string
	}{
		"ollama":                      {runtimedb.Butler{Provider: "ollama", ProviderModel: "qwen3", Endpoint: "http://box:11434/"}, true, "http://box:11434/v1"},
		"openai":                      {runtimedb.Butler{Provider: "openai", ProviderModel: "gpt-5"}, true, "https://api.openai.com/v1"},
		"openrouter":                  {runtimedb.Butler{Provider: "openrouter", ProviderModel: "x/y"}, true, "https://openrouter.ai/api/v1"},
		"compatible":                  {runtimedb.Butler{Provider: "openai-compatible", ProviderModel: "m", Endpoint: "http://lm:1234/v1"}, true, "http://lm:1234/v1"},
		"compatible without endpoint": {runtimedb.Butler{Provider: "openai-compatible", ProviderModel: "m"}, false, ""},
		"anthropic":                   {runtimedb.Butler{Provider: "anthropic", ProviderModel: "claude"}, false, ""},
		"no model":                    {runtimedb.Butler{Provider: "ollama"}, false, ""},
		"unknown":                     {runtimedb.Butler{Provider: "mystery", ProviderModel: "m"}, false, ""},
	} {
		route, ok := nativeRoute(c.b)
		if ok != c.ok || route.BaseURL != c.base {
			t.Errorf("%s: %+v %v", name, route, ok)
		}
	}
}

// The person goes away mid-turn: the job is stopped, the open model
// checkpoint is closed, and the task is parked — not left "running"
// with nobody driving it.
func TestANativeTurnWhoseCallerLeavesIsParked(t *testing.T) {
	var rt *scriptRuntime
	inModel := make(chan struct{})
	f, rt := nativeFixture(t, func(ctx context.Context, j agentruntime.Job) agentruntime.Status {
		if err := j.Hooks.BeforeModel(ctx); err != nil {
			return failed(err)
		}
		close(inModel)
		<-rt.stopped(j.ID) // a model call that never returns until cancelled
		return agentruntime.Status{State: agentruntime.StateCancelled}
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-inModel; cancel() }()
	ans, err := f.butlers.Serve(ctx, f.turn("alfred", "anything"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want the caller's cancellation", err)
	}
	if task := taskOf(t, f, ans.TaskID); task.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("task %s, want waiting rather than running with nobody driving it", task.State)
	}
	steps, err := f.butlers.Ledger.Steps(ans.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range steps {
		if st.Kind == "model" && st.State != "failed" {
			t.Fatalf("the interrupted model step was left %q", st.State)
		}
	}
}
