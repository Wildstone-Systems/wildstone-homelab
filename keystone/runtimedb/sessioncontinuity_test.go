package runtimedb

import (
	"testing"
	"time"
)

func TestContinuationSnapshotExcludesConcurrentlyCommittedTrustedTurn(t *testing.T) {
	db := householdWith(t, "alice")
	at := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily)
	id, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "alice", Provider: "test", Mode: ButlerModeFull,
		Surface: ButlerSurfaceFamily, Butler: "sandy", InboundConnection: at.Name(), InboundConnectionRef: at.Seq,
		Turns: []ButlerTurn{{Role: ButlerRoleUser, Text: "external question"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, allowed, err := db.GetButlerSessionForContinuation(id, at.Seq, func(origins []SessionConnection, _ Connection) bool {
		if len(origins) != 1 || origins[0].Connection == nil || origins[0].Connection.Seq != at.Seq {
			t.Fatalf("origins=%+v", origins)
		}
		// This writes through another pooled connection while the reader's
		// WAL snapshot is open. The commit finishes before history is read.
		if err := db.AppendButlerTurnsFrom(id, []ButlerTurn{{Role: ButlerRoleUser, Text: "trusted-only secret"}}, "console", 0); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if err != nil || !allowed {
		t.Fatalf("load: allowed=%v err=%v", allowed, err)
	}
	if len(loaded.Turns) != 1 || loaded.Turns[0].Text != "external question" {
		t.Fatalf("newly protected turn crossed snapshot: %+v", loaded.Turns)
	}
	latest, err := db.GetButlerSession(id)
	if err != nil || len(latest.Turns) != 2 {
		t.Fatalf("concurrent write did not commit: %+v err=%v", latest.Turns, err)
	}
	loaded, allowed, err = db.GetButlerSessionForContinuation(id, at.Seq, func(origins []SessionConnection, _ Connection) bool {
		for _, origin := range origins {
			if origin.ConnectionName == "console" {
				return false
			}
		}
		t.Fatal("new snapshot omitted committed trusted provenance")
		return true
	})
	if err != nil || allowed || loaded.ID != "" || len(loaded.Turns) != 0 {
		t.Fatalf("denied load returned history: %+v allowed=%v err=%v", loaded, allowed, err)
	}
}

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

func TestContinuationDestinationAndHistoryShareASnapshot(t *testing.T) {
	db := householdWith(t, "alice")
	origin := connect(t, db, "sandy", "matrix", ButlerSurfaceFamily, "origin-bot")
	destination := connect(t, db, "sandy", "telegram", ButlerSurfaceFamily, "destination-bot")
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='provider'`); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "alice", Mode: ButlerModeFull, Surface: ButlerSurfaceFamily, Butler: "sandy", InboundConnection: origin.Name(), InboundConnectionRef: origin.Seq, Turns: []ButlerTurn{{Role: ButlerRoleUser, Text: "authorized snapshot"}}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, allowed, err := db.GetButlerSessionForContinuation(id, destination.Seq, func(origins []SessionConnection, current Connection) bool {
		if current.Seq != destination.Seq || len(origins) != 1 || origins[0].Connection == nil || !DisclosureAllowsContent(origins[0].Connection.Disclosure, current.Disclosure) {
			t.Fatalf("incorrect snapshot destination=%+v origins=%+v", current, origins)
		}
		// Both changes commit on another pooled connection while the read
		// transaction is open. Neither may enter the authorized snapshot.
		if _, err := db.sql.Exec(`UPDATE butler_addresses SET revoked_at='revoked' WHERE seq=?`, destination.Seq); err != nil {
			t.Fatal(err)
		}
		if err := db.AppendButlerTurnsFrom(id, []ButlerTurn{{Role: ButlerRoleUser, Text: "later trusted secret"}}, "console", 0); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if err != nil || !allowed || len(loaded.Turns) != 1 || loaded.Turns[0].Text != "authorized snapshot" {
		t.Fatalf("snapshot load: %+v allowed=%v err=%v", loaded, allowed, err)
	}
	loaded, allowed, err = db.GetButlerSessionForContinuation(id, destination.Seq, func([]SessionConnection, Connection) bool {
		t.Fatal("revoked destination reached authorization callback")
		return true
	})
	if err != nil || allowed || loaded.ID != "" || len(loaded.Turns) != 0 {
		t.Fatalf("revoked load: %+v allowed=%v err=%v", loaded, allowed, err)
	}
}
