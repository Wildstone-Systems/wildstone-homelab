package imapread

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	mailapi "github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/imapread/imapfake"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
)

const corpus = "../../../../testing/fixtures/mail"

type harness struct {
	t    *testing.T
	srv  *imapfake.Server
	pool *x509.CertPool
	mode Security
}

func newHarness(t *testing.T, mode Security) *harness {
	t.Helper()
	tlsCfg, pool, err := imapfake.Certificate("imap.example.internal")
	if err != nil {
		t.Fatal(err)
	}
	opts := imapfake.Options{TLS: tlsCfg}
	if mode == SecurityStartTLS {
		opts = imapfake.Options{StartTLS: tlsCfg}
	}
	srv, err := imapfake.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: srv, pool: pool, mode: mode}
	t.Cleanup(func() {
		// Every test, whatever it asserts, also proves nothing mutated
		// the mailbox.
		if v := srv.Violations(); len(v) > 0 {
			t.Errorf("mutating IMAP commands reached the server: %v", v)
		}
		_ = srv.Close()
	})
	srv.AddUser("ada@example.internal", "REPLACED-APP-PASSWORD-ADA")
	srv.AddUser("grace@example.internal", "REPLACED-APP-PASSWORD-GRACE")
	return h
}

func (h *harness) config(user string, f Filters) Config {
	pw := "REPLACED-APP-PASSWORD-ADA"
	key := bytes.Repeat([]byte{'a'}, 32)
	if strings.HasPrefix(user, "grace") {
		pw = "REPLACED-APP-PASSWORD-GRACE"
		key = bytes.Repeat([]byte{'g'}, 32)
	}
	return Config{
		Addr: h.srv.Addr(), Security: h.mode, ServerName: "imap.example.internal", RootCAs: h.pool,
		Username: user, Password: pw, Key: key, Filters: f, Timeout: 10 * time.Second,
	}
}

func (h *harness) reader(user string, f Filters) *Reader {
	h.t.Helper()
	r, err := New(h.config(user, f))
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func letter(i int, from, subject string) []byte {
	return []byte(fmt.Sprintf("From: %s\r\nTo: ada@example.internal\r\nSubject: %s\r\n"+
		"Date: Mon, 05 Oct 2026 09:%02d:00 +0000\r\nMessage-ID: <m%d@example.internal>\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\nbody %d\r\n", from, subject, i%60, i, i))
}

func (h *harness) deliver(user string, n, start int) []uint32 {
	var uids []uint32
	for i := start; i < start+n; i++ {
		uids = append(uids, h.srv.Deliver(user, "INBOX", letter(i, "office@school.example.internal", fmt.Sprintf("Note %d", i))))
	}
	return uids
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func texts(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Content.Text)
	}
	return out
}

func TestBaselineThenNewMail(t *testing.T) {
	for _, mode := range []Security{SecurityTLS, SecurityStartTLS} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t, mode)
			h.deliver("ada@example.internal", 3, 0)
			r := h.reader("ada@example.internal", Filters{})

			res := r.Changes(ctx(t), Cursor{}, 0)
			if res.Outcome != OutcomeEmpty || len(res.Messages) != 0 || res.Folders[0].Outcome != "baseline" {
				t.Fatalf("first run must baseline, not import the archive: %+v", res)
			}
			if fc, _ := cursorFolder(res.Next, "INBOX"); fc.LastUID != 3 || fc.UIDValidity == 0 {
				t.Fatalf("baseline cursor %+v", fc)
			}
			h.deliver("ada@example.internal", 2, 3)
			res = r.Changes(ctx(t), res.Next, 0)
			if res.Outcome != OutcomeOK || !reflect.DeepEqual(texts(res.Messages), []string{"body 3", "body 4"}) {
				t.Fatalf("new mail: %s %v", res.Outcome, texts(res.Messages))
			}
			res = r.Changes(ctx(t), res.Next, 0)
			if res.Outcome != OutcomeEmpty || len(res.Messages) != 0 {
				t.Fatalf("caught up: %+v", res)
			}
		})
	}
}

func TestBaselineWithoutUIDNext(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	h.deliver("ada@example.internal", 4, 0)
	h.srv.SetFaults(imapfake.Faults{OmitUIDNext: true})
	r := h.reader("ada@example.internal", Filters{})
	res := r.Changes(ctx(t), Cursor{}, 0)
	if fc, _ := cursorFolder(res.Next, "INBOX"); fc.LastUID != 4 {
		t.Fatalf("baseline without UIDNEXT: %+v", res.Next)
	}
	h.deliver("ada@example.internal", 1, 4)
	res = r.Changes(ctx(t), res.Next, 0)
	if len(res.Messages) != 1 || res.Messages[0].Content.Text != "body 4" {
		t.Fatalf("after baseline: %v", texts(res.Messages))
	}
	// And with nothing new, the "UID n:*" quirk (the highest UID
	// matches) must not re-deliver it.
	res = r.Changes(ctx(t), res.Next, 0)
	if res.Outcome != OutcomeEmpty {
		t.Fatalf("UID n:* quirk re-delivered: %v", texts(res.Messages))
	}
}

func startAt(h *harness, user string) Cursor {
	return Cursor{Folders: []FolderCursor{{Folder: "INBOX", UIDValidity: h.srv.UIDValidity(user, "INBOX"), LastUID: 0}}}
}

func TestBatchLimitAndMore(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	h.deliver("ada@example.internal", 7, 0)
	r := h.reader("ada@example.internal", Filters{})
	cur := startAt(h, "ada@example.internal")
	var got []string
	for i := 0; i < 10; i++ {
		res := r.Changes(ctx(t), cur, 3)
		if len(res.Messages) > 3 {
			t.Fatalf("batch over limit: %d", len(res.Messages))
		}
		got = append(got, texts(res.Messages)...)
		cur = res.Next
		if !res.More {
			break
		}
	}
	want := []string{"body 0", "body 1", "body 2", "body 3", "body 4", "body 5", "body 6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches: %v", got)
	}
	if res := r.Changes(ctx(t), cur, 100); res.Outcome != OutcomeEmpty {
		t.Fatalf("limit above MaxBatch must clamp, and nothing is left: %+v", res)
	}
}

func TestRunByteLimitStopsEarlyWithoutSkipping(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	h.deliver("ada@example.internal", 5, 0)
	cfg := h.config("ada@example.internal", Filters{})
	cfg.MaxRunBytes = 400 // roughly two of these messages
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cur := startAt(h, "ada@example.internal")
	var got []string
	for i := 0; i < 10; i++ {
		res := r.Changes(ctx(t), cur, 0)
		if len(res.Messages) == 0 && res.More {
			t.Fatal("run limit made no progress")
		}
		got = append(got, texts(res.Messages)...)
		cur = res.Next
		if !res.More {
			break
		}
	}
	if len(got) != 5 {
		t.Fatalf("run-limited batches lost or duplicated mail: %v", got)
	}
}

func TestUIDValidityResetIsExplicitAndLosesNothing(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	h.deliver("ada@example.internal", 3, 0)
	r := h.reader("ada@example.internal", Filters{})
	res := r.Changes(ctx(t), startAt(h, "ada@example.internal"), 0)
	if len(res.Messages) != 3 {
		t.Fatalf("setup: %v", texts(res.Messages))
	}
	seen := map[string]bool{}
	for _, m := range res.Messages {
		seen[m.Fingerprint] = true
	}
	old := res.Messages[0].Ref
	prevValidity, _ := cursorFolder(res.Next, "INBOX")

	// The provider rebuilds the mailbox, and a new message arrives.
	newValidity := h.srv.ResetUIDValidity("ada@example.internal", "INBOX")
	h.deliver("ada@example.internal", 1, 3)

	res = r.Changes(ctx(t), res.Next, 0)
	if res.Outcome != OutcomeCursorReset || len(res.Messages) != 0 {
		t.Fatalf("reset must be explicit and deliver nothing yet: %+v", res)
	}
	f := res.Folders[0]
	if f.Outcome != "cursor_reset" || f.PreviousUIDValidity != prevValidity.UIDValidity || f.CurrentUIDValidity != newValidity {
		t.Fatalf("folder result %+v", f)
	}
	if fc, _ := cursorFolder(res.Next, "INBOX"); fc.UIDValidity != newValidity || fc.LastUID != 0 {
		t.Fatalf("reset cursor %+v", fc)
	}

	res = r.Changes(ctx(t), res.Next, 0)
	if len(res.Messages) != 4 {
		t.Fatalf("after reset every message must come back (none skipped): %v", texts(res.Messages))
	}
	fresh := 0
	for _, m := range res.Messages {
		if !seen[m.Fingerprint] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("fingerprints must be stable across a reset: %d unseen, want 1", fresh)
	}

	// A reference from before the reset is stale, not silently
	// re-pointed at whatever now has that UID.
	mr, err := r.Message(ctx(t), old)
	if err != nil || mr.Outcome != OutcomeStale {
		t.Fatalf("old ref: %+v %v", mr, err)
	}
}

// consumer models the caller (S03/S05): it processes messages one by
// one, recording fingerprints, and commits the cursor only after the
// whole batch. crashAfter < 0 means no crash.
type consumer struct {
	committed Cursor
	processed map[string]int
}

func (c *consumer) run(t *testing.T, r *Reader, crashAfter int) (done bool) {
	res := r.Changes(ctx(t), c.committed, 2)
	if res.Outcome == OutcomeUnavailable || res.Outcome == OutcomeAuthFailed {
		if !reflect.DeepEqual(res.Next, c.committed) {
			t.Fatalf("a failed run moved the cursor: %+v -> %+v", c.committed, res.Next)
		}
		return false
	}
	for i, m := range res.Messages {
		if i == crashAfter {
			return false // crash: the commit below never happens
		}
		c.processed[m.Fingerprint]++
	}
	if crashAfter >= 0 && crashAfter >= len(res.Messages) && crashAfter < 1000 {
		// Crash after processing everything, before committing.
		return false
	}
	c.committed = res.Next
	return !res.More && res.Outcome != OutcomeCursorReset
}

func TestCrashAtEveryCursorBoundarySkipsNothing(t *testing.T) {
	const n = 5
	for crashRun := 0; crashRun < 4; crashRun++ {
		for crashAfter := 0; crashAfter <= 2; crashAfter++ {
			t.Run(fmt.Sprintf("run%d-after%d", crashRun, crashAfter), func(t *testing.T) {
				h := newHarness(t, SecurityTLS)
				h.deliver("ada@example.internal", n, 0)
				r := h.reader("ada@example.internal", Filters{})
				c := &consumer{committed: startAt(h, "ada@example.internal"), processed: map[string]int{}}
				for run := 0; run < 20; run++ {
					crash := -1
					if run == crashRun {
						crash = crashAfter
					}
					if run == 2 {
						// Mail keeps arriving; one unread message is
						// expunged by another client mid-stream.
						h.deliver("ada@example.internal", 2, n)
						h.srv.Remove("ada@example.internal", "INBOX", uint32(n+1))
					}
					if c.run(t, r, crash) && run > crashRun && run > 2 {
						break
					}
				}
				// Everything that still exists was processed; the
				// expunged one may or may not have been, nothing else is
				// missing.
				want := map[string]bool{}
				all := r.Changes(ctx(t), startAt(h, "ada@example.internal"), MaxBatch)
				for _, m := range all.Messages {
					want[m.Fingerprint] = true
				}
				if len(want) != n+1 {
					t.Fatalf("setup: %d messages exist", len(want))
				}
				for fp := range want {
					if c.processed[fp] == 0 {
						t.Errorf("committed cursor skipped message %s", fp)
					}
				}
			})
		}
	}
}

func TestConnectionDropAtEveryFetchLeavesCursorUnchanged(t *testing.T) {
	const n = 4
	for drop := 1; drop <= n; drop++ {
		t.Run(fmt.Sprint(drop), func(t *testing.T) {
			h := newHarness(t, SecurityTLS)
			h.deliver("ada@example.internal", n, 0)
			r := h.reader("ada@example.internal", Filters{})
			cur := startAt(h, "ada@example.internal")

			h.srv.SetFaults(imapfake.Faults{DropOnBodyFetch: drop})
			res := r.Changes(ctx(t), cur, 0)
			if res.Outcome != OutcomeUnavailable || len(res.Messages) != 0 || !reflect.DeepEqual(res.Next, cur) {
				t.Fatalf("a mid-batch drop must be unavailable with the cursor unchanged: %s %d %+v", res.Outcome, len(res.Messages), res.Next)
			}
			h.srv.SetFaults(imapfake.Faults{})
			res = r.Changes(ctx(t), cur, 0)
			if len(res.Messages) != n {
				t.Fatalf("after reconnect: %v", texts(res.Messages))
			}
		})
	}
}

func TestDuplicateFetchResponsesYieldOneMessageEach(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	h.deliver("ada@example.internal", 3, 0)
	h.srv.SetFaults(imapfake.Faults{DuplicateFetch: true})
	r := h.reader("ada@example.internal", Filters{})
	res := r.Changes(ctx(t), startAt(h, "ada@example.internal"), 0)
	if !reflect.DeepEqual(texts(res.Messages), []string{"body 0", "body 1", "body 2"}) {
		t.Fatalf("duplicates: %v", texts(res.Messages))
	}
}

// goneAfterSearch expunges a message between SEARCH and FETCH, the
// race a real mailbox has with other clients.
type goneAfterSearch struct {
	readSession
	srv  *imapfake.Server
	user string
	uid  uint32
}

func (g goneAfterSearch) searchUIDs(q searchQuery) ([]uint32, error) {
	uids, err := g.readSession.searchUIDs(q)
	g.srv.Remove(g.user, "INBOX", g.uid)
	return uids, err
}

func TestExpungeBetweenSearchAndFetch(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	uids := h.deliver("ada@example.internal", 3, 0)
	r := h.reader("ada@example.internal", Filters{})
	r.dial = func(c context.Context, cfg *Config) (readSession, error) {
		s, err := dialSession(c, cfg)
		if err != nil {
			return nil, err
		}
		return goneAfterSearch{readSession: s, srv: h.srv, user: "ada@example.internal", uid: uids[1]}, nil
	}
	res := r.Changes(ctx(t), startAt(h, "ada@example.internal"), 0)
	if !reflect.DeepEqual(texts(res.Messages), []string{"body 0", "body 2"}) || res.Folders[0].Gone != 1 {
		t.Fatalf("expunged mid-batch: %v %+v", texts(res.Messages), res.Folders)
	}
	if fc, _ := cursorFolder(res.Next, "INBOX"); fc.LastUID != uids[2] {
		t.Fatalf("cursor must pass the expunged UID: %+v", fc)
	}
}

func TestDistinctFailureOutcomes(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	h.deliver("ada@example.internal", 1, 0)

	cfg := h.config("ada@example.internal", Filters{})
	cfg.Password = "REPLACED-WRONG"
	r, _ := New(cfg)
	if res := r.Changes(ctx(t), Cursor{}, 0); res.Outcome != OutcomeAuthFailed {
		t.Errorf("wrong password: %+v", res)
	}
	if res := r.Capabilities(ctx(t)); res.Outcome != OutcomeAuthFailed {
		t.Errorf("capabilities, wrong password: %+v", res)
	}

	h.srv.SetFaults(imapfake.Faults{LoginUnavailable: true})
	r = h.reader("ada@example.internal", Filters{})
	if res := r.Changes(ctx(t), Cursor{}, 0); res.Outcome != OutcomeUnavailable {
		t.Errorf("provider maintenance is not an auth failure: %+v", res)
	}
	h.srv.SetFaults(imapfake.Faults{})

	// Certificate validation is never skipped: an untrusted chain and a
	// wrong name are both refused.
	cfg = h.config("ada@example.internal", Filters{})
	cfg.RootCAs = x509.NewCertPool()
	r, _ = New(cfg)
	if res := r.Changes(ctx(t), Cursor{}, 0); res.Outcome != OutcomeUnavailable || res.Reason != "tls" {
		t.Errorf("untrusted certificate: %+v", res)
	}
	cfg = h.config("ada@example.internal", Filters{})
	cfg.ServerName = "other.example.internal"
	r, _ = New(cfg)
	if res := r.Changes(ctx(t), Cursor{}, 0); res.Outcome != OutcomeUnavailable || res.Reason != "tls" {
		t.Errorf("wrong server name: %+v", res)
	}

	// Nothing listening.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg = h.config("ada@example.internal", Filters{})
	cfg.Addr = addr
	r, _ = New(cfg)
	if res := r.Changes(ctx(t), Cursor{}, 0); res.Outcome != OutcomeUnavailable || res.Reason != "connection" {
		t.Errorf("refused: %+v", res)
	}

	// A server that accepts and never speaks.
	silent, _ := net.Listen("tcp", "127.0.0.1:0")
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	cfg = h.config("ada@example.internal", Filters{})
	cfg.Addr = silent.Addr().String()
	cfg.Timeout = 300 * time.Millisecond
	r, _ = New(cfg)
	if res := r.Changes(ctx(t), Cursor{}, 0); res.Outcome != OutcomeUnavailable || res.Reason != "timeout" {
		t.Errorf("silent server: %+v", res)
	}

	// A folder that does not exist.
	r = h.reader("ada@example.internal", Filters{Folders: []string{"Nope"}})
	if res := r.Changes(ctx(t), Cursor{}, 0); res.Outcome != OutcomeUnavailable || res.Reason != "folder_missing" {
		t.Errorf("missing folder: %+v", res)
	}
}

func TestTwoConnectionsOnOneProviderAreIsolated(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	shared := letter(1, "office@school.example.internal", "Family night")
	h.srv.Deliver("ada@example.internal", "INBOX", shared)
	h.srv.Deliver("grace@example.internal", "INBOX", shared)
	h.srv.Deliver("grace@example.internal", "INBOX", letter(2, "doctor@example.internal", "Grace's appointment"))

	ada := h.reader("ada@example.internal", Filters{})
	grace := h.reader("grace@example.internal", Filters{})
	ra := ada.Changes(ctx(t), startAt(h, "ada@example.internal"), 0)
	rg := grace.Changes(ctx(t), startAt(h, "grace@example.internal"), 0)
	if len(ra.Messages) != 1 || len(rg.Messages) != 2 {
		t.Fatalf("each sees only their own mailbox: %d / %d", len(ra.Messages), len(rg.Messages))
	}
	// The same message has unrelated fingerprints per connection, so
	// comparing them reveals nothing.
	if ra.Messages[0].Fingerprint == rg.Messages[0].Fingerprint {
		t.Error("fingerprints correlate across connections")
	}
	// A reference is only accepted by the connection that issued it —
	// even though the folder and UID exist in the other mailbox.
	for _, ref := range []string{rg.Messages[0].Ref, rg.Messages[1].Ref} {
		if _, err := ada.Message(ctx(t), ref); !errors.Is(err, ErrBadRef) {
			t.Errorf("ada accepted grace's reference: %v", err)
		}
	}
	if _, err := grace.Message(ctx(t), ra.Messages[0].Ref); !errors.Is(err, ErrBadRef) {
		t.Errorf("grace accepted ada's reference: %v", err)
	}
	// Tampering with a reference's UID breaks its MAC.
	tampered := []byte(ra.Messages[0].Ref)
	tampered[len(refPrefix)+8] ^= 1
	if _, err := ada.Message(ctx(t), string(tampered)); !errors.Is(err, ErrBadRef) {
		t.Errorf("tampered reference accepted: %v", err)
	}
	// Search stays in its own mailbox too.
	sr, err := ada.Search(ctx(t), SearchRequest{Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Subject: "appointment"})
	if err != nil || sr.Outcome != OutcomeEmpty {
		t.Errorf("ada's search found grace's mail: %+v %v", sr, err)
	}
}

func TestFilters(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	h.srv.CreateMailbox("ada@example.internal", "School")
	u := "ada@example.internal"
	h.srv.Deliver(u, "INBOX", letter(0, "office@school.example.internal", "Family night"))
	h.srv.Deliver(u, "INBOX", letter(1, "coach@sports.school.example.internal", "Practice moved"))
	h.srv.Deliver(u, "INBOX", letter(2, "shop@example.invalid", "Family night sale"))
	h.srv.Deliver(u, "INBOX", letter(3, "friend@example.internal", "Dinner"))
	h.srv.Deliver(u, "School", letter(4, "office@school.example.internal", "Picture day"))

	f := Filters{Folders: []string{"INBOX", "School"}, Senders: []string{"school.example.internal", "friend@example.internal"}, SubjectContains: []string{"family", "practice", "picture"}}
	r := h.reader(u, f)
	cur := Cursor{Folders: []FolderCursor{
		{Folder: "INBOX", UIDValidity: h.srv.UIDValidity(u, "INBOX")},
		{Folder: "School", UIDValidity: h.srv.UIDValidity(u, "School")},
	}}
	res := r.Changes(ctx(t), cur, 0)
	got := texts(res.Messages)
	if !reflect.DeepEqual(got, []string{"body 0", "body 1", "body 4"}) {
		t.Fatalf("filtered: %v", got)
	}
	if res.Folders[0].Filtered != 2 || res.Folders[0].Scanned != 4 {
		t.Errorf("filter accounting: %+v", res.Folders[0])
	}
	// The cursor passes filtered mail too: it is out of scope, not unread.
	if fc, _ := cursorFolder(res.Next, "INBOX"); fc.LastUID != 4 {
		t.Errorf("cursor stopped at filtered mail: %+v", fc)
	}
	// A reference issued under a wider scope returns nothing once the
	// scope narrows (indistinguishable from gone).
	narrow := h.reader(u, Filters{Folders: []string{"INBOX", "School"}, Senders: []string{"friend@example.internal"}})
	mr, err := narrow.Message(ctx(t), res.Messages[0].Ref)
	if err != nil || mr.Outcome != OutcomeEmpty || mr.Message != nil {
		t.Errorf("narrowed scope leaked: %+v %v", mr, err)
	}
	// A folder outside the scope is not reachable by reference.
	inboxOnly := h.reader(u, Filters{})
	if _, err := inboxOnly.Message(ctx(t), res.Messages[2].Ref); !errors.Is(err, ErrBadRef) {
		t.Errorf("out-of-scope folder: %v", err)
	}
	if err := (Filters{Folders: []string{"a*"}}).Validate(); err == nil {
		t.Error("wildcard folder accepted")
	}
}

func TestMessageByReference(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	uids := h.deliver("ada@example.internal", 2, 0)
	r := h.reader("ada@example.internal", Filters{})
	res := r.Changes(ctx(t), startAt(h, "ada@example.internal"), 0)
	mr, err := r.Message(ctx(t), res.Messages[1].Ref)
	if err != nil || mr.Outcome != OutcomeOK || mr.Message.Content.Text != "body 1" || mr.Message.Fingerprint != res.Messages[1].Fingerprint {
		t.Fatalf("message: %+v %v", mr, err)
	}
	h.srv.Remove("ada@example.internal", "INBOX", uids[1])
	if mr, _ := r.Message(ctx(t), res.Messages[1].Ref); mr.Outcome != OutcomeEmpty {
		t.Errorf("expunged: %+v", mr)
	}
	if _, err := r.Message(ctx(t), "not-a-ref"); !errors.Is(err, ErrBadRef) {
		t.Errorf("garbage ref: %v", err)
	}
}

func TestSearchIsBounded(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	u := "ada@example.internal"
	for i := 0; i < 12; i++ {
		from := "office@school.example.internal"
		if i%3 == 0 {
			from = "shop@example.invalid"
		}
		h.srv.Deliver(u, "INBOX", letter(i, from, fmt.Sprintf("Update %d", i)))
	}
	r := h.reader(u, Filters{Senders: []string{"school.example.internal"}})
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, bad := range []SearchRequest{
		{},
		{Since: since, Before: since.Add(-time.Hour)},
		{Since: since.Add(-400 * 24 * time.Hour), Before: since},
		{Since: since, Folders: []string{"Elsewhere"}},
		{Since: since, Subject: "a\r\nb"},
	} {
		if _, err := r.Search(ctx(t), bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("accepted unbounded/out-of-scope search %+v: %v", bad, err)
		}
	}
	sr, err := r.Search(ctx(t), SearchRequest{Since: since, Before: since.Add(60 * 24 * time.Hour), Limit: 3})
	if err != nil || sr.Outcome != OutcomeOK || len(sr.Results) != 3 || !sr.Truncated {
		t.Fatalf("search: %+v %v", sr, err)
	}
	if sr.Results[0].Headers.Subject != "Update 11" {
		t.Errorf("newest first: %q", sr.Results[0].Headers.Subject)
	}
	for _, s := range sr.Results {
		if s.Headers.From[0].Address == "shop@example.invalid" {
			t.Errorf("search escaped the connection's filters: %+v", s)
		}
	}
	sr, _ = r.Search(ctx(t), SearchRequest{Since: since, Before: since.Add(60 * 24 * time.Hour), Subject: "update 4"})
	if len(sr.Results) != 1 || sr.Results[0].Headers.Subject != "Update 4" {
		t.Errorf("subject search: %+v", sr.Results)
	}
	b, _ := json.Marshal(sr)
	if strings.Contains(string(b), "body 4") {
		t.Error("search returned a body")
	}
}

func TestHostileCorpusThroughTheReader(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	u := "ada@example.internal"
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".eml" {
			raw, _ := os.ReadFile(filepath.Join(corpus, e.Name()))
			h.srv.Deliver(u, "INBOX", raw)
			names = append(names, e.Name())
		}
	}
	big := append(letter(99, "office@school.example.internal", "Huge"), bytes.Repeat([]byte("x"), 10<<20)...)
	h.srv.Deliver(u, "INBOX", big)
	names = append(names, "(synthesized 10 MiB)")

	r := h.reader(u, Filters{})
	res := r.Changes(ctx(t), startAt(h, u), 0)
	if len(res.Messages) != len(names) {
		t.Fatalf("got %d messages for %d fixtures", len(res.Messages), len(names))
	}
	outcomes := map[mimenorm.Outcome]int{}
	for i, m := range res.Messages {
		outcomes[m.Content.Outcome]++
		if len(m.Content.Text) > mimenorm.DefaultLimits().MaxTextBytes {
			t.Errorf("%s: unbounded text", names[i])
		}
	}
	last := res.Messages[len(res.Messages)-1].Content
	if last.Outcome != mimenorm.Oversized || last.Text != "" || last.Headers.Subject != "Huge" {
		t.Errorf("oversized message: %+v", last.Outcome)
	}
	for _, o := range []mimenorm.Outcome{mimenorm.OK, mimenorm.Malformed, mimenorm.Oversized, mimenorm.TooDeep} {
		if outcomes[o] == 0 {
			t.Errorf("corpus produced no %s outcome: %v", o, outcomes)
		}
	}
}

func TestResultsCarryNoCredentials(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	h.deliver("ada@example.internal", 1, 0)
	cfg := h.config("ada@example.internal", Filters{})
	r, _ := New(cfg)
	var outs []any
	outs = append(outs, r.Changes(ctx(t), startAt(h, "ada@example.internal"), 0), r.Capabilities(ctx(t)))
	bad := cfg
	bad.Password = "REPLACED-WRONG-SECRET"
	rb, _ := New(bad)
	outs = append(outs, rb.Changes(ctx(t), Cursor{}, 0))
	for _, o := range outs {
		b, _ := json.Marshal(o)
		for _, secret := range []string{cfg.Password, bad.Password, string(cfg.Key)} {
			if strings.Contains(string(b), secret) {
				t.Errorf("result carries a secret: %s", b)
			}
		}
	}
	for _, s := range []string{fmt.Sprint(cfg), fmt.Sprintf("%v %+v %#v %s", cfg, cfg, cfg, cfg)} {
		if strings.Contains(s, cfg.Password) || strings.Contains(s, string(cfg.Key)) {
			t.Errorf("Config renders a secret: %s", s)
		}
	}
}

func TestUsernameDomain(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	cfg := h.config("ada@example.internal", Filters{})
	cfg.Username, cfg.UsernameDomain = "ada", "example.internal"
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Capabilities(ctx(t)); res.Outcome != OutcomeOK || !res.ReadOnly {
		t.Fatalf("bare username + domain: %+v", res)
	}
}

func TestNewRejectsUnsafeConfig(t *testing.T) {
	base := Config{Addr: "imap.example.internal:993", Username: "a", Password: "REPLACED", Key: bytes.Repeat([]byte{1}, 32)}
	for name, mut := range map[string]func(*Config){
		"plaintext":  func(c *Config) { c.Security = "none" },
		"short key":  func(c *Config) { c.Key = []byte("short") },
		"no address": func(c *Config) { c.Addr = "imap.example.internal" },
		"no secret":  func(c *Config) { c.Password = "" },
	} {
		c := base
		mut(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := New(base); err != nil {
		t.Errorf("base: %v", err)
	}
}

func TestCursorJSONRoundTrip(t *testing.T) {
	c := Cursor{Folders: []FolderCursor{{Folder: "INBOX", UIDValidity: 7, LastUID: 42}}}
	b, _ := json.Marshal(mailapi.ChangesRequest{Cursor: &c, Limit: 5})
	var back mailapi.ChangesRequest
	if err := json.Unmarshal(b, &back); err != nil || back.Cursor == nil || !reflect.DeepEqual(*back.Cursor, c) {
		t.Fatalf("round trip: %s %+v %v", b, back, err)
	}
	keys := []string{}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"cursor", "limit"}) {
		t.Errorf("wire shape: %v", keys)
	}
}
