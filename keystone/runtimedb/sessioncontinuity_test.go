package runtimedb

import (
	"testing"
	"time"
)

func TestSessionCandidatesArePersonAndAgentScoped(t *testing.T) {
	db := householdWith(t, "alice", "bob")
	connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	connect(t, db, "joan", "matrix", ButlerSurfaceFamily, "chat-matrix-joan")

	makeSession := func(person, agent, question string) string {
		t.Helper()
		id, err := db.InsertButlerSession(ButlerSession{
			Principal: "butler", ActingFor: person, Provider: "test",
			Mode: ButlerModeFull, Surface: ButlerSurfaceFamily, Butler: agent,
			FinalText: "answer", Turns: []ButlerTurn{
				{Role: ButlerRoleUser, Text: question},
				{Role: ButlerRoleAssistant, Text: "answer"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	want := makeSession("alice", "sandy", "alice and sandy")
	makeSession("bob", "sandy", "bob and sandy")
	makeSession("alice", "joan", "alice and joan")

	got, err := db.RecentSessionCandidates("alice", "sandy", time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != want || got[0].Subject != "alice and sandy" {
		t.Fatalf("candidates = %+v, want only %s", got, want)
	}
}

func TestSessionCandidateDoesNotLoadTranscriptAndRevocationRemovesIt(t *testing.T) {
	db := householdWith(t, "alice")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	id, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "alice", Provider: "test",
		Mode: ButlerModeFull, Surface: ButlerSurfaceFamily, Butler: "sandy",
		FinalText: "private answer", Turns: []ButlerTurn{
			{Role: ButlerRoleUser, Text: "private subject"},
			{Role: ButlerRoleAssistant, Text: "private answer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.NoteButlerSessionConnection(id, "inbound", at.Name(), at.Seq); err != nil {
		t.Fatal(err)
	}

	got, err := db.RecentSessionCandidates("alice", "sandy", time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Provenance) != 1 || got[0].Provenance[0].Connection == nil || got[0].Provenance[0].Connection.Seq != at.Seq {
		t.Fatalf("candidate provenance = %+v", got)
	}
	if err := db.RevokeButlerSession(id, "alice"); err != nil {
		t.Fatal(err)
	}
	got, err = db.RecentSessionCandidates("alice", "sandy", time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("revoked session remained a candidate: %+v", got)
	}
}

func TestNewConversationExcludesEarlierCandidatesUntilOneIsChosen(t *testing.T) {
	db := householdWith(t, "alice")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	id, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "alice", Provider: "test",
		Mode: ButlerModeFull, Surface: ButlerSurfaceFamily, Butler: "sandy",
		FinalText: "answer", Turns: []ButlerTurn{{Role: ButlerRoleUser, Text: "old"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetChatThreadSession(at, "dm", "alice", id); err != nil {
		t.Fatal(err)
	}
	if err := db.BreakChatThread(at, "dm", "alice"); err != nil {
		t.Fatal(err)
	}
	broken, err := db.ChatThreadBrokenAt(at, "dm", "alice")
	if err != nil || broken.IsZero() {
		t.Fatalf("break = %v, err=%v", broken, err)
	}
	got, err := db.RecentSessionCandidates("alice", "sandy", time.Now().Add(-time.Hour), broken)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("/new rediscovered the old conversation: %+v", got)
	}
}

func TestResumeOfferIsScopedAndSingleUse(t *testing.T) {
	db := householdWith(t, "alice", "bob")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	id, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "alice", Provider: "test",
		Mode: ButlerModeFull, Surface: ButlerSurfaceFamily, Butler: "sandy",
		Turns: []ButlerTurn{{Role: ButlerRoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSessionResumeOffer("digest", "alice", "sandy", at.Seq, id, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeSessionResumeOffer("digest", "bob", "sandy", at.Seq); err != ErrNotFound {
		t.Fatalf("bob consumed alice's offer: %v", err)
	}
	got, err := db.ConsumeSessionResumeOffer("digest", "alice", "sandy", at.Seq)
	if err != nil || got != id {
		t.Fatalf("consume = %q, %v", got, err)
	}
	if _, err := db.ConsumeSessionResumeOffer("digest", "alice", "sandy", at.Seq); err != ErrNotFound {
		t.Fatalf("offer was reusable: %v", err)
	}
}

func TestRevokedSessionCannotSpendAnIssuedResumeOffer(t *testing.T) {
	db := householdWith(t, "alice")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	id, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "alice", Provider: "test",
		Mode: ButlerModeFull, Surface: ButlerSurfaceFamily, Butler: "sandy",
		Turns: []ButlerTurn{{Role: ButlerRoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSessionResumeOffer("revoked", "alice", "sandy", at.Seq, id, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeButlerSession(id, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeSessionResumeOffer("revoked", "alice", "sandy", at.Seq); err != ErrNotFound {
		t.Fatalf("revoked session resumed: %v", err)
	}
}

func TestTurnAndInboundConnectionAreRecordedTogether(t *testing.T) {
	db := householdWith(t, "alice")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	id, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "alice", Provider: "test",
		Mode: ButlerModeFull, Surface: ButlerSurfaceFamily, Butler: "sandy",
		InboundConnection: at.Name(), InboundConnectionRef: at.Seq,
		Turns: []ButlerTurn{{Role: ButlerRoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AppendButlerTurnsFrom(id, []ButlerTurn{{Role: ButlerRoleAssistant, Text: "hi"}}, at.Name(), at.Seq); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM butler_session_connections WHERE direction = 'inbound'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("inbound provenance rows = %d, want one per recorded turn", n)
	}
}
