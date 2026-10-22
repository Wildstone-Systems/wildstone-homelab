package runtimedb

import "testing"

func TestConversationParentCarriesBudgetAcrossTurnLedgers(t *testing.T) {
	db := openWith(t, "mika")
	first := newTask(t, db, nil)
	if _, err := db.AppendButlerTaskStep(first, ButlerTaskStep{Kind: ButlerStepTool, Name: "calendar.read"}); err != nil {
		t.Fatal(err)
	}
	session := newSession(t, db, "one answer", []ButlerTurn{{Role: ButlerRoleUser, Text: "first"}, {Role: ButlerRoleAssistant, Text: "one answer"}})
	if err := db.SetButlerTaskSession(first, session); err != nil {
		t.Fatal(err)
	}
	second := newTask(t, db, func(task *ButlerTask) { task.SessionID = session })
	one, err := db.ConversationWorkForTask(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := db.ConversationWorkForTask(second)
	if err != nil || one.ID != two.ID || two.ToolCalls != 1 {
		t.Fatalf("parent lost across turns: first=%+v second=%+v err=%v", one, two, err)
	}
	if _, err := db.AppendButlerTaskStep(second, ButlerTaskStep{Kind: ButlerStepTool, Name: "calendar.read"}); err != nil {
		t.Fatal(err)
	}
	two, err = db.ConversationWorkForTask(second)
	if err != nil || two.ToolCalls != 2 {
		t.Fatalf("aggregate count: %+v %v", two, err)
	}
	for i := 2; i < 64; i++ {
		if _, err := db.AppendButlerTaskStep(second, ButlerTaskStep{Kind: ButlerStepTool, Name: "calendar.read"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AppendButlerTaskStep(second, ButlerTaskStep{Kind: ButlerStepTool, Name: "calendar.read"}); err == nil {
		t.Fatal("later turn reset the aggregate budget")
	}
	if err := db.RecordButlerMember("julie", RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("julie", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReviewConversationWork(second, "julie", "extend-budget", "more calendar checks needed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendButlerTaskStep(second, ButlerTaskStep{Kind: ButlerStepTool, Name: "calendar.read"}); err != nil {
		t.Fatalf("review did not extend durable budget: %v", err)
	}
}

func TestConversationParentPreservesUncertainOutcome(t *testing.T) {
	db := openWith(t, "mika")
	if err := db.RecordButlerMember("julie", RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("julie", "root-admin"); err != nil {
		t.Fatal(err)
	}
	first := newTask(t, db, nil)
	session := newSession(t, db, "answer", []ButlerTurn{{Role: ButlerRoleUser, Text: "first"}, {Role: ButlerRoleAssistant, Text: "answer"}})
	if err := db.SetButlerTaskSession(first, session); err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionButlerTask(first, ButlerTaskRunning, "started", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenButlerTaskWait(first, ButlerTaskWait{Reason: ButlerWaitUncertainOutcome, Detail: "external call may have succeeded"}); err != nil {
		t.Fatal(err)
	}
	next := newTask(t, db, func(task *ButlerTask) { task.SessionID = session })
	work, err := db.ConversationWorkForTask(next)
	if err != nil || !work.UncertainOutcome {
		t.Fatalf("uncertainty reset on later turn: %+v %v", work, err)
	}
	if _, err := db.AppendButlerTaskStep(next, ButlerTaskStep{Kind: ButlerStepTool, Name: "calendar.write"}); err == nil {
		t.Fatal("unresolved outcome allowed another effect")
	}
	if err := db.ReviewConversationWork(next, "sam", "resolve-outcome", "checked calendar"); err == nil {
		t.Fatal("Member resolved ambiguous work")
	}
	if err := db.ReviewConversationWork(next, "julie", "resolve-outcome", "checked calendar; no booking appeared"); err != nil {
		t.Fatal(err)
	}
	work, err = db.ConversationWorkForTask(next)
	if err != nil || work.UncertainOutcome {
		t.Fatalf("Owner review did not release parent: %+v %v", work, err)
	}
	prior, err := db.GetButlerTask(first)
	if err != nil || prior.State != ButlerTaskFailed {
		t.Fatalf("ambiguous turn could be retried: %+v %v", prior, err)
	}
}
