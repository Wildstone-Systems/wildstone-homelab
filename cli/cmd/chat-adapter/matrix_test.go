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

func waitForMatrixInvites(t *testing.T, m *matrix) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.inviteMu.Lock()
		joining := len(m.joiningInvites)
		m.inviteMu.Unlock()
		if joining == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for Matrix invitation worker")
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
	waitForMatrixInvites(t, m)
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
	syncs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@butler:example.org"}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/sync"):
			syncs++
			if syncs == 1 {
				w.Write([]byte(`{"next_batch":"after-invite","rooms":{"invite":{"!new:example.org":{}}}}`)) //nolint:errcheck
				return
			}
			w.Write([]byte(`{"next_batch":"after-retry","rooms":{"join":{}}}`)) //nolint:errcheck
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
	problems, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("asynchronous join returned immediate diagnostics: %+v", problems)
	}
	if m.since != "after-invite" {
		t.Fatalf("cursor = %q after failed join, want it advanced", m.since)
	}
	waitForMatrixInvites(t, m)
	problems, err = m.Receive(context.Background())
	if err != nil {
		t.Fatalf("retry Receive: %v", err)
	}
	if len(problems) != 1 || problems[0].Problem == "" {
		t.Fatalf("failed join diagnostics = %+v, want one", problems)
	}
	waitForMatrixInvites(t, m)
	if joinAttempts != 2 || m.since != "after-retry" {
		t.Fatalf("join attempts = %d, cursor = %q; want retry and then advancement", joinAttempts, m.since)
	}
}

func TestMatrixFailedInvitationDoesNotStarveJoinedRooms(t *testing.T) {
	const sync = `{"next_batch":"next","rooms":{
	  "invite":{"!unreachable:example.org":{}},
	  "join":{"!working:example.org":{"summary":{"m.joined_member_count":2},"timeline":{"events":[
	    {"type":"m.room.message","event_id":"$hello","sender":"@sam:example.org","content":{"msgtype":"m.text","body":"hello"}}
	  ]}}}}}`
	joinStarted := make(chan struct{}, 1)
	releaseJoin := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseJoin)
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@butler:example.org"}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/sync"):
			w.Write([]byte(sync)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/join/"):
			joinStarted <- struct{}{}
			<-releaseJoin
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client(), since: "before"}
	msgs, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 1 || msgs[0].MessageID != "$hello" {
		t.Fatalf("messages = %+v, want joined-room message without waiting for the join", msgs)
	}
	if m.since != "next" {
		t.Fatalf("cursor = %q, want next", m.since)
	}
	select {
	case <-joinStarted:
	case <-time.After(time.Second):
		t.Fatal("background invitation join did not start")
	}
	close(releaseJoin)
	released = true
	waitForMatrixInvites(t, m)
}

func TestMatrixDoesNotRetryPermanentInvitationFailure(t *testing.T) {
	syncs := 0
	joinAttempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@butler:example.org"}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/sync"):
			syncs++
			if syncs == 1 {
				w.Write([]byte(`{"next_batch":"one","rooms":{"invite":{"!forbidden:example.org":{}}}}`)) //nolint:errcheck
				return
			}
			w.Write([]byte(`{"next_batch":"later","rooms":{}}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/join/"):
			joinAttempts++
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	waitForMatrixInvites(t, m)
	problems, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("diagnostic Receive: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Problem, "will not be retried") {
		t.Fatalf("permanent failure diagnostics = %+v", problems)
	}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatalf("later Receive: %v", err)
	}
	if joinAttempts != 1 {
		t.Fatalf("permanent failure join attempts = %d, want 1", joinAttempts)
	}
}

func TestMatrixDropsWithdrawnInvitationBeforeQueuedJoin(t *testing.T) {
	syncs := 0
	blockedStarted := make(chan struct{}, 1)
	releaseBlocked := make(chan struct{})
	withdrawnJoined := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@butler:example.org"}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/sync"):
			syncs++
			if syncs == 1 {
				w.Write([]byte(`{"next_batch":"one","rooms":{"invite":{"!blocked:example.org":{},"!withdrawn:example.org":{}}}}`)) //nolint:errcheck
				return
			}
			w.Write([]byte(`{"next_batch":"two","rooms":{"leave":{"!withdrawn:example.org":{}}}}`)) //nolint:errcheck
		case strings.Contains(r.URL.Path, "/join/!blocked:"):
			blockedStarted <- struct{}{}
			<-releaseBlocked
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case strings.Contains(r.URL.Path, "/join/!withdrawn:"):
			withdrawnJoined <- struct{}{}
			w.Write([]byte(`{"room_id":"!withdrawn:example.org"}`)) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	select {
	case <-blockedStarted:
	case <-time.After(time.Second):
		t.Fatal("first queued join did not start")
	}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatalf("withdrawal Receive: %v", err)
	}
	close(releaseBlocked)
	waitForMatrixInvites(t, m)
	select {
	case <-withdrawnJoined:
		t.Fatal("adapter attempted to join a withdrawn invitation")
	default:
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

// An application-service bot (ADR-W033 B7.2) registers its own user before
// it asks who it is, accepts that the user already exists, and names
// itself only when its name is wrong.
func TestMatrixAppserviceBotRegistersAndNamesItself(t *testing.T) {
	for _, tc := range []struct {
		name          string
		registerCode  int
		registerBody  string
		currentName   string
		wantErr       bool
		wantNameWrite bool
	}{
		{"first start", 200, `{"user_id":"@wildstone-joan:example.org"}`, "wildstone-joan", false, true},
		{"restart", 400, `{"errcode":"M_USER_IN_USE","error":"User ID already taken."}`, "Joan", false, false},
		{"refused", 403, `{"errcode":"M_EXCLUSIVE","error":"This user ID is reserved by an application service."}`, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registered map[string]any
			var nameWritten string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/_matrix/client/v3/register":
					json.NewDecoder(r.Body).Decode(&registered) //nolint:errcheck // test fake
					w.WriteHeader(tc.registerCode)
					w.Write([]byte(tc.registerBody)) //nolint:errcheck // test fake
				case strings.HasSuffix(r.URL.Path, "/account/whoami"):
					// The registration's token alone is its sender.
					w.Write([]byte(`{"user_id":"@wildstone-joan-as:example.org"}`)) //nolint:errcheck // test fake
				case strings.HasSuffix(r.URL.Path, "/displayname") && r.URL.Query().Get("user_id") != "@wildstone-joan:example.org":
					t.Errorf("display name %s not made as the bot: %s", r.Method, r.URL)
				case strings.HasSuffix(r.URL.Path, "/displayname") && r.Method == http.MethodGet:
					json.NewEncoder(w).Encode(map[string]string{"displayname": tc.currentName}) //nolint:errcheck // test fake
				case strings.HasSuffix(r.URL.Path, "/displayname") && r.Method == http.MethodPut:
					var body map[string]string
					json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test fake
					nameWritten = body["displayname"]
					w.Write([]byte(`{}`)) //nolint:errcheck // test fake
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			m := &matrix{homeserver: srv.URL, token: "as-tok", timeout: time.Second, http: srv.Client(),
				appserviceLocalpart: "wildstone-joan", displayName: "Joan"}

			id, _, err := m.Identify(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("a refused registration was treated as success")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id != "@wildstone-joan:example.org" {
				t.Errorf("Identify = %q", id)
			}
			if registered["type"] != "m.login.application_service" || registered["username"] != "wildstone-joan" || registered["inhibit_login"] != true {
				t.Errorf("registration request = %v", registered)
			}
			if tc.wantNameWrite && nameWritten != "Joan" {
				t.Errorf("display name written = %q, want Joan", nameWritten)
			}
			if !tc.wantNameWrite && nameWritten != "" {
				t.Errorf("display name rewritten to %q although it was already right", nameWritten)
			}
		})
	}
}

// A bot on somebody else's homeserver never tries to register itself.
func TestMatrixTokenBotDoesNotRegister(t *testing.T) {
	srv, paths := matrixServer(t, matrixSync)
	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if _, _, err := m.Identify(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range *paths {
		if strings.Contains(p, "/register") || strings.Contains(p, "/displayname") {
			t.Errorf("a token bot called %s", p)
		}
	}
}

// Synapse refuses /sync to a registration's sender, so an
// application-service bot acts as its own user on every request after it
// has learned the server name: sync, join, send and media alike.
func TestMatrixAppserviceBotActsAsItsUser(t *testing.T) {
	var unmasked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/register":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"errcode":"M_USER_IN_USE"}`)) //nolint:errcheck // test fake
			return
		case strings.HasSuffix(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@wildstone-joan-as:example.org"}`)) //nolint:errcheck // test fake
			return
		}
		if r.URL.Query().Get("user_id") != "@wildstone-joan:example.org" {
			unmasked = append(unmasked, r.Method+" "+r.URL.String())
		}
		switch {
		case strings.Contains(r.URL.Path, "/sync"):
			w.Write([]byte(`{"next_batch":"s1","rooms":{"join":{}}}`)) //nolint:errcheck // test fake
		default:
			w.Write([]byte(`{"event_id":"$sent"}`)) //nolint:errcheck // test fake
		}
	}))
	defer srv.Close()
	m := &matrix{homeserver: srv.URL, token: "as-tok", timeout: time.Second, http: srv.Client(),
		appserviceLocalpart: "wildstone-joan"}
	if id, _, err := m.Identify(context.Background()); err != nil || id != "@wildstone-joan:example.org" {
		t.Fatalf("Identify = %q, %v; want the bot, not the registration's sender", id, err)
	}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(context.Background(), "!room:example.org", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.download(context.Background(), "/_matrix/client/v1/media/download/example.org/abc", 1<<10); err != nil {
		t.Fatal(err)
	}
	if len(unmasked) != 0 {
		t.Errorf("requests made as the registration's sender, which Synapse refuses to sync: %v", unmasked)
	}
}

// Synapse sends a room summary only to a client that lazy-loads members,
// and only when membership changes — the first sync, a join. A message in
// a quiet private chat arrives with no summary at all, and must still be
// answered as the private chat it is: that is how a Person's key reaches
// Keystone without their having to mention the bot.
func TestMatrixPrivateChatSurvivesASyncWithoutASummary(t *testing.T) {
	var filters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.Write([]byte(`{"user_id":"@butler:example.org"}`)) //nolint:errcheck // test fake
		case strings.Contains(r.URL.Path, "/sync"):
			filters = append(filters, r.URL.Query().Get("filter"))
			if !strings.Contains(r.URL.RawQuery, "since=") {
				// The initial sync: the only summary the direct room gets.
				w.Write([]byte(`{"next_batch":"s1","rooms":{"join":{
				  "!direct:example.org":{"summary":{"m.joined_member_count":2},"timeline":{"events":[]}},
				  "!unknown:example.org":{"timeline":{"events":[]}}}}}`)) //nolint:errcheck // test fake
				return
			}
			w.Write([]byte(`{"next_batch":"s2","rooms":{"join":{
			  "!direct:example.org":{"timeline":{"events":[
			    {"type":"m.room.message","event_id":"$k","sender":"@sam:example.org","content":{"msgtype":"m.text","body":"KEY-123"}}]}},
			  "!unknown:example.org":{"timeline":{"events":[
			    {"type":"m.room.message","event_id":"$u","sender":"@sam:example.org","content":{"msgtype":"m.text","body":"hello"}}]}}}}}`)) //nolint:errcheck // test fake
		}
	}))
	defer srv.Close()
	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if _, err := m.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := m.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	addressed := map[string]bool{}
	for _, in := range got {
		addressed[in.Conversation] = in.Addressed
	}
	if !addressed["!direct:example.org"] {
		t.Error("a message in a known two-person room, sent without a summary, was treated as a group chat and ignored")
	}
	if addressed["!unknown:example.org"] {
		t.Error("a room whose size was never reported was treated as private; unknown must stay a group")
	}
	for _, f := range filters {
		if !strings.Contains(f, `"lazy_load_members":true`) {
			t.Errorf("sync filter %q does not lazy-load members, so Synapse sends no summary", f)
		}
	}
}
