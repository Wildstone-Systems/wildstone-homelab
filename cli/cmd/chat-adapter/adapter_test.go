package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTransportProblemIsLoggedAndNeverRelayed(t *testing.T) {
	var logs bytes.Buffer
	a := &adapter{
		transport: &fakeTransport{},
		log:       slog.New(slog.NewTextHandler(&logs, nil)),
	}
	// keystone is deliberately nil: handling a transport problem must return
	// before any user content or synthetic substitute can cross that boundary.
	a.handle(context.Background(), Incoming{
		Conversation: "!secret:example.org",
		MessageID:    "$cipher",
		Problem:      "Matrix end-to-end encryption is not supported",
	})
	got := logs.String()
	if !strings.Contains(got, "message cannot be handled") || !strings.Contains(got, "end-to-end encryption") {
		t.Fatalf("diagnostic log = %q", got)
	}
}

// fakeTransport stands in for a chat vendor: queued incoming messages,
// recorded outgoing ones. No network, no vendor.
type fakeTransport struct {
	mu           sync.Mutex
	incoming     [][]Incoming
	sent         []sentMessage
	sendErr      error
	advertised   [][]Command
	advertiseErr error
}

type sentMessage struct{ conversation, body string }

func (f *fakeTransport) Name() string { return "faketransport" }

func (f *fakeTransport) Advertise(_ context.Context, cmds []Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advertised = append(f.advertised, cmds)
	return f.advertiseErr
}

func (f *fakeTransport) menus() [][]Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]Command(nil), f.advertised...)
}

func (f *fakeTransport) Identify(context.Context) (string, string, error) {
	return "fakebot", "Fake Bot", nil
}

func (f *fakeTransport) Receive(ctx context.Context) ([]Incoming, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.incoming) == 0 {
		// A long poll that found nothing: the normal quiet case.
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Millisecond):
		}
		return nil, nil
	}
	batch := f.incoming[0]
	f.incoming = f.incoming[1:]
	return batch, nil
}

func (f *fakeTransport) Send(_ context.Context, conversation, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, sentMessage{conversation, body})
	return nil
}

func (f *fakeTransport) sentMessages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

// fakeKeystone implements the three contract endpoints and records what
// the adapter sent, so a test can assert on the WIRE rather than on the
// adapter's internals.
type fakeKeystone struct {
	mu        sync.Mutex
	inbound   []keystoneInbound
	redeems   []map[string]string
	acked     []int64
	pending   []outboundMessage
	issued    []outboundMessage
	denyReady bool
	rejectID  string
	// registered is every self-registration the adapter posted.
	registered []map[string]string
	// commands is what registration answers with — the surface's menu.
	commands []Command
	// speech is each message's reply read aloud; spoken is every seq
	// the adapter asked for, in order.
	speech map[int64][]byte
	spoken []int64
}

func (f *fakeKeystone) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/chat/messages", func(w http.ResponseWriter, r *http.Request) {
		var msg keystoneInbound
		json.NewDecoder(r.Body).Decode(&msg) //nolint:errcheck // test fake
		f.mu.Lock()
		f.inbound = append(f.inbound, msg)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"accepted":true,"bound":true,"queued":1}`)) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/chat/bindings/redeem", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test fake
		f.mu.Lock()
		f.redeems = append(f.redeems, body)
		reject := body["code"] == f.rejectID
		f.mu.Unlock()
		if reject {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"that code is not valid"}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`)) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/chat/adapters/self", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test fake
		f.mu.Lock()
		f.registered = append(f.registered, body)
		cmds := f.commands
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test fake
			"ok": true, "commands": cmds,
		})
	})
	mux.HandleFunc("GET /api/v1/chat/outbound", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		msgs := f.pending
		f.pending = nil
		f.issued = append(f.issued, msgs...)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(msgs) //nolint:errcheck // test fake
	})
	mux.HandleFunc("GET /api/v1/chat/outbound/{seq}/ready", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.denyReady {
			w.WriteHeader(http.StatusGone)
			w.Write([]byte(`{"error":"delivery stopped after authority changed"}`)) //nolint:errcheck // test fake
			return
		}
		for _, m := range f.issued {
			if r.PathValue("seq") == fmt.Sprint(m.Seq) {
				json.NewEncoder(w).Encode(m) //nolint:errcheck // test fake
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /api/v1/chat/outbound/{seq}/speech", func(w http.ResponseWriter, r *http.Request) {
		var seq int64
		fmt.Sscan(r.PathValue("seq"), &seq) //nolint:errcheck // test fake
		f.mu.Lock()
		f.spoken = append(f.spoken, seq)
		audio, ok := f.speech[seq]
		delete(f.speech, seq)
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(audio) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/chat/outbound/{seq}/ack", func(w http.ResponseWriter, r *http.Request) {
		var seq int64
		for _, c := range r.PathValue("seq") {
			seq = seq*10 + int64(c-'0')
		}
		f.mu.Lock()
		f.acked = append(f.acked, seq)
		f.mu.Unlock()
		w.Write([]byte(`{"acked":true}`)) //nolint:errcheck
	})
	return mux
}

func newTestAdapter(t *testing.T, ks *fakeKeystone, tr Transport) (*adapter, func()) {
	t.Helper()
	srv := httptest.NewServer(ks.handler())
	a := &adapter{
		keystone:  &keystoneClient{Base: srv.URL, Token: "wst-test", HTTP: srv.Client()},
		transport: tr,
		log:       testLogger(),
		pollWait:  10 * time.Millisecond,
	}
	return a, srv.Close
}

// The adapter relays what it heard and delivers what it was given, and
// nothing it sends names a surface — the server decides that from the
// token's role (ADR-W026 A3/A8).
func TestAdapterRelaysAndDelivers(t *testing.T) {
	ks := &fakeKeystone{pending: []outboundMessage{{Seq: 7, Target: "chat-1", Body: "Breakfast was logged at 07:15."}}}
	tr := &fakeTransport{incoming: [][]Incoming{{{
		Account: "12345", Conversation: "chat-1", Text: "did anyone have breakfast?",
		MessageID: "m-1", DisplayName: "@sam",
	}}}}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		return len(ks.inbound) > 0 && len(ks.acked) > 0
	})

	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.inbound) != 1 {
		t.Fatalf("inbound = %+v, want one relayed message", ks.inbound)
	}
	got := ks.inbound[0]
	if got.Account != "12345" || got.Text != "did anyone have breakfast?" || got.MessageID != "m-1" {
		t.Fatalf("relayed %+v", got)
	}
	if got.Transport != "faketransport" {
		t.Fatalf("transport = %q, want the driver's own name", got.Transport)
	}
	sent := tr.sentMessages()
	if len(sent) != 1 || sent[0].conversation != "chat-1" || !strings.Contains(sent[0].body, "Breakfast") {
		t.Fatalf("sent = %+v", sent)
	}
	if len(ks.acked) != 1 || ks.acked[0] != 7 {
		t.Fatalf("acked = %v, want the delivered seq", ks.acked)
	}
}

func TestAdapterRechecksQueuedDeliveryBeforeSend(t *testing.T) {
	ks := &fakeKeystone{
		pending:   []outboundMessage{{Seq: 9, Target: "chat-1", Body: "private payload"}},
		denyReady: true,
	}
	tr := &fakeTransport{}
	a, done := newTestAdapter(t, ks, tr)
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	a.deliverLoop(ctx)
	if sent := tr.sentMessages(); len(sent) != 0 {
		t.Fatalf("adapter delivered a message revoked after polling: %+v", sent)
	}
}

// The adapter's request body has no field for a surface at all: it
// cannot ask for one, correctly or otherwise.
func TestAdapterCannotNameASurface(t *testing.T) {
	body, err := json.Marshal(keystoneInbound{Transport: "telegram", Account: "1", Text: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(body)), "surface") {
		t.Fatalf("the adapter's inbound payload names a surface: %s", body)
	}
}

// A message that is a claim code is offered as one; a message that
// merely looks wordy is not.
func TestClaimCodeDetection(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"K7QP-3XMR-9TWD", true},
		{"k7qp3xmr9twd", true},
		{" K7QP 3XMR 9TWD ", true},
		{"did anyone have breakfast?", false},
		{"K7QP-3XMR", false},
		{"K7QP-3XMR-9TWD please", false},
		// The alphabet excludes vowels and 0/1/O/I/L, so an ordinary
		// word cannot be mistaken for a code.
		{"CONSIDERATE", false},
	} {
		if _, ok := claimCodeIn(tc.text); ok != tc.want {
			t.Fatalf("claimCodeIn(%q) = %v, want %v", tc.text, ok, tc.want)
		}
	}
}

// A claim code is redeemed rather than relayed as a question. A good one
// is answered "you're connected"; a refused one is answered with nothing
// at all, because whoever sent it is not somebody the household knows
// (ADR-W033 B9.1).
func TestAdapterRedeemsClaimCodes(t *testing.T) {
	ks := &fakeKeystone{rejectID: "ZZZZ-ZZZZ-ZZZZ"}
	tr := &fakeTransport{incoming: [][]Incoming{
		{{Account: "12345", Conversation: "chat-1", Text: "K7QP-3XMR-9TWD", MessageID: "m-1", DisplayName: "@sam"}},
		{{Account: "12345", Conversation: "chat-1", Text: "ZZZZ-ZZZZ-ZZZZ", MessageID: "m-2"}},
	}}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Wait for the REPLIES as well as the redemptions. The adapter
	// records a redemption and then answers it, so a condition on the
	// redemptions alone can return between those two steps and find one
	// reply where this test asserts two.
	runUntil(ctx, a, func() bool {
		ks.mu.Lock()
		redeemed := len(ks.redeems)
		ks.mu.Unlock()
		return redeemed >= 2 && len(tr.sentMessages()) >= 1
	})

	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.redeems) != 2 {
		t.Fatalf("redeems = %+v", ks.redeems)
	}
	if ks.redeems[0]["account"] != "12345" || ks.redeems[0]["displayName"] != "@sam" {
		t.Fatalf("redeem body = %+v", ks.redeems[0])
	}
	// The direct conversation travels with the redemption: on a
	// transport where it differs from the account (Matrix), it is the
	// only value a rule can later send to (ADR-W026 A8).
	if ks.redeems[0]["conversation"] != "chat-1" {
		t.Fatalf("redeem did not report the conversation: %+v", ks.redeems[0])
	}
	if len(ks.inbound) != 0 {
		t.Fatalf("a claim code was also relayed as a question: %+v", ks.inbound)
	}
	sent := tr.sentMessages()
	if len(sent) != 1 || !strings.Contains(sent[0].body, "connected") {
		t.Fatalf("sent = %+v, want only the successful redeem answered", sent)
	}
}

// In a group only what is addressed to the bot leaves the adapter
// (ADR-W033 B9.1): other people's conversation never reaches Keystone.
// And a claim code there is not redeemed even when addressed: everyone
// in the room can read it, and binding on it would bind whoever typed it
// in front of an audience.
func TestAGroupMessageReachesKeystoneOnlyWhenAddressed(t *testing.T) {
	ks := &fakeKeystone{}
	tr := &fakeTransport{incoming: [][]Incoming{
		{{Account: "12345", Conversation: "group-1", Group: true, Text: "anyone for dinner?", MessageID: "m-1"}},
		{{Account: "12345", Conversation: "group-1", Group: true, Addressed: true, Text: "K7QP-3XMR-9TWD", MessageID: "m-2"}},
	}}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		return len(ks.inbound) > 0
	})

	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.redeems) != 0 {
		t.Fatalf("a code in a group was redeemed: %+v", ks.redeems)
	}
	if len(ks.inbound) != 1 || ks.inbound[0].MessageID != "m-2" || !ks.inbound[0].Addressed {
		t.Fatalf("inbound = %+v, want only the addressed message, marked addressed", ks.inbound)
	}
	if sent := tr.sentMessages(); len(sent) != 0 {
		t.Fatalf("the adapter itself answered in a group: %+v", sent)
	}
}

// Delivery is at-least-once: a send that fails is not acked, so the
// message comes back on the next poll.
func TestAdapterDoesNotAckAFailedSend(t *testing.T) {
	ks := &fakeKeystone{pending: []outboundMessage{{Seq: 3, Target: "chat-1", Body: "hello"}}}
	tr := &fakeTransport{sendErr: errNotConfigured}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	a.run(ctx)

	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.acked) != 0 {
		t.Fatalf("acked %v after a failed send", ks.acked)
	}
}

// runUntil runs the adapter's loops until cond holds or ctx expires.
func runUntil(ctx context.Context, a *adapter, cond func() bool) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.run(runCtx)
	for {
		if cond() {
			return
		}
		select {
		case <-runCtx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A tapped invite link arrives as "/start <code>", not as a bare code.
// Without stripping the command the one-tap invite silently does
// nothing: the message reads as an ordinary sentence and is relayed to
// a Butler that has no idea who is speaking.
func TestTappedInviteLinkRedeems(t *testing.T) {
	ks := &fakeKeystone{}
	tr := &fakeTransport{incoming: [][]Incoming{{{
		Account: "12345", Conversation: "12345", Text: "/start K7QP-3XMR-9TWD", MessageID: "m-1",
	}}}}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		return len(ks.redeems) > 0
	})

	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.redeems) != 1 || ks.redeems[0]["code"] != "K7QP-3XMR-9TWD" {
		t.Fatalf("redeems = %+v, want the code out of the /start payload", ks.redeems)
	}
	if len(ks.inbound) != 0 {
		t.Fatalf("a claim was also relayed as a message: %+v", ks.inbound)
	}
}

// Pressing Start with no invite gets nothing (ADR-W033 B9.1): whoever
// does it is nobody the household has introduced, and a greeting tells
// them what the bot is.
func TestBareStartGetsNoReply(t *testing.T) {
	ks := &fakeKeystone{}
	tr := &fakeTransport{incoming: [][]Incoming{{{
		Account: "12345", Conversation: "12345", Text: "/start", MessageID: "m-1",
	}}}}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runUntil(ctx, a, func() bool { return len(tr.sentMessages()) > 0 })

	if sent := tr.sentMessages(); len(sent) != 0 {
		t.Fatalf("sent = %+v, want silence to a stranger", sent)
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.inbound) != 0 || len(ks.redeems) != 0 {
		t.Fatalf("a bare /start reached Keystone: inbound=%+v redeems=%+v", ks.inbound, ks.redeems)
	}
}

// "/startle the cat" is a sentence. Only the command is stripped.
func TestStartStrippingIsExact(t *testing.T) {
	for text, want := range map[string]string{
		"/start K7QP-3XMR-9TWD":         "K7QP-3XMR-9TWD",
		"/start@house_bot K7QP3XMR9TWD": "K7QP3XMR9TWD",
		"/start":                        "",
		"/startle the cat":              "/startle the cat",
		"tell me about /start":          "tell me about /start",
	} {
		got, _ := withoutStartCommand(text)
		if got != want {
			t.Fatalf("withoutStartCommand(%q) = %q, want %q", text, got, want)
		}
	}
}

// The adapter reports which bot it is, so an invite can be a link. It is
// the only party that can: Keystone holds the bot token write-only.
func TestAdapterRegistersItsHandle(t *testing.T) {
	ks := &fakeKeystone{}
	tr := &fakeTransport{}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		return len(ks.registered) > 0
	})

	ks.mu.Lock()
	defer ks.mu.Unlock()
	got := ks.registered[0]
	if got["handle"] != "fakebot" || got["transport"] != "faketransport" {
		t.Fatalf("registered %+v, want the transport's own handle", got)
	}
}

// The menu comes from Keystone, which knows this adapter's surface from
// the credential it authenticated with. The adapter carries no copy of
// the list: a household bot advertising /status is precisely the drift
// a second source of truth would produce.
func TestAdapterPublishesTheMenuKeystoneSent(t *testing.T) {
	ks := &fakeKeystone{commands: []Command{
		{Command: "help", Description: "what this bot answers"},
		{Command: "today", Description: "what has happened in the house today"},
	}}
	tr := &fakeTransport{}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool { return len(tr.menus()) > 0 })

	menus := tr.menus()
	if len(menus) == 0 {
		t.Fatal("the adapter published no menu")
	}
	got := menus[0]
	if len(got) != 2 || got[0].Command != "help" || got[1].Command != "today" {
		t.Fatalf("published %+v, want exactly what Keystone sent", got)
	}
}

// nil and empty are different answers, and the difference decides
// whether a stale menu is cleared. Reported by Codex on #112.
func TestEmptyMenuIsPublishedButUnknownIsNot(t *testing.T) {
	// Keystone says this surface offers nothing: the vendor must be
	// told, or the bot keeps advertising commands it no longer answers.
	empty := &fakeTransport{}
	ksEmpty := &fakeKeystone{commands: []Command{}}
	a, done := newTestAdapter(t, ksEmpty, empty)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	runUntil(ctx, a, func() bool { return len(empty.menus()) > 0 })
	cancel()
	done()
	if got := empty.menus(); len(got) == 0 {
		t.Fatal("an explicit empty menu was never published, so a stale one would survive")
	} else if len(got[0]) != 0 {
		t.Fatalf("published %+v, want the empty list", got[0])
	}

	// Keystone told us nothing — an older control plane, or a response
	// we could not read. Publishing would wipe a working menu.
	unknown := &fakeTransport{}
	b, done2 := newTestAdapter(t, &fakeKeystone{}, unknown)
	defer done2()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel2()
	runUntil(ctx2, b, func() bool { return len(unknown.menus()) > 0 })
	for _, m := range unknown.menus() {
		if m != nil {
			t.Fatalf("published %+v when Keystone said nothing", m)
		}
	}
}

// A vendor that rejects the menu must not take the adapter down with
// it. Commands still work when the menu is stale; they do not work at
// all if the adapter stops relaying messages.
func TestMenuFailureDoesNotStopMessages(t *testing.T) {
	ks := &fakeKeystone{
		commands: []Command{{Command: "help", Description: "what this bot answers"}},
		pending:  []outboundMessage{{Seq: 3, Target: "chat-1", Body: "hello"}},
	}
	tr := &fakeTransport{advertiseErr: errors.New("telegram said no")}
	a, done := newTestAdapter(t, ks, tr)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool { return len(tr.sentMessages()) > 0 })

	if got := tr.sentMessages(); len(got) != 1 || got[0].body != "hello" {
		t.Fatalf("a failed menu update stopped delivery: %+v", got)
	}
}
