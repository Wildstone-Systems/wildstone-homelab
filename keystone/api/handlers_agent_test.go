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
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi/conformance"
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
	specs := []provider.Tool{
		{Name: "peek", Description: "read something. It changes nothing."},
		// "remember" is not gated on readOnly: the gateway's own Remember
		// (agentgateway/memory.go) refuses household scope before it ever
		// dispatches here, and every other refusal this fixture cares
		// about (an unknown tool, a foreign handle) is exercised through
		// "peek" and "propose_change" instead.
		{Name: "remember", Description: "remember a fact. (conformance fixture: never actually persisted; see agentgateway's own memory tests for the real round trip.)"},
	}
	if !readOnly {
		specs = append(specs, provider.Tool{Name: "propose_change", Description: "propose a change for approval."})
	}
	return specs
}

func (conformanceToolset) Execute(_ context.Context, name string, _ map[string]any, _ bool) (butler.ToolOutcome, error) {
	switch name {
	case "peek":
		return butler.ToolOutcome{Content: "peek: all clear"}, nil
	case "remember":
		// A minimal, deterministic success: this fixture writes nothing
		// durable, so conformance.Target.RequireRecallHit is left false
		// when this toolset backs a conformance.Run (Recall still runs
		// against the real memory store and is scope-checked either way).
		return butler.ToolOutcome{Content: "remembered"}, nil
	case "propose_change":
		return butler.ToolOutcome{Content: "proposed", Plan: &plan.Plan{Summary: "conformance proposal"}, ResourceYAML: conformanceProposalYAML}, nil
	default:
		return butler.ToolOutcome{}, errors.New("conformanceToolset: unknown tool " + name)
	}
}

func (conformanceToolset) Capabilities(readOnly bool) butler.CapabilityManifest {
	m := butler.CapabilityManifest{AgentClass: butler.AgentClassOwner, ModelVerdict: butler.VerdictFull}
	m.Tools = append(m.Tools, butler.CapabilityTool{Name: "peek", Available: true})
	m.Tools = append(m.Tools, butler.CapabilityTool{Name: "remember", Available: true})
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

// comparableAgentScript is every response runAgentScript produces that
// is worth comparing byte for byte — exported fields, unlike a private
// bookkeeping struct, because conformance.Compare marshals it through
// plain encoding/json with no field access of its own into this
// package.
type comparableAgentScript struct {
	Manifest      agentapi.Manifest
	ReadInvoke    agentapi.InvokeResult
	ProposeInvoke agentapi.InvokeResult
	Checkpoint    agentapi.CheckpointReceipt
	Recall        []agentapi.MemoryHit
}

// runAgentScript is the ONE script both arms of
// TestInProcessAndHTTPAreByteIdentical run, now driven by whichever
// context and handle the conformance.Target it is called for supplies:
// manifest, then start (Complete needs a running task), invoke a
// read-only tool, invoke a mutating one that becomes a proposal,
// checkpoint a model step, recall, and complete with evidence. Fatal on
// any unexpected error so a divergence in ERROR behaviour — not just in
// a successful response — fails the test immediately rather than being
// silently skipped.
func runAgentScript(t *testing.T, ctx context.Context, client agentapi.Client, handle agentapi.Handle) comparableAgentScript {
	t.Helper()
	var out comparableAgentScript
	var err error

	out.Manifest, err = client.Manifest(ctx, handle)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if err := client.Start(ctx, agentapi.WorkStart{Handle: handle, Reason: "conformance"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out.ReadInvoke, err = client.Invoke(ctx, agentapi.InvokeRequest{Handle: handle, Tool: "peek"})
	if err != nil {
		t.Fatalf("Invoke(peek): %v", err)
	}
	out.ProposeInvoke, err = client.Invoke(ctx, agentapi.InvokeRequest{
		Handle: handle, Tool: "propose_change", Args: json.RawMessage(`{"target":"n1"}`),
	})
	if err != nil {
		t.Fatalf("Invoke(propose_change): %v", err)
	}
	out.Checkpoint, err = client.Checkpoint(ctx, agentapi.Checkpoint{
		Handle: handle, State: agentapi.StepProposed, Kind: agentapi.StepModel,
		Name: "round-1", Params: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	out.Recall, err = client.Recall(ctx, agentapi.RecallRequest{Handle: handle, Text: "n1"})
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

	// The two conformance.Targets share exactly the fields runAgentScript
	// needs: a Client, a context (Arm A's own carries the in-process
	// runtime principal; Arm B's authority travels in its Bearer, so a
	// plain context.Background is enough), and a Handle closure that
	// opens the one task each arm's own script runs against, capturing
	// its id for the ledger comparisons below.
	var taskA, taskB string
	targetA := conformance.Target{
		Client:  clientA,
		Context: func() context.Context { return agentgateway.WithPrincipal(context.Background(), principalName) },
		Handle: func(t *testing.T) agentapi.Handle {
			id, h := openAgentTask(t, dbA, ledgerA, butlerA)
			taskA = id
			return h
		},
	}
	targetB := conformance.Target{
		Client: clientB,
		Handle: func(t *testing.T) agentapi.Handle {
			id, h := openAgentTask(t, dbB, ledgerB, butlerB)
			taskB = id
			return h
		},
	}
	conformance.Compare(t, targetA, targetB, func(t *testing.T, target conformance.Target) any {
		return runAgentScript(t, target.Ctx(), target.Client, target.Handle(t))
	})

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

// comparableDeliveryScript is runDeliveryScript's own return value:
// exported fields, so conformance.Compare's plain encoding/json marshal
// sees them.
type comparableDeliveryScript struct {
	First              []agentapi.Delivery
	AfterFirstAck      []agentapi.Delivery
	AfterSkippingItsID []agentapi.Delivery
}

// seedRestrictedDeliveries records a proposal_outcome and a turn
// delivery for b, both over a task, session, memory and plan wired so
// runtimedb.CheckSessionPrivacy and runtimedb.CheckPlanPrivacy see them
// as local_only-restricted — the same provenance chain
// migration 0070_privacy_policy.sql's privacy_edges view already walks
// for a resumed conversation (butlerlive.privacyProvider, C10), built
// here at the delivery boundary instead: a memory sourced from the
// task's own conversation makes the task's session local_only, and
// LinkPlanSessionPrivacy makes a plan filed from that session inherit
// it (its own doc comment: "Plans generated from a conversation remain
// subject to that conversation's live disclosure policy").
func seedRestrictedDeliveries(t *testing.T, db *runtimedb.DB, ledger *butlerstore.Ledger, b runtimedb.Butler) {
	t.Helper()
	taskID, handle := openAgentTask(t, db, ledger, b)
	sessionID, err := db.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "test", ActingFor: "mika", Butler: b.Name, Surface: b.Surface,
	})
	if err != nil {
		t.Fatalf("InsertButlerSession: %v", err)
	}
	if err := db.SetButlerTaskSession(taskID, sessionID); err != nil {
		t.Fatalf("SetButlerTaskSession: %v", err)
	}
	seq, err := db.RecordMemory(runtimedb.MemoryRecord{
		Scope: runtimedb.MemoryScopePersonal, Kind: runtimedb.MemoryFact,
		Content: "the safe combination is nine four one seven",
		Owner:   "mika", Author: "mika", Butler: b.Name,
		Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household",
		SourceKind: "conversation", SourceID: taskID,
	})
	if err != nil {
		t.Fatalf("RecordMemory: %v", err)
	}
	if err := db.SetMemoryPrivacy("mika", seq, runtimedb.PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatalf("SetMemoryPrivacy: %v", err)
	}
	planID, err := db.InsertPlan("runtime-alfred-1", "mika", "a restricted plan", "low", `{}`)
	if err != nil {
		t.Fatalf("InsertPlan: %v", err)
	}
	if err := db.LinkPlanSessionPrivacy(sessionID, []string{planID}); err != nil {
		t.Fatalf("LinkPlanSessionPrivacy: %v", err)
	}
	if err := db.CheckPlanPrivacy(planID, runtimedb.ModelCloud, runtimedb.MemoryDestination{}); !errors.Is(err, runtimedb.ErrPrivacy) {
		t.Fatalf("fixture is broken: plan %s is not local_only-restricted (err=%v)", planID, err)
	}

	outcomePayload, err := json.Marshal(map[string]any{"plan": planID, "outcome": "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordAgentDelivery(b.Seq, agentapi.DeliveryProposalOutcome, string(outcomePayload)); err != nil {
		t.Fatalf("recording the restricted proposal_outcome: %v", err)
	}
	turnPayload, err := json.Marshal(map[string]any{"handle": string(handle)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordAgentDelivery(b.Seq, agentapi.DeliveryTurn, string(turnPayload)); err != nil {
		t.Fatalf("recording the restricted turn: %v", err)
	}
}

// runDeliveryScript is the ONE script both arms of
// TestDeliveryClientIsByteIdenticalInProcessAndOverHTTP run: read the
// four seeded deliveries — two the disclosure filter has nothing to
// resolve (an unfamiliar plan id, an unknown task handle) and two that
// carry a local_only-restricted plan and turn (seedRestrictedDeliveries)
// which this Agent's undeclared, fail-closed-to-cloud runtime must never
// see — check that only the latter two came back withheld, ack the
// first (twice, to prove idempotency) AND one of the withheld ones (to
// prove a redacted delivery can still be acked, ADR-W039 §7.2 Phase 5,
// #345), read what is left, then prove after= the remaining one's own
// id excludes it. ExpiresAt is zeroed before comparison — each arm is
// its own database and calls time.Now() independently — but everything
// a runtime actually reads to act (id, kind, subscription, handle,
// payload) travels into the comparison untouched.
func runDeliveryScript(t *testing.T, target conformance.Target) comparableDeliveryScript {
	t.Helper()
	ctx := target.Ctx()
	sameShape := func(d agentapi.Delivery) agentapi.Delivery { d.ExpiresAt = time.Time{}; return d }
	shapeAll := func(ds []agentapi.Delivery) []agentapi.Delivery {
		out := make([]agentapi.Delivery, len(ds))
		for i, d := range ds {
			out[i] = sameShape(d)
		}
		return out
	}
	withheld := func(t *testing.T, d agentapi.Delivery) {
		t.Helper()
		payload := string(d.Payload)
		if !strings.Contains(payload, `"withheld":true`) {
			t.Fatalf("%s delivery %s was not withheld: %+v", d.Kind, d.ID, d)
		}
		if !strings.Contains(payload, runtimedb.ErrPrivacy.Error()) {
			t.Fatalf("%s delivery %s's refusal is missing runtimedb.ErrPrivacy's wording: %+v", d.Kind, d.ID, d)
		}
		if d.Handle != "" {
			t.Fatalf("%s delivery %s still carries a handle after being withheld: %+v", d.Kind, d.ID, d)
		}
		// None of the original payload's own fields — the plan id, the
		// task handle, the outcome or the subscription — may survive
		// into the refusal: it is a marker, not a redaction of the
		// content in place.
		for _, field := range []string{`"plan"`, `"handle"`, `"outcome"`, `"subscription"`} {
			if strings.Contains(payload, field) {
				t.Fatalf("%s delivery %s's withheld payload still carries %s: %s", d.Kind, d.ID, field, payload)
			}
		}
	}

	var out comparableDeliveryScript
	got, err := target.Deliveries.Deliveries(ctx, "", 0)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("Deliveries = %+v, want 4", got)
	}
	out.First = shapeAll(got)

	// got[0] and got[1] are the pre-existing fixture rows: a
	// proposal_outcome naming a plan id that does not exist, and a
	// subscription delivery naming a task handle that does not either.
	// The filter has nothing to check either against, so both travel
	// unchanged (deliveryPrivacyCheck's own "leave it alone" rule).
	if got[0].Kind != agentapi.DeliveryProposalOutcome || string(got[0].Payload) != `{"plan":"p-1","outcome":"approved"}` {
		t.Fatalf("got[0] should be the unresolvable proposal_outcome, unchanged: %+v", got[0])
	}
	if got[1].Kind != agentapi.DeliverySubscription || string(got[1].Payload) != `{"handle":"bt-999999.secret","subscription":1}` {
		t.Fatalf("got[1] should be the unresolvable subscription, unchanged: %+v", got[1])
	}
	// got[2] and got[3] are seedRestrictedDeliveries' own proposal_outcome
	// and turn, over a plan and a task/session this Agent's runtime (no
	// declared processing class, failing closed exactly like "cloud")
	// may never see the local_only content of.
	if got[2].Kind != agentapi.DeliveryProposalOutcome {
		t.Fatalf("got[2] should be the restricted proposal_outcome: %+v", got[2])
	}
	withheld(t, got[2])
	if got[3].Kind != agentapi.DeliveryTurn {
		t.Fatalf("got[3] should be the restricted turn: %+v", got[3])
	}
	withheld(t, got[3])

	// Ack the first, then a repeat ack is a no-op.
	if err := target.Deliveries.Ack(ctx, got[0].ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := target.Deliveries.Ack(ctx, got[0].ID); err != nil {
		t.Fatalf("repeat Ack: %v", err)
	}
	// A withheld delivery is still delivered, so it must still be
	// ackable: the runtime that cannot see its payload can still move
	// its own cursor past it.
	if err := target.Deliveries.Ack(ctx, got[2].ID); err != nil {
		t.Fatalf("Ack on a withheld delivery: %v", err)
	}

	after, err := target.Deliveries.Deliveries(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("after acking two = %+v, want exactly two left", after)
	}
	out.AfterFirstAck = shapeAll(after)

	last := after[len(after)-1]
	skip, err := target.Deliveries.Deliveries(ctx, last.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(skip) != 0 {
		t.Fatalf("after= the last remaining id = %+v, want none", skip)
	}
	out.AfterSkippingItsID = shapeAll(skip)
	return out
}

// TestDeliveryClientIsByteIdenticalInProcessAndOverHTTP is
// agentapi.DeliveryClient's own conformance evidence (ADR-W039 §3.6,
// §4.1 "pull fallback"; Phase 4, #341), the same in-process/HTTP shape
// TestInProcessAndHTTPAreByteIdentical proves for agentapi.Client,
// covering the ONE method pair that interface deliberately leaves out
// (see agentapi.DeliveryClient's own doc comment for why). Timestamps
// (ExpiresAt) are excluded from the byte-identical comparison — each
// arm is its own database and calls time.Now() independently — but
// everything a runtime actually reads to act (id, kind, subscription,
// handle, payload) is compared byte for byte, and both arms' ack
// behaviour (idempotent, after= excludes) is proven identically, via
// runDeliveryScript and conformance.Compare above.
func TestDeliveryClientIsByteIdenticalInProcessAndOverHTTP(t *testing.T) {
	dbA, ledgerA, butlerA := newAgentEnv(t)
	gwA := &agentgateway.Gateway{DB: dbA, Ledger: ledgerA, Gateways: conformanceGateways(), Clock: conformanceClock}
	clientA := agentgateway.InProcess(gwA)
	deliveriesA := clientA.(agentapi.DeliveryClient)

	dbB, ledgerB, butlerB := newAgentEnv(t)
	p := &principal.Store{DB: dbB}
	stateB, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(p, dbB, stateB, nil)
	gwB := &agentgateway.Gateway{DB: dbB, Ledger: ledgerB, Gateways: conformanceGateways(), Clock: conformanceClock}
	s.AgentGateway = agentgateway.InProcess(gwB)
	s.Ledger = ledgerB
	srv := newTestHTTPServer(t, s)
	token, err := p.Mint("runtime-alfred-1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	clientB := &httpclient.Client{BaseURL: srv, Bearer: token}

	for _, env := range []struct {
		db     *runtimedb.DB
		ledger *butlerstore.Ledger
		b      runtimedb.Butler
	}{{dbA, ledgerA, butlerA}, {dbB, ledgerB, butlerB}} {
		if _, err := env.db.RecordAgentDelivery(env.b.Seq, agentapi.DeliveryProposalOutcome, `{"plan":"p-1","outcome":"approved"}`); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.RecordSubscriptionDelivery(env.b.Seq, "7", `{"handle":"bt-999999.secret","subscription":1}`); err != nil {
			t.Fatal(err)
		}
		// Neither of newAgentEnv's Agents declares a processing class
		// (undeclared fails closed, exactly like "cloud"), so a
		// local_only-restricted plan and turn recorded for it must come
		// back withheld from BOTH readers, identically.
		seedRestrictedDeliveries(t, env.db, env.ledger, env.b)
	}

	targetA := conformance.Target{
		Client: clientA, Deliveries: deliveriesA,
		Context: func() context.Context { return agentgateway.WithPrincipal(context.Background(), "runtime-alfred-1") },
	}
	targetB := conformance.Target{Client: clientB, Deliveries: clientB}
	conformance.Compare(t, targetA, targetB, func(t *testing.T, target conformance.Target) any {
		return runDeliveryScript(t, target)
	})
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
	gw := &agentgateway.Gateway{
		DB: h.s.DB, Ledger: ledger, Gateways: conformanceGateways(), Clock: conformanceClock,
		// Wired so Propose (a resource document, or
		// ProposableAgentSubscription) files for real rather than
		// returning the pre-Phase-3 "not wired" refusal — no test in
		// this file relied on that refusal, and the Events family's own
		// tests need a real Plan.
		Proposals: h.s.ProposalDeps(),
	}
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

// openAgentTaskOnConnection is openAgentTask with a real Connection
// (butler_addresses row) as its origin, the one thing Notify needs and
// openAgentTask deliberately leaves unset (its own tasks are
// Console-origin, ref 0, on purpose — every other test in this file
// wants that).
func openAgentTaskOnConnection(t *testing.T, db *runtimedb.DB, ledger *butlerstore.Ledger, b runtimedb.Butler, transport, externalID string) (string, agentapi.Handle, int64) {
	t.Helper()
	connSeq, err := db.AddButlerAddress(b.Name, transport, externalID, "", "mika")
	if err != nil {
		t.Fatalf("AddButlerAddress: %v", err)
	}
	id, err := db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate,
		OriginConnection: transport + ":" + externalID, OriginConnectionRef: connSeq,
		Objective: "conformance script", ObjectiveKind: runtimedb.ObjectiveInformational,
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
	return id, h, connSeq
}

// TestAgentNotifyReachesTheTasksOwnConnection (ADR-W039 §3.2
// Notifications, §4 item 8; Phase 4, #341): Notify queues onto the
// task's own Connection, never a room, and a task with no Connection at
// all has nowhere Notify can reach. Recall and Propose were the same
// "not implemented yet" shape through Phase 2c; Phase 3 implemented
// them, and Notify is the last of the three, so no route in this family
// answers 501 any more.
func TestAgentNotifyReachesTheTasksOwnConnection(t *testing.T) {
	h, ledger := agentEnvHarness(t)
	createAgent(t, h, "alfred")
	cred := rotate(t, h, "alfred", nil)
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}

	_, handle, connSeq := openAgentTaskOnConnection(t, h.s.DB, ledger, b, "telegram", "chat-alfred")
	if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/notify",
		map[string]any{"handle": string(handle), "text": "the bins go out tomorrow"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("notify = %d", res.StatusCode)
	}
	queued, err := h.s.DB.ChatOutboundAfter(runtimedb.Connection{Seq: connSeq}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Body != "the bins go out tomorrow" || queued[0].Target != "chat-alfred" {
		t.Fatalf("queued outbound = %+v, want exactly one message to chat-alfred", queued)
	}

	// A room never receives one, whatever the runtime asks for. The
	// room is marked on ITS OWN connection — chat_rooms is keyed by
	// (connection, conversation), and this test's helper gives every
	// external id its own connection row.
	_, roomHandle, roomConnSeq := openAgentTaskOnConnection(t, h.s.DB, ledger, b, "telegram", "chat-room")
	if err := h.s.DB.NoteChatRoom(roomConnSeq, "chat-room"); err != nil {
		t.Fatal(err)
	}
	res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/notify",
		map[string]any{"handle": string(roomHandle), "text": "should never arrive"}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("notify to a room = %d, want 403", res.StatusCode)
	}

	// A task with no Connection at all (openAgentTask's own shape) has
	// nowhere Notify can reach — refused, not silently dropped.
	_, noConnHandle := openAgentTask(t, h.s.DB, ledger, b)
	if res := h.doToken(cred.Secret, http.MethodPost, "/api/v1/agent/notify",
		map[string]any{"handle": string(noConnHandle), "text": "nowhere to go"}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("notify with no connection = %d, want 403", res.StatusCode)
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

// conformanceRunHarness builds one Agent, wired exactly as
// agentEnvHarness wires it, and returns everything a conformance.Target
// needs: the harness (for direct DB access — SeedDelivery — and for
// building an HTTP server), the ledger (for Handle), and a rotated
// runtime credential (for the HTTP target's Bearer).
func conformanceRunHarness(t *testing.T) (h *harness, ledger *butlerstore.Ledger, b runtimedb.Butler, cred runtimeCredentialResponse) {
	t.Helper()
	h, ledger = agentEnvHarness(t)
	createAgent(t, h, "alfred")
	cred = rotate(t, h, "alfred", nil)
	b, err := h.s.DB.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	return h, ledger, b, cred
}

// conformanceRunTarget is the Target both TestInProcessGatewayIsConformant
// and TestHTTPClientIsConformant fill in around their own Client and
// Deliveries: the fixture's own "peek" (read-only), "conformance-unknown-
// tool" (deliberately absent from the manifest) and "propose_change"
// (never applied — the conformanceToolset fixture has no real effect to
// apply in the first place, which is what NeverApplied reports), plus a
// SeedDelivery that writes directly to the database so
// DeliveryAckIsIdempotent has something to ack.
//
// RequireRecallHit is left at its zero value (false): conformanceToolset's
// own "remember" tool (see its own doc comment) answers success without
// persisting anything durable, so Recall legitimately finds nothing here
// — the real remember/recall round trip is agentgateway/memory_test.go's
// own proof, over a Toolset that actually wires memory.
func conformanceRunTarget(h *harness, ledger *butlerstore.Ledger, b runtimedb.Butler) conformance.Target {
	return conformance.Target{
		ReadOnlyTool: "peek",
		UnknownTool:  "conformance-unknown-tool",
		ProposeTool:  "propose_change",
		ProposeArgs:  json.RawMessage(`{"target":"n1"}`),
		NeverApplied: func(t *testing.T) bool { return true },
		Handle: func(t *testing.T) agentapi.Handle {
			_, handle := openAgentTask(t, h.s.DB, ledger, b)
			return handle
		},
		SeedDelivery: func(t *testing.T) {
			if _, err := h.s.DB.RecordAgentDelivery(b.Seq, agentapi.DeliveryProposalOutcome, `{"plan":"p-1","outcome":"approved"}`); err != nil {
				t.Fatal(err)
			}
		},
	}
}

// TestInProcessGatewayIsConformant runs the full ADR-W039 §4 conformance
// table (deliverable 1, #343) against keystone/agentgateway's own
// in-process agentapi.Client — proof that the suite itself, not only
// the hand-picked byte-identical script above, holds for it.
func TestInProcessGatewayIsConformant(t *testing.T) {
	h, ledger, b, _ := conformanceRunHarness(t)
	target := conformanceRunTarget(h, ledger, b)
	target.Client = h.s.AgentGateway
	target.Deliveries = h.s.AgentGateway.(agentapi.DeliveryClient)
	target.Context = func() context.Context {
		return agentgateway.WithPrincipal(context.Background(), "runtime-alfred-1")
	}
	conformance.Run(t, target)
}

// TestHTTPClientIsConformant runs the identical table against
// keystone/agentapi/httpclient.Client speaking to a real httptest.Server
// running api.Server — proof that the suite holds for the OTHER
// transport ADR-W040 names, exercised exactly as an external runtime
// would reach it: a bearer and net/http, nothing else.
func TestHTTPClientIsConformant(t *testing.T) {
	h, ledger, b, cred := conformanceRunHarness(t)
	srv := newTestHTTPServer(t, h.s)
	client := &httpclient.Client{BaseURL: srv, Bearer: cred.Secret}
	target := conformanceRunTarget(h, ledger, b)
	target.Client = client
	target.Deliveries = client
	conformance.Run(t, target)
}
