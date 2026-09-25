package butlerlive

// Serving a turn on the native agent runtime (ADR-W041).
//
// When Butlers.Native is set and the Butler's route speaks a protocol
// the runtime understands, the turn's reasoning loop runs there — the
// OpenAI Agents SDK, in its sidecar — instead of in the legacy Runner. What
// does NOT move is everything the Runner's loop is surrounded by:
//
//   - the durable ledger: every model attempt and tool call is
//     checkpointed through legacy.Slice, which is the Runner's own code
//     with the loop taken out and reaches the task only through
//     agentapi.Client, so budgets, verified-read reuse, fencing and
//     completion evidence are identical — and are the Agent API's, the
//     contract every runtime is held to (ADR-W039);
//   - authority: re-checked before every model attempt, tool dispatch
//     and outcome, by the same calls;
//   - privacy: the turn's destination policy is checked before and after
//     every model attempt, retries included, and before an outcome is
//     accepted — privacyProvider's rule, applied through the runtime's
//     hooks because the model call itself happens in the sidecar;
//   - the tools: the same scoped gateway, reached only through Slice.
//
// A route the runtime cannot speak (Anthropic's own API) runs on the
// Runner as before. That is not a fallback between MODELS — the Butler
// still talks to exactly the model it was assigned — only a choice of
// which loop drives it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime/legacy"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// nativeJobTimeout is the hard wall-clock bound on one turn's reasoning.
const nativeJobTimeout = 10 * time.Minute

// nativeToolTimeout bounds one tool call. Household actions reach real
// devices and services, so it is generous; it exists so that one that
// never answers cannot hold the turn forever.
const nativeToolTimeout = 2 * time.Minute

// nativeStopWait bounds how long a turn whose caller went away waits for
// its cancelled job to stop before parking the task anyway.
const nativeStopWait = 10 * time.Second

var nativeJobSeq atomic.Uint64

// nativeRoute is the model route a Butler runs on, in the runtime's
// terms, or false when the runtime cannot speak it.
func nativeRoute(b runtimedb.Butler) (agentruntime.ModelRoute, bool) {
	kind, ok := provider.LookupKind(b.Provider)
	if !ok || strings.TrimSpace(b.ProviderModel) == "" {
		return agentruntime.ModelRoute{}, false
	}
	endpoint := kind.Endpoint(b.Endpoint)
	route := agentruntime.ModelRoute{Protocol: agentruntime.ProtocolOpenAIChat, Model: b.ProviderModel}
	switch kind.Client {
	case "openai":
		if endpoint == "" {
			return agentruntime.ModelRoute{}, false
		}
		route.BaseURL = endpoint
		route.Credential = kind.Key()
	case "ollama":
		// Ollama serves the OpenAI Chat Completions API under /v1 on
		// the same address as its own.
		if endpoint == "" {
			return agentruntime.ModelRoute{}, false
		}
		route.BaseURL = strings.TrimRight(endpoint, "/") + "/v1"
	default:
		return agentruntime.ModelRoute{}, false
	}
	return route, true
}

// nativeRouteFor is nativeRoute when this deployment runs the native
// runtime at all.
func (bs *Butlers) nativeRouteFor(b runtimedb.Butler) (agentruntime.ModelRoute, bool) {
	if bs.Native == nil {
		return agentruntime.ModelRoute{}, false
	}
	return nativeRoute(b)
}

// runNative drives one slice of taskID on the native runtime. It returns
// what runner.Run would: the transcript, and an error only when the turn
// could not be answered.
func (bs *Butlers) runNative(ctx context.Context, runner *legacy.Runner,
	privacy *runtimedb.PrivacyTurn, route agentruntime.ModelRoute, t Turn,
) (*butler.Transcript, error) {
	taskID := runner.Handle.TaskID()
	s, tr, err := runner.BeginSlice(ctx, t.Text)
	if s == nil {
		return tr, err
	}
	tools := &sliceTools{s: s}
	hooks := &sliceHooks{s: s, privacy: privacy}
	id := fmt.Sprintf("%s-n%d", taskID, nativeJobSeq.Add(1))
	job := agentruntime.Job{
		ID:           id,
		Instructions: s.SystemPrompt(),
		Input:        t.Text,
		Actor:        t.Member,
		Tools:        tools,
		Recorder:     auditRecorder{db: bs.DB},
		Conversation: utterances(t.History),
		Model:        route,
		Hooks:        hooks,
		Limits: agentruntime.Limits{
			JobTimeout:  nativeJobTimeout,
			TurnTimeout: runner.StallTimeout,
			ToolTimeout: nativeToolTimeout,
			MaxTurns:    runner.MaxSliceRounds,
			// No MaxToolCalls: the task's aggregate budget is the
			// gateway's, spent per Invoke through Slice.Call, and a
			// spent one parks the task. The runtime's own default is
			// a backstop above it.
		},
	}
	if _, err := bs.Native.Start(ctx, job); err != nil {
		return s.Transcript(), s.Unavailable("the native agent runtime refused the turn: " + err.Error())
	}
	st, err := bs.Native.Wait(ctx, id)
	if err != nil {
		// The person went away (or the request timed out). Stop the
		// work rather than let it finish for nobody, and leave the task
		// the way the Runner leaves it when its call is cancelled:
		// parked, with every verified read kept, and not "running" with
		// nobody driving it.
		//
		// The job is waited on first, boundedly, so no late hook of the
		// stopped run can write to the ledger after it is parked.
		_, _ = bs.Native.Cancel(context.Background(), id)
		stopCtx, cancel := context.WithTimeout(context.Background(), nativeStopWait)
		_, _ = bs.Native.Wait(stopCtx, id)
		cancel()
		_ = s.ModelEnded(false, nil)
		_ = s.Unavailable("the request ended before the answer: " + err.Error())
		return s.Transcript(), err
	}

	// A boundary that refused is the answer, whatever state that left
	// the job in: it is why the job stopped.
	if err := hooks.failure(); err != nil {
		return s.Transcript(), err
	}
	if err := tools.failure(); err != nil {
		return s.Transcript(), err
	}
	if tools.isParked() {
		// The slice already parked the task on its budget and said so.
		return s.Transcript(), nil
	}
	switch st.State {
	case agentruntime.StateCompleted:
		return s.Transcript(), s.Finish(ctx, st.Output)
	case agentruntime.StateWaitingForUser:
		return s.Transcript(), s.AskUser(st.Question)
	case agentruntime.StateBlocked:
		if st.Reason == agentruntime.ReasonTurnBudget {
			return s.Transcript(), s.Pause()
		}
		return s.Transcript(), s.Blocked(st.Reason)
	case agentruntime.StateCancelled:
		return s.Transcript(), fmt.Errorf("butlerlive: the turn was cancelled")
	default:
		// FAILED: the model or the runtime could not be used. The task
		// waits with every verified read intact, as the Runner leaves it
		// when its retries are spent.
		return s.Transcript(), s.Unavailable(st.Reason)
	}
}

// utterances is a conversation's earlier text, as the runtime takes it.
func utterances(history []provider.Message) []agentruntime.Utterance {
	var out []agentruntime.Utterance
	for _, m := range provider.SafeMessages(history) {
		if (m.Role == "user" || m.Role == "assistant") && strings.TrimSpace(m.Text) != "" {
			out = append(out, agentruntime.Utterance{Role: m.Role, Text: m.Text})
		}
	}
	return out
}

// sliceTools serves the slice's gateway to the runtime. Every call goes
// through Slice.Call, so it is checkpointed, budgeted and authorized
// exactly as the Runner's own calls are.
type sliceTools struct {
	s *legacy.Slice

	mu     sync.Mutex
	err    error
	parked bool
}

func (st *sliceTools) Specs() []agentruntime.ToolSpec {
	specs := st.s.Specs()
	out := make([]agentruntime.ToolSpec, 0, len(specs))
	for _, t := range specs {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, agentruntime.ToolSpec{Name: t.Name, Description: t.Description, Parameters: params})
	}
	return out
}

func (st *sliceTools) Call(ctx context.Context, name string, input map[string]any) (agentruntime.ToolResult, error) {
	res, err := st.s.Call(ctx, name, input)
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case err != nil:
		if st.err == nil {
			st.err = err
		}
		return agentruntime.ToolResult{}, err
	case res.Parked:
		st.parked = true
		return agentruntime.ToolResult{Block: "this conversation's tool budget is used up"}, nil
	default:
		return agentruntime.ToolResult{Content: res.Content, Failed: res.Failed}, nil
	}
}

func (st *sliceTools) failure() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.err
}

func (st *sliceTools) isParked() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.parked
}

// sliceHooks keeps the Runner's per-attempt boundaries and the turn's
// privacy policy around a loop the platform does not run.
type sliceHooks struct {
	s       *legacy.Slice
	privacy *runtimedb.PrivacyTurn

	mu  sync.Mutex
	err error
}

func (h *sliceHooks) fail(err error) error {
	if err == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = err
	}
	return err
}

func (h *sliceHooks) failure() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *sliceHooks) BeforeModel(context.Context) error {
	if err := h.privacy.Check(); err != nil {
		return h.fail(err)
	}
	return h.fail(h.s.ModelStarted())
}

func (h *sliceHooks) AfterModel(_ context.Context, ok bool, toolCalls []string) error {
	if err := h.s.ModelEnded(ok, toolCalls); err != nil {
		return h.fail(err)
	}
	if ok {
		// "Again before accepting output": what the model said is not
		// used if the destination policy closed while it was saying it.
		return h.fail(h.privacy.Check())
	}
	return nil
}

func (h *sliceHooks) BeforeOutcome(context.Context, agentruntime.State) error {
	return h.fail(h.privacy.Check())
}

// auditRecorder writes the runtime's audit trail to the event stream.
//
// Only the structure is kept — job, tool, state, who — never Detail. A
// detail can carry what the model said about a private conversation
// (its blocked reason, a refusal it was shown), and the event stream is
// read by the operator. The conversation's own content already lives in
// the task's trace, under the task's audience.
type auditRecorder struct {
	db *runtimedb.DB
}

func (r auditRecorder) Record(ctx context.Context, e agentruntime.AuditEvent) error {
	if r.db == nil {
		return errors.New("butlerlive: no event store")
	}
	e.Detail = ""
	return agentruntime.EventLogRecorder{Events: r.db}.Record(ctx, e)
}
