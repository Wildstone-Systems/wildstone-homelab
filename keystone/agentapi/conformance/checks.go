package conformance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// The named checks. Each one is traced, in its own doc comment, to the
// ADR-W039 §4 runtime-contract MUST it evidences (or, where none of the
// eleven names it exactly, to the ADR section the property comes from).
// Not every MUST has a check here: §4 items 1 (credential rotation
// survives without restart), 4 (treat results as untrusted data), 8
// (notify only through Notifications, never a household credential
// directly) and 11 (stay inside the platform network) are properties of
// how a runtime is OPERATED and DEPLOYED, not of what a single call to
// agentapi.Client returns, so no fixed script here can observe them —
// they are named in this package's own doc comment as what conformance
// with THIS suite does not, by itself, prove.

// checkManifestListsToolsWithValidSchemasAndReadOnlyHint: ADR-W039 §4
// MUST 3 ("discover capabilities from the manifest and treat it as the
// complete tool surface"). A manifest a runtime could not parse into a
// tool list at all would make MUST 3 impossible to honour, so this
// checks the shape every tool must have: a name, valid JSON (when a
// schema is given at all — an empty schema is a legitimate "no
// arguments"), and, for the target's own declared ReadOnlyTool, the
// ReadOnlyHint the invoke checks below rely on.
func checkManifestListsToolsWithValidSchemasAndReadOnlyHint(t *testing.T, target Target) {
	t.Helper()
	h := requireHandle(t, target)
	m, err := target.Client.Manifest(target.Ctx(), h)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(m.Tools) == 0 {
		t.Fatal("the manifest offers no tools at all")
	}
	for _, tool := range m.Tools {
		if tool.Name == "" {
			t.Error("a tool in the manifest has an empty name")
		}
		if len(tool.InputSchema) == 0 {
			continue
		}
		var v any
		if err := json.Unmarshal(tool.InputSchema, &v); err != nil {
			t.Errorf("tool %q's InputSchema is not valid JSON: %v", tool.Name, err)
			continue
		}
		if _, ok := v.(map[string]any); !ok {
			t.Errorf("tool %q's InputSchema is valid JSON but not a JSON-Schema object (got %T)", tool.Name, v)
		}
	}
	if target.ReadOnlyTool == "" {
		return
	}
	for _, tool := range m.Tools {
		if tool.Name != target.ReadOnlyTool {
			continue
		}
		if !tool.ReadOnlyHint {
			t.Errorf("tool %q is the target's own ReadOnlyTool but the manifest does not mark it ReadOnlyHint", tool.Name)
		}
		return
	}
	t.Errorf("the manifest does not list the target's own ReadOnlyTool %q", target.ReadOnlyTool)
}

// checkInvokeOfAReadOnlyToolSucceeds: ADR-W039 §4 MUST 3 (the manifest
// is the complete surface: a tool it lists as available must actually
// run) and MUST 5 (ok is the ordinary case every other outcome is
// compared against).
func checkInvokeOfAReadOnlyToolSucceeds(t *testing.T, target Target) {
	t.Helper()
	if target.ReadOnlyTool == "" {
		t.Skip("target names no ReadOnlyTool")
	}
	h := requireHandle(t, target)
	res, err := target.Client.Invoke(target.Ctx(), agentapi.InvokeRequest{
		Handle: h, Tool: target.ReadOnlyTool, Args: target.ReadOnlyArgs,
	})
	if err != nil {
		t.Fatalf("Invoke(%s): %v", target.ReadOnlyTool, err)
	}
	if res.Outcome != agentapi.OutcomeOK {
		t.Errorf("invoking the read-only tool %q = %q (%s), want ok", target.ReadOnlyTool, res.Outcome, res.Reason)
	}
}

// checkInvokeOfAnUnknownToolIsNeverASilentSuccess: ADR-W039 §4 MUST 5
// ("accept denied, waiting(...) ... as ordinary outcomes and stop,
// rather than retrying a consequential effect blindly"). The ADR's own
// list is written for a runtime deciding how to react to what it gets
// back; the property a conformance suite can pin from the OTHER side is
// narrower but still real: an unrecognised tool name must never be
// treated as ok (nothing was run, so nothing should look like it
// succeeded), and it must never surface as a transport-level error
// either — Invoke's own contract is that a refusal is an answer, not a
// protocol failure (agentapi.Client's own doc comment on Invoke). Which
// Outcome exactly (denied, rejected, or a Toolset's own dispatch error
// reported as OutcomeError) is left to the target: Wildstone's own
// ToolGateway today answers OutcomeError for a structurally-absent tool
// (butler/gateway.go's dispatch, "Structurally absent... says the same
// thing [as the resolver would]"), which is still an ordinary Outcome in
// a nil-error response, never a blind retry target.
func checkInvokeOfAnUnknownToolIsNeverASilentSuccess(t *testing.T, target Target) {
	t.Helper()
	h := requireHandle(t, target)
	name := target.UnknownTool
	if name == "" {
		name = "conformance-tool-that-does-not-exist"
	}
	res, err := target.Client.Invoke(target.Ctx(), agentapi.InvokeRequest{Handle: h, Tool: name})
	if err != nil {
		t.Fatalf("Invoke(%q) returned a transport error rather than an ordinary Outcome (ADR-W039 §4 MUST 5): %v", name, err)
	}
	if !res.Outcome.Valid() {
		t.Errorf("invoking the unknown tool %q returned an invalid outcome %q", name, res.Outcome)
	}
	if res.Outcome == agentapi.OutcomeOK {
		t.Errorf("invoking the unknown tool %q = ok; an unrecognised tool must never silently succeed", name)
	}
}

// forgedHandle returns a syntactically plausible handle this package
// invented — a task id no target ever issued, and 32 random bytes for
// the secret half — good for proving a foreign/forged handle is refused
// without needing a second real Agent wired up by the caller.
func forgedHandle() agentapi.Handle {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return agentapi.Handle("conformance-forged-task-" + hex.EncodeToString(secret[:4]) + "." + hex.EncodeToString(secret))
}

// assertNoSecretLeak fails t if text contains h's own secret half —
// ADR-W040 §7's "a Handle is <taskID>.<secret>, and its String()
// redacts the secret half; every log line and audit row prints the
// handle through that method, so a leak never yields a working
// credential", checked here against what a real refusal actually put in
// a response rather than just against the redacting method existing.
func assertNoSecretLeak(t *testing.T, h agentapi.Handle, text string) {
	t.Helper()
	if text == "" {
		return
	}
	_, secret, ok := strings.Cut(string(h), ".")
	if !ok || secret == "" {
		return
	}
	if strings.Contains(text, secret) {
		t.Errorf("the handle's secret leaked into a response: %q", text)
	}
}

// checkInvokeWithAForgedHandleIsDenied: ADR-W039 §4 MUST 2 ("act only as
// the one Agent its credential is bound to; carry the Wildstone-issued
// task handle on every capability... call") — the other side of that
// sentence is what Wildstone must do with a handle it never issued: it
// is refused, and the refusal never hands back the very secret it is
// refusing.
func checkInvokeWithAForgedHandleIsDenied(t *testing.T, target Target) {
	t.Helper()
	h := forgedHandle()
	tool := target.ReadOnlyTool
	if tool == "" {
		tool = target.UnknownTool
	}
	if tool == "" {
		tool = "conformance-probe"
	}
	res, err := target.Client.Invoke(target.Ctx(), agentapi.InvokeRequest{Handle: h, Tool: tool})
	if err != nil {
		// A transport-level refusal (e.g. an HTTP 403 before Invoke's own
		// outcome-folding ever runs) is an acceptable answer too — but its
		// message must still never carry the forged handle's secret half.
		assertNoSecretLeak(t, h, err.Error())
		return
	}
	if res.Outcome != agentapi.OutcomeDenied {
		t.Errorf("invoking with a forged handle = %q, want denied", res.Outcome)
	}
	assertNoSecretLeak(t, h, res.Reason)
	assertNoSecretLeak(t, h, res.Content)
}

// checkProposeChangeFilesAProposalAndNeverApplies: ADR-W039 §3.2
// Proposals and §4 MUST 5's "ok" case — propose_change is the one call
// whose ordinary success still must not have done the thing it
// describes: "the Plan is filed as proposed... and never applied here".
func checkProposeChangeFilesAProposalAndNeverApplies(t *testing.T, target Target) {
	t.Helper()
	if target.ProposeTool == "" {
		t.Skip("target names no ProposeTool")
	}
	if target.NeverApplied == nil {
		t.Skip("target names no NeverApplied probe")
	}
	h := requireHandle(t, target)
	res, err := target.Client.Invoke(target.Ctx(), agentapi.InvokeRequest{
		Handle: h, Tool: target.ProposeTool, Args: target.ProposeArgs,
	})
	if err != nil {
		t.Fatalf("Invoke(%s): %v", target.ProposeTool, err)
	}
	if res.Outcome != agentapi.OutcomeOK {
		t.Fatalf("invoking %s = %q (%s), want ok", target.ProposeTool, res.Outcome, res.Reason)
	}
	if res.Proposal == nil || res.Proposal.ID == "" {
		t.Errorf("invoking %s did not return a Proposal reference", target.ProposeTool)
	}
	if !target.NeverApplied(t) {
		t.Errorf("invoking %s appears to have applied its change directly; a propose_change tool must only ever file a Plan for Owner approval, never apply it (ADR-W039 §3.2)", target.ProposeTool)
	}
}

// checkCheckpointRoundTripReturnsAReceipt: ADR-W039 §4 MUST 7 ("report
// its run lifecycle for a task... and each capability call it believes
// it made") — a runtime's report is only worth comparing in audit if
// Wildstone hands back something to compare it against.
func checkCheckpointRoundTripReturnsAReceipt(t *testing.T, target Target) {
	t.Helper()
	h := requireHandle(t, target)
	receipt, err := target.Client.Checkpoint(target.Ctx(), agentapi.Checkpoint{
		Handle: h, State: agentapi.StepProposed, Kind: agentapi.StepModel,
		Name: "conformance-checkpoint", Params: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if receipt.Step == 0 {
		t.Error("the checkpoint receipt carries no step sequence")
	}
}

// checkStartWaitCompleteLifecycle: ADR-W039 §4 MUST 7 ("started, ended
// with outcome"), exercising the whole lifecycle a runtime reports
// through, including a wait-then-resume in the middle — the shape a
// real turn takes far more often than start-then-immediately-complete.
func checkStartWaitCompleteLifecycle(t *testing.T, target Target) {
	t.Helper()
	h := requireHandle(t, target)
	ctx := target.Ctx()
	if err := target.Client.Start(ctx, agentapi.WorkStart{Handle: h, Reason: "conformance"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := target.Client.Wait(ctx, agentapi.WorkWait{
		Handle: h, Reason: agentapi.WorkWaitClarification, Detail: "conformance",
	}); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := target.Client.Start(ctx, agentapi.WorkStart{Handle: h, Reason: "conformance-resume"}); err != nil {
		t.Fatalf("Start after Wait: %v", err)
	}
	if err := target.Client.Complete(ctx, agentapi.WorkComplete{
		Handle: h, Summary: "the conformance lifecycle check ran to completion",
		Evidence: []agentapi.Evidence{{Kind: "observation", Description: "the lifecycle check ran"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// checkHeartbeatSucceeds: ADR-W039 §4 MUST 10 ("answer a health probe")
// — the nearest thing agentapi.Client has to that MUST is the call a
// runtime uses to report its own liveness, which Wildstone's health
// determination is built from (Heartbeat's own doc comment: "health is
// Wildstone's own determination from when the last one arrived").
func checkHeartbeatSucceeds(t *testing.T, target Target) {
	t.Helper()
	if err := target.Client.Heartbeat(target.Ctx(), agentapi.Heartbeat{At: time.Now().UTC(), Version: "conformance/1"}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}

// checkRecallReturnsHitsWithValidScopes: ADR-W039 §4 MUST 9 ("write
// nothing into Wildstone memory except through remember") — the round
// trip this pins is that what a runtime remembers can be found again
// through recall, carrying one of the closed set of scopes a runtime
// must never assert for itself, only ever read back.
func checkRecallReturnsHitsWithValidScopes(t *testing.T, target Target) {
	t.Helper()
	h := requireHandle(t, target)
	ctx := target.Ctx()
	content := target.RememberContent
	if content == "" {
		content = fmt.Sprintf("conformance fact %d", time.Now().UnixNano())
	}
	if err := target.Client.Remember(ctx, agentapi.RememberRequest{
		Handle: h, Kind: agentapi.MemoryFact, Subject: target.RememberSubject, Content: content,
	}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	hits, err := target.Client.Recall(ctx, agentapi.RecallRequest{Handle: h, Text: content})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if target.RequireRecallHit && len(hits) == 0 {
		t.Fatal("Recall found nothing for what was just remembered")
	}
	for _, hit := range hits {
		if !hit.Scope.Valid() {
			t.Errorf("a recall hit carries an invalid scope %q", hit.Scope)
		}
		if hit.Kind != "" && !hit.Kind.Valid() {
			t.Errorf("a recall hit carries an invalid kind %q", hit.Kind)
		}
	}
}

// checkRememberRefusesHouseholdScope: ADR-W039 §4 MUST 9's other half —
// "keep its own memory local and never treat it as authority" is the
// runtime's obligation; Wildstone's is the one this pins, §3.5 rule 1:
// "the scope may be personal or agent_local and never household...
// promotion to Household is a Person-approved control", never something
// a runtime's own remember call can reach for itself.
func checkRememberRefusesHouseholdScope(t *testing.T, target Target) {
	t.Helper()
	h := requireHandle(t, target)
	err := target.Client.Remember(target.Ctx(), agentapi.RememberRequest{
		Handle: h, Scope: agentapi.MemoryScopeHousehold, Kind: agentapi.MemoryFact,
		Content: "conformance household attempt",
	})
	if err == nil {
		t.Error("Remember with household scope succeeded; ADR-W039 §3.5 rule 1 says promotion to household is a Person-approved control, never a runtime's own remember")
	}
}

// checkDeliveriesLongPollReturnsThenBlocks: ADR-W039 §4 MUST 6 ("accept
// deliveries (push) or poll (pull) and acknowledge each one") —
// DeliveryClient's own long-poll half. An immediate wait=0 call returns
// right away; a longer wait with nothing new to deliver blocks close to
// that bound rather than returning instantly (ADR-W040 §4: "an empty
// response with no error is a quiet period, not an error, and the
// runtime asks again").
func checkDeliveriesLongPollReturnsThenBlocks(t *testing.T, target Target) {
	t.Helper()
	if target.Deliveries == nil {
		t.Skip("target carries no DeliveryClient")
	}
	ctx := target.Ctx()
	start := time.Now()
	if _, err := target.Deliveries.Deliveries(ctx, "", 0); err != nil {
		t.Fatalf("Deliveries(wait=0): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Deliveries with wait=0 took %s, want an immediate return", elapsed)
	}

	const wait = 400 * time.Millisecond
	start = time.Now()
	got, err := target.Deliveries.Deliveries(ctx, "", wait)
	if err != nil {
		t.Fatalf("Deliveries(wait=%s): %v", wait, err)
	}
	if elapsed := time.Since(start); len(got) == 0 && elapsed < wait/2 {
		t.Errorf("an empty long-poll returned after %s, want it to block for close to %s", elapsed, wait)
	}
}

// checkDeliveryAckIsIdempotent: ADR-W039 §4 MUST 6's other half —
// "tolerate redelivery by idempotency key" is the runtime's obligation;
// Wildstone's, pinned here, is DeliveryClient.Ack's own doc comment:
// "Idempotent: acking an already-acked id a second time succeeds
// without effect."
func checkDeliveryAckIsIdempotent(t *testing.T, target Target) {
	t.Helper()
	if target.Deliveries == nil {
		t.Skip("target carries no DeliveryClient")
	}
	if target.SeedDelivery == nil {
		t.Skip("target names no SeedDelivery; nothing to ack")
	}
	ctx := target.Ctx()
	target.SeedDelivery(t)
	got, err := target.Deliveries.Deliveries(ctx, "", 0)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("SeedDelivery ran but the target still has no pending delivery to ack")
	}
	id := got[0].ID
	if err := target.Deliveries.Ack(ctx, id); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := target.Deliveries.Ack(ctx, id); err != nil {
		t.Fatalf("a repeat Ack of the same id must be idempotent, got: %v", err)
	}
}

// namedCheck is one row of the table Run walks: a subtest name (also
// the Skip map's key), the ADR-W039 §4 MUST it evidences, and the check
// itself.
type namedCheck struct {
	name  string
	must  string
	check func(t *testing.T, target Target)
}

// checks is the whole conformance table, in the order Run runs it. The
// order matters a little: propose_change runs before the lifecycle
// check completes its own task, and the delivery checks run in the
// sequence that lets DeliveryAckIsIdempotent find nothing left over
// from DeliveriesLongPollReturnsThenBlocks (which seeds no delivery of
// its own) to ack by accident.
var checks = []namedCheck{
	{"ManifestListsToolsWithValidSchemasAndReadOnlyHint", "MUST 3", checkManifestListsToolsWithValidSchemasAndReadOnlyHint},
	{"InvokeOfAReadOnlyToolSucceeds", "MUST 3, 5", checkInvokeOfAReadOnlyToolSucceeds},
	{"InvokeOfAnUnknownToolIsNeverASilentSuccess", "MUST 5", checkInvokeOfAnUnknownToolIsNeverASilentSuccess},
	{"InvokeWithAForgedHandleIsDenied", "MUST 2", checkInvokeWithAForgedHandleIsDenied},
	{"ProposeChangeFilesAProposalAndNeverApplies", "§3.2 Proposals; MUST 5", checkProposeChangeFilesAProposalAndNeverApplies},
	{"CheckpointRoundTripReturnsAReceipt", "MUST 7", checkCheckpointRoundTripReturnsAReceipt},
	{"StartWaitCompleteLifecycle", "MUST 7", checkStartWaitCompleteLifecycle},
	{"HeartbeatSucceeds", "MUST 10", checkHeartbeatSucceeds},
	{"RecallReturnsHitsWithValidScopes", "MUST 9", checkRecallReturnsHitsWithValidScopes},
	{"RememberRefusesHouseholdScope", "MUST 9", checkRememberRefusesHouseholdScope},
	{"DeliveriesLongPollReturnsThenBlocks", "MUST 6", checkDeliveriesLongPollReturnsThenBlocks},
	{"DeliveryAckIsIdempotent", "MUST 6", checkDeliveryAckIsIdempotent},
}

// Run exercises target against every named check above, each as its own
// subtest (so `go test -run TestX/CheckName` isolates one), skipping —
// with the reason on record — a check whose Target field it needs is
// left unset, or whose name appears in target.Skip.
func Run(t *testing.T, target Target) {
	t.Helper()
	if target.Client == nil {
		t.Fatal("conformance.Run needs a Target.Client")
	}
	if target.Handle == nil {
		t.Fatal("conformance.Run needs a Target.Handle")
	}
	for _, c := range checks {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			t.Logf("ADR-W039 %s", c.must)
			if reason, skip := target.Skip[c.name]; skip {
				t.Skip(reason)
			}
			c.check(t, target)
		})
	}
}
