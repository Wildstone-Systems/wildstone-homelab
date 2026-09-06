package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTransport stands in for a chat vendor: queued incoming messages,
// recorded outgoing ones. No network, no vendor.
type fakeTransport struct {
	mu       sync.Mutex
	incoming [][]Incoming
	sent     []sentMessage
	sendErr  error
}

type sentMessage struct{ conversation, body string }

func (f *fakeTransport) Name() string { return "faketransport" }

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
	mu       sync.Mutex
	inbound  []keystoneInbound
	redeems  []map[string]string
	acked    []int64
	pending  []outboundMessage
	rejectID string
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
	mux.HandleFunc("GET /api/v1/chat/outbound", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		msgs := f.pending
		f.pending = nil
		f.mu.Unlock()
		json.NewEncoder(w).Encode(msgs) //nolint:errcheck // test fake
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

// A claim code is redeemed rather than relayed as a question — and a
// refused one produces a vague reply, because Keystone does not say why
// and neither should the adapter (ADR-W026 A4).
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
	runUntil(ctx, a, func() bool {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		return len(ks.redeems) >= 2
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
	if len(sent) != 2 {
		t.Fatalf("sent = %+v, want one reply per code", sent)
	}
	if !strings.Contains(sent[0].body, "connected") {
		t.Fatalf("successful redeem reply = %q", sent[0].body)
	}
	if !strings.Contains(sent[1].body, "did not work") || strings.Contains(strings.ToLower(sent[1].body), "expired") {
		t.Fatalf("refusal reply says too much: %q", sent[1].body)
	}
}

// A claim code sent in a GROUP is not redeemed: everyone in the room can
// read it, and binding on it would bind whoever typed it in front of an
// audience.
func TestAdapterIgnoresClaimCodesInGroups(t *testing.T) {
	ks := &fakeKeystone{}
	tr := &fakeTransport{incoming: [][]Incoming{{{
		Account: "12345", Conversation: "group-1", Group: true, Text: "K7QP-3XMR-9TWD", MessageID: "m-1",
	}}}}
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
	if len(ks.inbound) != 1 || !ks.inbound[0].Group {
		t.Fatalf("inbound = %+v, want it relayed as an ordinary group message", ks.inbound)
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
