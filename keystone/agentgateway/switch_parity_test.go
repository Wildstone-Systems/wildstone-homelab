package agentgateway_test

// The regression proof ADR-W039 Phase 2b's wiring switch exists for
// (#337): keystone/butlerlive.Butlers.UseAgentGateway picks between the
// in-process Agent Gateway (true, the default this feature branch
// ships) and the closure path (false) it is kept beside, and both must
// drive the SAME fake-provider script to the SAME ledger rows and the
// SAME transcript. This lives here, as an external test package,
// because it is the one place that can import both
// keystone/agentgateway and keystone/butlerlive without a cycle:
// keystone/agentgateway imports keystone/butlerlive, so
// keystone/butlerlive cannot import keystone/agentgateway back, and
// this package's own internal tests cannot import keystone/agentgateway
// either (they would hit the identical cycle through their own package
// name). An external test package sits above both.

import (
	"context"
	"reflect"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentgateway"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerlive"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// scriptedProvider replays one fixed sequence of responses, identically
// for both switch values: the two runs it drives are on separate
// tasks, so nothing about this needs to be shared between them.
type scriptedProvider struct {
	name  string
	steps []*provider.ChatResponse
	at    int
}

func (p *scriptedProvider) Name() string { return p.name }
func (p *scriptedProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	if p.at >= len(p.steps) {
		return &provider.ChatResponse{Text: "(no more scripted responses)"}, nil
	}
	r := p.steps[p.at]
	p.at++
	return r, nil
}

func script() []*provider.ChatResponse {
	return []*provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "get_health", Input: map[string]any{"node": "n1"}}}, StopReason: "tool_use"},
		{Text: "n1 is fine"},
		// The objective verifier's own tool-free call, always asked
		// after a final answer.
		{Text: `{"kind":"informational"}`},
	}
}

// countingToolset is the same minimal admin Toolset
// keystone/butlerlive's own fixture uses.
type countingToolset struct{}

func (countingToolset) SetAuthorizer(butler.ToolAuthorizer)         {}
func (countingToolset) SetSessionTask(string)                       {}
func (countingToolset) SetCaller(butler.CallerKind, string, string) {}
func (countingToolset) Permits(string, bool) (bool, string)         { return true, "" }
func (countingToolset) OfferedSpecs(bool) []provider.Tool {
	return []provider.Tool{{Name: "get_health", Description: "read a node's health"}}
}
func (countingToolset) Execute(_ context.Context, name string, input map[string]any, _ bool) (butler.ToolOutcome, error) {
	return butler.ToolOutcome{Content: name + "/" + input["node"].(string) + ": ok"}, nil
}
func (countingToolset) SystemPrompt() string { return "operator surface" }
func (countingToolset) Capabilities(bool) butler.CapabilityManifest {
	return butler.CapabilityManifest{
		AgentClass: butler.AgentClassOwner, ModelVerdict: butler.VerdictFull,
		Tools: []butler.CapabilityTool{{Name: "get_health", Available: true}},
	}
}

// rowKey and traceKey are what must agree between the two switch
// values: everything but ids and timestamps.
type rowKey struct {
	Kind, Name, State, Params string
}
type traceKey struct {
	Type, PermissionLabel, Audience string
	SourceIDs                       []string
}

func stepsOf(t *testing.T, db *runtimedb.DB, taskID string) []rowKey {
	t.Helper()
	steps, err := db.ListButlerTaskSteps(taskID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]rowKey, 0, len(steps))
	for _, s := range steps {
		out = append(out, rowKey{Kind: string(s.Kind), Name: s.Name, State: string(s.State), Params: s.ParamsJSON})
	}
	return out
}

func traceOf(t *testing.T, db *runtimedb.DB, taskID string) []traceKey {
	t.Helper()
	events, err := db.ListButlerTrace(taskID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]traceKey, 0, len(events))
	for _, e := range events {
		out = append(out, traceKey{Type: e.Type, PermissionLabel: e.PermissionLabel, Audience: string(e.Audience), SourceIDs: e.SourceIDs})
	}
	return out
}

func TestBothSwitchValuesProduceIdenticalLedgerRowsForTheSameScript(t *testing.T) {
	db, err := runtimedb.Open(t.TempDir() + "/keystone.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Endpoint: "http://kitchen:11434", Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	seq, err := db.CreateButler(runtimedb.Butler{
		Name: "alfred", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box",
	}, "mika")
	if err != nil {
		t.Fatalf("CreateButler: %v", err)
	}
	// The native runtime record api.createButler mints
	// (ensureNativeRuntime); runtimedb.CreateButler does not.
	if err := db.SetButlerRuntime(seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative}, "mika"); err != nil {
		t.Fatalf("SetButlerRuntime: %v", err)
	}
	if err := db.GrantButlerAccess("alfred", "mika", "mika"); err != nil {
		t.Fatal(err)
	}
	rt, err := db.GetButlerRuntimeByName("alfred")
	if err != nil {
		t.Fatal(err)
	}

	lease, err := db.AcquireCoordinator("keystone-test")
	if err != nil {
		t.Fatalf("AcquireCoordinator: %v", err)
	}
	ledger := butlerstore.New(db, lease)
	ledger.DerivedAuthorityChecks()

	gateways := butlerlive.GatewayFunc(func(b runtimedb.Butler, _ butlerlive.Turn) (butler.Toolset, error) {
		return countingToolset{}, nil
	})

	withPrincipal := func(ctx context.Context, name string) context.Context {
		bindingRT, err := db.GetButlerRuntimeByName(name)
		if err != nil || bindingRT.Principal == "" {
			return ctx
		}
		return agentgateway.WithPrincipal(ctx, bindingRT.Principal)
	}
	_ = rt

	// Two independent Butlers, sharing the database, the ledger and the
	// gateway factory, but each with its OWN provider-client cache
	// (Butlers.providerFor caches by route, keyed on the struct
	// instance) — otherwise the second Serve call would reuse the
	// first one's already-exhausted scripted provider instead of
	// replaying the script from the top.
	newButlers := func(useGateway bool) *butlerlive.Butlers {
		bs := &butlerlive.Butlers{
			DB: db, Ledger: ledger, Gateways: gateways,
			Providers: func(runtimedb.Butler) (provider.Provider, error) {
				return &scriptedProvider{name: "ollama", steps: script()}, nil
			},
			UseAgentGateway: useGateway,
		}
		gw := &agentgateway.Gateway{DB: db, Ledger: ledger, Gateways: gateways, Clock: bs.ClockLine}
		bs.AgentClient = agentgateway.InProcess(gw)
		bs.WithPrincipal = withPrincipal
		return bs
	}

	turn := func() butlerlive.Turn {
		return butlerlive.Turn{Butler: "alfred", Member: "mika", Text: "is n1 healthy?"}
	}

	closureAnswer, err := newButlers(false).Serve(context.Background(), turn())
	if err != nil {
		t.Fatalf("closure path Serve: %v", err)
	}

	gatewayAnswer, err := newButlers(true).Serve(context.Background(), turn())
	if err != nil {
		t.Fatalf("gateway path Serve: %v", err)
	}

	if closureAnswer.FinalText != gatewayAnswer.FinalText {
		t.Fatalf("final text differs:\n closure %q\n gateway %q", closureAnswer.FinalText, gatewayAnswer.FinalText)
	}
	if closureAnswer.Capped != gatewayAnswer.Capped {
		t.Fatalf("capped differs: closure %v gateway %v", closureAnswer.Capped, gatewayAnswer.Capped)
	}
	if len(closureAnswer.Calls) != len(gatewayAnswer.Calls) {
		t.Fatalf("call counts differ: closure %d gateway %d", len(closureAnswer.Calls), len(gatewayAnswer.Calls))
	}
	for i := range closureAnswer.Calls {
		c, g := closureAnswer.Calls[i], gatewayAnswer.Calls[i]
		if c.Name != g.Name || c.Result != g.Result || c.Rejected != g.Rejected {
			t.Fatalf("call %d differs:\n closure %+v\n gateway %+v", i, c, g)
		}
	}

	closureSteps, gatewaySteps := stepsOf(t, db, closureAnswer.TaskID), stepsOf(t, db, gatewayAnswer.TaskID)
	if !reflect.DeepEqual(closureSteps, gatewaySteps) {
		t.Fatalf("steps differ:\n closure %+v\n gateway %+v", closureSteps, gatewaySteps)
	}
	closureTrace, gatewayTrace := traceOf(t, db, closureAnswer.TaskID), traceOf(t, db, gatewayAnswer.TaskID)
	if !reflect.DeepEqual(closureTrace, gatewayTrace) {
		t.Fatalf("trace differs:\n closure %+v\n gateway %+v", closureTrace, gatewayTrace)
	}

	closureTask, err := db.GetButlerTask(closureAnswer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	gatewayTask, err := db.GetButlerTask(gatewayAnswer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if closureTask.State != gatewayTask.State {
		t.Fatalf("task state differs: closure %s gateway %s", closureTask.State, gatewayTask.State)
	}
}

// dyingProvider answers its first round and then goes unreachable,
// standing in for a model lost mid-turn.
type dyingProvider struct {
	first *provider.ChatResponse
	rest  []*provider.ChatResponse
	round int
}

func (p *dyingProvider) Name() string { return "ollama" }
func (p *dyingProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	p.round++
	if p.round == 1 {
		return p.first, nil
	}
	return nil, errFakeUnreachable
}

var errFakeUnreachable = &fakeUnreachableError{}

type fakeUnreachableError struct{}

func (*fakeUnreachableError) Error() string { return "provider unreachable" }

// A resumed conversation does not re-dispatch an already-verified tool
// call — the durable-runner guarantee ADR-W030 P3 exists for — when it
// is driven through the Agent Gateway path (ADR-W039 Phase 2b, #337).
// keystone/butlerlive's own TestAConversationSurvivesARestartWithout
// RepeatingADispatch pins the identical guarantee on the closure path
// (UseAgentGateway defaults false in its fixture); this is that same
// proof for UseAgentGateway=true, which keystone/butlerlive's internal
// tests cannot themselves construct (see the file doc comment).
func TestResumeWithoutRepeatingADispatchUnderTheGatewayPath(t *testing.T) {
	db, err := runtimedb.Open(t.TempDir() + "/keystone.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Endpoint: "http://kitchen:11434", Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	seq, err := db.CreateButler(runtimedb.Butler{
		Name: "alfred", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box",
	}, "mika")
	if err != nil {
		t.Fatalf("CreateButler: %v", err)
	}
	if err := db.SetButlerRuntime(seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative}, "mika"); err != nil {
		t.Fatalf("SetButlerRuntime: %v", err)
	}
	if err := db.GrantButlerAccess("alfred", "mika", "mika"); err != nil {
		t.Fatal(err)
	}

	lease, err := db.AcquireCoordinator("keystone-test")
	if err != nil {
		t.Fatalf("AcquireCoordinator: %v", err)
	}
	ledger := butlerstore.New(db, lease)
	ledger.DerivedAuthorityChecks()
	gateways := butlerlive.GatewayFunc(func(runtimedb.Butler, butlerlive.Turn) (butler.Toolset, error) {
		return countingToolset{}, nil
	})
	withPrincipal := func(ctx context.Context, name string) context.Context {
		rt, err := db.GetButlerRuntimeByName(name)
		if err != nil || rt.Principal == "" {
			return ctx
		}
		return agentgateway.WithPrincipal(ctx, rt.Principal)
	}
	newButlers := func(p provider.Provider) *butlerlive.Butlers {
		bs := &butlerlive.Butlers{
			DB: db, Ledger: ledger, Gateways: gateways, UseAgentGateway: true,
			Providers: func(runtimedb.Butler) (provider.Provider, error) { return p, nil },
		}
		gw := &agentgateway.Gateway{DB: db, Ledger: ledger, Gateways: gateways, Clock: bs.ClockLine}
		bs.AgentClient = agentgateway.InProcess(gw)
		bs.WithPrincipal = withPrincipal
		return bs
	}

	dying := &dyingProvider{first: &provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{ID: "c1", Name: "get_health", Input: map[string]any{"node": "n1"}}}, StopReason: "tool_use",
	}}
	first, err := newButlers(dying).Serve(context.Background(), butlerlive.Turn{Butler: "alfred", Member: "mika", Text: "is n1 healthy?"})
	if err == nil {
		t.Fatal("expected the first turn to fail when its model went away")
	}
	if first.TaskID == "" {
		t.Fatal("no durable task was opened, so there is nothing a resume could continue")
	}
	beforeSteps := stepsOf(t, db, first.TaskID)
	if len(beforeSteps) == 0 {
		t.Fatal("the tool call that ran before the provider died left no step")
	}

	sessionID, err := db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "ollama",
		Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerTaskSession(first.TaskID, sessionID); err != nil {
		t.Fatal(err)
	}

	resumed := &scriptedProvider{name: "ollama", steps: []*provider.ChatResponse{
		{Text: "n1 is fine"},
		{Text: `{"kind":"informational"}`},
	}}
	second, err := newButlers(resumed).Serve(context.Background(), butlerlive.Turn{
		Butler: "alfred", Member: "mika", Text: "continue", SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("resuming through the gateway path: %v", err)
	}
	if !second.Resumed || second.TaskID != first.TaskID {
		t.Fatalf("the follow-up did not resume the original task: resumed=%v task=%s want=%s", second.Resumed, second.TaskID, first.TaskID)
	}
	// The resume dispatches fresh model rounds (a resumed slice
	// reconstructs context by asking the model again) but must not
	// dispatch get_health a second time: that read is already verified,
	// and only a NEW tool step would mean it was taken again.
	toolSteps := func(steps []rowKey) int {
		n := 0
		for _, s := range steps {
			if s.Kind == string(runtimedb.ButlerStepTool) {
				n++
			}
		}
		return n
	}
	afterSteps := stepsOf(t, db, first.TaskID)
	if toolSteps(afterSteps) != toolSteps(beforeSteps) {
		t.Fatalf("the resumed slice re-dispatched a verified tool call:\n before %+v\n after  %+v", beforeSteps, afterSteps)
	}
	task, err := db.GetButlerTask(first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task state after the resume = %q, want completed", task.State)
	}
}
