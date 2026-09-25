package butlerstore

// P3's evidence (ADR-W030): a diagnosis needing at least eight reads
// completes or resumes without repeating verified reads, termination at
// each checkpoint resumes safely, and a second coordinator cannot
// dispatch concurrently.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime/legacy"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// countingToolset is a minimal Toolset that records how many times each
// tool was actually dispatched — the measurement the "no repeated
// verified reads" claim rests on.
type countingToolset struct {
	calls map[string]int
	fail  map[string]bool
	// refused is what authority says no to RIGHT NOW, so a test can
	// withdraw a tool between a result being recorded and being served
	// back from the ledger.
	refused map[string]string
}

func newCountingToolset() *countingToolset {
	return &countingToolset{
		calls: map[string]int{}, fail: map[string]bool{}, refused: map[string]string{},
	}
}

func (ts *countingToolset) Permits(name string, _ bool) (bool, string) {
	if why, ok := ts.refused[name]; ok {
		return false, why
	}
	return true, ""
}

func (ts *countingToolset) OfferedSpecs(bool) []provider.Tool {
	return []provider.Tool{{Name: "get_health", Description: "read a node's health"}}
}

func (ts *countingToolset) Execute(_ context.Context, name string, input map[string]any, _ bool) (butler.ToolOutcome, error) {
	key := fmt.Sprintf("%s/%v", name, input["node"])
	ts.calls[key]++
	if ts.fail[key] {
		return butler.ToolOutcome{}, errors.New("node unreachable")
	}
	return butler.ToolOutcome{Content: fmt.Sprintf("%s: ok", key)}, nil
}

func (ts *countingToolset) SystemPrompt() string { return "test" }

func (ts *countingToolset) Capabilities(readOnly bool) butler.CapabilityManifest {
	return butler.CapabilityManifest{
		AgentClass: butler.AgentClassOwner, ModelVerdict: butler.VerdictFull,
		Tools: []butler.CapabilityTool{{Name: "get_health", Available: true}},
	}
}

// scriptedProvider replays a fixed sequence of responses and can be told
// to die partway through, standing in for a killed process.
type scriptedProvider struct {
	steps  []*provider.ChatResponse
	at     int
	dieAt  int // -1 for never
	rounds int
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	if p.dieAt >= 0 && p.rounds >= p.dieAt {
		return nil, errors.New("provider died")
	}
	p.rounds++
	if p.at >= len(p.steps) {
		return &provider.ChatResponse{Text: "done"}, nil
	}
	resp := p.steps[p.at]
	p.at++
	return resp, nil
}

// readsFor builds a model turn that asks for one health read per node.
func readsFor(nodes ...string) *provider.ChatResponse {
	resp := &provider.ChatResponse{}
	for i, n := range nodes {
		resp.ToolCalls = append(resp.ToolCalls, provider.ToolCall{
			ID: fmt.Sprintf("c%d", i), Name: "get_health", Input: map[string]any{"node": n},
		})
	}
	return resp
}

// migrated is the schema, built once for this whole test binary rather
// than replayed into every database these tests open (#231). Each test
// still gets its own file.
var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

// record puts the cast these fixtures use on record as Persons.
//
// Since migration 0043 a task's author and owner, and a memory record's
// author, are foreign keys to a Person, so a fixture cannot file work
// for "mika" without saying who mika is. Machine principals stay out
// of it: "butler" and "root-admin" still appear here as actors, and
// actor columns still hold recorded text.
func record(t *testing.T, db *runtimedb.DB, handles ...string) {
	t.Helper()
	for _, h := range handles {
		if err := db.RecordButlerMember(h, runtimedb.RoleMember, "", ""); err != nil {
			t.Fatalf("recording %q: %v", h, err)
		}
	}
}

func testLedger(t *testing.T, path string) (*runtimedb.DB, *Ledger) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Into(t, path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	record(t, db, "mika", "sam")
	lease, err := db.AcquireCoordinator("keystone-a")
	if err != nil {
		t.Fatalf("AcquireCoordinator: %v", err)
	}
	return db, New(db, lease)
}

func newTask(t *testing.T, db *runtimedb.DB) string {
	t.Helper()
	id, err := db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: "operator",
		Audience:      runtimedb.ButlerAudiencePrivate,
		ObjectiveKind: runtimedb.ObjectiveInformational,
		Objective:     "why is photos unhealthy?", CompletionCriteria: "root cause named",
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	return id
}

// testGateway is a minimal, test-local agentapi.Client standing in for
// keystone/agentgateway.Gateway (ADR-W039 Phase 2b, #337): this package
// cannot import agentgateway (it imports keystone/butlerstore, which
// would cycle), so these tests exercise keystone/agentruntime/legacy's
// Runner against the same ledger-driving sequence
// keystone/agentgateway/invoke.go documents, reimplemented here over
// the countingToolset and the *Ledger already in scope. It is
// deliberately close kin to keystone/butlerlive's own closureClient,
// for the same reason: two packages that cannot see keystone/agentgateway
// still need one Runner to drive their Toolset through this exact
// sequence.
type testGateway struct {
	Toolset butler.Toolset
	Ledger  *Ledger
	// ToolCalls bounds a task's tool calls; 0 means unbounded — these
	// tests never wire a durable conversation's parent budget
	// (UseParentBudget did that pre-Phase-2, and nothing here needs it).
	ToolCalls int
	// ObservationTTL is how long a verified read may be reused; 0
	// defaults to an hour, long enough that nothing here goes stale, so
	// a repeated dispatch means the ledger failed rather than that a
	// reading aged out on its own.
	ObservationTTL time.Duration
	Now            func() time.Time

	mu      sync.Mutex
	records []butler.ToolCallRecord
}

var _ agentapi.Client = (*testGateway)(nil)

func (g *testGateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *testGateway) observationTTL() time.Duration {
	if g.ObservationTTL > 0 {
		return g.ObservationTTL
	}
	return time.Hour
}

func (g *testGateway) Manifest(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
	caps := g.Toolset.Capabilities(false)
	offered := g.Toolset.OfferedSpecs(false)
	tools := make([]agentapi.ToolSpec, 0, len(offered))
	for _, spec := range offered {
		tools = append(tools, agentapi.ToolSpec{Name: spec.Name, Description: spec.Description})
	}
	return agentapi.Manifest{
		AgentClass:   agentapi.AgentClass(caps.AgentClass),
		SystemPrompt: g.Toolset.SystemPrompt(),
		Tools:        tools,
		Fingerprint:  caps.Fingerprint(),
	}, nil
}

func (g *testGateway) Invoke(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
	taskID := req.Handle.TaskID()
	var input map[string]any
	if len(req.Args) > 0 {
		_ = json.Unmarshal(req.Args, &input)
	}
	rec := butler.ToolCallRecord{Name: req.Tool, Input: input}

	if err := g.Ledger.Authorize(taskID); err != nil {
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: err.Error()}, nil
	}
	steps, err := g.Ledger.Steps(taskID)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("testGateway: reading the step ledger: %w", err)
	}
	if g.ToolCalls > 0 && countToolSteps(steps) >= g.ToolCalls {
		return agentapi.InvokeResult{
			Outcome: agentapi.OutcomeWaiting, WaitReason: agentapi.WaitBudgetExhausted,
			Reason: fmt.Sprintf("%d tool calls used; this task's budget needs review before it continues", countToolSteps(steps)),
		}, nil
	}
	fp := butler.CallFingerprint(req.Tool, input)
	verified := butler.VerifiedResults(steps, g.now(), g.observationTTL())
	if content, ok := verified[fp]; ok && fp != "" && req.Tool != "recall" {
		if allowed, why := g.Toolset.Permits(req.Tool, false); !allowed {
			rec.Rejected, rec.Result = true, why
			g.record(rec)
			return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Reason: why}, nil
		}
		rec.Result = content
		g.record(rec)
		g.trace(taskID, butler.LedgerTraceEvent{
			Type: butler.TraceToolResult, SourceIDs: []string{req.Tool}, Payload: jsonPayload(map[string]any{"servedFromLedger": true}),
		})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: content, ServedFromLedger: true}, nil
	}

	if err := g.Ledger.Fence(); err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("testGateway: %w", err)
	}
	step, err := g.Ledger.AppendStep(taskID, butler.LedgerStepTool, req.Tool, jsonPayload(input))
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("testGateway: checkpointing a tool call: %w", err)
	}
	rec.StepSeq = step
	attempt, err := g.Ledger.StartAttempt(step)
	if err != nil {
		return agentapi.InvokeResult{}, fmt.Errorf("testGateway: checkpointing a tool call: %w", err)
	}
	g.trace(taskID, butler.LedgerTraceEvent{Type: butler.TraceToolDispatched, Step: step, Attempt: attempt.Seq, SourceIDs: []string{req.Tool}})

	id := operation.Identity{TaskID: taskID, Step: step, PermissionLabel: "operator", Audience: string(runtimedb.ButlerAudiencePrivate)}
	outcome, err := g.Toolset.Execute(operation.With(ctx, id), req.Tool, input, false)
	if err != nil {
		rec.Error = err.Error()
		g.record(rec)
		_ = g.Ledger.FinishAttempt(attempt.Seq, false, "", err.Error())
		_ = g.Ledger.SetStepState(step, butler.LedgerStepFailed)
		g.trace(taskID, butler.LedgerTraceEvent{Type: butler.TraceToolResult, Step: step, Attempt: attempt.Seq, Payload: jsonPayload(map[string]any{"error": true})})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeError, Content: fmt.Sprintf("error: %v", err), Reason: err.Error()}, nil
	}
	rec.Result, rec.Plan, rec.ResourceYAML, rec.Rejected = outcome.Content, outcome.Plan, outcome.ResourceYAML, outcome.Rejected
	g.record(rec)

	result := butler.ToolStepResult{
		EffectPending: outcome.EffectPending && !outcome.Rejected, EffectConfirmed: outcome.EffectConfirmed && !outcome.Rejected,
		ProposalConfirmed: outcome.ProposalConfirmed && !outcome.Rejected, Version: 1, Fingerprint: fp,
		Content: outcome.Content, Rejected: outcome.Rejected,
	}
	body, _ := json.Marshal(result)
	if err := g.Ledger.FinishAttempt(attempt.Seq, true, string(body), ""); errors.Is(err, runtimedb.ErrAttemptFenced) {
		_ = g.Ledger.SetStepState(step, butler.LedgerStepFailed)
		g.trace(taskID, butler.LedgerTraceEvent{Type: butler.TraceAttemptFenced, Step: step, Attempt: attempt.Seq, SourceIDs: []string{req.Tool}})
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeError, Reason: "the attempt was fenced while the tool ran, so its result was refused"}, nil
	}
	if outcome.Rejected {
		_ = g.Ledger.SetStepState(step, butler.LedgerStepFailed)
	} else {
		_ = g.Ledger.SetStepState(step, butler.LedgerStepVerified)
	}
	g.trace(taskID, butler.LedgerTraceEvent{
		Type: butler.TraceToolResult, Step: step, Attempt: attempt.Seq, SourceIDs: []string{req.Tool},
		Payload: jsonPayload(map[string]any{"rejected": outcome.Rejected, "bytes": len(outcome.Content)}),
	})
	switch {
	case outcome.Rejected:
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeDenied, Content: outcome.Content, Reason: outcome.Content}, nil
	case outcome.EffectPending:
		return agentapi.InvokeResult{Outcome: agentapi.OutcomeWaiting, WaitReason: agentapi.WaitConfirmPending, Content: outcome.Content}, nil
	}
	return agentapi.InvokeResult{Outcome: agentapi.OutcomeOK, Content: outcome.Content}, nil
}

func (g *testGateway) record(rec butler.ToolCallRecord) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.records = append(g.records, rec)
}

func (g *testGateway) trace(taskID string, e butler.LedgerTraceEvent) {
	e.PermissionLabel, e.Audience = "operator", string(runtimedb.ButlerAudiencePrivate)
	_ = g.Ledger.Trace(taskID, e)
}

func (g *testGateway) Checkpoint(ctx context.Context, ck agentapi.Checkpoint) (agentapi.CheckpointReceipt, error) {
	taskID := ck.Handle.TaskID()
	if err := g.Ledger.Fence(); err != nil {
		return agentapi.CheckpointReceipt{}, fmt.Errorf("testGateway: %w", err)
	}
	if err := g.Ledger.Authorize(taskID); err != nil {
		return agentapi.CheckpointReceipt{}, err
	}
	if ck.State == agentapi.StepProposed {
		params := string(ck.Params)
		if params == "" {
			params = "{}"
		}
		step, err := g.Ledger.AppendStep(taskID, string(ck.Kind), ck.Name, params)
		if err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: step}, nil
	}
	switch ck.State {
	case agentapi.StepRunning:
		attempt, err := g.Ledger.StartAttempt(ck.Step)
		if err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: ck.Step, Attempt: attempt.Seq, Epoch: attempt.Epoch}, nil
	case agentapi.StepVerified, agentapi.StepFailed:
		if ck.Attempt != 0 {
			if err := g.Ledger.FinishAttempt(ck.Attempt, ck.State == agentapi.StepVerified, string(ck.Result), ck.Error); err != nil {
				return agentapi.CheckpointReceipt{}, err
			}
		}
		if err := g.Ledger.SetStepState(ck.Step, string(ck.State)); err != nil {
			return agentapi.CheckpointReceipt{}, err
		}
		return agentapi.CheckpointReceipt{Step: ck.Step, Attempt: ck.Attempt}, nil
	}
	return agentapi.CheckpointReceipt{}, fmt.Errorf("testGateway: %q is not a checkpoint state", ck.State)
}

func (g *testGateway) Start(ctx context.Context, w agentapi.WorkStart) error {
	taskID := w.Handle.TaskID()
	if err := g.Ledger.Fence(); err != nil {
		return fmt.Errorf("testGateway: %w", err)
	}
	if err := g.Ledger.Authorize(taskID); err != nil {
		return err
	}
	reason := w.Reason
	if reason == "" {
		reason = "slice started"
	}
	return g.Ledger.Running(taskID, reason)
}

func (g *testGateway) Wait(ctx context.Context, w agentapi.WorkWait) error {
	if err := g.Ledger.Fence(); err != nil {
		return fmt.Errorf("testGateway: %w", err)
	}
	return g.Ledger.Wait(w.Handle.TaskID(), string(w.Reason), w.Detail)
}

func (g *testGateway) Complete(ctx context.Context, w agentapi.WorkComplete) error {
	if err := g.Ledger.Fence(); err != nil {
		return fmt.Errorf("testGateway: refusing to complete work after this coordinator was superseded: %w", err)
	}
	evidence := make([]butler.LedgerEvidence, 0, len(w.Evidence))
	for _, e := range w.Evidence {
		evidence = append(evidence, butler.LedgerEvidence{
			ReceiptID: e.ReceiptID, ProposalOnly: e.ProposalOnly, ReceiptStep: e.ReceiptStep,
			Kind: e.Kind, Description: e.Description, SourceID: e.SourceID,
			PermissionLabel: "operator", Audience: string(runtimedb.ButlerAudiencePrivate),
		})
	}
	return g.Ledger.Complete(w.Handle.TaskID(), evidence, w.Summary)
}

func (g *testGateway) Fail(ctx context.Context, w agentapi.WorkFail) error {
	taskID := w.Handle.TaskID()
	if err := g.Ledger.Fence(); err != nil {
		return fmt.Errorf("testGateway: %w", err)
	}
	if err := g.Ledger.Fail(taskID, w.Reason); err != nil {
		return err
	}
	_, err := g.Ledger.FenceAttempts(taskID)
	return err
}

func (g *testGateway) Heartbeat(context.Context, agentapi.Heartbeat) error { return nil }
func (g *testGateway) Recall(context.Context, agentapi.RecallRequest) ([]agentapi.MemoryHit, error) {
	return nil, &agentapi.NotImplementedError{Method: "Recall", Phase: "n/a"}
}
func (g *testGateway) Remember(context.Context, agentapi.RememberRequest) error {
	return &agentapi.NotImplementedError{Method: "Remember", Phase: "n/a"}
}
func (g *testGateway) Propose(context.Context, agentapi.ProposeRequest) (agentapi.ProposalRef, error) {
	return agentapi.ProposalRef{}, &agentapi.NotImplementedError{Method: "Propose", Phase: "n/a"}
}
func (g *testGateway) Notify(context.Context, agentapi.NotifyRequest) error {
	return &agentapi.NotImplementedError{Method: "Notify", Phase: "n/a"}
}

func jsonPayload(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(body)
}

// countToolSteps counts the tool steps a task's ledger already carries,
// the same accounting the pre-Phase-2 Runner kept locally across a
// slice; recomputing it fresh from the ledger on every Invoke call
// gives the same numbers, since every earlier dispatch in the same run
// already appended its step by the time the next call is made.
func countToolSteps(steps []butler.LedgerStep) int {
	n := 0
	for _, s := range steps {
		if s.Kind == butler.LedgerStepTool {
			n++
		}
	}
	return n
}

// newRunner builds a legacy.Runner bound to id through a fresh
// testGateway, with the same defaults the pre-Phase-2 helper gave every
// test: a 12-round slice budget and the operator's own label/audience.
// Its Toolset is reachable at r.Client.(*testGateway).Toolset for a
// test that swaps it after construction, exactly as the old helper let
// a test overwrite Runner.Gateway.
func newRunner(t *testing.T, ts butler.Toolset, l *Ledger, id string) *legacy.Runner {
	t.Helper()
	h, err := l.IssueHandle(id)
	if err != nil {
		t.Fatalf("IssueHandle: %v", err)
	}
	return &legacy.Runner{
		Client: &testGateway{Toolset: ts, Ledger: l}, Handle: h, Ledger: l,
		MaxSliceRounds:  12,
		PermissionLabel: "operator", Audience: string(runtimedb.ButlerAudiencePrivate), Actor: "butler",
	}
}

// newRunnerWithProvider is newRunner plus the one field every single-
// shot call site also needs, since legacy.Runner.Run no longer takes
// the provider or the task id as call arguments (both moved onto the
// Runner: Provider is a field, and the task comes from Handle).
func newRunnerWithProvider(t *testing.T, ts butler.Toolset, l *Ledger, id string, p provider.Provider) *legacy.Runner {
	t.Helper()
	r := newRunner(t, ts, l, id)
	r.Provider = p
	return r
}

// An operator diagnosis that needs eight reads completes, and every read
// is dispatched exactly once even though the model asks for several of
// them twice.
func TestEightReadDiagnosisDoesNotRepeatVerifiedReads(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := newCountingToolset()

	p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
		readsFor("n1", "n2", "n3"),
		readsFor("n4", "n5"),
		// The model re-asks for reads it already has, which is exactly
		// what a long diagnosis does.
		readsFor("n1", "n2", "n6"),
		readsFor("n7", "n8", "n3"),
		{Text: "photos is down because n4's disk is full"},
	}}
	tr, err := newRunnerWithProvider(t, ts, ledger, id, p).Run(context.Background(), "why is photos unhealthy?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tr.FinalText != "photos is down because n4's disk is full" {
		t.Fatalf("final text = %q", tr.FinalText)
	}
	if len(ts.calls) != 8 {
		t.Fatalf("distinct reads = %d, want 8: %v", len(ts.calls), ts.calls)
	}
	for key, n := range ts.calls {
		if n != 1 {
			t.Fatalf("read %s was dispatched %d times; a verified read must not repeat", key, n)
		}
	}
	task, err := db.GetButlerTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task state = %q, want completed", task.State)
	}
	ev, err := db.ListButlerTaskEvidence(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 9 {
		t.Fatalf("evidence rows = %d, want one per verified read plus the delivery", len(ev))
	}
}

// Termination partway through resumes safely: the verified reads are
// served from the ledger, only the unfinished work is dispatched again,
// and the task completes.
func TestInterruptedRunResumesWithoutRepeatingReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, ledger := testLedger(t, path)
	id := newTask(t, db)
	ts := newCountingToolset()

	// The provider dies after two rounds, five reads in.
	dying := &scriptedProvider{dieAt: 2, steps: []*provider.ChatResponse{
		readsFor("n1", "n2", "n3"),
		readsFor("n4", "n5"),
	}}
	if _, err := newRunnerWithProvider(t, ts, ledger, id, dying).Run(context.Background(), "why is photos unhealthy?"); err == nil {
		t.Fatal("Run succeeded against a provider that died")
	}
	if len(ts.calls) != 5 {
		t.Fatalf("reads before the crash = %d, want 5: %v", len(ts.calls), ts.calls)
	}
	task, _ := db.GetButlerTask(id)
	if task.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("state after a provider failure = %q, want waiting", task.State)
	}
	// The process really goes away.
	db.Close()

	db2, ledger2 := testLedger(t, path)
	defer db2.Close()
	resumed := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
		// A resume reconstructs context by asking again, so the model
		// re-requests everything it needs.
		readsFor("n1", "n2", "n3", "n4", "n5"),
		readsFor("n6"),
		{Text: "n6 is the root cause"},
	}}
	tr, err := newRunnerWithProvider(t, ts, ledger2, id, resumed).Run(context.Background(), "why is photos unhealthy?")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if tr.FinalText != "n6 is the root cause" {
		t.Fatalf("final text = %q", tr.FinalText)
	}
	if len(ts.calls) != 6 {
		t.Fatalf("distinct reads after resume = %d, want 6: %v", len(ts.calls), ts.calls)
	}
	for key, n := range ts.calls {
		if n != 1 {
			t.Fatalf("read %s was dispatched %d times across the restart", key, n)
		}
	}
	if task, _ := db2.GetButlerTask(id); task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("resumed task state = %q, want completed", task.State)
	}
}

// Termination at EACH checkpoint resumes safely: whichever step the
// process dies on, reopening finds a task that is not completed and can
// be driven to completion without redoing a verified read.
func TestTerminationAtEveryCheckpointResumesSafely(t *testing.T) {
	for dieAt := 1; dieAt <= 4; dieAt++ {
		t.Run(fmt.Sprintf("dies_after_round_%d", dieAt), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keystone.db")
			db, ledger := testLedger(t, path)
			id := newTask(t, db)
			ts := newCountingToolset()

			rounds := []*provider.ChatResponse{
				readsFor("n1"), readsFor("n2"), readsFor("n3"), readsFor("n4"),
			}
			dying := &scriptedProvider{dieAt: dieAt, steps: rounds}
			if _, err := newRunnerWithProvider(t, ts, ledger, id, dying).Run(context.Background(), "diagnose"); err == nil {
				t.Fatal("run survived a dead provider")
			}
			if task, _ := db.GetButlerTask(id); task.State == runtimedb.ButlerTaskCompleted {
				t.Fatal("an interrupted task reported completed")
			}
			before := len(ts.calls)
			db.Close()

			db2, ledger2 := testLedger(t, path)
			defer db2.Close()
			finish := &scriptedProvider{dieAt: -1, steps: append(append([]*provider.ChatResponse{}, rounds...),
				&provider.ChatResponse{Text: "done"})}
			if _, err := newRunnerWithProvider(t, ts, ledger2, id, finish).Run(context.Background(), "diagnose"); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if len(ts.calls) < before {
				t.Fatal("the resume lost reads that were already verified")
			}
			for key, n := range ts.calls {
				if n != 1 {
					t.Fatalf("read %s was dispatched %d times across the restart", key, n)
				}
			}
			if task, _ := db2.GetButlerTask(id); task.State != runtimedb.ButlerTaskCompleted {
				t.Fatalf("state after resume = %q, want completed", task.State)
			}
		})
	}
}

// A second coordinator cannot dispatch. It cannot even take the lease,
// and a ledger built on a stale lease is fenced at its first checkpoint.
func TestSecondCoordinatorCannotDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, ledger := testLedger(t, path)
	defer db.Close()
	id := newTask(t, db)

	if _, err := db.AcquireCoordinator("keystone-b"); !errors.Is(err, runtimedb.ErrCoordinatorHeld) {
		t.Fatalf("a second coordinator acquired the lease: %v", err)
	}
	// The same coordinator restarting is not a second coordinator.
	if _, err := db.AcquireCoordinator("keystone-a"); err != nil {
		t.Fatalf("the holder could not re-acquire its own lease: %v", err)
	}

	// An explicit, fenced promotion is the only takeover, and it stops
	// the previous holder at its next checkpoint.
	promoted, err := db.PromoteCoordinator("keystone-b")
	if err != nil {
		t.Fatalf("PromoteCoordinator: %v", err)
	}
	if promoted.Epoch != 2 || promoted.PromotedFrom != "keystone-a" {
		t.Fatalf("promotion = %+v", promoted)
	}
	ts := newCountingToolset()
	p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{readsFor("n1")}}
	_, err = newRunnerWithProvider(t, ts, ledger, id, p).Run(context.Background(), "diagnose")
	if !errors.Is(err, runtimedb.ErrCoordinatorFenced) {
		t.Fatalf("the fenced coordinator dispatched: %v", err)
	}
	if len(ts.calls) != 0 {
		t.Fatalf("the fenced coordinator ran %d tools", len(ts.calls))
	}
	// And promoting to the current holder is refused rather than
	// silently accepted.
	if _, err := db.PromoteCoordinator("keystone-b"); err == nil {
		t.Fatal("a coordinator promoted itself")
	}
}

// The slice budget pauses without discarding evidence; the aggregate
// budget stops for review and does not reset on the next slice.
func TestSliceAndAggregateBudgets(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	ts := newCountingToolset()

	t.Run("slice limit waits and keeps verified reads", func(t *testing.T) {
		id := newTask(t, db)
		p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
			readsFor("s1"), readsFor("s2"), readsFor("s3"), {Text: "done"},
		}}
		r := newRunnerWithProvider(t, ts, ledger, id, p)
		r.MaxSliceRounds = 2
		tr, err := r.Run(context.Background(), "diagnose")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !tr.CappedIterations || !strings.Contains(tr.FinalText, "continue") {
			t.Fatalf("slice limit did not offer a continue: %+v", tr)
		}
		task, _ := db.GetButlerTask(id)
		if task.State != runtimedb.ButlerTaskWaiting {
			t.Fatalf("state = %q, want waiting", task.State)
		}
		steps, _ := db.ListButlerTaskSteps(id)
		verified := 0
		for _, s := range steps {
			if s.Kind == runtimedb.ButlerStepTool && s.State == runtimedb.ButlerStepVerified {
				verified++
			}
		}
		if verified != 2 {
			t.Fatalf("verified reads after the pause = %d, want 2 kept", verified)
		}
		// Continuing gets another slice and finishes.
		r.Provider = &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{{Text: "done"}}}
		if _, err := r.Run(context.Background(), "continue"); err != nil {
			t.Fatalf("continue: %v", err)
		}
		if task, _ := db.GetButlerTask(id); task.State != runtimedb.ButlerTaskCompleted {
			t.Fatalf("state after continue = %q", task.State)
		}
	})

	t.Run("aggregate limit does not reset on the next slice", func(t *testing.T) {
		id := newTask(t, db)
		p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
			readsFor("a1"), readsFor("a2"), readsFor("a3"),
		}}
		r := newRunnerWithProvider(t, ts, ledger, id, p)
		// Room for more rounds than the aggregate budget allows, so it
		// is the tool budget that stops this run and not the slice one.
		r.MaxSliceRounds = 6
		r.Client.(*testGateway).ToolCalls = 2
		tr, err := r.Run(context.Background(), "diagnose")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(tr.FinalText, "budget") {
			t.Fatalf("aggregate limit did not say so: %q", tr.FinalText)
		}
		waits, _ := db.ListButlerTaskWaits(id)
		if len(waits) != 1 || waits[0].Reason != runtimedb.ButlerWaitBudgetExhausted {
			t.Fatalf("waits = %+v", waits)
		}
		// A second slice finds the budget still spent.
		r.Provider = &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{readsFor("a4")}}
		tr2, err := r.Run(context.Background(), "continue")
		if err != nil {
			t.Fatalf("continue: %v", err)
		}
		if !strings.Contains(tr2.FinalText, "budget") {
			t.Fatalf("the aggregate budget reset on the next slice: %q", tr2.FinalText)
		}
	})
}

// The trace explains the run: which model answered, under which
// manifest, what was dispatched, and what was served from the ledger.
func TestRunProducesAnExplanatoryTrace(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := newCountingToolset()
	p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
		readsFor("n1"), readsFor("n1"), {Text: "n1 is fine"},
	}}
	if _, err := newRunnerWithProvider(t, ts, ledger, id, p).Run(context.Background(), "check n1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	trace, err := db.ListButlerTrace(id)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range trace {
		seen[e.Type]++
		if e.PermissionLabel == "" || e.Audience == "" {
			t.Fatalf("trace event %q carries no permission metadata: %+v", e.Type, e)
		}
	}
	for _, want := range []string{butler.TraceSliceStarted, butler.TraceModelCall,
		butler.TraceModelAnswer, butler.TraceToolDispatched, butler.TraceToolResult,
		butler.TraceOutcome} {
		if seen[want] == 0 {
			t.Fatalf("trace has no %q event: %v", want, seen)
		}
	}
	// The second read was served from the ledger, so there is one
	// dispatch and two results.
	if seen[butler.TraceToolDispatched] != 1 || seen[butler.TraceToolResult] != 2 {
		t.Fatalf("dispatch/result counts = %d/%d, want 1/2",
			seen[butler.TraceToolDispatched], seen[butler.TraceToolResult])
	}
	var manifestSeen bool
	for _, e := range trace {
		if e.Type == butler.TraceModelCall && e.ManifestVersion != "" && e.ModelEndpoint == "scripted" {
			manifestSeen = true
		}
	}
	if !manifestSeen {
		t.Fatal("no model-call event records the endpoint and manifest version")
	}
}

// Found by Codex: the fence before a dispatch proves this coordinator
// held the database when the call went out, and a model call takes
// seconds. A promotion during one changes the COORDINATOR lease, not
// the task's attempt epoch — so FinishAttempt's own epoch check does
// not see it, and a superseded coordinator could finish its attempt and
// complete work its replacement had already taken over.
func TestASupersededCoordinatorCannotFinishOrComplete(t *testing.T) {
	db, ledger := testLedger(t, filepath.Join(t.TempDir(), "keystone.db"))
	defer db.Close()
	ts := newCountingToolset()

	id := newTask(t, db)
	// An answer with NO tool calls, which is the case Codex named: it
	// passes no later dispatch fence, so it goes straight to
	// completion. A response WITH tool calls is caught by the next
	// round's pre-dispatch fence and would prove nothing about this.
	p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{{Text: "the answer"}}}

	// The promotion happens DURING the model call, which is the race
	// this guards. Promoting before it would be caught by the
	// pre-dispatch fence that already existed, and would prove nothing
	// about the new one.
	promoting := &promotingProvider{inner: p, db: db}
	r := newRunnerWithProvider(t, ts, ledger, id, promoting)

	_, err := r.Run(context.Background(), "diagnose")
	if err == nil {
		t.Fatal("a superseded coordinator ran to completion")
	}
	if !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
	// And the work is NOT marked complete, which is the thing that
	// would have stopped its replacement resuming it.
	task, err := db.GetButlerTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if task.State == runtimedb.ButlerTaskCompleted {
		t.Fatal("a superseded coordinator completed the task anyway")
	}
}

// promotingProvider hands the database to another coordinator while the
// model call is in flight — the window between the fence before a
// dispatch and the acceptance of its result.
type promotingProvider struct {
	inner *scriptedProvider
	db    *runtimedb.DB
	done  bool
}

func (p *promotingProvider) Name() string { return "promoting" }

func (p *promotingProvider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	resp, err := p.inner.Chat(ctx, req)
	if !p.done {
		p.done = true
		if _, perr := p.db.PromoteCoordinator("keystone-b"); perr != nil {
			return nil, perr
		}
	}
	return resp, err
}

// chatConnection gives an Agent a bot and returns the Connection a
// queued message leaves by.
//
// Since migration 0045 the outbound queue is per Connection rather than
// per surface, so a notifier's message belongs to one Agent's bot — not
// to whichever bot happens to serve the operator surface.
func chatConnection(t *testing.T, db *runtimedb.DB, butler string) runtimedb.Connection {
	t.Helper()
	if err := db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	const capability = "chat-telegram-admin"
	if _, err := db.GetButler(butler); err != nil {
		if _, err := db.CreateButler(runtimedb.Butler{
			Name: butler, Directive: "look after the house", Surface: runtimedb.ButlerSurfaceAdmin,
			AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box",
		}, "mika"); err != nil {
			t.Fatalf("CreateButler(%s): %v", butler, err)
		}
		if _, err := db.AddButlerAddress(butler, "telegram", capability, "", "mika"); err != nil {
			t.Fatalf("addressing %s: %v", butler, err)
		}
	}
	at, err := db.ChatConnectionAt("telegram", capability)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
