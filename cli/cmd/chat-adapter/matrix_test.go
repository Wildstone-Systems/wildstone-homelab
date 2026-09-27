package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A recorded /sync body: one direct room (two members), one shared room
// (four), one of the bot's own messages echoed back, and one non-text
// event.
const matrixSync = `{
 "next_batch":"s72_1",
 "rooms":{"join":{
   "!direct:example.org":{
     "summary":{"m.joined_member_count":2},
     "timeline":{"events":[
       {"type":"m.room.message","event_id":"$e1","sender":"@sam:example.org",
        "content":{"msgtype":"m.text","body":"did anyone have breakfast?"}},
       {"type":"m.room.message","event_id":"$e2","sender":"@butler:example.org",
        "content":{"msgtype":"m.text","body":"Breakfast was logged at 07:15."}}
     ]}},
   "!kitchen:example.org":{
     "summary":{"m.joined_member_count":4},
     "timeline":{"events":[
       {"type":"m.room.message","event_id":"$e3","sender":"@mika:example.org",
        "content":{"msgtype":"m.text","body":"whose turn for the bins?"}},
       {"type":"m.reaction","event_id":"$e4","sender":"@sam:example.org","content":{}}
     ]}}
 }}}`

func matrixServer(t *testing.T, sync string) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@butler:example.org"}`)) //nolint:errcheck // test fake
		case strings.Contains(r.URL.Path, "/sync"):
			if strings.Contains(r.URL.RawQuery, "since=") {
				w.Write([]byte(sync)) //nolint:errcheck // test fake
				return
			}
			// The first sync takes a cursor and no backlog.
			w.Write([]byte(`{"next_batch":"s72_0","rooms":{"join":{}}}`)) //nolint:errcheck
		default:
			w.Write([]byte(`{"event_id":"$sent"}`)) //nolint:errcheck // test fake
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

// The second transport maps into the SAME Incoming the first does — the
// point of G5: the contract above the driver did not move.
func TestMatrixReceiveMapping(t *testing.T) {
	srv, paths := matrixServer(t, matrixSync)
	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}

	// First poll: cursor only, no backlog. A bot that starts up and
	// answers a week of history is a bot nobody invites back.
	first, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	if len(first) != 0 {
		t.Fatalf("first sync relayed backlog: %+v", first)
	}
	if m.userID != "@butler:example.org" {
		t.Fatalf("userID = %q, want the whoami answer", m.userID)
	}

	msgs, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("msgs = %+v, want the two messages from other people", msgs)
	}
	byID := map[string]Incoming{}
	for _, m := range msgs {
		byID[m.MessageID] = m
	}
	direct, ok := byID["$e1"]
	if !ok || direct.Group {
		t.Fatalf("two-member room was not treated as direct: %+v", direct)
	}
	if direct.Account != "@sam:example.org" || direct.Conversation != "!direct:example.org" {
		t.Fatalf("direct message mapped as %+v", direct)
	}
	group, ok := byID["$e3"]
	if !ok || !group.Group {
		t.Fatalf("four-member room was not treated as a group: %+v", group)
	}
	// The bot's own message and the reaction were both skipped: one
	// would have it answering itself forever, the other is not text.
	if _, echoed := byID["$e2"]; echoed {
		t.Fatalf("the adapter relayed its own message back to itself")
	}
	if _, reaction := byID["$e4"]; reaction {
		t.Fatalf("a non-text event was relayed")
	}
	if !strings.Contains(strings.Join(*paths, " "), "since=s72_0") {
		t.Fatalf("the second sync did not carry the cursor: %v", *paths)
	}
}

// A room whose membership cannot be read is treated as a GROUP: the
// group rules are more careful, not less, so erring that way is the safe
// direction (ADR-W026 A7).
func TestMatrixUnknownMembershipIsAGroup(t *testing.T) {
	const noSummary = `{"next_batch":"s2","rooms":{"join":{"!x:example.org":{"timeline":{"events":[
	  {"type":"m.room.message","event_id":"$e9","sender":"@sam:example.org",
	   "content":{"msgtype":"m.text","body":"hello"}}]}}}}}`
	srv, _ := matrixServer(t, noSummary)
	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	msgs, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 1 || !msgs[0].Group {
		t.Fatalf("msgs = %+v, want one message treated as a group", msgs)
	}
}

func TestMatrixAcceptsInvitedRoomsBeforeAdvancingTheCursor(t *testing.T) {
	var joined []string
	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@butler:example.org"}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/sync"):
			w.Write([]byte(`{"next_batch":"after-invite","rooms":{"invite":{"!new:example.org":{"invite_state":{"events":[]}}}}}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/join/"):
			joined = append(joined, r.URL.Path)
			authorization = r.Header.Get("Authorization")
			if r.Method != http.MethodPost {
				t.Errorf("join method = %s, want POST", r.Method)
			}
			w.Write([]byte(`{"room_id":"!new:example.org"}`)) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	msgs, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("invitation produced messages before the room was joined: %+v", msgs)
	}
	if len(joined) != 1 || !strings.Contains(joined[0], "/join/!new:example.org") {
		t.Fatalf("join requests = %v, want the invited room", joined)
	}
	if authorization != "Bearer tok" {
		t.Fatalf("join authorization = %q", authorization)
	}
	if m.since != "after-invite" {
		t.Fatalf("cursor = %q, want after-invite after a successful join", m.since)
	}
}

func TestMatrixRetriesInvitationWhenJoiningFails(t *testing.T) {
	joinAttempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@butler:example.org"}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/sync"):
			w.Write([]byte(`{"next_batch":"after-invite","rooms":{"invite":{"!new:example.org":{}}}}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/join/"):
			joinAttempts++
			if joinAttempts == 1 {
				http.Error(w, "try again", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte(`{"room_id":"!new:example.org"}`)) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if _, err := m.Receive(context.Background()); err == nil {
		t.Fatal("Receive succeeded despite failed room join")
	}
	if m.since != "" {
		t.Fatalf("cursor advanced to %q after failed join", m.since)
	}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatalf("retry Receive: %v", err)
	}
	if joinAttempts != 2 || m.since != "after-invite" {
		t.Fatalf("join attempts = %d, cursor = %q; want retry and then advancement", joinAttempts, m.since)
	}
}

func TestMatrixReportsEncryptedRoomsOnceInsteadOfFailingSilently(t *testing.T) {
	const encrypted = `{"next_batch":"s2","rooms":{"join":{"!secret:example.org":{
	  "summary":{"m.joined_member_count":2},"timeline":{"events":[
	  {"type":"m.room.encrypted","event_id":"$cipher","sender":"@sam:example.org","content":{"ciphertext":"opaque"}}
	]}}}}}`
	srv, _ := matrixServer(t, encrypted)
	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	msgs, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("encrypted Receive: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Problem == "" || msgs[0].Conversation != "!secret:example.org" {
		t.Fatalf("encrypted event = %+v, want one room diagnostic", msgs)
	}
	again, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("repeated encrypted Receive: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("encrypted room repeated its diagnostic: %+v", again)
	}
}

func TestMatrixSendIsIdempotentPerMessage(t *testing.T) {
	var sentPaths []string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentPaths = append(sentPaths, r.URL.Path)
		json.NewDecoder(r.Body).Decode(&body)   //nolint:errcheck // test fake
		w.Write([]byte(`{"event_id":"$sent"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if err := m.Send(context.Background(), "!kitchen:example.org", "the bins go out tonight"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := m.Send(context.Background(), "!kitchen:example.org", "and the recycling"); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if body["msgtype"] != "m.text" || body["body"] != "and the recycling" {
		t.Fatalf("sent body = %+v", body)
	}
	// Matrix deduplicates on the transaction id, so two sends must not
	// share one: a retried send is not a second message, and two real
	// messages are not one.
	if len(sentPaths) != 2 || sentPaths[0] == sentPaths[1] {
		t.Fatalf("transaction ids collided: %v", sentPaths)
	}
	// The server sees the decoded path; the request escapes the room id
	// on the way out.
	if !strings.Contains(sentPaths[0], "/rooms/!kitchen:example.org/send/m.room.message/") {
		t.Fatalf("send path = %q", sentPaths[0])
	}
}

func TestMatrixRequiresConfiguration(t *testing.T) {
	if _, err := newMatrix("", "tok", time.Second); err == nil {
		t.Fatalf("an empty homeserver was accepted")
	}
	if _, err := newMatrix("https://example.org", "", time.Second); err == nil {
		t.Fatalf("an empty access token was accepted")
	}
}

// Both drivers satisfy the same interface, and adapter.go holds nothing
// vendor-specific: adding a transport is implementing three methods.
func TestBothTransportsSatisfyTheContract(t *testing.T) {
	var _ Transport = (*telegram)(nil)
	var _ Transport = (*matrix)(nil)
}
