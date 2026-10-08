package runtimedb

import (
	"testing"
)

func newSession(t *testing.T, db *DB, finalText string, turns []ButlerTurn) string {
	t.Helper()
	id, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "mika", Provider: "scripted",
		Mode: ButlerModeFull, Surface: ButlerSurfaceAdmin,
		FinalText: finalText, Turns: turns,
	})
	if err != nil {
		t.Fatalf("InsertButlerSession: %v", err)
	}
	return id
}

func TestButlerTurnsAreStoredInOrder(t *testing.T) {
	db := open(t)
	id := newSession(t, db, "two", []ButlerTurn{
		{Role: ButlerRoleUser, Text: "one"},
		{Role: ButlerRoleAssistant, Text: "two", PlanIDs: []string{"plan-a", "plan-b"}},
	})

	sess, err := db.GetButlerSession(id)
	if err != nil {
		t.Fatalf("GetButlerSession: %v", err)
	}
	if len(sess.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(sess.Turns))
	}
	if sess.Turns[0].Ordinal != 0 || sess.Turns[1].Ordinal != 1 {
		t.Fatalf("ordinals = %d,%d", sess.Turns[0].Ordinal, sess.Turns[1].Ordinal)
	}
	if got := sess.Turns[1].PlanIDs; len(got) != 2 || got[0] != "plan-a" || got[1] != "plan-b" {
		t.Fatalf("planIds = %v", got)
	}
	// A turn with no plans reads back as none, not as one empty string.
	if got := sess.Turns[0].PlanIDs; len(got) != 0 {
		t.Fatalf("user turn planIds = %v, want none", got)
	}
}

// The session's own final_text has to follow the newest answer, or every
// reader that predates turns — the CLI, the chat adapters, an audit
// query — keeps showing the first thing the Butler ever said.
func TestAppendButlerTurnsMovesFinalText(t *testing.T) {
	db := open(t)
	id := newSession(t, db, "two", []ButlerTurn{
		{Role: ButlerRoleUser, Text: "one"},
		{Role: ButlerRoleAssistant, Text: "two"},
	})
	if err := db.AppendButlerTurns(id, []ButlerTurn{
		{Role: ButlerRoleUser, Text: "three"},
		{Role: ButlerRoleAssistant, Text: "four", Capped: true, TranscriptJSON: `{"provider":"scripted"}`},
	}); err != nil {
		t.Fatalf("AppendButlerTurns: %v", err)
	}

	sess, err := db.GetButlerSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(sess.Turns))
	}
	if sess.Turns[3].Ordinal != 3 || sess.Turns[3].Text != "four" {
		t.Fatalf("appended turn = %+v", sess.Turns[3])
	}
	if sess.FinalText != "four" || !sess.Capped {
		t.Fatalf("session final = %q capped=%v, want the newest answer", sess.FinalText, sess.Capped)
	}
	if sess.TranscriptJSON != `{"provider":"scripted"}` {
		t.Fatalf("transcript = %q, want the newest turn's", sess.TranscriptJSON)
	}
}

// A follow-up to a conversation that is not there is a bug in the
// caller, not a new conversation.
func TestAppendButlerTurnsRefusesAnUnknownSession(t *testing.T) {
	db := open(t)
	for _, id := range []string{"bs-999", "not-an-id"} {
		if err := db.AppendButlerTurns(id, []ButlerTurn{{Role: ButlerRoleAssistant, Text: "x"}}); err == nil {
			t.Fatalf("AppendButlerTurns(%q) succeeded, want ErrNotFound", id)
		}
	}
}

func TestButlerTurnRoleIsClosed(t *testing.T) {
	db := open(t)
	_, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "mika", Surface: ButlerSurfaceAdmin,
		Turns: []ButlerTurn{{Role: "system", Text: "no"}},
	})
	if err == nil {
		t.Fatal("a turn with role \"system\" was accepted")
	}
}

// A spoken question is stored as one and read back as one; a typed one
// reads back as nothing. Only a person's turn can be spoken, and
// "voice" is the only mark there is.
func TestTurnSourceIsStoredAndClosed(t *testing.T) {
	db := open(t)
	id := newSession(t, db, "ok", []ButlerTurn{
		{Role: ButlerRoleUser, Text: "spoken", Source: TurnSourceVoice},
		{Role: ButlerRoleAssistant, Text: "ok"},
	})
	if err := db.AppendButlerTurns(id, []ButlerTurn{
		{Role: ButlerRoleUser, Text: "typed"},
		{Role: ButlerRoleAssistant, Text: "ok"},
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := db.GetButlerSession(id)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{TurnSourceVoice, "", "", ""}
	for i, turn := range sess.Turns {
		if turn.Source != want[i] {
			t.Errorf("turn %d source = %q, want %q", i, turn.Source, want[i])
		}
	}

	for _, bad := range []ButlerTurn{
		{Role: ButlerRoleAssistant, Text: "an answer is not spoken", Source: TurnSourceVoice},
		{Role: ButlerRoleUser, Text: "an unknown mark", Source: "telepathy"},
	} {
		if _, err := db.InsertButlerSession(ButlerSession{
			Principal: "butler", ActingFor: "mika", Surface: ButlerSurfaceAdmin,
			Turns: []ButlerTurn{bad},
		}); err == nil {
			t.Errorf("turn %+v was accepted", bad)
		}
	}
}
