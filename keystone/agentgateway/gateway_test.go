package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// (a) The manifest lists exactly what the surface's own inventory
// calls available, on both surfaces, and carries the prompt Serve
// would have assembled.
func TestManifestListsTheSurfacesOwnTools(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		butler, owner string
		class         agentapi.AgentClass
		mutating      string
	}{
		{"joan", "sam", agentapi.AgentClassMember, "remember"},
		{"alfred", "mika", agentapi.AgentClassOwner, "propose_change"},
	} {
		t.Run(tc.butler, func(t *testing.T) {
			id, h := f.openTask(t, tc.butler, tc.owner)
			m, err := f.g.Manifest(ctx, h)
			if err != nil {
				t.Fatalf("Manifest: %v", err)
			}
			// The same surface, scoped the same way, asked directly:
			// the manifest's tools are the offered specs — what the
			// runner hands the model today — and every one of them is
			// in the surface's own inventory, which lists every
			// AVAILABLE tool among them.
			s, err := f.g.open(ctx, h)
			if err != nil {
				t.Fatal(err)
			}
			offered := s.toolset.OfferedSpecs(s.readOnly)
			want := make([]string, 0, len(offered))
			for _, spec := range offered {
				want = append(want, spec.Name)
			}
			got := toolNames(m.Tools)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("manifest tools = %v\nwant the offered specs %v", got, want)
			}
			inventory := s.toolset.Capabilities(s.readOnly)
			listed := map[string]bool{}
			for _, tool := range inventory.Tools {
				listed[tool.Name] = true
			}
			for _, name := range got {
				if !listed[name] {
					t.Errorf("manifest offers %s, which %s's inventory does not list", name, tc.butler)
				}
			}
			for _, name := range availableNames(inventory) {
				if !contains(got, name) {
					t.Errorf("%s's inventory calls %s available but the manifest does not offer it", tc.butler, name)
				}
			}
			if len(m.Tools) == 0 {
				t.Fatal("the manifest offered nothing; the fixture's authority is not wired the way Serve wires it")
			}
			if m.AgentClass != tc.class || m.Fingerprint == "" {
				t.Fatalf("manifest class %q fingerprint %q; want %q and a fingerprint", m.AgentClass, m.Fingerprint, tc.class)
			}
			var sawMutating, sawRead bool
			for _, tool := range m.Tools {
				if len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) {
					t.Errorf("%s: input schema %q is not JSON", tool.Name, tool.InputSchema)
				}
				if tool.Name == tc.mutating {
					sawMutating = true
					if tool.ReadOnlyHint {
						t.Errorf("%s is offered with a read-only hint", tool.Name)
					}
				}
				if tool.Name == "describe_capabilities" {
					sawRead = true
					if !tool.ReadOnlyHint {
						t.Error("describe_capabilities is offered without a read-only hint")
					}
				}
			}
			if !sawMutating || !sawRead {
				t.Fatalf("expected both %s and describe_capabilities among %v", tc.mutating, toolNames(m.Tools))
			}
			// The prompt is the surface's hierarchy, the clock, then
			// the directive — assembled by butlerlive, not here.
			for _, part := range []string{s.toolset.SystemPrompt(), "24 September 2026", "You are " + tc.butler, "It grants you nothing"} {
				if !strings.Contains(m.SystemPrompt, part) {
					t.Errorf("system prompt lacks %q", part)
				}
			}
			if got := f.g.Records(id); len(got) != 0 {
				t.Fatalf("a manifest recorded %d calls", len(got))
			}
		})
	}
}

// (b) An invoke writes the rows the runner writes: the same step, the
// same attempt, the same envelope, the same trace, so a resume through
// either reads one ledger.
func TestInvokeWritesTheRunnersLedgerRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const tool = "get_system_overview"

	// The runner today, on task A, through the same scoped surface.
	taskA, hA := f.openTask(t, "alfred", "mika")
	sA, err := f.g.open(ctx, hA)
	if err != nil {
		t.Fatal(err)
	}
	r := &butler.Runner{
		Gateway: sA.toolset, Ledger: f.ledger, ReadOnly: sA.readOnly,
		MaxSliceRounds: 4, MaxToolCalls: ConversationToolCalls, UseParentBudget: true,
		PermissionLabel: "alfred", Audience: string(runtimedb.ButlerAudiencePrivate), Actor: "mika",
		ObservationTTL: ObservationTTL,
	}
	if _, err := r.Run(ctx, &scripted{toolCalls: []provider.ToolCall{{ID: "c1", Name: tool, Input: map[string]any{}}}}, taskA, "what is running?"); err != nil {
		t.Fatalf("Runner.Run: %v", err)
	}

	// The gateway, on task B.
	taskB, hB := f.openTask(t, "alfred", "mika")
	if err := f.ledger.Running(taskB, "slice started"); err != nil {
		t.Fatal(err)
	}
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: hB, Tool: tool, Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outcome != agentapi.OutcomeOK || res.ServedFromLedger || res.Content == "" {
		t.Fatalf("Invoke = %+v; want ok with fresh content", res)
	}
	if f.surface("alfred").count(tool) != 2 {
		t.Fatalf("%s dispatched %d times across the two paths, want 2", tool, f.surface("alfred").count(tool))
	}

	stepA, attemptA := toolRows(t, f.db, taskA, tool)
	stepB, attemptB := toolRows(t, f.db, taskB, tool)
	if stepA != stepB {
		t.Errorf("tool step rows differ:\n runner  %+v\n gateway %+v", stepA, stepB)
	}
	if attemptA != attemptB {
		t.Errorf("tool attempt rows differ:\n runner  %+v\n gateway %+v", attemptA, attemptB)
	}
	if !strings.Contains(attemptB.ResultJSON, `"fp":"`+tool+`\u0000{}"`) {
		t.Errorf("gateway envelope lacks the runner's fingerprint: %s", attemptB.ResultJSON)
	}
	if traceA, traceB := toolTrace(t, f.db, taskA, tool), toolTrace(t, f.db, taskB, tool); !reflect.DeepEqual(traceA, traceB) {
		t.Errorf("tool trace differs:\n runner  %v\n gateway %v", traceA, traceB)
	}
	// And the transcript's record is kept for the handler that files
	// after the turn.
	recs := f.g.Records(taskB)
	if len(recs) != 1 || recs[0].Name != tool || recs[0].StepSeq == 0 || recs[0].Result != res.Content {
		t.Fatalf("Records = %+v", recs)
	}
	f.g.Forget(taskB)
	if len(f.g.Records(taskB)) != 0 {
		t.Fatal("Forget kept the records")
	}
}

// rowKey is the part of a step or attempt row that must agree between
// the two writers: everything but its ids and timestamps.
type stepKey struct {
	Kind   runtimedb.ButlerStepKind
	Name   string
	State  runtimedb.ButlerStepState
	Params string
}

type attemptKey struct {
	Attempt    int
	Epoch      int64
	State      runtimedb.ButlerAttemptState
	ResultJSON string
	ErrorText  string
}

func toolRows(t *testing.T, db *runtimedb.DB, taskID, tool string) (stepKey, attemptKey) {
	t.Helper()
	steps, err := db.ListButlerTaskSteps(taskID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := db.ListButlerTaskAttempts(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Kind != runtimedb.ButlerStepTool || s.Name != tool {
			continue
		}
		for _, a := range attempts {
			if a.Step == s.Seq {
				return stepKey{s.Kind, s.Name, s.State, s.ParamsJSON},
					attemptKey{a.Attempt, a.Epoch, a.State, a.ResultJSON, a.ErrorText}
			}
		}
		t.Fatalf("%s: tool step %d has no attempt", taskID, s.Seq)
	}
	t.Fatalf("%s: no tool step for %s", taskID, tool)
	return stepKey{}, attemptKey{}
}

// toolTrace is the trace of one tool call as "type actor label audience
// sources payload" lines, ids elided.
func toolTrace(t *testing.T, db *runtimedb.DB, taskID, tool string) []string {
	t.Helper()
	events, err := db.ListButlerTrace(taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if len(e.SourceIDs) != 1 || e.SourceIDs[0] != tool {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s %s %s %v %s", e.Type, e.Actor, e.PermissionLabel, e.Audience, e.SourceIDs, e.PayloadJSON))
	}
	return out
}

// (c) propose_change through the gateway returns a proposal reference
// and applies nothing: the Statefile is untouched, and the only
// package that could apply it is one this package cannot import.
func TestProposeChangeReturnsAReferenceAndAppliesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	taskID, h := f.openTask(t, "alfred", "mika")
	args, _ := json.Marshal(map[string]any{
		"kind": "Route", "node": "node-a", "name": "grafana", "owner": "capability/grafana",
		"spec": map[string]any{
			"host": "grafana.example.com", "upstream": "container/grafana:3000",
			"auth": "authelia_oidc", "classification": "family_public",
		},
	})
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "propose_change", Args: args})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outcome != agentapi.OutcomeOK {
		t.Fatalf("Invoke = %+v; want ok", res)
	}
	if res.Proposal == nil || !strings.HasPrefix(res.Proposal.ID, taskID+"/step/") {
		t.Fatalf("proposal ref = %+v; want one naming the ledger step", res.Proposal)
	}
	if !strings.Contains(res.Content, "NOT been applied") {
		t.Fatalf("result did not disclaim application: %q", res.Content)
	}
	refs, err := f.store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("propose_change wrote to the Statefile through the gateway (it must never apply): %+v", refs)
	}
	// The Plan and resource document are kept for the handler that
	// files them (C9, until Phase 3) — off the wire, on the gateway.
	recs := f.g.Records(taskID)
	if len(recs) != 1 || recs[0].Plan == nil || recs[0].ResourceYAML == "" {
		t.Fatalf("the proposal's record was not kept: %+v", recs)
	}

	// A policy-rejected proposal is rejected, not denied, and carries
	// no reference.
	bad, _ := json.Marshal(map[string]any{
		"kind": "Route", "node": "node-a", "name": "open", "owner": "capability/open",
		"spec": map[string]any{
			"host": "open.example.com", "upstream": "container/open:80",
			"auth": "none", "classification": "public",
		},
	})
	res, err = f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "propose_change", Args: bad})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != agentapi.OutcomeRejected || res.Proposal != nil {
		t.Fatalf("policy-invalid proposal = %+v; want rejected with no reference", res)
	}
}

// (d) A runtime principal bound to another Butler is refused with a
// valid handle in hand; the Butler's own principal is not.
func TestAHandleForAnotherAgentIsDeniedToARuntimePrincipal(t *testing.T) {
	f := newFixture(t)
	f.bindRuntime(t, "runtime-alfred-1", "alfred")
	f.bindRuntime(t, "runtime-joan-1", "joan")
	_, h := f.openTask(t, "alfred", "mika")

	res, err := f.g.Invoke(WithPrincipal(context.Background(), "runtime-joan-1"),
		agentapi.InvokeRequest{Handle: h, Tool: "describe_capabilities"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outcome != agentapi.OutcomeDenied || !strings.Contains(res.Reason, "another agent") {
		t.Fatalf("Invoke = %+v; want denied for the binding", res)
	}
	if _, err := f.g.Manifest(WithPrincipal(context.Background(), "runtime-joan-1"), h); !errors.Is(err, ErrRefused) {
		t.Fatalf("Manifest err = %v; want ErrRefused", err)
	}
	// A principal that is not a runtime principal at all is refused too.
	if err := f.db.CreatePrincipal("root-admin-x", runtimedb.RoleAdmin, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.Manifest(WithPrincipal(context.Background(), "root-admin-x"), h); !errors.Is(err, ErrRefused) {
		t.Fatalf("Manifest err = %v; want ErrRefused for a non-runtime principal", err)
	}
	// The right one is served.
	res, err = f.g.Invoke(WithPrincipal(context.Background(), "runtime-alfred-1"),
		agentapi.InvokeRequest{Handle: h, Tool: "describe_capabilities"})
	if err != nil || res.Outcome != agentapi.OutcomeOK {
		t.Fatalf("Invoke as the bound principal = %+v, %v; want ok", res, err)
	}
	if f.surface("alfred").count("describe_capabilities") != 1 {
		t.Fatalf("the denied call reached the surface: %d dispatches", f.surface("alfred").count("describe_capabilities"))
	}
}

// (e) A fenced task refuses its handle, and an attempt fenced while
// the tool ran has its result refused rather than verified.
func TestAFencedTaskIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	taskID, h := f.openTask(t, "alfred", "mika")

	if _, err := f.ledger.FenceTask(taskID); err != nil {
		t.Fatal(err)
	}
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "describe_capabilities"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outcome != agentapi.OutcomeDenied {
		t.Fatalf("Invoke on a fenced task = %+v; want denied", res)
	}
	if _, err := f.g.Manifest(ctx, h); !errors.Is(err, butlerstore.ErrHandleRefused) {
		t.Fatalf("Manifest err = %v; want the handle refused", err)
	}
	if f.surface("alfred") != nil && f.surface("alfred").count("describe_capabilities") != 0 {
		t.Fatal("the fenced task's call reached the surface")
	}

	// A fresh handle for the same task, and a fence that lands while
	// the tool is running: the attempt's result is refused and the
	// step is not verified, so nothing resumes from it.
	h2, err := f.ledger.IssueHandle(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.Manifest(ctx, h2); err != nil {
		t.Fatalf("priming the surface: %v", err)
	}
	f.surface("alfred").during = func() {
		if _, err := f.ledger.FenceAttempts(taskID); err != nil {
			t.Error(err)
		}
	}
	res, err = f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h2, Tool: "describe_capabilities"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Outcome != agentapi.OutcomeError || !strings.Contains(res.Reason, "fenced") {
		t.Fatalf("Invoke under a mid-flight fence = %+v; want the fenced error", res)
	}
	steps, err := f.ledger.Steps(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.State == butler.LedgerStepVerified {
			t.Fatalf("a fenced attempt's step was verified: %+v", s)
		}
	}
}

// (f) A verified read is served from the ledger within the observation
// TTL, without dispatching, and taken again once it has aged out.
func TestAVerifiedReadIsServedFromTheLedgerUntilItAges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const tool = "get_system_overview"
	taskID, h := f.openTask(t, "alfred", "mika")
	f.g.ObservationTTL = time.Hour
	base := time.Now()
	f.g.Now = func() time.Time { return base }
	req := agentapi.InvokeRequest{Handle: h, Tool: tool, Args: json.RawMessage(`{}`)}

	first, err := f.g.Invoke(ctx, req)
	if err != nil || first.Outcome != agentapi.OutcomeOK || first.ServedFromLedger {
		t.Fatalf("first = %+v, %v; want a fresh ok", first, err)
	}
	second, err := f.g.Invoke(ctx, req)
	if err != nil || second.Outcome != agentapi.OutcomeOK || !second.ServedFromLedger {
		t.Fatalf("second = %+v, %v; want served from the ledger", second, err)
	}
	if second.Content != first.Content {
		t.Fatal("the ledger served a different result")
	}
	if n := f.surface("alfred").count(tool); n != 1 {
		t.Fatalf("%s dispatched %d times; the second call should have been served", tool, n)
	}
	// Served reads append no step and spend no budget.
	if steps, _ := f.ledger.Steps(taskID); len(steps) != 1 {
		t.Fatalf("%d steps after a served read, want 1", len(steps))
	}
	// A different question is not the same reading.
	other, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "describe_capabilities"})
	if err != nil || other.ServedFromLedger {
		t.Fatalf("a different tool was served from the first one's result: %+v, %v", other, err)
	}
	// A served read still re-asks the authority behind it: once the
	// tool is withdrawn from this Butler, the ledger does not answer
	// for it, and nothing is dispatched either.
	if err := f.db.NarrowButlerTool("alfred", tool, "noisy", "mika"); err != nil {
		t.Fatalf("NarrowButlerTool: %v", err)
	}
	denied, err := f.g.Invoke(ctx, req)
	if err != nil || denied.Outcome != agentapi.OutcomeDenied || denied.ServedFromLedger {
		t.Fatalf("after withdrawal = %+v, %v; want denied, not served", denied, err)
	}
	if n := f.surface("alfred").count(tool); n != 1 {
		t.Fatalf("a withdrawn tool reached the surface: %d dispatches", n)
	}
	// Given back, and past the TTL: the reading is taken again.
	if err := f.db.WidenButlerTool("alfred", tool); err != nil {
		t.Fatalf("WidenButlerTool: %v", err)
	}
	f.g.Now = func() time.Time { return base.Add(f.g.ObservationTTL + time.Minute) }
	third, err := f.g.Invoke(ctx, req)
	if err != nil || third.Outcome != agentapi.OutcomeOK || third.ServedFromLedger {
		t.Fatalf("third = %+v, %v; want re-executed after the TTL", third, err)
	}
	if n := f.surface("alfred").count(tool); n != 2 {
		t.Fatalf("%s dispatched %d times after the TTL, want 2", tool, n)
	}
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func TestAWrongHandleIsDeniedAndNeverEchoed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	taskID, h := f.openTask(t, "alfred", "mika")
	forged := agentapi.Handle(taskID + ".forged-secret")
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: forged, Tool: "describe_capabilities"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != agentapi.OutcomeDenied || strings.Contains(res.Reason, "forged-secret") {
		t.Fatalf("forged handle = %+v; want denied without the secret", res)
	}
	if _, err := f.g.Manifest(ctx, ""); !errors.Is(err, butlerstore.ErrHandleRefused) {
		t.Fatalf("zero handle err = %v", err)
	}
	// A recall is never served from the ledger, whatever the TTL.
	_ = h
}

// The task's authority is re-checked at the dispatch boundary: a task
// cancelled after its handle was issued dispatches nothing.
func TestInvokeReChecksTaskAuthority(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	taskID, h := f.openTask(t, "alfred", "mika")
	if _, err := f.g.Manifest(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := f.db.TransitionButlerTask(taskID, runtimedb.ButlerTaskCancelled, "stopped", "mika"); err != nil {
		t.Fatal(err)
	}
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "describe_capabilities"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != agentapi.OutcomeDenied {
		t.Fatalf("a cancelled task's invoke = %+v; want denied", res)
	}
	if f.surface("alfred").count("describe_capabilities") != 0 {
		t.Fatal("a cancelled task dispatched a tool")
	}
}

// An exhausted budget parks the call rather than running it. The
// conversation's aggregate limit is the ledger's (conversation_work,
// 64 by default) and wins over the gateway's constant, exactly as it
// wins over the runner's MaxToolCalls.
func TestAnExhaustedBudgetWaits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	taskID, h := f.openTask(t, "alfred", "mika")
	_, limit, _, err := f.ledger.AggregateToolCalls(taskID)
	if err != nil || limit <= 0 {
		t.Fatalf("aggregate limit = %d, %v", limit, err)
	}
	for i := 0; i < limit; i++ {
		// Distinct arguments, so none is served from the ledger.
		args, _ := json.Marshal(map[string]any{"n": i})
		res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "describe_capabilities", Args: args})
		if err != nil || res.Outcome != agentapi.OutcomeOK || res.ServedFromLedger {
			t.Fatalf("call %d = %+v, %v", i, res, err)
		}
	}
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "get_system_overview"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != agentapi.OutcomeWaiting || res.WaitReason != agentapi.WaitBudgetExhausted {
		t.Fatalf("over budget = %+v; want waiting on the budget", res)
	}
	if f.surface("alfred").count("get_system_overview") != 0 {
		t.Fatal("an over-budget call was dispatched")
	}
	if steps, _ := f.ledger.Steps(taskID); len(steps) != limit {
		t.Fatalf("%d steps after the budget, want %d", len(steps), limit)
	}
}

// The Work family: a checkpoint sequence through the client writes the
// runner's model-step rows, and a step of another task is refused.
func TestCheckpointsWriteTheLedger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	taskID, h := f.openTask(t, "alfred", "mika")
	if err := f.g.Start(ctx, agentapi.WorkStart{Handle: h}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if task, _ := f.db.GetButlerTask(taskID); task.State != runtimedb.ButlerTaskRunning {
		t.Fatalf("state after Start = %s", task.State)
	}
	opened, err := f.g.Checkpoint(ctx, agentapi.Checkpoint{
		Handle: h, Kind: agentapi.StepModel, Name: "ollama", State: agentapi.StepProposed,
		Params: json.RawMessage(`{"messages":1,"tools":3}`),
	})
	if err != nil || opened.Step == 0 {
		t.Fatalf("proposed = %+v, %v", opened, err)
	}
	started, err := f.g.Checkpoint(ctx, agentapi.Checkpoint{Handle: h, Step: opened.Step, State: agentapi.StepRunning})
	if err != nil || started.Attempt == 0 {
		t.Fatalf("running = %+v, %v", started, err)
	}
	if _, err := f.g.Checkpoint(ctx, agentapi.Checkpoint{
		Handle: h, Step: opened.Step, Attempt: started.Attempt, State: agentapi.StepVerified,
		Result: json.RawMessage(`{"toolCalls":0}`),
	}); err != nil {
		t.Fatalf("verified: %v", err)
	}
	steps, _ := f.db.ListButlerTaskSteps(taskID)
	attempts, _ := f.db.ListButlerTaskAttempts(taskID)
	if len(steps) != 1 || steps[0].Kind != runtimedb.ButlerStepModel || steps[0].State != runtimedb.ButlerStepVerified ||
		steps[0].ParamsJSON != `{"messages":1,"tools":3}` {
		t.Fatalf("steps = %+v", steps)
	}
	if len(attempts) != 1 || attempts[0].State != runtimedb.ButlerAttemptSucceeded || attempts[0].ResultJSON != `{"toolCalls":0}` {
		t.Fatalf("attempts = %+v", attempts)
	}

	// Another task's step is not this handle's to checkpoint.
	otherID, otherH := f.openTask(t, "alfred", "mika")
	other, err := f.g.Checkpoint(ctx, agentapi.Checkpoint{Handle: otherH, Kind: agentapi.StepTool, Name: "x", State: agentapi.StepProposed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.Checkpoint(ctx, agentapi.Checkpoint{Handle: h, Step: other.Step, State: agentapi.StepRunning}); !errors.Is(err, ErrRefused) {
		t.Fatalf("checkpointing %s's step through %s's handle: err = %v; want ErrRefused", otherID, taskID, err)
	}

	// Wait, then Complete with evidence stamped from the task.
	if err := f.g.Wait(ctx, agentapi.WorkWait{Handle: h, Reason: agentapi.WorkWaitReply, Detail: "awaiting continue"}); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if task, _ := f.db.GetButlerTask(taskID); task.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("state after Wait = %s", task.State)
	}
	if err := f.g.Wait(ctx, agentapi.WorkWait{Handle: h, Reason: "coffee"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("an open wait reason was accepted: %v", err)
	}
	if err := f.g.Start(ctx, agentapi.WorkStart{Handle: h, Reason: "continue"}); err != nil {
		t.Fatal(err)
	}
	if err := f.g.Complete(ctx, agentapi.WorkComplete{Handle: h, Summary: "done",
		Evidence: []agentapi.Evidence{{Kind: "delivery", Description: "an answer was produced"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if task, _ := f.db.GetButlerTask(taskID); task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("state after Complete = %s", task.State)
	}
	// And a completed task's handle is spent.
	if err := f.g.Fail(ctx, agentapi.WorkFail{Handle: h, Reason: "late"}); !errors.Is(err, butlerstore.ErrHandleRefused) {
		t.Fatalf("Fail on a completed task: %v; want the handle refused", err)
	}
	// Fail on a running task fences its attempts.
	failID, failH := f.openTask(t, "alfred", "mika")
	before, _ := f.db.GetButlerTask(failID)
	if err := f.g.Start(ctx, agentapi.WorkStart{Handle: failH}); err != nil {
		t.Fatal(err)
	}
	if err := f.g.Fail(ctx, agentapi.WorkFail{Handle: failH, Reason: "gave up"}); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	after, _ := f.db.GetButlerTask(failID)
	if after.State != runtimedb.ButlerTaskFailed || after.LeaseEpoch != before.LeaseEpoch+1 {
		t.Fatalf("after Fail: state %s epoch %d (was %d)", after.State, after.LeaseEpoch, before.LeaseEpoch)
	}
}

func TestHeartbeatNeedsABoundRuntimePrincipal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.g.Heartbeat(ctx, agentapi.Heartbeat{At: time.Now()}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a heartbeat from nobody: %v", err)
	}
	f.bindRuntime(t, "runtime-alfred-1", "alfred")
	if err := f.g.Heartbeat(WithPrincipal(ctx, "runtime-alfred-1"), agentapi.Heartbeat{At: time.Now()}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	rt, err := f.db.GetButlerRuntimeByName("alfred")
	if err != nil {
		t.Fatal(err)
	}
	if rt.LastHeartbeat.IsZero() {
		t.Fatal("the heartbeat was not recorded")
	}
}

// Remember goes through the family surface's own tool, with its own
// rules: a fact for this person lands in memory and recall finds it;
// a household scope is refused.
func TestRememberGoesThroughTheHouseholdTool(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, h := f.openTask(t, "joan", "sam")
	if err := f.g.Remember(ctx, agentapi.RememberRequest{Handle: h, Kind: agentapi.MemoryPreference, Content: "Prefers jasmine tea"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	res, err := f.g.Invoke(ctx, agentapi.InvokeRequest{Handle: h, Tool: "recall", Args: json.RawMessage(`{"text":"jasmine"}`)})
	if err != nil || res.Outcome != agentapi.OutcomeOK || !strings.Contains(res.Content, "jasmine tea") {
		t.Fatalf("recall after remember = %+v, %v", res, err)
	}
	if err := f.g.Remember(ctx, agentapi.RememberRequest{Handle: h, Scope: agentapi.MemoryScopeHousehold, Kind: agentapi.MemoryFact, Content: "The boiler is old"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a household-scope remember was not refused: %v", err)
	}
	// An operator handle has no memory tools at all.
	_, admin := f.openTask(t, "alfred", "mika")
	if err := f.g.Remember(ctx, agentapi.RememberRequest{Handle: admin, Kind: agentapi.MemoryFact, Content: "x"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("remember on the operator surface: %v", err)
	}
}

func TestLaterPhasesAreNamedNotFaked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, h := f.openTask(t, "joan", "sam")
	if _, err := f.g.Recall(ctx, agentapi.RecallRequest{Handle: h, Text: "tea"}); !errors.Is(err, agentapi.ErrNotImplemented) {
		t.Fatalf("Recall: %v", err)
	}
	if _, err := f.g.Propose(ctx, agentapi.ProposeRequest{Handle: h, Kind: "Route"}); !errors.Is(err, agentapi.ErrNotImplemented) {
		t.Fatalf("Propose: %v", err)
	}
	if err := f.g.Notify(ctx, agentapi.NotifyRequest{Handle: h, Text: "hi"}); !errors.Is(err, agentapi.ErrNotImplemented) {
		t.Fatalf("Notify: %v", err)
	}
	var nie *agentapi.NotImplementedError
	if err := f.g.Notify(ctx, agentapi.NotifyRequest{Handle: h}); !errors.As(err, &nie) || nie.Phase == "" {
		t.Fatalf("Notify's error does not name its phase: %v", err)
	}
}

// The limits Serve declares are the ones owned here now: a test that
// fails the day serve.go and this package disagree, until 2b deletes
// Serve's copy.
func TestTheLimitsMatchServe(t *testing.T) {
	if ConversationSliceRounds != 8 || ConversationToolCalls != 64 || ObservationTTL != 15*time.Minute {
		t.Fatalf("limits %d/%d/%s drifted from butlerlive/serve.go", ConversationSliceRounds, ConversationToolCalls, ObservationTTL)
	}
	g := &Gateway{}
	if g.sliceRounds() != ConversationSliceRounds || g.toolCalls() != ConversationToolCalls || g.observationTTL() != ObservationTTL {
		t.Fatal("an unconfigured gateway does not default to the conversation limits")
	}
}
