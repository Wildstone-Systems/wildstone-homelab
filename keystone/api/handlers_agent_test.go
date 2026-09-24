package api

// The Agent API over HTTP (ADR-W039 Phase 2c, #337; ADR-W040): the
// conformance evidence Phase 2's own bar asks for (§8, "the in-process
// and eventual HTTP clients can be asserted byte-identical") plus the
// two loose ends 2a/2b left — fenceRuntimeTasks and the health flip on
// a runtime's first authenticated call.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi/httpclient"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentgateway"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// conformanceProposalYAML is the one resource document the conformance
// script's propose_change files (ADR-W039 Phase 3, #339): real,
// policy-valid YAML, built through resource.Marshal so its wire shape is
// exactly what a real propose_change would produce, never a hand-rolled
// approximation of it.
var conformanceProposalYAML = func() string {
	data, err := resource.Marshal(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: "conformance", Owner: "capability/test", Node: "n1"},
		Container:  &resource.ContainerSpec{Image: "ghcr.io/example/conformance@" + testDigest},
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}()

// conformanceToolset is a minimal, wholly deterministic Toolset used by
// every test in this file: one read-only tool ("peek") and one mutating
// tool ("propose_change") that always files a Plan (never applies one),
// the shape agentgateway.Invoke maps onto OutcomeOK+Proposal. Kept
// separate from the real ToolGateway/HouseholdGateway so a conformance
// test proves the WIRE is byte-identical rather than incidentally
// depending on the Statefile or memory seams being wired the same way
// twice.
type conformanceToolset struct{}

func (conformanceToolset) SetAuthorizer(butler.ToolAuthorizer)         {}
func (conformanceToolset) SetSessionTask(string)                       {}
func (conformanceToolset) SetCaller(butler.CallerKind, string, string) {}
func (conformanceToolset) Permits(string, bool) (bool, string)         { return true, "" }
func (conformanceToolset) SystemPrompt() string                        { return "conformance surface" }

func (conformanceToolset) OfferedSpecs(readOnly bool) []provider.Tool {
	specs := []provider.Tool{{Name: "peek", Description: "read something. It changes nothing."}}
	if !readOnly {
		specs = append(specs, provider.Tool{Name: "propose_change", Description: "propose a change for approval."})
	}
	return specs
}

func (conformanceToolset) Execute(_ context.Context, name string, _ map[string]any, _ bool) (butler.ToolOutcome, error) {
	switch name {
	case "peek":
		return butler.ToolOutcome{Content: "peek: all clear"}, nil
	case "propose_change":
		return butler.ToolOutcome{Content: "proposed", Plan: &plan.Plan{Summary: "conformance proposal"}, ResourceYAML: conformanceProposalYAML}, nil
	default:
		return butler.ToolOutcome{}, errors.New("conformanceToolset: unknown tool " + name)
	}
}

func (conformanceToolset) Capabilities(readOnly bool) butler.CapabilityManifest {
	m := butler.CapabilityManifest{AgentClass: butler.AgentClassOwner, ModelVerdict: butler.VerdictFull}
	m.Tools = append(m.Tools, butler.CapabilityTool{Name: "peek", Available: true})
	pc := butler.CapabilityTool{Name: "propose_change", Mutating: true, Available: !readOnly}
	if readOnly {
		pc.Reason = "this session runs read-only, so tools that change anything are withheld"
	}
	m.Tools = append(m.Tools, pc)
	return m
}

func conformanceGateways() butlerlive.GatewayFunc {
	return butlerlive.GatewayFunc(func(runtimedb.Butler, butlerlive.Turn) (butler.Toolset, error) {
		return conformanceToolset{}, nil
	})
}

// conformanceClock is the fixed clock line both arms of the byte-
// identical test share — a real one would carry the current time, and
// two independently-built environments must never differ only because
// they were built a few milliseconds apart.
func conformanceClock() string { return "" }

// newAgentEnv builds a fresh runtime database, one owner, one Agent on
// the native runtime, and the coordinator-fenced ledger every Work call
// needs — everything agentgateway.Gateway.open resolves before it ever
// looks at a handle. It is deliberately its own database per caller
// (never dbtemplate's shared fixture) because the byte-identical test
// needs two environments whose sequence numbers — the Butler's seq, the
// task's, every step's — land on the identical values, which only
// holds when each one is the first and only thing ever written to its
// own database.
func newAgentEnv(t *testing.T) (*runtimedb.DB, *butlerstore.Ledger, runtimedb.Butler) {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
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
	// A minted, bound native runtime principal, as Phase 1 gives every
	// Butler (createButler's own ensureNativeRuntime) — needed so an
	// in-process propose_change call, with no principal in ctx, has the
	// Agent's own runtime principal to file under rather than the Console
	// caller's (ADR-W039 §3.2 Proposals, Phase 3, #339). Named to match
	// what TestInProcessAndHTTPAreByteIdentical binds for Arm B.
	if err := db.CreatePrincipal("runtime-alfred-1", runtimedb.RoleButler, ""); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if err := db.BindRuntimePrincipal("runtime-alfred-1", seq); err != nil {
		t.Fatalf("BindRuntimePrincipal: %v", err)
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
	b, err := db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	return db, ledger, b
}

// openAgentTask opens a conversation task as Serve does at admission
// and issues its handle — the same shape agentgateway's own fixture
// uses, reproduced here because that one is unexported to its package.
func openAgentTask(t *testing.T, db *runtimedb.DB, ledger *butlerstore.Ledger, b runtimedb.Butler) (string, agentapi.Handle) {
	t.Helper()
	id, err := db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		OriginConnection: butlerlive.ConsoleConnection,
		Objective:        "conformance script", ObjectiveKind: runtimedb.ObjectiveInformational,
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	if err := db.SetTaskButler(id, b.Name); err != nil {
		t.Fatalf("SetTaskButler: %v", err)
	}
	h, err := ledger.IssueHandle(id)
	if err != nil {
		t.Fatalf("IssueHandle: %v", err)
	}
	return id, h
}

// agentScriptResult is every response the shared script produces that
// carries a body worth comparing byte for byte.
type agentScriptResult struct {
	manifest      agentapi.Manifest
	readInvoke    agentapi.InvokeResult
	proposeInvoke agentapi.InvokeResult
	recall        []agentapi.MemoryHit
	checkpoint    agentapi.CheckpointReceipt
}

// runAgentScript is the ONE script both arms of the conformance test
// run: manifest, then start (Complete needs a running task), invoke a
// read-only tool, invoke a mutating one that becomes a proposal,
// checkpoint a model step, and complete with evidence. Fatal on any
// unexpected error so a divergence in ERROR behaviour — not just in a
// successful response — fails the test immediately rather than being
// silently skipped.
func runAgentScript(t *testing.T, client agentapi.Client, handle agentapi.Handle) agentScriptResult {
	t.Helper()
	ctx := context.Background()
	var out agentScriptResult
	var err error

	out.manifest, err = client.Manifest(ctx, handle)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if err := client.Start(ctx, agentapi.WorkStart{Handle: handle, Reason: "conformance"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out.readInvoke, err = client.Invoke(ctx, agentapi.InvokeRequest{Handle: handle, Tool: "peek"})
	if err != nil {
		t.Fatalf("Invoke(peek): %v", err)
	}
	out.proposeInvoke, err = client.Invoke(ctx, agentapi.InvokeRequest{
		Handle: handle, Tool: "propose_change", Args: json.RawMessage(`{"target":"n1"}`),
	})
	if err != nil {
		t.Fatalf("Invoke(propose_change): %v", err)
	}
	out.checkpoint, err = client.Checkpoint(ctx, agentapi.Checkpoint{
		Handle: handle, State: agentapi.StepProposed, Kind: agentapi.StepModel,
		Name: "round-1", Params: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	out.recall, err = client.Recall(ctx, agentapi.RecallRequest{Handle: handle, Text: "n1"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if err := client.Complete(ctx, agentapi.WorkComplete{
		Handle: handle, Summary: "the conformance script finished",
		Evidence: []agentapi.Evidence{{Kind: "observation", Description: "the script ran to completion"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return out
}

// assertSameJSON is the byte-identical assertion itself: both values
// marshal, through encoding/json, to the exact same bytes.
func assertSameJSON(t *testing.T, label string, inProcess, overHTTP any) {
	t.Helper()
	a, err := json.Marshal(inProcess)
	if err != nil {
		t.Fatalf("marshaling the in-process %s: %v", label, err)
	}
	b, err := json.Marshal(overHTTP)
	if err != nil {
		t.Fatalf("marshaling the HTTP %s: %v", label, err)
	}
	if string(a) != string(b) {
		t.Errorf("%s is not byte-identical:\n in-process %s\n over HTTP  %s", label, a, b)
	}
}

// ledgerRowKey and ledgerTraceKey are what the two arms' ledgers must
// agree on beyond the responses themselves: the same shape
// agentgateway's own switch-parity test compares (Kind/Name/State/
// Params, and a trace's Type/PermissionLabel/Audience/SourceIDs),
// unlike that test kept WITH the sequence numbers, since both arms here
// are built from scratch and are expected to land on the identical
// ones.
type ledgerRowKey struct{ Kind, Name, State, Params string }
type ledgerTraceKey struct {
	Type, PermissionLabel, Audience string
	SourceIDs                       []string
}

func ledgerStepsOf(t *testing.T, db *runtimedb.DB, taskID string) []ledgerRowKey {
	t.Helper()
	steps, err := db.ListButlerTaskSteps(taskID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]ledgerRowKey, 0, len(steps))
	for _, s := range steps {
		out = append(out, ledgerRowKey{Kind: string(s.Kind), Name: s.Name, State: string(s.State), Params: s.ParamsJSON})
	}
	return out
}

func ledgerTraceOf(t *testing.T, db *runtimedb.DB, taskID string) []ledgerTraceKey {
	t.Helper()
	events, err := db.ListButlerTrace(taskID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]ledgerTraceKey, 0, len(events))
	for _, e := range events {
		out = append(out, ledgerTraceKey{Type: e.Type, PermissionLabel: e.PermissionLabel, Audience: string(e.Audience), SourceIDs: e.SourceIDs})
	}
	return out
}

// TestInProcessAndHTTPAreByteIdentical is ADR-W039 Phase 2's own
// evidence bar (§8) given to Phase 2c: the SAME script, run once
// through the in-process keystone/agentgateway.Gateway and once through
// keystone/agentapi/httpclient.Client against an httptest.Server
// running the real api.Server with a runtime bearer, must answer with
// byte-identical JSON at every step and leave byte-identical ledger
// rows — because both paths call the exact same Gateway methods
// (ADR-W040 §8), so a difference here would mean the HTTP handlers
// added or lost something the in-process caller never sees.
func TestInProcessAndHTTPAreByteIdentical(t *testing.T) {
	// Arm A: in-process, no transport at all.
	dbA, ledgerA, butlerA := newAgentEnv(t)
	stateA, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	gwA := &agentgateway.Gateway{
		DB: dbA, Ledger: ledgerA, Gateways: conformanceGateways(), Clock: conformanceClock,
		// Wired identically to Arm B below, so propose_change files a
		// real Plan on both arms rather than only returning the
		// pre-Phase-3 placeholder (ADR-W039 Phase 3, #339).
		Proposals: proposals.Deps{DB: dbA, State: stateA, Policy: policy.NewEngine(nil)},
	}
	clientA := agentgateway.InProcess(gwA)
	taskA, handleA := openAgentTask(t, dbA, ledgerA, butlerA)
	resultA := runAgentScript(t, clientA, handleA)

	// Arm B: the real api.Server, over HTTP, authenticated as a real
	// runtime principal's bearer — everything a Hermes bridge would
	// actually hold.
	dbB, ledgerB, butlerB := newAgentEnv(t)
	p := &principal.Store{DB: dbB}
	state, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	s := New(p, dbB, state, nil)
	gwB := &agentgateway.Gateway{
		DB: dbB, Ledger: ledgerB, Gateways: conformanceGateways(), Clock: conformanceClock,
		Proposals: proposals.Deps{DB: dbB, State: state, Policy: policy.NewEngine(nil)},
	}
	s.AgentGateway = agentgateway.InProcess(gwB)
	s.Ledger = ledgerB
	srv := newTestHTTPServer(t, s)

	// newAgentEnv already creates and binds "runtime-alfred-1" (the
	// native runtime principal every Butler is minted, Phase 1) so Arm
	// A's in-process, no-ctx-principal propose_change call has one to
	// file under too; here it only needs a bearer minted for it.
	const principalName = "runtime-alfred-1"
	token, err := p.Mint(principalName, "", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	clientB := &httpclient.Client{BaseURL: srv, Bearer: token}
	taskB, handleB := openAgentTask(t, dbB, ledgerB, butlerB)
	resultB := runAgentScript(t, clientB, handleB)

	assertSameJSON(t, "manifest", resultA.manifest, resultB.manifest)
	assertSameJSON(t, "invoke(peek)", resultA.readInvoke, resultB.readInvoke)
	assertSameJSON(t, "invoke(propose_change)", resultA.proposeInvoke, resultB.proposeInvoke)
	assertSameJSON(t, "checkpoint", resultA.checkpoint, resultB.checkpoint)
	assertSameJSON(t, "recall", resultA.recall, resultB.recall)

	if taskA != taskB {
		t.Fatalf("the two arms opened different task ids (%s vs %s); the fixtures have drifted apart", taskA, taskB)
	}
	stepsA, stepsB := ledgerStepsOf(t, dbA, taskA), ledgerStepsOf(t, dbB, taskB)
	if len(stepsA) == 0 || len(stepsA) != len(stepsB) {
		t.Fatalf("step counts differ: in-process %d, http %d", len(stepsA), len(stepsB))
	}
	for i := range stepsA {
		if stepsA[i] != stepsB[i] {
			t.Errorf("step %d differs:\n in-process %+v\n http       %+v", i, stepsA[i], stepsB[i])
		}
	}
	traceA, traceB := ledgerTraceOf(t, dbA, taskA), ledgerTraceOf(t, dbB, taskB)
	if len(traceA) == 0 || len(traceA) != len(traceB) {
		t.Fatalf("trace counts differ: in-process %d, http %d", len(traceA), len(traceB))
	}
	for i := range traceA {
		if traceA[i].Type != traceB[i].Type || traceA[i].PermissionLabel != traceB[i].PermissionLabel ||
			traceA[i].Audience != traceB[i].Audience {
			t.Errorf("trace event %d differs:\n in-process %+v\n http       %+v", i, traceA[i], traceB[i])
		}
	}

	taskRowA, err := dbA.GetButlerTask(taskA)
	if err != nil {
		t.Fatal(err)
	}
	taskRowB, err := dbB.GetButlerTask(taskB)
	if err != nil {
		t.Fatal(err)
	}
	if taskRowA.State != taskRowB.State || taskRowA.State != runtimedb.ButlerTaskCompleted {
		t.Errorf("task states differ or did not complete: in-process %s, http %s", taskRowA.State, taskRowB.State)
	}
}

// newTestHTTPServer starts s.Handler() and returns its base URL,
// closing it when the test ends.
func newTestHTTPServer(t *testing.T, s *Server) string {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// agentEnvHarness is one Agent, on the native runtime, wired for the
// Agent API's HTTP routes — the shared setup for every test in this
// file that is not the byte-identical comparison above (that one needs
// two independent environments; these need only one, driven entirely
// over HTTP as an external runtime would drive it).
func agentEnvHarness(t *testing.T) (*harness, *butlerstore.Ledger) {
	t.Helper()
	h := butlerConsole(t)
	if err := h.s.DB.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	lease, err := h.s.DB.AcquireCoordinator("keystone-test")
	if err != nil {
		t.Fatalf("AcquireCoordinator: %v", err)
	}
	ledger := butlerstore.New(h.s.DB, lease)
	ledger.DerivedAuthorityChecks()
	gw := &agentgateway.Gateway{DB: h.s.DB, Ledger: ledger, Gateways: conformanceGateways(), Clock: conformanceClock}
	h.s.AgentGateway = agentgateway.InProcess(gw)
	h.s.Ledger = ledger
	return h, ledger
}

// TestAgentRoutesRefuseACredentialThatIsNotARuntimePrincipal is
// deliverable 1's "require RuntimePrincipalButler(id.Name) ok (else 403
// with no detail about the handle)": a credential that holds every verb
// these routes ask for — an admin token — is still refused, because
// role and runtime-principal are different questions, and it is refused
// before its body is even decoded.
func TestAgentRoutesRefuseACredentialThatIsNotARuntimePrincipal(t *testing.T) {
	h, _ := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	for _, route := range []string{
		"/api/v1/agent/manifest", "/api/v1/agent/invoke", "/api/v1/agent/checkpoint",
		"/api/v1/agent/start", "/api/v1/agent/heartbeat",
	} {
		res := h.do(runtimedb.RoleAdmin, http.MethodPost, route, map[string]any{}, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s with an admin (non-runtime) token = %d, want 403", route, res.StatusCode)
		}
	}
}

// TestAgentInvokeDeniedIsAnOrdinary200 pins ADR-W040 §2's own rule: a
// gateway refusal is an Outcome in a 200 body, never an HTTP error —
// the property that lets a runtime treat denied the same way whichever
// transport it is on.
func TestAgentInvokeDeniedIsAnOrdinary200(t *testing.T) {
	h, ledger := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	cred := rotate(t, h, "alfred", nil)
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	_, handle := openAgentTask(t, h.s.DB, ledger, b)

	var out agentapi.InvokeResult
	res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/invoke",
		map[string]any{"handle": string(handle), "tool": ""}, &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a denied invoke = %d, want 200", res.StatusCode)
	}
	if out.Outcome != agentapi.OutcomeDenied {
		t.Errorf("outcome = %q, want denied", out.Outcome)
	}
}

// TestAgentNotifyReturns501WithThePhase pins deliverable 1's error
// mapping for the one family ADR-W039 has not reached yet as of Phase 3:
// NotImplementedError becomes a 501 whose body names the method and the
// phase that serves it, not a bare 404 or 500 that gives an operator
// nothing to act on. Recall and Propose were the same shape through
// Phase 2c; Phase 3 implements them (see TestInProcessAndHTTPAreByte
// Identical's recall comparison and handlers_agent_test.go's own
// propose coverage below).
func TestAgentNotifyReturns501WithThePhase(t *testing.T) {
	h, ledger := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	cred := rotate(t, h, "alfred", nil)
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	_, handle := openAgentTask(t, h.s.DB, ledger, b)

	var body map[string]any
	res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/notify",
		map[string]any{"handle": string(handle), "text": "hello"}, &body)
	if res.StatusCode != http.StatusNotImplemented {
		t.Fatalf("notify = %d, want 501", res.StatusCode)
	}
	if body["method"] != "Notify" || body["phase"] != "Phase 4" {
		t.Errorf("notify body = %+v, want method \"Notify\" phase \"Phase 4\"", body)
	}
}

// TestAgentMalformedBodyIsBadRequest: JSON that does not decode as the
// route's agentapi.* type never reaches the gateway at all.
func TestAgentMalformedBodyIsBadRequest(t *testing.T) {
	h, _ := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	cred := rotate(t, h, "alfred", nil)
	res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/invoke", map[string]any{"handle": 12345}, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed body = %d, want 400", res.StatusCode)
	}
}

// TestWrongButlerBearerCannotReachAnotherAgentsHandle pins §4.1's
// identity binding at the HTTP layer: a runtime principal is a real
// credential and authenticates fine, but a handle for another Agent's
// task is refused however valid the bearer is.
//
// Manifest, not Invoke: Invoke folds every refusal — a wrong-Agent
// bearer included — into an ordinary `denied` Outcome in a 200 body
// (ADR-W039 §4 item 5, agentgateway.refusalOutcome), which is a
// deliberate answer, not a transport failure. Every other Agent API
// method surfaces the identical §4.1 refusal as an ordinary error, and
// that error maps to 403 here (see TestAgentInvokeDeniedIsAnOrdinary200
// and TestRevokingFencesOpenTasksAndWithdrawsTheHandle for both halves
// of that split).
func TestWrongButlerBearerCannotReachAnotherAgentsHandle(t *testing.T) {
	h, ledger := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	createAgent(t, h, "beatrice")
	alfred, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	_, handle := openAgentTask(t, h.s.DB, ledger, alfred)
	beatriceCred := rotate(t, h, "beatrice", nil)

	res := h.doToken(beatriceCred.Secret, http.MethodPost, "/api/v1/agent/manifest",
		map[string]any{"handle": string(handle)}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("beatrice's bearer against alfred's handle = %d, want 403", res.StatusCode)
	}
}

// TestRevokedBearerCannotAuthenticate: revocation is checked by
// Principals.Authenticate, per request, before RBAC or the runtime-
// principal check ever run — 401, not 403.
func TestRevokedBearerCannotAuthenticate(t *testing.T) {
	h, _ := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	cred := rotate(t, h, "alfred", nil)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/alfred/runtime/revoke", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", res.StatusCode)
	}
	res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/heartbeat",
		map[string]any{"at": time.Now().UTC().Format(time.RFC3339)}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked bearer = %d, want 401", res.StatusCode)
	}
}

// TestAgentCallFlipsUnregisteredRuntimeToConnectedAndTouchesHeartbeat is
// Phase 1b's loose end (#336): the first successfully authenticated
// call from a runtime whose health is still `unregistered` flips it to
// `connected` (§4.1), observed here through the ordinary
// GET .../runtime route, and stamps the heartbeat every authenticated
// runtime finds.
func TestAgentCallFlipsUnregisteredRuntimeToConnectedAndTouchesHeartbeat(t *testing.T) {
	h, _ := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	// Move off native, whose health is connected the moment it is
	// created, onto a runtime type that starts unregistered until it
	// actually authenticates.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/alfred/runtime", map[string]any{
		"type": "hermes", "endpoint": "https://hermes-alfred.internal/",
	}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("PUT runtime = %d", res.StatusCode)
	}
	cred := rotate(t, h, "alfred", nil)

	before := readRuntime(t, h, "alfred")
	if before.Health != agentapi.HealthUnregistered {
		t.Fatalf("health before any call = %q, want unregistered", before.Health)
	}
	if !before.LastHeartbeat.IsZero() {
		t.Fatalf("a heartbeat was recorded before any call: %v", before.LastHeartbeat)
	}

	res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/heartbeat",
		map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "version": "hermes-test/1"}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat = %d", res.StatusCode)
	}

	after := readRuntime(t, h, "alfred")
	if after.Health != agentapi.HealthConnected {
		t.Errorf("health after the first authenticated call = %q, want connected", after.Health)
	}
	if after.LastHeartbeat.IsZero() {
		t.Error("the heartbeat was not recorded on the first authenticated call")
	}

	// A second call must not need the unregistered->connected branch
	// again, and still touches the heartbeat.
	firstBeat := after.LastHeartbeat
	time.Sleep(time.Millisecond)
	if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/heartbeat",
		map[string]any{"at": time.Now().UTC().Format(time.RFC3339)}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("second heartbeat = %d", res.StatusCode)
	}
	again := readRuntime(t, h, "alfred")
	if again.Health != agentapi.HealthConnected {
		t.Errorf("health after a second call = %q, want still connected", again.Health)
	}
	if !again.LastHeartbeat.After(firstBeat) {
		t.Errorf("the second call did not advance the heartbeat: first %v, second %v", firstBeat, again.LastHeartbeat)
	}
}

// TestRevokingFencesOpenTasksAndWithdrawsTheHandle is deliverable 3's
// evidence: fenceRuntimeTasks, filled in from 2a/2b's documented no-op,
// withdraws every open task's handle on revocation (ADR-W039 §3.7).
//
// The withdrawal is proven two ways, because Invoke and every other
// method disagree on how a refusal travels (§4 item 5): Manifest
// surfaces the handle refusal as an ordinary error (ErrHandleRefused
// in-process, 403 over HTTP), while Invoke folds the identical refusal
// into an ordinary `denied` Outcome in a 200 body — both in-process and
// over HTTP, including under a FRESH, otherwise-valid bearer minted for
// the same Agent afterwards, which shows what revocation withdrew was
// the handle itself, not merely the credential that once held it.
func TestRevokingFencesOpenTasksAndWithdrawsTheHandle(t *testing.T) {
	h, ledger := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	first := rotate(t, h, "alfred", nil)
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	_, handle := openAgentTask(t, h.s.DB, ledger, b)

	// Sanity: the handle works, over HTTP, before revocation.
	var before agentapi.Manifest
	if res := h.doToken(first.Secret, http.MethodPost, "/api/v1/agent/manifest",
		map[string]any{"handle": string(handle)}, &before); res.StatusCode != http.StatusOK {
		t.Fatalf("manifest before revoke = %d", res.StatusCode)
	}

	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/alfred/runtime/revoke", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", res.StatusCode)
	}

	// In-process, under no bearer's authority at all: FenceTask's own
	// withdrawal is what refuses this.
	if _, err := h.s.AgentGateway.Manifest(context.Background(), handle); !errors.Is(err, butlerstore.ErrHandleRefused) {
		t.Errorf("in-process manifest on a withdrawn handle = %v, want ErrHandleRefused", err)
	}
	invokeResult, err := h.s.AgentGateway.Invoke(context.Background(), agentapi.InvokeRequest{Handle: handle, Tool: "peek"})
	if err != nil {
		t.Fatalf("in-process invoke on a withdrawn handle returned an error rather than a denial: %v", err)
	}
	if invokeResult.Outcome != agentapi.OutcomeDenied {
		t.Errorf("in-process invoke on a withdrawn handle = %q, want denied", invokeResult.Outcome)
	}

	// Re-registering mints a fresh, valid credential for the SAME
	// Agent; the old task's handle is still refused under it.
	second := rotate(t, h, "alfred", nil)
	if res := h.doToken(second.Secret, http.MethodPost, "/api/v1/agent/manifest",
		map[string]any{"handle": string(handle)}, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("manifest on the withdrawn handle under a fresh credential = %d, want 403", res.StatusCode)
	}
	var afterInvoke agentapi.InvokeResult
	if res := h.doToken(second.Secret, http.MethodPost, "/api/v1/agent/invoke",
		map[string]any{"handle": string(handle), "tool": "peek"}, &afterInvoke); res.StatusCode != http.StatusOK || afterInvoke.Outcome != agentapi.OutcomeDenied {
		t.Errorf("invoke on the withdrawn handle under a fresh credential = %d %+v, want 200 denied", res.StatusCode, afterInvoke)
	}
}

// TestReconnectingRuntimeReOffersItsWaitingTasks is health-driven
// waiting's other half (ADR-W039 §3.7, Phase 4, #340): a task already
// parked in waiting(runtime-offline) gets a fresh handle and a `turn`
// delivery the moment its runtime's health returns to connected — the
// same arrival that already flips unregistered/offline to connected.
func TestReconnectingRuntimeReOffersItsWaitingTasks(t *testing.T) {
	h, ledger := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/alfred/runtime", map[string]any{
		"type": "hermes", "endpoint": "https://hermes-alfred.internal/",
	}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("PUT runtime = %d", res.StatusCode)
	}
	cred := rotate(t, h, "alfred", nil)
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := openAgentTask(t, h.s.DB, ledger, b)
	if err := ledger.Running(taskID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Wait(taskID, string(agentapi.WorkWaitRuntimeOffline), "the runtime is offline"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthOffline); err != nil {
		t.Fatal(err)
	}

	before, err := h.s.DB.ListAgentDeliveries(b.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("deliveries before reconnect = %+v, want none", before)
	}

	// The reconnect: any authenticated call touches arrival.
	if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/heartbeat",
		map[string]any{"at": time.Now().UTC().Format(time.RFC3339)}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat = %d", res.StatusCode)
	}

	after := readRuntime(t, h, "alfred")
	if after.Health != agentapi.HealthConnected {
		t.Fatalf("health after reconnect = %q, want connected", after.Health)
	}

	deliveries, err := h.s.DB.ListAgentDeliveries(b.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Kind != agentapi.DeliveryTurn {
		t.Fatalf("deliveries after reconnect = %+v, want exactly one turn delivery", deliveries)
	}
	var payload struct {
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal([]byte(deliveries[0].Payload), &payload); err != nil {
		t.Fatalf("delivery payload did not decode: %v", err)
	}
	if payload.Handle == "" {
		t.Error("the re-offered delivery carries no handle")
	}

	// The fresh handle actually works — the runtime that follows it up
	// can resume the task rather than being handed a dead reference.
	var manifest agentapi.Manifest
	if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/manifest",
		map[string]any{"handle": payload.Handle}, &manifest); res.StatusCode != http.StatusOK {
		t.Fatalf("manifest on the re-offered handle = %d", res.StatusCode)
	}

	// A second reconnect (already connected) must not re-offer again:
	// only the arrival that actually flips health does.
	if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/heartbeat",
		map[string]any{"at": time.Now().UTC().Format(time.RFC3339)}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("second heartbeat = %d", res.StatusCode)
	}
	deliveries, err = h.s.DB.ListAgentDeliveries(b.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries after a second, already-connected heartbeat = %+v, want still exactly one", deliveries)
	}
}
