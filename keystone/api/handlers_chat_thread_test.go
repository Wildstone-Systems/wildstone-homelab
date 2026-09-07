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
	if out.Reason != "started a new conversation" {
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
func TestChatResetIsExactMatchOnly(t *testing.T) {
	for _, s := range []string{"/new", "/reset", "/clear", " /NEW ", "/New"} {
		if !isChatReset(s) {
			t.Errorf("isChatReset(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"new", "what is new?", "/newsletter", "clear the cache", "reset the router"} {
		if isChatReset(s) {
			t.Errorf("isChatReset(%q) = true, want false", s)
		}
	}
}
