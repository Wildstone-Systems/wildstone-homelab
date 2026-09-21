package api

import (
	"context"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"strings"
	"testing"
)

type proposalCompletingButlers struct {
	*butlerlive.Butlers
	answer butlerlive.Answer
}

func (f *proposalCompletingButlers) Serve(context.Context, butlerlive.Turn) (butlerlive.Answer, error) {
	return f.answer, nil
}
func TestPersistedProposalCompletesOnlyAProposalObjective(t *testing.T) {
	for _, mode := range []string{"proposal", "execute", "mixed"} {
		proposalOnly := mode == "proposal"
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.person("sam")
			lease, err := h.s.DB.AcquireCoordinator("proposal-test")
			if err != nil {
				t.Fatal(err)
			}
			id := failedTurnTask(t, h)
			if err = h.s.DB.TransitionButlerTask(id, runtimedb.ButlerTaskRunning, "", ""); err != nil {
				t.Fatal(err)
			}
			step := proposalProofStep(t, h, id)
			required := []runtimedb.RequiredEffect{{Description: "create proposal", SourceKey: fmt.Sprintf("step:%d", step), ProposalOnly: mode == "mixed"}}
			if mode == "mixed" {
				memoryStep, e := h.s.DB.AppendButlerTaskStep(id, runtimedb.ButlerTaskStep{Kind: runtimedb.ButlerStepTool, Name: "remember"})
				if e != nil {
					t.Fatal(e)
				}
				attempt, e := h.s.DB.StartButlerAttempt(memoryStep)
				if e != nil {
					t.Fatal(e)
				}
				if e = h.s.DB.FinishButlerAttempt(attempt.Seq, runtimedb.ButlerAttemptSucceeded, `{"effectConfirmed":true,"content":"stored"}`, ""); e != nil {
					t.Fatal(e)
				}
				if e = h.s.DB.SetButlerStepState(memoryStep, runtimedb.ButlerStepVerified); e != nil {
					t.Fatal(e)
				}
				required = append(required, runtimedb.RequiredEffect{Description: "store memory", SourceKey: fmt.Sprintf("step:%d", memoryStep)})
				// The runner's failed completion keeps the receipt but rolls back evidence links.
				ledger := butlerstore.New(h.s.DB, lease)
				if e = ledger.Complete(id, []butler.LedgerEvidence{{Kind: "tool-result", Description: "remembered", ReceiptStep: memoryStep, PermissionLabel: "sandy", Audience: string(runtimedb.ButlerAudiencePrivate)}}, "done"); e == nil {
					t.Fatal("missing proposal completed")
				}

			}
			if err = h.s.DB.ClassifyButlerObjectiveEffects(id, runtimedb.ObjectiveEffectful, proposalOnly, required); err != nil {
				t.Fatal(err)
			}
			if err = h.s.DB.OpenButlerTaskWait(id, runtimedb.ButlerTaskWait{Reason: runtimedb.ButlerWaitReply, Detail: "no receipt yet"}); err != nil {
				t.Fatal(err)
			}
			live := &butlerlive.Butlers{DB: h.s.DB, Ledger: butlerstore.New(h.s.DB, lease)}
			h.s.Butlers = &proposalCompletingButlers{Butlers: live, answer: butlerlive.Answer{TaskID: id, Butler: "sandy", Surface: runtimedb.ButlerSurfaceFamily, Provider: "scripted", FinalText: "unverified action", Calls: []butler.ToolCallRecord{{StepSeq: step, Name: "propose_change", ResourceYAML: containerYAML(t, "grafana", "ghcr.io/example/grafana@"+testDigest, nil)}}}}
			out, code, err := h.s.runNamedButlerSession(context.Background(), butlerlive.Turn{Member: "sam", Text: "create a proposal"}, nil)
			if err != nil || code != 200 {
				t.Fatalf("proposal failed: %d %v", code, err)
			}
			response := out.(askResponse)
			if len(response.PlanIDs) != 1 || len(response.ProposalErrors) != 0 {
				t.Fatalf("proposal not persisted: %+v", response)
			}
			task, err := h.s.DB.GetButlerTask(id)
			if err != nil {
				t.Fatal(err)
			}
			if (task.State == runtimedb.ButlerTaskCompleted) != (mode != "execute") {
				t.Fatalf("proposalOnly=%v state=%s", proposalOnly, task.State)
			}
			if mode == "mixed" && (!strings.Contains(response.FinalText, "store memory") || !strings.Contains(response.FinalText, "create proposal")) {
				t.Fatalf("mixed outcomes lost: %s", response.FinalText)
			}
			if proposalOnly && !strings.Contains(response.FinalText, "have not been applied") {
				t.Fatal("proposal was described as execution")
			}
		})
	}
}

func TestFailedTurnKeepsProposalProofForVerifiedResume(t *testing.T) {
	h := newHarness(t)
	h.person("sam")
	lease, err := h.s.DB.AcquireCoordinator("failed-proposal-test")
	if err != nil {
		t.Fatal(err)
	}
	id := failedTurnTask(t, h)
	if err = h.s.DB.TransitionButlerTask(id, runtimedb.ButlerTaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	step := proposalProofStep(t, h, id)
	live := &butlerlive.Butlers{DB: h.s.DB, Ledger: butlerstore.New(h.s.DB, lease)}
	h.s.Butlers = &proposalCompletingButlers{Butlers: live}
	ans := butlerlive.Answer{TaskID: id, Butler: "sandy", Surface: runtimedb.ButlerSurfaceFamily, Provider: "scripted", Calls: []butler.ToolCallRecord{{StepSeq: step, Name: "propose_change", ResourceYAML: containerYAML(t, "grafana", "ghcr.io/example/grafana@"+testDigest, nil)}}}
	out := h.s.preserveFailedTurn(butlerlive.Turn{Member: "sam", Text: "prepare a proposal"}, nil, ans)
	response, ok := out.(askResponse)
	if !ok || len(response.PlanIDs) != 1 {
		t.Fatalf("proposal lost: %+v", out)
	}
	// Failed classification must not guess that an execution request was proposal-only.
	if err = live.Ledger.Complete(id, nil, "done"); err == nil {
		t.Fatal("unclassified action accepted proposal proof")
	}
	if err = h.s.DB.ClassifyButlerObjectiveEffects(id, runtimedb.ObjectiveEffectful, true, []runtimedb.RequiredEffect{{Description: "create proposal", SourceKey: fmt.Sprintf("step:%d", step)}}); err != nil {
		t.Fatal(err)
	}
	// A resumed runner can use the saved proof without replaying ResourceYAML.
	if err = live.Ledger.Complete(id, nil, "Plan is ready for review; no changes applied"); err != nil {
		t.Fatalf("saved proposal not completable: %v", err)
	}
}

func proposalProofStep(t *testing.T, h *harness, id string) int64 {
	t.Helper()
	step, err := h.s.DB.AppendButlerTaskStep(id, runtimedb.ButlerTaskStep{Kind: runtimedb.ButlerStepTool, Name: "propose_change", ParamsJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := h.s.DB.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.FinishButlerAttempt(attempt.Seq, runtimedb.ButlerAttemptSucceeded, `{"content":"proposal accepted"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.SetButlerStepState(step, runtimedb.ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	return step
}

func TestPartialProposalPersistenceKeepsSuccessfulReceipts(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			h := newHarness(t)
			h.person("sam")
			lease, err := h.s.DB.AcquireCoordinator("partial-proposals")
			if err != nil {
				t.Fatal(err)
			}
			id := failedTurnTask(t, h)
			if err = h.s.DB.TransitionButlerTask(id, runtimedb.ButlerTaskRunning, "", ""); err != nil {
				t.Fatal(err)
			}
			first := proposalProofStep(t, h, id)
			second := proposalProofStep(t, h, id)
			if err = h.s.DB.ClassifyButlerObjectiveEffects(id, runtimedb.ObjectiveEffectful, true, []runtimedb.RequiredEffect{{Description: "first proposal", SourceKey: fmt.Sprintf("step:%d", first)}, {Description: "second proposal", SourceKey: fmt.Sprintf("step:%d", second)}}); err != nil {
				t.Fatal(err)
			}
			live := &butlerlive.Butlers{DB: h.s.DB, Ledger: butlerstore.New(h.s.DB, lease)}
			ans := butlerlive.Answer{TaskID: id, Butler: "sandy", Surface: runtimedb.ButlerSurfaceFamily, Provider: "scripted", Calls: []butler.ToolCallRecord{{StepSeq: first, Name: "propose_change", ResourceYAML: containerYAML(t, "grafana", "ghcr.io/example/grafana@"+testDigest, nil)}, {StepSeq: second, Name: "propose_change", ResourceYAML: "not yaml: ["}}}
			h.s.Butlers = &proposalCompletingButlers{Butlers: live, answer: ans}
			var out any
			if failed {
				out = h.s.preserveFailedTurn(butlerlive.Turn{Member: "sam", Text: "prepare both proposals"}, nil, ans)
			} else {
				out, _, err = h.s.runNamedButlerSession(context.Background(), butlerlive.Turn{Member: "sam", Text: "prepare both proposals"}, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			response, ok := out.(askResponse)
			if !ok || len(response.PlanIDs) != 1 || len(response.ProposalErrors) != 1 {
				t.Fatalf("partial proposal outcome lost: %+v", out)
			}
			evidence, err := h.s.DB.PersistedEffectEvidence(id)
			if err != nil || len(evidence) != 1 || evidence[0].SourceID != response.PlanIDs[0] {
				t.Fatalf("successful sibling lost proof: %+v %v", evidence, err)
			}
			if err = live.Ledger.Complete(id, nil, "both done"); err == nil {
				t.Fatal("partial proposals completed all effects")
			}
		})
	}
}
func TestChatAcknowledgementCertifiesOnlyTheOriginalEffect(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(fmt.Sprint(replaced), func(t *testing.T) {
			h := newHarness(t)
			h.person("sam")
			lease, err := h.s.DB.AcquireCoordinator("chat-delivery")
			if err != nil {
				t.Fatal(err)
			}
			at := h.agentAt(t, "family", "telegram")
			binding := h.bindAccount(t, "family", "sam", "telegram", "12345")
			id := failedTurnTask(t, h)
			if err = h.s.DB.TransitionButlerTask(id, runtimedb.ButlerTaskRunning, "", ""); err != nil {
				t.Fatal(err)
			}
			step, err := h.s.DB.AppendButlerTaskStep(id, runtimedb.ButlerTaskStep{Kind: runtimedb.ButlerStepTool, Name: "do_household_action", ParamsJSON: `{"tool":"chat.telegram.send","message":"jasmine"}`})
			if err != nil {
				t.Fatal(err)
			}
			if err = h.s.DB.ClassifyButlerObjectiveEffects(id, runtimedb.ObjectiveEffectful, false, []runtimedb.RequiredEffect{{Description: "send jasmine", SourceKey: fmt.Sprintf("step:%d", step)}}); err != nil {
				t.Fatal(err)
			}
			ctx := operation.With(context.Background(), operation.Identity{TaskID: id, Step: step, PermissionLabel: "sandy", Audience: string(runtimedb.ButlerAudiencePrivate)})
			outbound, err := h.s.QueueChatPush(ctx, at, "12345", binding.ID, "jasmine", "", "chat-delivery-proof")
			if err != nil {
				t.Fatal(err)
			}
			ledger := butlerstore.New(h.s.DB, lease)
			if err = ledger.Complete(id, nil, "sent"); err == nil {
				t.Fatal("enqueue certified delivery")
			}
			if replaced {
				if _, err = h.s.DB.ExecForTest("UPDATE chat_outbound SET body='privacy warning' WHERE seq=?", outbound); err != nil {
					t.Fatal(err)
				}
			}
			response := h.do(runtimedb.RoleConcierge, http.MethodPost, fmt.Sprintf("/api/v1/chat/outbound/%d/ack", outbound), nil, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatal(response.Status)
			}
			if err = ledger.Complete(id, nil, "delivered"); (err == nil) == replaced {
				t.Fatalf("replacement=%v completion=%v", replaced, err)
			}
		})
	}
}
