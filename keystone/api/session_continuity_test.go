package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerlive"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/portal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestContinuationRetainsEarlierDisclosureRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		encryption  string
		retention   string
		builtIn     bool
		unresolved  bool
		visible     bool
		showSubject bool
		showTime    bool
	}{
		{name: "protected subject", encryption: "end_to_end", retention: "provider", visible: true, showTime: true},
		{name: "protected activity", encryption: "transport", retention: "none", visible: true, showSubject: true},
		{name: "trusted origin", builtIn: true},
		{name: "unresolved origin", unresolved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.person("sam")
			origin := h.agentAt(t, "family", "matrix")
			current := h.agentAt(t, "family", "telegram")
			if _, err := h.s.DB.ExecForTest(`UPDATE connection_disclosure SET reach='external', encryption='transport', retention='provider', surface='external_messaging' WHERE connection=?`, current.Seq); err != nil {
				t.Fatal(err)
			}
			var err error
			current, err = h.s.DB.ChatConnectionBySeq(current.Seq)
			if err != nil {
				t.Fatal(err)
			}
			id := insertCandidateSession(t, h, origin, "sam", "protected original subject")
			switch {
			case tc.builtIn:
				err = h.s.DB.NoteButlerSessionConnection(id, "inbound", butlerlive.ConsoleConnection, 0)
			case tc.unresolved:
				_, err = h.s.DB.ExecForTest(`DELETE FROM connection_disclosure WHERE connection=?`, origin.Seq)
			default:
				_, err = h.s.DB.ExecForTest(`UPDATE connection_disclosure SET reach='external', encryption=?, retention=?, surface='external_messaging' WHERE connection=?`, tc.encryption, tc.retention, origin.Seq)
			}
			if err != nil {
				t.Fatal(err)
			}
			for turn := 0; turn < 3; turn++ {
				if turn > 0 {
					if err := h.s.DB.AppendButlerTurnsFrom(id, []runtimedb.ButlerTurn{{Role: runtimedb.ButlerRoleUser, Text: "continued on weaker connection"}}, current.Name(), current.Seq); err != nil {
						t.Fatal(err)
					}
				}
				candidates, protected, err := h.s.chatSessionCandidates(chatCommandCtx{At: current, Identity: "sam", Conversation: "12345"})
				if err != nil {
					t.Fatal(err)
				}
				if !tc.visible {
					if len(candidates) != 0 || !protected {
						t.Fatalf("turn %d: protected origin became visible: %+v", turn, candidates)
					}
					continue
				}
				if len(candidates) != 1 {
					t.Fatalf("turn %d: candidates = %+v", turn, candidates)
				}
				c := candidates[0]
				if c.ShowSubject != tc.showSubject || c.ShowTime != tc.showTime {
					t.Fatalf("turn %d: subject=%v time=%v", turn, c.ShowSubject, c.ShowTime)
				}
				if strings.Contains(renderCandidate(c, "choice"), "protected original subject") != tc.showSubject {
					t.Fatalf("turn %d: rendered subject disclosure changed", turn)
				}
				if turn > 0 && len(c.Provenance) != 2 {
					t.Fatalf("turn %d: distinct origins = %d", turn, len(c.Provenance))
				}
			}
		})
	}
}

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

func TestActiveChatPointerRechecksDisclosureBeforeLoadingHistory(t *testing.T) {
	for _, origin := range []string{butlerlive.ConsoleConnection, portal.Connection} {
		t.Run(origin, func(t *testing.T) {
			h := newHarness(t)
			h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
			at := h.agentAt(t, "family", "telegram")
			h.person("sam")
			h.bindAccount(t, "family", "sam", "telegram", "12345")
			id := insertCandidateSession(t, h, at, "sam", "external subject")
			if err := h.s.DB.SetChatThreadSession(at, "12345", "sam", id); err != nil {
				t.Fatal(err)
			}
			ctx := chatCommandCtx{At: at, Identity: "sam", Conversation: "12345"}
			prior, reply, err := h.s.resolveChatContinuation(ctx)
			if err != nil || prior == nil || prior.SessionID != id || reply != "" {
				t.Fatalf("safe active pointer: prior=%+v reply=%q err=%v", prior, reply, err)
			}
			if err := h.s.DB.AppendButlerTurnsFrom(id, []runtimedb.ButlerTurn{{Role: runtimedb.ButlerRoleUser, Text: "trusted-only secret"}}, origin, 0); err != nil {
				t.Fatal(err)
			}
			prior, reply, err = h.s.resolveChatContinuation(ctx)
			if err != nil || prior != nil || reply != trustedSurfaceRedirect {
				t.Fatalf("protected active pointer: prior=%+v reply=%q err=%v", prior, reply, err)
			}
			// The same person's own one-to-one chat on a Connection they
			// bound is verified, and continues it: the conversation is
			// theirs, and nobody else reads the reply.
			binding, err := h.s.DB.LookupChatBinding(at, "12345")
			if err != nil {
				t.Fatal(err)
			}
			verified := chatCommandCtx{At: at, Identity: "sam", Conversation: "12345", Binding: binding}
			prior, reply, err = h.s.resolveChatContinuation(verified)
			if err != nil || prior == nil || prior.SessionID != id || reply != "" {
				t.Fatalf("verified active pointer: prior=%+v reply=%q err=%v", prior, reply, err)
			}
			// A room is never verified, however its speaker is bound.
			room := verified
			room.Group = true
			if prior, reply, err = h.s.resolveChatContinuation(room); err != nil || prior != nil || reply != trustedSurfaceRedirect {
				t.Fatalf("group chat continued a protected conversation: prior=%+v reply=%q err=%v", prior, reply, err)
			}
		})
	}
}

func TestTrustedConversationExistenceIsHiddenFromExternalMessaging(t *testing.T) {
	current := runtimedb.Connection{Seq: 7, Disclosure: runtimedb.Disclosure{
		Reach: runtimedb.ReachExternal, Encryption: runtimedb.EncryptionTransport,
		Retention: runtimedb.RetentionProvider, Surface: runtimedb.TrustExternalMessaging,
	}}
	candidate := runtimedb.SessionCandidate{
		SessionID: "bs-42", Subject: "the private homepage conversation",
		Provenance: []runtimedb.SessionConnection{{ConnectionRef: 0, ConnectionName: butlerlive.ConsoleConnection}},
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
	candidate := runtimedb.SessionCandidate{SessionID: "bs-42", Provenance: []runtimedb.SessionConnection{{ConnectionRef: 8, Connection: &origin}}}
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
	shown, visible := candidateOnConnection(runtimedb.SessionCandidate{Provenance: []runtimedb.SessionConnection{{Connection: &origin}}}, externalContinuationConnection(current))
	if !visible || shown.ShowSubject || !shown.ShowTime {
		t.Fatalf("disclosure = visible %v subject %v time %v", visible, shown.ShowSubject, shown.ShowTime)
	}
}

func TestContinuationContentRequiresAllDisclosureClassifications(t *testing.T) {
	for _, classification := range []string{"compatible", "encryption", "retention", "unknown", "trusted"} {
		t.Run(classification, func(t *testing.T) {
			h := newHarness(t)
			h.person("sam")
			origin := h.agentAt(t, "family", "matrix")
			current := h.agentAt(t, "family", "telegram")
			if _, err := h.s.DB.ExecForTest(`UPDATE connection_disclosure SET reach='external',encryption='transport',retention='provider',surface='external_messaging'`); err != nil {
				t.Fatal(err)
			}
			id := insertCandidateSession(t, h, origin, "sam", "jasmine private transcript")
			var err error
			switch classification {
			case "encryption":
				_, err = h.s.DB.ExecForTest(`UPDATE connection_disclosure SET encryption='end_to_end' WHERE connection=?`, origin.Seq)
			case "retention":
				_, err = h.s.DB.ExecForTest(`UPDATE connection_disclosure SET retention='none' WHERE connection=?`, origin.Seq)
			case "unknown":
				_, err = h.s.DB.ExecForTest(`UPDATE connection_disclosure SET retention='unknown'`)
			case "trusted":
				err = h.s.DB.NoteButlerSessionConnection(id, "inbound", butlerlive.ConsoleConnection, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			current, err = h.s.DB.ChatConnectionBySeq(current.Seq)
			if err != nil {
				t.Fatal(err)
			}
			// Include a latest compatible turn: checking only that turn would
			// erase the restrictions attached to the earlier transcript.
			if err = h.s.DB.NoteButlerSessionConnection(id, "inbound", current.Name(), current.Seq); err != nil {
				t.Fatal(err)
			}
			loaded, allowed, err := h.s.loadChatContinuation(id, current, false)
			if err != nil || allowed != (classification == "compatible") {
				t.Fatalf("allowed=%v err=%v", allowed, err)
			}
			if !allowed && (loaded.ID != "" || len(loaded.Turns) > 0) {
				t.Fatal("denied transcript was loaded")
			}
		})
	}
}

func TestContinuationRejectsAStaleDestination(t *testing.T) {
	for _, change := range []string{"compatible", "reclassified", "revoked", "unclassified"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t)
			h.person("sam")
			origin := h.agentAt(t, "family", "matrix")
			destination := h.agentAt(t, "family", "telegram")
			id := insertCandidateSession(t, h, origin, "sam", "private prior transcript")
			// The destination is deliberately NOT an origin of this session.
			// This is the ingress snapshot that must not authorize history.
			stale, err := h.s.DB.ChatConnectionBySeq(destination.Seq)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "reclassified":
				_, err = h.s.DB.ExecForTest(`UPDATE connection_disclosure SET encryption='end_to_end' WHERE connection=?`, destination.Seq)
			case "revoked":
				_, err = h.s.DB.ExecForTest(`UPDATE butler_addresses SET revoked_at='revoked' WHERE seq=?`, destination.Seq)
			case "unclassified":
				_, err = h.s.DB.ExecForTest(`DELETE FROM connection_disclosure WHERE connection=?`, destination.Seq)
			}
			if err != nil {
				t.Fatal(err)
			}
			loaded, allowed, err := h.s.loadChatContinuation(id, stale, false)
			if err != nil || allowed != (change == "compatible") {
				t.Fatalf("allowed=%v err=%v", allowed, err)
			}
			if !allowed && (loaded.ID != "" || len(loaded.Turns) != 0) {
				t.Fatal("denied continuation returned private history")
			}
		})
	}
}
