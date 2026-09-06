package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A recorded Telegram getUpdates response, mapped into the adapter's own
// shape. The mapping is the whole driver, so this is the test that says
// whether "telegram" is wired correctly.
const telegramUpdates = `{"ok":true,"result":[
 {"update_id":101,"message":{"message_id":9,"text":"did anyone have breakfast?",
   "from":{"id":12345,"username":"sam","first_name":"Sam"},
   "chat":{"id":12345,"type":"private"}}},
 {"update_id":102,"message":{"message_id":10,"text":"whose turn for the bins?",
   "from":{"id":777,"first_name":"Mika"},
   "chat":{"id":-99,"type":"supergroup"}}},
 {"update_id":103,"message":{"message_id":11,
   "from":{"id":12345,"username":"sam"},
   "chat":{"id":12345,"type":"private"}}}
]}`

func TestTelegramReceiveMapping(t *testing.T) {
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.String()
		w.Write([]byte(telegramUpdates)) //nolint:errcheck // test fake
	}))
	defer srv.Close()

	tg := &telegram{token: "bot-token", base: srv.URL, timeout: time.Second, http: srv.Client()}
	// The first poll only takes the cursor (see
	// TestTelegramFirstPollTakesTheCursorNotTheBacklog); the mapping is
	// what the polls after it do.
	if _, err := tg.Receive(context.Background()); err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	msgs, err := tg.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	// Three updates, one of them textless (a sticker or a join event) —
	// which is not something to relay.
	if len(msgs) != 2 {
		t.Fatalf("msgs = %+v, want the two text messages", msgs)
	}
	direct := msgs[0]
	if direct.Account != "12345" || direct.Conversation != "12345" || direct.Group {
		t.Fatalf("direct message mapped as %+v", direct)
	}
	if direct.MessageID != "9" || direct.DisplayName != "@sam" {
		t.Fatalf("direct message ids = %+v", direct)
	}
	group := msgs[1]
	if !group.Group {
		t.Fatalf("a supergroup message was not marked as a group: %+v", group)
	}
	// The reply goes to the GROUP, while the account is the person who
	// spoke — that split is what ADR-W026 A7's attribution rests on.
	if group.Account != "777" || group.Conversation != "-99" || group.DisplayName != "Mika" {
		t.Fatalf("group message mapped as %+v", group)
	}

	// The offset advanced past the highest update, so Telegram does not
	// redeliver what was already taken.
	if tg.offset != 104 {
		t.Fatalf("offset = %d, want 104", tg.offset)
	}
	if _, err := tg.Receive(context.Background()); err != nil {
		t.Fatalf("second Receive: %v", err)
	}
	if !strings.Contains(lastPath, "offset=104") {
		t.Fatalf("second poll did not carry the offset: %s", lastPath)
	}
	// Long-polling is the point: the request asks Telegram to hold the
	// connection, so the adapter dials out and nothing dials in.
	if !strings.Contains(lastPath, "timeout=1") {
		t.Fatalf("poll is not long-polling: %s", lastPath)
	}
}

func TestTelegramSend(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck // test fake
		gotBody = r.Form.Encode()
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer srv.Close()

	tg := &telegram{token: "bot-token", base: srv.URL, timeout: time.Second, http: srv.Client()}
	if err := tg.Send(context.Background(), "-99", "the bins go out tonight"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(gotBody, "chat_id=-99") || !strings.Contains(gotBody, "bins") {
		t.Fatalf("sent body = %q", gotBody)
	}
}

// An API error is an error, not a silently dropped message.
func TestTelegramReportsAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	tg := &telegram{token: "wrong", base: srv.URL, timeout: time.Second, http: srv.Client()}
	if _, err := tg.Receive(context.Background()); err == nil {
		t.Fatalf("an unauthorized poll returned no error")
	}
	if err := tg.Send(context.Background(), "1", "hi"); err == nil {
		t.Fatalf("an unauthorized send returned no error")
	}
}

// The token is never a flag: it comes from the environment, and its
// absence is a refusal to start rather than a bot that quietly does
// nothing.
func TestTelegramRequiresAToken(t *testing.T) {
	if _, err := newTelegram("", time.Second); err == nil {
		t.Fatalf("an empty token was accepted")
	}
}

// A restarted adapter must not answer the backlog (ADR-W026 A8).
//
// Telegram holds undelivered updates for 24 hours and hands them all to
// the next getUpdates that carries no offset. Keystone's dedupe key is
// anchored on the message id, but a queued reply EXPIRES — so an adapter
// restarted a day later would re-answer yesterday's questions, and the
// household would get a burst of replies to things they had forgotten
// asking. The Matrix driver takes a cursor and no backlog on its first
// sync; this is the same rule for the same reason.
func TestTelegramFirstPollTakesTheCursorNotTheBacklog(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		w.Write([]byte(telegramUpdates)) //nolint:errcheck // test fake
	}))
	defer srv.Close()

	tg := &telegram{token: "bot-token", base: srv.URL, timeout: time.Second, http: srv.Client()}
	first, err := tg.Receive(context.Background())
	if err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	if len(first) != 0 {
		t.Fatalf("the first poll relayed %d backlogged message(s): %+v", len(first), first)
	}
	// It still advanced past them, so the second poll is live traffic
	// only — and it asked Telegram for just the tail rather than the
	// whole backlog.
	if tg.offset != 104 {
		t.Fatalf("offset = %d, want 104 after taking the cursor", tg.offset)
	}
	if !strings.Contains(paths[0], "offset=-1") || !strings.Contains(paths[0], "timeout=0") {
		t.Fatalf("first poll did not ask for the tail: %s", paths[0])
	}

	msgs, err := tg.Receive(context.Background())
	if err != nil {
		t.Fatalf("second Receive: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("second poll = %+v, want the live messages", msgs)
	}
}
