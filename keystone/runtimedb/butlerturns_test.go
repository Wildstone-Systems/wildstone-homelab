package runtimedb

import (
	"path/filepath"
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
	db := butlerTestDB(t)
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
	db := butlerTestDB(t)
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
	db := butlerTestDB(t)
	for _, id := range []string{"bs-999", "not-an-id"} {
		if err := db.AppendButlerTurns(id, []ButlerTurn{{Role: ButlerRoleAssistant, Text: "x"}}); err == nil {
			t.Fatalf("AppendButlerTurns(%q) succeeded, want ErrNotFound", id)
		}
	}
}

func TestButlerTurnRoleIsClosed(t *testing.T) {
	db := butlerTestDB(t)
	_, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "mika", Surface: ButlerSurfaceAdmin,
		Turns: []ButlerTurn{{Role: "system", Text: "no"}},
	})
	if err == nil {
		t.Fatal("a turn with role \"system\" was accepted")
	}
}

// 0014 backfills every session that predates turns with its answer.
// There is no user turn to recover, because the question was never
// stored — a reader sees a conversation that opens with an answer,
// which is true, rather than an invented question.
func TestMigration0014BackfillsExistingSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.migrateThrough("migrations/0013_chat_adapters.sql"); err != nil {
		t.Fatalf("migrating through 0013: %v", err)
	}
	id := insertSessionAtSchema(t, db, "n2 has been offline since 14:04.", nil)
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies 0014): %v", err)
	}
	defer db2.Close()
	sess, err := db2.GetButlerSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Turns) != 1 {
		t.Fatalf("turns = %d, want the answer alone: %+v", len(sess.Turns), sess.Turns)
	}
	if sess.Turns[0].Role != ButlerRoleAssistant || sess.Turns[0].Text != "n2 has been offline since 14:04." {
		t.Fatalf("backfilled turn = %+v", sess.Turns[0])
	}
	if sess.Turns[0].TranscriptJSON != `{"provider":"ollama"}` {
		t.Fatalf("backfilled turn lost its transcript: %q", sess.Turns[0].TranscriptJSON)
	}
	// And the backfilled session can be continued like any other.
	if err := db2.AppendButlerTurns(id, []ButlerTurn{
		{Role: ButlerRoleUser, Text: "why?"},
		{Role: ButlerRoleAssistant, Text: "Its agent session closed."},
	}); err != nil {
		t.Fatalf("appending to a backfilled session: %v", err)
	}
	sess, _ = db2.GetButlerSession(id)
	if len(sess.Turns) != 3 || sess.Turns[2].Ordinal != 2 {
		t.Fatalf("after append: %+v", sess.Turns)
	}
}

// insertSessionAtSchema writes one session the way the schema of the day
// would have written it, for the migration tests that run against a
// PARTIALLY migrated database.
//
// They cannot use InsertButlerSession: the Go insert always targets the
// current column list, so every later migration that adds a column would
// break tests that are about something else entirely. What those tests
// assert is that a row written before a migration reads back correctly
// after it, and that only needs the columns that existed then.
func insertSessionAtSchema(t *testing.T, db *DB, finalText string, turns []ButlerTurn) string {
	t.Helper()
	res, err := db.sql.Exec(
		`INSERT INTO butler_sessions (created_at, principal, acting_for, provider, mode, surface, final_text, capped, transcript_json)
		 VALUES (?, 'butler', 'mika', 'ollama', 'full', 'admin', ?, 0, ?)`,
		now(), finalText, `{"provider":"ollama"}`)
	if err != nil {
		t.Fatalf("inserting a session at this schema: %v", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for i, turn := range turns {
		if _, err := db.sql.Exec(
			`INSERT INTO butler_session_turns (session, ordinal, role, text, created_at, capped, tool_json, plan_ids)
			 VALUES (?, ?, ?, ?, ?, 0, '{}', '')`,
			seq, i, turn.Role, turn.Text, now()); err != nil {
			t.Fatalf("inserting a turn at this schema: %v", err)
		}
	}
	return butlerSessionID(seq)
}
