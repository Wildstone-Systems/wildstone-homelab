package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func insertCandidateSession(t *testing.T, h *harness, at runtimedb.Connection, person, subject string) string {
	t.Helper()
	id, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{
		Principal: "butler", ActingFor: person, Provider: "test",
		Mode: runtimedb.ButlerModeFull, Surface: at.Surface, Butler: at.Butler,
		FinalText: "earlier answer", Turns: []runtimedb.ButlerTurn{
			{Role: runtimedb.ButlerRoleUser, Text: subject},
			{Role: runtimedb.ButlerRoleAssistant, Text: "earlier answer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.NoteButlerSessionConnection(id, "inbound", at.Name(), at.Seq); err != nil {
		t.Fatal(err)
	}
	return id
}

// A chat destination pointer is not conversation identity. With no pointer at
// all, the same Person reaching the same Agent finds the sole recent session.
func TestChatContinuesTheSolePersonAgentSession(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("continued answer"), runtimedb.ButlerModeFull)
	at := h.agentAt(t, "family", "telegram")
	h.person("sam")
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	want := insertCandidateSession(t, h, at, "sam", "the earlier question")

	resp, _ := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "and now?", "messageId": "m-1",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	sess, err := h.s.DB.GetButlerSession(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Turns) != 4 {
		t.Fatalf("turns = %d, want the sole Person+Agent candidate continued", len(sess.Turns))
	}
}

// Two recent sessions are two contexts the Person kept apart. No context is
// loaded and no model runs until an opaque /resume choice is spent.
func TestAmbiguousChatContinuationOffersOpaqueChoicesBeforeInference(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("must not run"), runtimedb.ButlerModeFull)
	at := h.agentAt(t, "family", "telegram")
	h.person("sam")
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	insertCandidateSession(t, h, at, "sam", "first private subject")
	insertCandidateSession(t, h, at, "sam", "second private subject")

	_, out := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "continue", "messageId": "m-1",
	})
	if !strings.Contains(out.Reason, "safe choice") {
		t.Fatalf("reason = %q", out.Reason)
	}
	if calls := h.providerCalls(t); calls != 0 {
		t.Fatalf("ambiguous continuation loaded context and ran the model %d time(s)", calls)
	}
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 || !strings.Contains(queued[0].Body, "/resume ") ||
		!strings.Contains(queued[0].Body, "first private subject") ||
		!strings.Contains(queued[0].Body, "second private subject") {
		t.Fatalf("choice prompt = %+v", queued)
	}
	if strings.Contains(queued[0].Body, "bs-") {
		t.Fatalf("choice prompt exposed a raw session id: %q", queued[0].Body)
	}
}

func TestSessionPagesDeliverEveryResumeChoiceWithoutTruncation(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("must not run"), runtimedb.ButlerModeFull)
	at := h.agentAt(t, "family", "telegram")
	h.person("sam")
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	want := make(map[string]bool)
	for i := 0; i < 19; i++ {
		// Four-byte runes exercise the queue's byte limit, not just a
		// character limit. The first message follows the automatic path.
		id := insertCandidateSession(t, h, at, "sam", strings.Repeat("😀", 80))
		want[id] = true
	}
	protected := insertCandidateSession(t, h, at, "sam", "protected subject")
	if err := h.s.DB.NoteButlerSessionConnection(protected, "inbound", "console", 0); err != nil {
		t.Fatal(err)
	}
	tokens := regexp.MustCompile("`/resume ([A-Za-z0-9_-]+)`")
	nextPage := regexp.MustCompile("Next page: `/sessions ([0-9]+)`")
	seen := make(map[string]bool)
	var after int64
	text := "continue"
	for page := 1; ; page++ {
		if page > 19 {
			t.Fatal("session pagination never ended")
		}
		h.send(runtimedb.RoleConcierge, map[string]any{
			"transport": "telegram", "account": "12345", "text": text,
			"messageId": fmt.Sprintf("page-%d", page),
		})
		queued := h.poll(t, runtimedb.RoleConcierge, fmt.Sprintf("?after=%d", after))
		if len(queued) != 1 {
			t.Fatalf("page %d queued %d messages", page, len(queued))
		}
		body := queued[0].Body
		after = queued[0].Seq
		if strings.Contains(body, "protected subject") || strings.Contains(body, "bs-") {
			t.Fatalf("page disclosed protected metadata: %q", body)
		}
		choices := tokens.FindAllStringSubmatch(body, -1)
		if len(choices) == 0 || len(body) > runtimedb.MaxChatBodyBytes {
			t.Fatalf("invalid page: %q", body)
		}
		for _, choice := range choices {
			id, err := h.s.DB.ConsumeSessionResumeOffer(resumeTokenHash(choice[1]), "sam", at.Butler, at.Seq)
			if err != nil {
				t.Fatalf("delivered token cannot be selected: %v", err)
			}
			if !want[id] || seen[id] {
				t.Fatalf("page returned an unexpected or repeated session %q", id)
			}
			seen[id] = true
		}
		next := nextPage.FindStringSubmatch(body)
		if next == nil {
			break
		}
		text = "/sessions " + next[1]
	}
	if len(seen) != len(want) {
		t.Fatalf("only %d of %d safe sessions were reachable", len(seen), len(want))
	}
	if calls := h.providerCalls(t); calls != 0 {
		t.Fatalf("listing choices ran the model %d times", calls)
	}
}

func TestSessionPagesRejectInvalidAndUnavailablePages(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	at := h.agentAt(t, "family", "telegram")
	h.person("sam")
	insertCandidateSession(t, h, at, "sam", "recent conversation")
	for _, page := range []string{"0", "-1", "2", "words", "1 2", "9223372036854775807", "999999999999999999999"} {
		t.Run(page, func(t *testing.T) {
			body, err := h.s.chatSessions(chatCommandCtx{At: at, Identity: "sam", Arg: page})
			if err != nil || !strings.Contains(body, "/sessions") || strings.Contains(body, "/resume") {
				t.Fatalf("invalid page response = %q, %v", body, err)
			}
		})
	}
}

func TestRevokedChatThreadStartsFresh(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("fresh answer"), runtimedb.ButlerModeFull)
	at := h.agentAt(t, "family", "telegram")
	h.person("sam")
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	old := insertCandidateSession(t, h, at, "sam", "revoked conversation")
	if err := h.s.DB.SetChatThreadSession(at, "12345", "sam", old); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.RevokeButlerSession(old, "sam"); err != nil {
		t.Fatal(err)
	}

	resp, _ := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "hello again", "messageId": "m-1",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	oldSession, err := h.s.DB.GetButlerSession(old)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldSession.Turns) != 2 {
		t.Fatalf("revoked session gained turns: %+v", oldSession.Turns)
	}
}

// A conversation over chat is a conversation. Before ADR-W027 wired
// this path, every inbound message opened a new session — so asking
// "why?" after an answer reached a Butler that had never heard the
// first question.
func TestChatMessagesContinueOneConversation(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("Two nodes: n1 and n2.", "n2 went offline at 14:04."), runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")

	for i, text := range []string{"what nodes do I have?", "is either of them down?"} {
		resp, out := h.send(runtimedb.RoleConcierge, map[string]any{
			"transport": "telegram", "account": "12345", "text": text,
			"messageId": string(rune('a' + i)),
		})
		if resp.StatusCode != http.StatusAccepted || !out.Bound {
			t.Fatalf("message %d: status = %d out = %+v", i, resp.StatusCode, out)
		}
	}

	// One session, four turns — not two sessions of two.
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want the second message to continue the first", len(sessions))
	}
	var sess runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/"+sessions[0].ID, nil, &sess)
	if len(sess.Turns) != 4 {
		t.Fatalf("turns = %d, want 4: %+v", len(sess.Turns), sess.Turns)
	}

	// And the model was actually told what was said before.
	convos := h.scripted(t).convos
	if len(convos) != 2 {
		t.Fatalf("provider saw %d conversations, want 2", len(convos))
	}
	second := convos[1]
	if len(second) != 3 || second[0].Text != "what nodes do I have?" || second[1].Text != "Two nodes: n1 and n2." {
		t.Fatalf("follow-up replayed %+v", second)
	}
}

// A Telegram direct chat is identified by the PERSON's id, which is the
// same value for the household bot and the operator bot. Threading on
// (transport, conversation) alone would let a household question
// continue an operator conversation.
func TestChatThreadsAreNotSharedAcrossSurfaces(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("family answer", "admin answer"), runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	h.bindAccount(t, "admin", "mika", "telegram", "12345")

	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "household question", "messageId": "m-1",
	})
	h.send(runtimedb.RoleValet, map[string]any{
		"transport": "telegram", "account": "12345", "text": "operator question", "messageId": "m-2",
	})

	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want one per surface: %+v", len(sessions), sessions)
	}
	for _, s := range sessions {
		var full runtimedb.ButlerSession
		h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/"+s.ID, nil, &full)
		if len(full.Turns) != 2 {
			t.Fatalf("session %s has %d turns; a surface picked up the other's thread", s.ID, len(full.Turns))
		}
	}
}

// One conversation, two front doors. A thread carried on in the Console
// must not be called stale because Telegram has been quiet — they are
// the same conversation, and idleness is a property of it rather than
// of the transport that last touched it.
func TestConsoleActivityKeepsAChatThreadAlive(t *testing.T) {
	h := newHarness(t)
	h.wireButler(answerTurns("from telegram", "from the console", "back on telegram"), runtimedb.ButlerModeFull)
	h.bindAccount(t, "admin", "mika", "telegram", "12345")

	h.send(runtimedb.RoleValet, map[string]any{
		"transport": "telegram", "account": "12345", "text": "first, on telegram",
		"messageId": "m-1",
	})
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("sessions after the first message = %d", len(sessions))
	}

	// Continue it from the Console, as the same PERSON. That is what
	// carries the conversation across Connections (ADR-W033 B10): the
	// Telegram binding and the Console session name mika, and the
	// transport is not the session identity.
	if resp := h.doToken(h.console(runtimedb.RoleOperator, "mika"), "POST",
		"/api/v1/butler/sessions/"+sessions[0].ID+"/ask",
		map[string]string{"message": "second, in the console"}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("console follow-up: status = %d", resp.StatusCode)
	}

	// Back to Telegram: still the same conversation.
	h.send(runtimedb.RoleValet, map[string]any{
		"transport": "telegram", "account": "12345", "text": "third, on telegram",
		"messageId": "m-2",
	})

	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want the conversation to have survived the round trip", len(sessions))
	}
	var full runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/"+sessions[0].ID, nil, &full)
	if len(full.Turns) != 6 {
		t.Fatalf("turns = %d, want 6 across both surfaces", len(full.Turns))
	}
	// And the last Telegram turn was told about the Console one.
	convos := h.scripted(t).convos
	last := convos[len(convos)-1]
	var sawConsole bool
	for _, m := range last {
		if m.Text == "second, in the console" {
			sawConsole = true
		}
	}
	if !sawConsole {
		t.Fatalf("the telegram follow-up did not replay the console turn: %+v", last)
	}
}

// A room opens no session for anybody (design §8.3).
//
// THIS TEST USED TO PROVE SOMETHING ELSE, and the thing it proved is
// worth keeping sight of. A group room has several bound members
// speaking into one conversation, and a session is principal-bound —
// Alice cannot continue Bob's. With one thread pointer per ROOM the
// speaker changing would rewrite it, and both of them would lose their
// thread on every alternate message. Codex reported that on #109 and
// the fix was to key the pointer on the person as well.
//
// Slice 11's disclosure half makes the room unreachable, so that bug
// is no longer approachable from here — which is not the same as the
// invariant no longer mattering. It still holds, in chat_threads'
// primary key, and it is pinned where it lives now:
// TestTwoPeopleInOneConversationKeepSeparateThreads in runtimedb.
// What this test asserts is the new, stronger property that replaced
// it: four addressed messages from two admitted members produce four
// redirects and zero sessions.
func TestAGroupRoomOpensNoSessionForAnybody(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("a1", "b1", "a2", "b2"), runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "alice", "telegram", "111")
	h.bindAccount(t, "family", "bob", "telegram", "222")

	// Alice, Bob, Alice, Bob — all in the same room, all addressed to
	// the bot, all from people it knows.
	for i, m := range []struct{ account, text string }{
		{"111", "alice one"}, {"222", "bob one"}, {"111", "alice two"}, {"222", "bob two"},
	} {
		resp, out := h.send(runtimedb.RoleConcierge, map[string]any{
			"transport": "telegram", "account": m.account, "conversation": "room-1",
			"group": true, "addressed": true, "text": m.text, "messageId": string(rune('a' + i)),
		})
		if resp.StatusCode != http.StatusAccepted || !out.Bound {
			t.Fatalf("message %d: status = %d out = %+v", i, resp.StatusCode, out)
		}
	}

	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 0 {
		t.Fatalf("a room opened %d session(s): %+v", len(sessions), sessions)
	}
	// Each speaker was redirected rather than ignored. Silence would
	// read as a broken bot to two people who did nothing wrong.
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 4 {
		t.Fatalf("four addressed messages produced %d replies: %+v", len(queued), queued)
	}
	for _, q := range queued {
		if q.Target != "room-1" || !strings.Contains(q.Body, "one-to-one") {
			t.Fatalf("reply = %+v, want a redirect into the room", q)
		}
	}
}
