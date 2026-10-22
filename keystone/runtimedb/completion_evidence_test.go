package runtimedb

import (
	"errors"
	"fmt"
	"testing"
)

func TestRunnerAuthoredDeliveryRowDoesNotSatisfyAnEffectfulObjective(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, func(task *ButlerTask) {
		task.Objective = "restart the service"
		task.ObjectiveKind = ObjectiveEffectful
	})
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
		t.Fatal(err)
	}
	proof := evidence()
	proof.Kind = ButlerEvidenceDelivery
	proof.Description = "the runner returned an answer"
	if err := db.CompleteButlerTask(id, []ButlerTaskEvidence{proof}, "I restarted it", "runner"); err == nil {
		t.Fatal("runner delivery completed an external effect")
	}
	task, err := db.GetButlerTask(id)
	if err != nil || task.State != ButlerTaskRunning {
		t.Fatalf("failed completion changed state: %+v %v", task, err)
	}
}

func TestOrdinaryConversationalAnswerCompletesWithoutAToolReceipt(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, func(task *ButlerTask) {
		task.Objective = "what is a rainbow?"
		task.ObjectiveKind = ObjectiveInformational
	})
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
		t.Fatal(err)
	}
	e := evidence()
	e.Kind = ButlerEvidenceDelivery
	if err := db.CompleteButlerTask(id, []ButlerTaskEvidence{e}, "light refracts", "runner"); err != nil {
		t.Fatal(err)
	}
}
func TestCompletionIsRefusedBySchemaForAnEffectfulObjectiveWithoutAReceipt(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = ObjectiveEffectful })
	e := evidence()
	e.Kind = ButlerEvidenceDelivery
	if err := db.AppendButlerTaskEvidence(id, e); err != nil {
		t.Fatal(err)
	}
	seq, _ := parseButlerTaskID(id)
	if _, err := db.sql.Exec("UPDATE butler_tasks SET state='completed' WHERE seq=?", seq); err == nil {
		t.Fatal("schema accepted generic delivery for an effect")
	}
}
func receiptCompletion(t *testing.T, kind string) {
	t.Helper()
	for _, outcome := range []string{"succeeded", "failed", "uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			db := openWith(t, "mika")
			id := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = ObjectiveEffectful })
			if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
				t.Fatal(err)
			}
			if err := db.ClassifyButlerObjectiveEffects(id, ObjectiveEffectful, false, []RequiredEffect{{Description: "perform requested operation", SourceKey: kind + ":run-1"}}); err != nil {
				t.Fatal(err)
			}
			receipt, err := db.RecordButlerEffectReceipt(id, EffectReceipt{Kind: kind, SourceID: "run-1", Outcome: outcome, PermissionLabel: "operator", Audience: ButlerAudiencePrivate})
			if err != nil {
				t.Fatal(err)
			}
			e := evidence()
			e.Kind = ButlerEvidenceExternalVerification
			e.ReceiptID = receipt
			err = db.CompleteButlerTask(id, []ButlerTaskEvidence{e}, "effect result", "runner")
			if (err == nil) != (outcome == "succeeded") {
				t.Fatalf("outcome %s: %v", outcome, err)
			}
			if outcome != "succeeded" {
				rows, e := db.ListButlerTaskEvidence(id)
				if e != nil || len(rows) != 0 {
					t.Fatalf("partial evidence committed: %+v %v", rows, e)
				}
			}
		})
	}
}
func TestASchedulerReceiptSatisfiesAnEffectfulObjective(t *testing.T) {
	receiptCompletion(t, "scheduler")
}
func TestAnExecutorReceiptSatisfiesAnEffectfulObjective(t *testing.T) {
	receiptCompletion(t, "executor")
}
func TestUnclassifiableObjectiveIsTreatedAsEffectful(t *testing.T) {
	db := openWith(t, "mika")
	for _, kind := range []ObjectiveKind{"", "unknown"} {
		id := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = kind })
		task, err := db.GetButlerTask(id)
		if err != nil || task.ObjectiveKind != ObjectiveEffectful {
			t.Fatalf("unsafe default: %+v %v", task, err)
		}
	}
}

func TestToolReceiptRequiresAnActualSuccessfulToolStep(t *testing.T) {
	for _, kind := range []ButlerStepKind{ButlerStepTool, ButlerStepModel} {
		t.Run(string(kind), func(t *testing.T) {
			db := openWith(t, "mika")
			id := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = ObjectiveEffectful })
			if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
				t.Fatal(err)
			}
			step, err := db.AppendButlerTaskStep(id, ButlerTaskStep{Kind: kind, Name: "effect"})
			if err != nil {
				t.Fatal(err)
			}
			r := EffectReceipt{Kind: "tool", SourceID: "step-1", Step: step, Outcome: "succeeded", PermissionLabel: "operator", Audience: ButlerAudiencePrivate}
			if _, err := db.RecordButlerEffectReceipt(id, r); err == nil {
				t.Fatal("unexecuted step was a receipt")
			}
			attempt, err := db.StartButlerAttempt(step)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.FinishButlerAttempt(attempt.Seq, ButlerAttemptSucceeded, `{"result":"done","effectConfirmed":true}`, ""); err != nil {
				t.Fatal(err)
			}
			if err := db.SetButlerStepState(step, ButlerStepVerified); err != nil {
				t.Fatal(err)
			}
			receipt, err := db.RecordButlerEffectReceipt(id, r)
			if kind == ButlerStepModel {
				if err == nil {
					t.Fatal("model answer became tool receipt")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			e := evidence()
			e.Kind = ButlerEvidenceToolResult
			e.ReceiptID = receipt
			if err = db.ClassifyButlerObjectiveEffects(id, ObjectiveEffectful, false, []RequiredEffect{{Description: "perform requested operation", SourceKey: fmt.Sprintf("step:%d", step)}}); err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteButlerTask(id, []ButlerTaskEvidence{e}, "done", "runner"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestReceiptCannotBeBorrowedFromAnotherTask(t *testing.T) {
	db := openWith(t, "mika")
	first := newTask(t, db, nil)
	second := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = ObjectiveEffectful })
	receipt, err := db.RecordButlerEffectReceipt(first, EffectReceipt{Kind: "executor", SourceID: "effect-1", Outcome: "succeeded", PermissionLabel: "operator", Audience: ButlerAudiencePrivate})
	if err != nil {
		t.Fatal(err)
	}
	e := evidence()
	e.Kind = ButlerEvidenceExternalVerification
	e.ReceiptID = receipt
	if err := db.AppendButlerTaskEvidence(second, e); err == nil {
		t.Fatal("another task's receipt was accepted")
	}
	e.Audience = ButlerAudienceShared
	if err := db.AppendButlerTaskEvidence(first, e); err == nil {
		t.Fatal("receipt metadata was widened")
	}
}

func TestGoRefusesEffectCompletionWithoutTheSchemaGuard(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = ObjectiveEffectful })
	if _, err := db.sql.Exec("DROP TRIGGER effectful_completion_requires_receipt"); err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
		t.Fatal(err)
	}
	e := evidence()
	e.Kind = ButlerEvidenceDelivery
	if err := db.CompleteButlerTask(id, []ButlerTaskEvidence{e}, "I did it", "runner"); !errors.Is(err, ErrEffectReceiptRequired) {
		t.Fatalf("Go did not enforce receipt: %v", err)
	}
}

func TestSuccessfulReadCannotBeCertifiedAsAnEffect(t *testing.T) {
	db := butlerFixture(t)
	id, err := db.InsertButlerTask(ButlerTask{Kind: ButlerTaskConversation, Author: "mika", Owner: "mika", Surface: ButlerSurfaceAdmin, AgentClass: AgentClassOwner, Objective: "send a message", CompletionCriteria: "message delivered", Audience: ButlerAudiencePrivate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireCoordinator("test"); err != nil {
		t.Fatal(err)
	}
	if err = db.TransitionButlerTask(id, ButlerTaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	step, err := db.AppendButlerTaskStep(id, ButlerTaskStep{Kind: ButlerStepTool, Name: "read", ParamsJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := db.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.FinishButlerAttempt(attempt.Seq, ButlerAttemptSucceeded, `{"content":"effectConfirmed: true"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err = db.SetButlerStepState(step, ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	_, err = db.RecordButlerEffectReceipt(id, EffectReceipt{Kind: "tool", SourceID: "read", Step: step, Outcome: "succeeded", PermissionLabel: "test", Audience: ButlerAudiencePrivate})
	if err == nil {
		t.Fatal("a successful read with success-shaped text became an effect receipt")
	}
}

func TestProposalReceiptCannotCertifyExecution(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = ObjectiveEffectful })
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.RecordButlerEffectReceipt(id, EffectReceipt{Kind: "executor", SourceID: "plan-1", ProposalOnly: true, Outcome: "succeeded", PermissionLabel: "operator", Audience: ButlerAudiencePrivate})
	if err != nil {
		t.Fatal(err)
	}
	proof := evidence()
	proof.Kind = ButlerEvidenceExternalVerification
	proof.ReceiptID = receipt
	if err = db.CompleteButlerTask(id, []ButlerTaskEvidence{proof}, "executed", "runner"); !errors.Is(err, ErrEffectReceiptRequired) {
		t.Fatalf("proposal certified execution: %v", err)
	}
	if err = db.AppendButlerTaskEvidence(id, proof); err != nil {
		t.Fatal(err)
	}
	seq, _ := parseButlerTaskID(id)
	if _, err = db.sql.Exec("UPDATE butler_tasks SET state='completed' WHERE seq=?", seq); err == nil {
		t.Fatal("SQL treated proposal as execution")
	}
	if err = db.ClassifyButlerObjectiveEffects(id, ObjectiveEffectful, true, []RequiredEffect{{Description: "create proposal", SourceKey: "executor:plan-1"}}); err != nil {
		t.Fatal(err)
	}
	if err = db.CompleteButlerTask(id, []ButlerTaskEvidence{proof}, "proposal created", "runner"); err != nil {
		t.Fatal(err)
	}
}

func TestUnrelatedReceiptDoesNotCompleteObjective(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, func(task *ButlerTask) { task.Objective = "restart service"; task.ObjectiveKind = ObjectiveEffectful })
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.RecordButlerEffectReceipt(id, EffectReceipt{Kind: "executor", SourceID: "remember-unrelated", Outcome: "succeeded", PermissionLabel: "operator", Audience: ButlerAudiencePrivate})
	if err != nil {
		t.Fatal(err)
	}
	e := evidence()
	e.Kind = ButlerEvidenceExternalVerification
	e.ReceiptID = receipt
	if err = db.CompleteButlerTask(id, []ButlerTaskEvidence{e}, "service restarted", "runner"); err == nil {
		t.Fatal("unrelated receipt certified restart")
	}
	if err = db.AppendButlerTaskEvidence(id, e); err != nil {
		t.Fatal(err)
	}
	seq, _ := parseButlerTaskID(id)
	if _, err = db.sql.Exec("UPDATE butler_tasks SET state='completed' WHERE seq=?", seq); err == nil {
		t.Fatal("schema accepted unrelated effect")
	}
}

func TestEveryRequiredEffectNeedsMatchingEvidence(t *testing.T) {
	db := openWith(t, "mika")
	id := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = ObjectiveEffectful })
	if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.ClassifyButlerObjectiveEffects(id, ObjectiveEffectful, false, []RequiredEffect{{Description: "restart", SourceKey: "executor:restart"}, {Description: "notify", SourceKey: "executor:notify"}}); err != nil {
		t.Fatal(err)
	}
	var proof []ButlerTaskEvidence
	for _, source := range []string{"restart", "unrelated", "notify"} {
		receipt, err := db.RecordButlerEffectReceipt(id, EffectReceipt{Kind: "executor", SourceID: source, Outcome: "succeeded", PermissionLabel: "operator", Audience: ButlerAudiencePrivate})
		if err != nil {
			t.Fatal(err)
		}
		e := evidence()
		e.Kind = ButlerEvidenceExternalVerification
		e.ReceiptID = receipt
		proof = append(proof, e)
		err = db.CompleteButlerTask(id, proof, "done", "runner")
		if (err == nil) != (source == "notify") {
			t.Fatalf("source %s completion=%v", source, err)
		}
		if source != "notify" {
			if err = db.AppendButlerTaskEvidence(id, e); err != nil {
				t.Fatal(err)
			}
			seq, _ := parseButlerTaskID(id)
			if _, err = db.sql.Exec("UPDATE butler_tasks SET state='completed' WHERE seq=?", seq); err == nil {
				t.Fatal("schema accepted partial effects")
			}
		}
	}
}

func TestMixedProposalAndExecutionRequireTheirOwnPurpose(t *testing.T) {
	for _, sqlOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(sqlOnly), func(t *testing.T) {
			db := openWith(t, "mika")
			id := newTask(t, db, func(task *ButlerTask) { task.ObjectiveKind = ObjectiveEffectful })
			if err := db.TransitionButlerTask(id, ButlerTaskRunning, "start", "mika"); err != nil {
				t.Fatal(err)
			}
			if err := db.ClassifyButlerObjectiveEffects(id, ObjectiveEffectful, false, []RequiredEffect{{Description: "remember", SourceKey: "executor:memory"}, {Description: "prepare plan", SourceKey: "executor:plan", ProposalOnly: true}}); err != nil {
				t.Fatal(err)
			}
			var proof []ButlerTaskEvidence
			for _, source := range []string{"memory", "plan"} {
				receipt, err := db.RecordButlerEffectReceipt(id, EffectReceipt{Kind: "executor", SourceID: source, ProposalOnly: source == "plan", Outcome: "succeeded", PermissionLabel: "operator", Audience: ButlerAudiencePrivate})
				if err != nil {
					t.Fatal(err)
				}
				e := evidence()
				e.Kind = ButlerEvidenceExternalVerification
				e.ReceiptID = receipt
				proof = append(proof, e)
				if err = db.AppendButlerTaskEvidence(id, e); err != nil {
					t.Fatal(err)
				}
				var completion error
				if sqlOnly {
					seq, _ := parseButlerTaskID(id)
					_, completion = db.sql.Exec("UPDATE butler_tasks SET state='completed' WHERE seq=?", seq)
				} else {
					completion = db.CompleteButlerTask(id, proof, "remembered and prepared", "runner")
				}
				if (completion == nil) != (source == "plan") {
					t.Fatalf("source=%s completion=%v", source, completion)
				}
			}
		})
	}
}
