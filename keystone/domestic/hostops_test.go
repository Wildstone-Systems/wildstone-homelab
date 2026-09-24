package domestic

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// --- (a) the menu is closed ---

func TestHostOpsExposesExactlyTheClosedMenu(t *testing.T) {
	h := &HostOps{}
	names := make([]string, 0)
	for _, e := range h.Executors() {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{
		"host.container_logs",
		"host.container_restart",
		"host.container_stats",
		"host.disk_usage",
		"host.prune_images",
		"host.reconcile_poke",
		"host.unit_status",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("HostOps.Executors() = %v, want exactly %v", names, want)
	}
	// Every declared tier must agree with the tier the executor actually
	// carries when dispatched (read vs confirm), and the declaration is
	// the second, independent copy of that fact (ADR-W024 A5).
	wantTier := map[string]string{
		"container_logs":    TierRead,
		"container_stats":   TierRead,
		"disk_usage":        TierRead,
		"unit_status":       TierRead,
		"container_restart": TierConfirm,
		"prune_images":      TierConfirm,
		"reconcile_poke":    TierConfirm,
	}
	if len(HostOpsAgentTools.Tools) != len(wantTier) {
		t.Fatalf("HostOpsAgentTools.Tools has %d entries, want %d", len(HostOpsAgentTools.Tools), len(wantTier))
	}
	for tool, tier := range wantTier {
		if got := HostOpsAgentTools.Tools[tool]; got != tier {
			t.Fatalf("HostOpsAgentTools.Tools[%q] = %q, want %q", tool, got, tier)
		}
		if !HostOpsAgentTools.OptIn[tool] {
			t.Fatalf("HostOpsAgentTools.OptIn[%q] is not set: the whole family must be opt-in (ADR-W033 §6.2)", tool)
		}
	}
}

// --- (e) no Exec(string)-style op is reachable ---

// TestHostNodeHasNoExecStyleMethod pins HostNode's method set the way
// proto's own generated comment pins the Wardstone operation surface: "a
// CLOSED ONEOF. There is no Exec(string)". This interface is narrower —
// six named methods, never a generic Operation — and this test fails the
// moment a future change adds a method shaped like a raw command: a
// single free-form string parameter with no other identifying
// parameters, or any parameter/return that is a []byte (a command line,
// a script, a blob of bytes to run).
func TestHostNodeHasNoExecStyleMethod(t *testing.T) {
	typ := reflect.TypeOf((*HostNode)(nil)).Elem()
	wantMethods := []string{
		"ContainerLogs", "ContainerStats", "DiskUsage", "UnitStatus", "StopContainer", "PruneImages",
	}
	if typ.NumMethod() != len(wantMethods) {
		names := make([]string, typ.NumMethod())
		for i := range names {
			names[i] = typ.Method(i).Name
		}
		t.Fatalf("HostNode has %d methods %v, want exactly %v", typ.NumMethod(), names, wantMethods)
	}
	for _, name := range wantMethods {
		m, ok := typ.MethodByName(name)
		if !ok {
			t.Fatalf("HostNode is missing %s", name)
		}
		if strings.Contains(strings.ToLower(name), "exec") {
			t.Fatalf("HostNode declares %s, which looks like a raw-command op — ground rule 2 forbids it", name)
		}
		sig := m.Type
		// Every parameter after ctx must be a plain identifier — string,
		// int, or a context.Context — never a []byte (a command, a
		// script, an argv) and never a struct that could carry one.
		for i := 0; i < sig.NumIn(); i++ {
			p := sig.In(i)
			switch p.Kind() {
			case reflect.String, reflect.Int, reflect.Interface: // interface{} covers context.Context here
				continue
			default:
				t.Fatalf("HostNode.%s takes a %s parameter — only plain identifiers (string/int) and "+
					"context.Context are allowed; anything else is a hole a raw command could travel "+
					"through", name, p.Kind())
			}
		}
		// Same for the returns, minus the trailing error.
		for i := 0; i < sig.NumOut()-1; i++ {
			r := sig.Out(i)
			switch r.Kind() {
			case reflect.String, reflect.Bool, reflect.Uint64, reflect.Uint32, reflect.Int:
				continue
			default:
				t.Fatalf("HostNode.%s returns a %s — only plain scalars are allowed", name, r.Kind())
			}
		}
	}
}

// --- (f) the executor never receives node secrets ---

// TestHostOpsCarriesNoSecretField is the structural pin for "the
// executor never receives node secrets" on THIS family. There is no
// per-node credential in this package at all: authentication to a node
// happens entirely inside *gateway.Gateway (mTLS, below this file), and
// HostNode's methods (pinned above) take nothing but a node id, a name
// and a count — nowhere for a token, password or key to travel through
// even if a caller tried to put one in Call.Params.
func TestHostOpsCarriesNoSecretField(t *testing.T) {
	suspicious := []string{"token", "password", "secret", "key", "credential"}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(HostOps{}),
		reflect.TypeOf(GatewayHostNode{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			for _, s := range suspicious {
				if strings.Contains(name, s) {
					t.Fatalf("%s.%s looks like a secret field — host.* must never hold node credentials",
						typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}

// --- fakes for the execution tests ---

type fakeHostNode struct {
	logsCalls, statsCalls, diskCalls, unitCalls, stopCalls, pruneCalls int

	logsPresent             bool
	logs                    string
	statsPresent            bool
	cpuNs, mem, memLimit    uint64
	totalBytes, fileCount   uint64
	active, sub, load       string
	stopPresent, wasRunning bool
	reclaimed               uint64
	deleted                 uint32
	err                     error
}

func (f *fakeHostNode) ContainerLogs(ctx context.Context, node, name string, tail int) (bool, string, error) {
	f.logsCalls++
	return f.logsPresent, f.logs, f.err
}
func (f *fakeHostNode) ContainerStats(ctx context.Context, node, name string) (bool, uint64, uint64, uint64, error) {
	f.statsCalls++
	return f.statsPresent, f.cpuNs, f.mem, f.memLimit, f.err
}
func (f *fakeHostNode) DiskUsage(ctx context.Context, node, path string) (uint64, uint64, error) {
	f.diskCalls++
	return f.totalBytes, f.fileCount, f.err
}
func (f *fakeHostNode) UnitStatus(ctx context.Context, node, unit string) (string, string, string, error) {
	f.unitCalls++
	return f.active, f.sub, f.load, f.err
}
func (f *fakeHostNode) StopContainer(ctx context.Context, node, name string) (bool, bool, error) {
	f.stopCalls++
	return f.stopPresent, f.wasRunning, f.err
}
func (f *fakeHostNode) PruneImages(ctx context.Context, node string) (uint64, uint32, error) {
	f.pruneCalls++
	return f.reclaimed, f.deleted, f.err
}

func (f *fakeHostNode) totalCalls() int {
	return f.logsCalls + f.statsCalls + f.diskCalls + f.unitCalls + f.stopCalls + f.pruneCalls
}

// hostGateway builds a domestic Gateway with the host.* family
// registered and declared, an audit recorder, and the given Confirm
// behaviour — the same shape cmd/keystone/main.go wires, minus the
// runtime DB.
func hostGateway(node HostNode, confirm func(ctx context.Context, req ConfirmRequest) (Confirmation, error)) (*Gateway, *[]Call, *[]error) {
	ops := &HostOps{Authority: permittedExecution{}, Node: node, ReconcilePoke: func() {}}
	g := &Gateway{
		Authority: permittedExecution{},
		Declared:  DeclaredFromPackages([]AgentToolSource{HostOpsAgentTools}),
		Confirm:   confirm,
	}
	var audited []Call
	var audErrs []error
	g.Audit = func(c Call, r Result, err error) {
		audited = append(audited, c)
		audErrs = append(audErrs, err)
	}
	for _, e := range ops.Executors() {
		g.Register(e)
	}
	return g, &audited, &audErrs
}

// --- (c) a read op executes via a fake node and is audited ---

func TestHostOpsReadExecutesViaFakeNodeAndIsAudited(t *testing.T) {
	node := &fakeHostNode{logsPresent: true, logs: "line one\nline two"}
	g, audited, audErrs := hostGateway(node, nil)

	res, err := g.Dispatch(context.Background(), Call{
		Tool: "host.container_logs", Tier: TierRead,
		Params: map[string]string{"node": "node-a", "container": "immich-server"},
		Rule:   "operator-session", Subject: "mika", Attended: true,
	})
	if err != nil {
		t.Fatalf("host.container_logs: %v", err)
	}
	if node.logsCalls != 1 {
		t.Fatalf("ContainerLogs called %d times, want 1", node.logsCalls)
	}
	if !strings.Contains(res.Summary, "line one") {
		t.Fatalf("Result.Summary = %q, want it to carry the log content", res.Summary)
	}
	if len(*audited) != 1 {
		t.Fatalf("audit recorded %d calls, want 1", len(*audited))
	}
	if (*audited)[0].Tool != "host.container_logs" || (*audited)[0].Tier != TierRead {
		t.Fatalf("audited call = %+v, want tool=host.container_logs tier=read", (*audited)[0])
	}
	if (*audErrs)[0] != nil {
		t.Fatalf("audited outcome recorded an error for a successful call: %v", (*audErrs)[0])
	}
}

// A denied read (unauthorized) still lands in the audit stream — "every
// call, allowed or denied" (H7 deliverable 3). Denial here is modelled
// the way domestic.Gateway itself denies: an unattended confirm-tier
// call, or a Declared mismatch. The read tier has no such axis, so this
// exercises the DECLARED-mismatch refusal instead, which any tool can
// hit and which the audit hook sees exactly like a success.
func TestHostOpsDeniedCallIsStillAudited(t *testing.T) {
	node := &fakeHostNode{}
	g, audited, audErrs := hostGateway(node, nil)

	_, err := g.Dispatch(context.Background(), Call{
		// Declares as TierConfirm in HostOpsAgentTools; asking for read
		// here is the caller-vs-declaration mismatch the gateway itself
		// checks (domestic.go's dispatch), independent of confirmation.
		Tool: "host.container_restart", Tier: TierRead,
		Params: map[string]string{"node": "node-a", "container": "immich-server"},
	})
	if err == nil {
		t.Fatal("expected a refusal for a tier that does not match the declaration")
	}
	if node.stopCalls != 0 {
		t.Fatalf("StopContainer was called on a refused dispatch")
	}
	if len(*audited) != 1 {
		t.Fatalf("a denied call was not audited: %d records", len(*audited))
	}
	if (*audErrs)[0] == nil {
		t.Fatal("a denied call's audit record carries no error")
	}
}

// --- (d) a mutating op without approval never calls the node ---

func TestHostOpsMutatingWithoutApprovalNeverCallsNodeAndWaits(t *testing.T) {
	node := &fakeHostNode{stopPresent: true}
	// Confirm reports "already asked, not yet answered" — the shape a
	// Person sees before they say yes (ADR-W024 A5.1).
	confirm := func(ctx context.Context, req ConfirmRequest) (Confirmation, error) {
		return ConfirmPending, nil
	}
	g, _, _ := hostGateway(node, confirm)

	_, err := g.Dispatch(context.Background(), Call{
		Tool: "host.container_restart", Tier: TierConfirm,
		Params:   map[string]string{"node": "node-a", "container": "immich-server"},
		Subject:  "mika",
		Attended: true,
	})
	if err == nil {
		t.Fatal("expected a waiting/refusal error, got nil")
	}
	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("expected a RefusalError (waiting on confirmation), got %T: %v", err, err)
	}
	if !strings.Contains(refusal.Reason, "waiting") && !strings.Contains(refusal.Reason, "confirm") {
		t.Fatalf("refusal reason %q does not read as an approval-pending answer", refusal.Reason)
	}
	if node.stopCalls != 0 {
		t.Fatalf("StopContainer was called %d times before approval — it must never be called", node.stopCalls)
	}

	// An unattended call (a routine, not a person) cannot even ask: the
	// same tier is refused outright rather than left pending forever.
	_, err = g.Dispatch(context.Background(), Call{
		Tool: "host.prune_images", Tier: TierConfirm,
		Params:   map[string]string{"node": "node-a"},
		Attended: false,
	})
	if err == nil {
		t.Fatal("expected an unattended confirm-tier call to be refused")
	}
	if node.pruneCalls != 0 {
		t.Fatalf("PruneImages was called %d times on an unattended call — it must never be called", node.pruneCalls)
	}
}

// Approval granted: the node IS called, exactly once, and the effect is
// reported confirmed.
func TestHostOpsMutatingWithApprovalCallsNodeOnce(t *testing.T) {
	node := &fakeHostNode{stopPresent: true, wasRunning: true}
	confirm := func(ctx context.Context, req ConfirmRequest) (Confirmation, error) {
		return ConfirmApproved, nil
	}
	g, audited, _ := hostGateway(node, confirm)

	res, err := g.Dispatch(context.Background(), Call{
		Tool: "host.container_restart", Tier: TierConfirm,
		Params:   map[string]string{"node": "node-a", "container": "immich-server"},
		Subject:  "mika",
		Attended: true,
	})
	if err != nil {
		t.Fatalf("host.container_restart with approval: %v", err)
	}
	if node.stopCalls != 1 {
		t.Fatalf("StopContainer called %d times, want exactly 1", node.stopCalls)
	}
	if !res.EffectConfirmed {
		t.Fatal("an approved, executed mutation must report EffectConfirmed")
	}
	if len(*audited) != 1 {
		t.Fatalf("audit recorded %d calls, want 1", len(*audited))
	}
}

// --- HostOps.Available / unwired deployment ---

func TestHostOpsUnavailableWithNoNodeConfigured(t *testing.T) {
	ops := &HostOps{}
	if ops.Available() {
		t.Fatal("HostOps with no Node must report Available() == false")
	}
	for _, e := range ops.Executors() {
		_, err := e.Execute(context.Background(), Call{Tool: e.Name(), Tier: TierRead})
		if err == nil {
			t.Fatalf("%s executed with no Node configured", e.Name())
		}
	}
}
