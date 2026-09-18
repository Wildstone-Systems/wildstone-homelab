package butlerstore

// P3's evidence (ADR-W030): a diagnosis needing at least eight reads
// completes or resumes without repeating verified reads, termination at
// each checkpoint resumes safely, and a second coordinator cannot
// dispatch concurrently.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// countingToolset is a minimal Toolset that records how many times each
// tool was actually dispatched — the measurement the "no repeated
// verified reads" claim rests on.
type countingToolset struct {
	calls map[string]int
	fail  map[string]bool
}

func newCountingToolset() *countingToolset {
	return &countingToolset{calls: map[string]int{}, fail: map[string]bool{}}
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
		Profile: butler.ProfileOperator, ModelVerdict: butler.VerdictFull,
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

func testLedger(t *testing.T, path string) (*runtimedb.DB, *Ledger) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Into(t, path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
		Surface: runtimedb.ButlerSurfaceAdmin, Profile: "operator",
		Audience:  runtimedb.ButlerAudiencePrivate,
		Objective: "why is photos unhealthy?", CompletionCriteria: "root cause named",
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	return id
}

func runner(ts *countingToolset, l *Ledger) *butler.Runner {
	return &butler.Runner{
		Gateway: ts, Ledger: l, MaxSliceRounds: 12,
		PermissionLabel: "operator", Audience: "private", Actor: "butler",
		// Long enough that nothing in these tests goes stale, so a
		// repeated dispatch means the ledger failed rather than that a
		// reading aged out. TestStaleObservationsAreRefreshed sets it
		// the other way on purpose.
		ObservationTTL: time.Hour,
	}
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
	tr, err := runner(ts, ledger).Run(context.Background(), p, id, "why is photos unhealthy?")
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
	if _, err := runner(ts, ledger).Run(context.Background(), dying, id, "why is photos unhealthy?"); err == nil {
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
	tr, err := runner(ts, ledger2).Run(context.Background(), resumed, id, "why is photos unhealthy?")
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
			if _, err := runner(ts, ledger).Run(context.Background(), dying, id, "diagnose"); err == nil {
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
			if _, err := runner(ts, ledger2).Run(context.Background(), finish, id, "diagnose"); err != nil {
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
	_, err = runner(ts, ledger).Run(context.Background(), p, id, "diagnose")
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
		r := runner(ts, ledger)
		r.MaxSliceRounds = 2
		p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
			readsFor("s1"), readsFor("s2"), readsFor("s3"), {Text: "done"},
		}}
		tr, err := r.Run(context.Background(), p, id, "diagnose")
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
		p2 := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{{Text: "done"}}}
		if _, err := r.Run(context.Background(), p2, id, "continue"); err != nil {
			t.Fatalf("continue: %v", err)
		}
		if task, _ := db.GetButlerTask(id); task.State != runtimedb.ButlerTaskCompleted {
			t.Fatalf("state after continue = %q", task.State)
		}
	})

	t.Run("aggregate limit does not reset on the next slice", func(t *testing.T) {
		id := newTask(t, db)
		r := runner(ts, ledger)
		// Room for more rounds than the aggregate budget allows, so it
		// is the tool budget that stops this run and not the slice one.
		r.MaxSliceRounds = 6
		r.MaxToolCalls = 2
		p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
			readsFor("a1"), readsFor("a2"), readsFor("a3"),
		}}
		tr, err := r.Run(context.Background(), p, id, "diagnose")
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
		p2 := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{readsFor("a4")}}
		tr2, err := r.Run(context.Background(), p2, id, "continue")
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
	if _, err := runner(ts, ledger).Run(context.Background(), p, id, "check n1"); err != nil {
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
	r := runner(ts, ledger)
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

	_, err := r.Run(context.Background(), promoting, id, "diagnose")
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
