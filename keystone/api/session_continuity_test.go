package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestLegacyRepliesRecordBuiltInOutboundProvenance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		admin      bool
		connection string
		outbound   int64
	}{
		{"operator console", true, butlerlive.ConsoleConnection, 2},
		{"household console", false, butlerlive.ConsoleConnection, 2},
		{"household portal", false, portal.Connection, 2},
		{"operator chat records delivery later", true, "telegram:12345", 0},
		{"household chat records delivery later", false, "telegram:12345", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.wireHouseholdButler(answerTurns("first answer", "second answer"), runtimedb.ButlerModeFull)
			h.person("sam")
			var prior *turnContext
			var sessionID string
			for range 2 {
				var body any
				var code int
				var err error
				if tc.admin {
					body, code, err = h.s.runAdminButlerSession(context.Background(), "hello", "sam", tc.connection, prior)
				} else {
					body, code, err = h.s.runFamilySession(context.Background(), "hello", "sam", "", tc.connection, false, prior)
				}
				if err != nil || code != http.StatusOK {
					t.Fatalf("session response = %d, %v", code, err)
				}
				sessionID = body.(askResponse).SessionID
				session, err := h.s.DB.GetButlerSession(sessionID)
				if err != nil {
					t.Fatal(err)
				}
				prior = &turnContext{SessionID: sessionID, History: historyFrom(session.Turns)}
			}
			for direction, want := range map[string]int64{"inbound": 2, "outbound": tc.outbound} {
				// A no-op update lets this API-package test count persisted
				// provenance through the existing raw-statement test seam.
				rows, err := h.s.DB.ExecForTest(`UPDATE butler_session_connections
					SET connection_name = connection_name
					WHERE session = (SELECT seq FROM butler_sessions WHERE 'bs-' || seq = ?)
					AND direction = ? AND connection_name = ? AND connection_ref = 0`,
					sessionID, direction, tc.connection)
				if err != nil {
					t.Fatal(err)
				}
				count, err := rows.RowsAffected()
				if err != nil || count != want {
					t.Fatalf("%s provenance rows = %d, %v; want %d", direction, count, err, want)
				}
			}
		})
	}
}

func TestZeroSafeCandidatesWithProtectedOnesFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	at := h.agentAt(t, "family", "telegram")
	h.person("sam")
	c := chatCommandCtx{At: at, Identity: "sam", Conversation: "12345"}
	for i := 0; i < 3; i++ {
		id := insertCandidateSession(t, h, at, "sam", "private subject")
		if err := h.s.DB.NoteButlerSessionConnection(id, "inbound", butlerlive.ConsoleConnection, 0); err != nil {
			t.Fatal(err)
		}
		// Both automatic continuation and /sessions must use the same
		// generic redirect for one or several withheld candidates.
		prior, automatic, err := h.s.resolveChatContinuation(c)
		if err != nil || prior != nil {
			t.Fatalf("protected continuation = %+v, %v", prior, err)
		}
		listed, err := h.s.chatSessions(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{automatic, listed} {
			if body != "Please use the household dashboard." {
				t.Fatalf("redirect must not refer to a session or hidden count: %q", body)
			}
		}
	}
	if calls := h.providerCalls(t); calls != 0 {
		t.Fatalf("protected continuation ran the model %d times", calls)
	}
}

func TestTrustedConversationExistenceIsHiddenFromExternalMessaging(t *testing.T) {
	current := runtimedb.Connection{Seq: 7, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionTransport,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	candidate := runtimedb.SessionCandidate{
		SessionID: "bs-42", Subject: "the private homepage conversation",
		ConnectionRef: 0, ConnectionName: butlerlive.ConsoleConnection,
	}
	if _, visible := candidateOnConnection(candidate, externalContinuationConnection(current)); visible {
		t.Fatal("an external Connection learned that a trusted-UI conversation exists")
	}
}

func TestUnresolvedCandidateDisclosureFailsClosed(t *testing.T) {
	current := runtimedb.Connection{Seq: 7, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionTransport,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	origin := runtimedb.Connection{Seq: 8} // no readable classification
	candidate := runtimedb.SessionCandidate{SessionID: "bs-42", ConnectionRef: 8, Connection: &origin}
	if _, visible := candidateOnConnection(candidate, externalContinuationConnection(current)); visible {
		t.Fatal("an unreadable classification disclosed a session candidate")
	}
}

func TestCandidateFieldsAreFilteredIndependently(t *testing.T) {
	current := runtimedb.Connection{Seq: 7, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionTransport,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	origin := runtimedb.Connection{Seq: 8, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionEndToEnd,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	shown, visible := candidateOnConnection(runtimedb.SessionCandidate{Connection: &origin}, externalContinuationConnection(current))
	if !visible || shown.ShowSubject || !shown.ShowTime {
		t.Fatalf("disclosure = visible %v subject %v time %v", visible, shown.ShowSubject, shown.ShowTime)
	}
}
