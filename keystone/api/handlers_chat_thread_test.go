package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

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
	h.bindAccount(t, "admin", "op", "telegram", "12345")

	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "household question", "messageId": "m-1",
		"surface": "family",
	})
	h.send(runtimedb.RoleValet, map[string]any{
		"transport": "telegram", "account": "12345", "text": "operator question", "messageId": "m-2",
		"surface": "admin",
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
	h.bindAccount(t, "admin", "op", "telegram", "12345")

	h.send(runtimedb.RoleValet, map[string]any{
		"transport": "telegram", "account": "12345", "text": "first, on telegram",
		"messageId": "m-1", "surface": "admin",
	})
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("sessions after the first message = %d", len(sessions))
	}

	// Continue it from the Console, as the same principal.
	if resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/butler/sessions/"+sessions[0].ID+"/ask",
		map[string]string{"message": "second, in the console"}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("console follow-up: status = %d", resp.StatusCode)
	}

	// Back to Telegram: still the same conversation.
	h.send(runtimedb.RoleValet, map[string]any{
		"transport": "telegram", "account": "12345", "text": "third, on telegram",
		"messageId": "m-2", "surface": "admin",
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

// A group room has several bound members speaking into one
// conversation, and a session is principal-bound — Alice cannot
// continue Bob's. With one pointer per room the speaker changing would
// rewrite it, and BOTH of them would lose their thread on every
// alternate message. Reported by Codex on #109.
func TestGroupMembersKeepSeparateThreads(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("a1", "b1", "a2", "b2"), runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "alice", "telegram", "111")
	h.bindAccount(t, "family", "bob", "telegram", "222")

	// Alice, Bob, Alice, Bob — all in the same room.
	for i, m := range []struct{ account, text string }{
		{"111", "alice one"}, {"222", "bob one"}, {"111", "alice two"}, {"222", "bob two"},
	} {
		resp, out := h.send(runtimedb.RoleConcierge, map[string]any{
			"transport": "telegram", "account": m.account, "conversation": "room-1",
			"group": true, "text": m.text, "messageId": string(rune('a' + i)),
		})
		if resp.StatusCode != http.StatusAccepted || !out.Bound {
			t.Fatalf("message %d: status = %d out = %+v", i, resp.StatusCode, out)
		}
	}

	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want one per member: %+v", len(sessions), sessions)
	}
	// Each member's second message continued their OWN first one.
	for _, s := range sessions {
		var full runtimedb.ButlerSession
		h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions/"+s.ID, nil, &full)
		if len(full.Turns) != 4 {
			t.Fatalf("%s (%s) has %d turns; the other speaker took its thread",
				s.ID, s.ActingFor, len(full.Turns))
		}
	}
}

// Without a way to say "start over", a fresh question inherits the
// previous topic until the idle window expires.
func TestChatResetStartsANewConversation(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("first answer", "second answer"), runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")

	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "first question", "messageId": "m-1",
	})
	_, out := h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "/new", "messageId": "m-2",
	})
	if out.Reason != "answered /new" {
		t.Fatalf("reset reason = %q", out.Reason)
	}
	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "text": "second question", "messageId": "m-3",
	})

	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want the reset to have split them", len(sessions))
	}
	// The reset itself must not have reached the model.
	if got := len(h.scripted(t).convos); got != 2 {
		t.Fatalf("provider saw %d conversations; the reset was sent to it", got)
	}
	// And the reply says the earlier conversation is still recorded.
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	var reset string
	for _, m := range queued {
		if strings.Contains(m.Body, "Starting fresh") {
			reset = m.Body
		}
	}
	if reset == "" || !strings.Contains(reset, "still recorded") {
		t.Fatalf("reset reply = %q", reset)
	}
}

// A message that merely contains the word is a question, not a command.
// Only a leading slash makes one, and the name has to match exactly.
func TestChatCommandParsingIsExact(t *testing.T) {
	for _, in := range []string{"new", "what is new?", "clear the cache", "reset the router", "", "  "} {
		if _, _, ok := parseChatCommand(in); ok {
			t.Errorf("parseChatCommand(%q) claimed a command", in)
		}
	}
	for _, tc := range []struct{ in, name, arg string }{
		{"/new", "new", ""},
		{" /RESET ", "reset", ""},
		{"/plan plan-0f3a91", "plan", "plan-0f3a91"},
		// Telegram addresses a command in a group as /status@thebot.
		{"/status@wildstone_bot", "status", ""},
		{"/newsletter", "newsletter", ""},
	} {
		name, arg, ok := parseChatCommand(tc.in)
		if !ok || name != tc.name || arg != tc.arg {
			t.Errorf("parseChatCommand(%q) = (%q, %q, %v)", tc.in, name, arg, ok)
		}
	}
}
