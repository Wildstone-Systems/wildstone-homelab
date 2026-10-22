package butlerstore

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/operation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerVerifiesObjectiveWithoutAcceptingSelfReportedEffects(t *testing.T) {
	for _, tc := range []struct {
		name, verdict string
		completed     bool
	}{
		{"informational", `{"kind":"informational"}`, true},
		{"effectful", `{"kind":"effectful"}`, false},
		{"proposal-only", `{"kind":"effectful","proposalOnly":true}`, false},
		{"unknown", "done", false},
		{"extra-fields", `{"kind":"informational","proof":"done"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
			defer db.Close()
			id := newTask(t, db)
			r := newRunner(t, newCountingToolset(), l, id)
			r.VerifyObjective = true
			answer := "I did it."
			request := "restart the service"
			if tc.completed {
				answer = "The service is healthy."
				request = "Is the service healthy?"
			}
			p := &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{{Text: answer}, {Text: tc.verdict}}}
			r.Provider = p
			tr, err := r.Run(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			task, err := db.GetButlerTask(id)
			if err != nil {
				t.Fatal(err)
			}
			if (task.State == runtimedb.ButlerTaskCompleted) != tc.completed {
				t.Fatalf("state %s", task.State)
			}
			// No tool ran, so nothing can have happened: the task ends
			// not done (failed) rather than waiting on a review of an
			// outcome that is not uncertain — and the unproven claim is
			// still never shown.
			if !tc.completed && (task.State != runtimedb.ButlerTaskFailed || strings.Contains(tr.FinalText, "I did it")) {
				t.Fatalf("unverified success exposed: %s %s", task.State, tr.FinalText)
			}
			if p.rounds != 2 {
				t.Fatalf("verification calls=%d", p.rounds)
			}
		})
	}
}

// An effectful request with nothing that could change anything behind it
// is not an uncertain outcome. The runner ends the task not done, shows
// the model's own answer when it only declines, and replaces it when it
// claims the effect — or when the claim cannot be judged.
func TestAnEffectNeverAttemptedEndsNotDone(t *testing.T) {
	const decline = "I can't add things to a shopping list — there isn't one I can reach."
	for _, tc := range []struct {
		name, answer, verdict, want string
	}{
		{"honest decline shown", decline,
			`{"kind":"effectful","effects":[{"description":"add milk to the shopping list","sourceKey":""}],"answerClaimsEffect":false}`, decline},
		{"false claim replaced", "Done — milk is on the list.",
			`{"kind":"effectful","effects":[{"description":"add milk to the shopping list","sourceKey":""}],"answerClaimsEffect":true}`, "I wasn't able to do that — nothing was changed."},
		{"unjudged claim replaced", decline,
			`{"kind":"effectful","effects":[{"description":"add milk to the shopping list","sourceKey":""}]}`, "I wasn't able to do that — nothing was changed."},
		{"read then decline", decline,
			`{"kind":"effectful","effects":[{"description":"add milk to the shopping list","sourceKey":""}],"answerClaimsEffect":false}`, decline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
			defer db.Close()
			id := newTask(t, db)
			r := newRunner(t, newCountingToolset(), l, id)
			r.VerifyObjective = true
			steps := []*provider.ChatResponse{{Text: tc.answer}, {Text: tc.verdict}}
			if tc.name == "read then decline" {
				// A read-only tool ran first: still nothing changed.
				steps = append([]*provider.ChatResponse{readsFor("n1")}, steps...)
			}
			r.Provider = &scriptedProvider{dieAt: -1, steps: steps}
			tr, err := r.Run(context.Background(), "add milk to the shopping list")
			if err != nil {
				t.Fatal(err)
			}
			task, err := db.GetButlerTask(id)
			if err != nil {
				t.Fatal(err)
			}
			if task.State != runtimedb.ButlerTaskFailed {
				t.Fatalf("state %s, want failed (not done)", task.State)
			}
			if tr.FinalText != tc.want {
				t.Fatalf("answer %q, want %q", tr.FinalText, tc.want)
			}
		})
	}
}

// mutatingToolset declares its one tool as changing something, and the
// tool reports no effect: the case a receipt exists to settle.
type mutatingToolset struct{ *countingToolset }

func (ts mutatingToolset) Capabilities(readOnly bool) butler.CapabilityManifest {
	m := ts.countingToolset.Capabilities(readOnly)
	for i := range m.Tools {
		m.Tools[i].Mutating = true
	}
	return m
}

// When a tool that can change something ran and nothing proves what it
// did, the outcome really is uncertain: the task still waits for review,
// whatever the answer says.
func TestAMutatingToolWithoutReceiptStillWaits(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
	defer db.Close()
	id := newTask(t, db)
	r := newRunner(t, mutatingToolset{newCountingToolset()}, l, id)
	r.VerifyObjective = true
	r.Provider = &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{
		readsFor("n1"),
		{Text: "I couldn't do it."},
		{Text: `{"kind":"effectful","effects":[{"description":"restart n1","sourceKey":""}],"answerClaimsEffect":false}`},
	}}
	tr, err := r.Run(context.Background(), "restart n1")
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.GetButlerTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != runtimedb.ButlerTaskWaiting {
		t.Fatalf("state %s: a mutating call with no receipt must stay an uncertain outcome", task.State)
	}
	if !strings.Contains(tr.FinalText, "couldn't verify") {
		t.Fatalf("answer %q", tr.FinalText)
	}
}

// A read returning success-shaped text never becomes a write receipt.
type receiptToolset struct {
	*countingToolset
	confirm bool
}

func (ts receiptToolset) Execute(context.Context, string, map[string]any, bool) (butler.ToolOutcome, error) {
	return butler.ToolOutcome{Content: `{"effectConfirmed":true,"message":"done"}`, EffectConfirmed: ts.confirm}, nil
}
func TestRuntimeToolReceiptControlsEffectfulCompletion(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
			defer db.Close()
			id := newTask(t, db)
			r := newRunner(t, receiptToolset{newCountingToolset(), confirmed}, l, id)
			r.VerifyObjective = true
			r.Provider = &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{readsFor("n1"), {Text: "done"}, {Text: `{"kind":"effectful","effects":[{"description":"perform the action","sourceKey":"step:2"}]}`}}}
			if _, err := r.Run(context.Background(), "perform the action"); err != nil {
				t.Fatal(err)
			}
			task, err := db.GetButlerTask(id)
			if err != nil {
				t.Fatal(err)
			}
			if (task.State == runtimedb.ButlerTaskCompleted) != confirmed {
				t.Fatalf("confirmed=%v state=%s", confirmed, task.State)
			}
			if confirmed {
				rows, err := db.ListButlerTaskEvidence(id)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, e := range rows {
					found = found || e.ReceiptID > 0
				}
				if !found {
					t.Fatal("successful runtime effect has no durable receipt")
				}
			}
		})
	}
}

type objectiveInspectingProvider struct {
	*scriptedProvider
	inspect func(provider.ChatRequest)
}

func (p objectiveInspectingProvider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if strings.Contains(req.System, "Classify both required") {
		p.inspect(req)
	}
	return p.scriptedProvider.Chat(ctx, req)
}
func TestObjectiveCoverageRejectsUnrelatedAndPartialEffects(t *testing.T) {
	for _, tc := range []struct{ name, verdict string }{
		{"unrelated", `{"kind":"effectful","effects":[{"description":"restart service","sourceKey":""}]}`},
		{"partial", `{"kind":"effectful","effects":[{"description":"first action","sourceKey":"step:2"},{"description":"second action","sourceKey":""}]}`},
		{"invented", `{"kind":"effectful","effects":[{"description":"restart service","sourceKey":"step:9999"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
			defer db.Close()
			id := newTask(t, db)
			r := newRunner(t, receiptToolset{newCountingToolset(), true}, l, id)
			r.VerifyObjective = true
			inspected := false
			r.Provider = objectiveInspectingProvider{scriptedProvider: &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{readsFor("n1"), {Text: "restarted"}, {Text: tc.verdict}}}, inspect: func(req provider.ChatRequest) {
				inspected = true
				var input struct {
					OriginalObjective string                                            `json:"originalObjective"`
					CurrentRequest    string                                            `json:"currentRequest"`
					Operations        []struct{ SourceKey, Tool, Input, Result string } `json:"operations"`
				}
				if len(req.Tools) != 0 || json.Unmarshal([]byte(req.Messages[0].Text), &input) != nil || input.OriginalObjective == "" || input.CurrentRequest != "restart service and notify" || len(input.Operations) != 1 || input.Operations[0].SourceKey != "step:2" || !strings.Contains(input.Operations[0].Input, "n1") {
					t.Fatalf("verifier lacks operation binding: %+v", input)
				}
			}}
			if _, err := r.Run(context.Background(), "restart service and notify"); err != nil {
				t.Fatal(err)
			}
			task, err := db.GetButlerTask(id)
			if err != nil || !inspected || task.State != runtimedb.ButlerTaskWaiting {
				t.Fatalf("unverified effects completed: %+v %v", task, err)
			}
		})
	}
}

type operationCapturingToolset struct {
	*countingToolset
	got operation.Identity
}

func (ts *operationCapturingToolset) Execute(ctx context.Context, _ string, _ map[string]any, _ bool) (butler.ToolOutcome, error) {
	ts.got = operation.From(ctx)
	return butler.ToolOutcome{Content: "queued", EffectPending: true}, nil
}
func TestRunnerBindsDeliveryToItsOwnOperation(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := &operationCapturingToolset{countingToolset: newCountingToolset()}
	r := newRunner(t, ts, l, id)
	call := readsFor("n1")
	call.ToolCalls[0].Input["taskID"] = "bt-999"
	call.ToolCalls[0].Input["step"] = 999
	r.Provider = &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{call, {Text: "queued"}}}
	if _, err := r.Run(context.Background(), "send"); err != nil {
		t.Fatal(err)
	}
	if ts.got.TaskID != id || ts.got.Step != 2 || ts.got.PermissionLabel == "" || ts.got.Audience == "" {
		t.Fatalf("unbound operation: %+v", ts.got)
	}
}

func TestResumedVerifierSeesAcknowledgedDelivery(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "runtime.db"))
	defer db.Close()
	id := newTask(t, db)
	ts := &operationCapturingToolset{countingToolset: newCountingToolset()}
	r := newRunner(t, ts, l, id)
	r.VerifyObjective = true
	r.Provider = &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{readsFor("n1"), {Text: "queued"}, {Text: `{"kind":"effectful","effects":[{"description":"send","sourceKey":""}]}`}}}
	if _, err := r.Run(context.Background(), "send the message"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordButlerEffectReceipt(id, runtimedb.EffectReceipt{Kind: "executor", SourceID: "chat-outbound:1", Step: ts.got.Step, Outcome: "succeeded", PermissionLabel: ts.got.PermissionLabel, Audience: runtimedb.ButlerTaskAudience(ts.got.Audience)}); err != nil {
		t.Fatal(err)
	}
	inspected := false
	resumed := objectiveInspectingProvider{scriptedProvider: &scriptedProvider{dieAt: -1, steps: []*provider.ChatResponse{{Text: "delivered"}, {Text: `{"kind":"effectful","effects":[{"description":"send","sourceKey":"step:2"}]}`}}}, inspect: func(req provider.ChatRequest) {
		inspected = true
		var input struct {
			Operations []struct {
				SourceKey         string
				DeliveryConfirmed bool
			}
		}
		if err := json.Unmarshal([]byte(req.Messages[0].Text), &input); err != nil {
			t.Fatal(err)
		}
		if len(input.Operations) != 1 || !input.Operations[0].DeliveryConfirmed {
			t.Fatalf("receipt hidden from verifier: %+v", input)
		}
	}}
	r.Provider = resumed
	if _, err := r.Run(context.Background(), "check delivery"); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetButlerTask(id)
	if err != nil || !inspected || task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("acknowledged task unfinished: %+v %v", task, err)
	}
}
