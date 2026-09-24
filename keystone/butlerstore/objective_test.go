package butlerstore

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
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
			if !tc.completed && (task.State != runtimedb.ButlerTaskWaiting || strings.Contains(tr.FinalText, "I did it")) {
				t.Fatalf("unverified success exposed: %s %s", task.State, tr.FinalText)
			}
			if p.rounds != 2 {
				t.Fatalf("verification calls=%d", p.rounds)
			}
		})
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
