package runtimedb

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func chatTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir() + "/keystone.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustEnqueue(t *testing.T, db *DB, m ChatOutbound) int64 {
	t.Helper()
	seq, err := db.EnqueueChatOutbound(m)
	if err != nil {
		t.Fatalf("EnqueueChatOutbound: %v", err)
	}
	return seq
}

// The cursor returns what still needs delivering, oldest first, and
// nothing else: not what was acked, not what expired (ADR-W026 A8).
func TestChatOutboundCursor(t *testing.T) {
	db := chatTestDB(t)
	first := mustEnqueue(t, db, ChatOutbound{
		Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "1",
		Body: "one", DedupeKey: "k1",
	})
	second := mustEnqueue(t, db, ChatOutbound{
		Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "1",
		Body: "two", DedupeKey: "k2",
	})
	// Another surface's message is not this surface's to deliver.
	mustEnqueue(t, db, ChatOutbound{
		Surface: ButlerSurfaceAdmin, Transport: "telegram", Target: "9",
		Body: "admin", DedupeKey: "k3",
	})
	// Already expired: queued, never collected, and now past its life.
	mustEnqueue(t, db, ChatOutbound{
		Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "1",
		Body: "stale", DedupeKey: "k4", ExpiresAt: time.Now().Add(-time.Minute),
	})

	msgs, err := db.ChatOutboundAfter(ButlerSurfaceFamily, "telegram", 0, 0)
	if err != nil {
		t.Fatalf("ChatOutboundAfter: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Seq != first || msgs[1].Seq != second {
		t.Fatalf("msgs = %+v, want the two live family messages oldest first", msgs)
	}

	if err := db.AckChatOutbound(ButlerSurfaceFamily, first); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	// Acking twice is fine: at-least-once delivery means an adapter may
	// re-ack a message it re-read.
	if err := db.AckChatOutbound(ButlerSurfaceFamily, first); err != nil {
		t.Fatalf("second Ack: %v", err)
	}
	// Acking another surface's message is not this adapter's to do.
	if err := db.AckChatOutbound(ButlerSurfaceFamily, 3); err == nil {
		t.Fatalf("a family adapter acked an admin message")
	}
	msgs, _ = db.ChatOutboundAfter(ButlerSurfaceFamily, "telegram", 0, 0)
	if len(msgs) != 1 || msgs[0].Seq != second {
		t.Fatalf("after ack, msgs = %+v", msgs)
	}
}

// A repeated dedupe key returns the EXISTING message rather than
// queueing a second one, so a retried producer converges on one message.
func TestChatOutboundDedupe(t *testing.T) {
	db := chatTestDB(t)
	first := mustEnqueue(t, db, ChatOutbound{
		Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "1",
		Body: "hello", DedupeKey: "same",
	})
	again, err := db.EnqueueChatOutbound(ChatOutbound{
		Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "1",
		Body: "hello", DedupeKey: "same",
	})
	if err != ErrDuplicateOutbound || again != first {
		t.Fatalf("re-enqueue = %d, %v; want the existing seq with ErrDuplicateOutbound", again, err)
	}
	msgs, _ := db.ChatOutboundAfter(ButlerSurfaceFamily, "", 0, 0)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %+v, want one", msgs)
	}
}

// The queue is a queue, not a log: settled messages are swept, and a
// message still waiting is never swept out from under the adapter that
// has not collected it yet.
func TestChatOutboundPrunesSettledMessages(t *testing.T) {
	db := chatTestDB(t)
	old := time.Now().Add(-ChatOutboundRetention - time.Hour).UTC().Format(time.RFC3339Nano)

	// Three settled-and-old rows and one that is old but STILL PENDING —
	// an adapter that has been down for a week must come back to its
	// undelivered message, not to an empty queue.
	for _, row := range []struct{ key, delivered, expires string }{
		{"delivered-old", time.Now().UTC().Format(time.RFC3339Nano), time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)},
		{"expired-old", "", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
		{"pending-old", "", time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)},
	} {
		if _, err := db.sql.Exec(
			`INSERT INTO chat_outbound (surface, transport, target, body, kind, dedupe_key, created_at, expires_at, delivered_at)
			 VALUES ('family','telegram','1','body','reply',?,?,?,?)`,
			row.key, old, row.expires, row.delivered); err != nil {
			t.Fatalf("seed %s: %v", row.key, err)
		}
	}

	// The sweep runs on enqueue.
	mustEnqueue(t, db, ChatOutbound{
		Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "1",
		Body: "fresh", DedupeKey: "fresh",
	})

	var keys []string
	rows, err := db.sql.Query(`SELECT dedupe_key FROM chat_outbound ORDER BY dedupe_key`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		keys = append(keys, k)
	}
	if len(keys) != 2 || keys[0] != "fresh" || keys[1] != "pending-old" {
		t.Fatalf("after the sweep, keys = %v; want the fresh message and the undelivered one", keys)
	}
}

// A question is claimed once: two replies arriving together produce one
// answer, and a question older than the window is not an answer to
// today's "hello" (ADR-W026 A8, ADR-W024 H4).
func TestChatQuestionClaimIsOnce(t *testing.T) {
	db := chatTestDB(t)
	if _, err := db.RecordChatQuestion(ChatQuestion{
		CorrelationID: "corr-1", Surface: ButlerSurfaceFamily,
		Transport: "telegram", Target: "12345", Rule: "mail-check", Subject: "sam",
	}); err != nil {
		t.Fatalf("RecordChatQuestion: %v", err)
	}

	q, err := db.ClaimChatAnswer("telegram", "12345", 0)
	if err != nil {
		t.Fatalf("ClaimChatAnswer: %v", err)
	}
	if q.CorrelationID != "corr-1" || q.Rule != "mail-check" || q.Subject != "sam" {
		t.Fatalf("claimed %+v", q)
	}
	if _, err := db.ClaimChatAnswer("telegram", "12345", 0); err != ErrNotFound {
		t.Fatalf("second claim = %v, want ErrNotFound", err)
	}

	// A different conversation's question is not this one's to answer.
	if _, err := db.RecordChatQuestion(ChatQuestion{
		CorrelationID: "corr-2", Surface: ButlerSurfaceFamily,
		Transport: "telegram", Target: "99999",
	}); err != nil {
		t.Fatalf("RecordChatQuestion: %v", err)
	}
	if _, err := db.ClaimChatAnswer("telegram", "12345", 0); err != ErrNotFound {
		t.Fatalf("a question for another conversation was claimed")
	}

	// And an old question expires rather than swallowing a later message.
	if _, err := db.ClaimChatAnswer("telegram", "99999", time.Nanosecond); err != ErrNotFound {
		t.Fatalf("a question outside the window was claimed")
	}
}

// A body longer than the bound is cut on a rune boundary. Cutting
// mid-rune leaves invalid UTF-8, which a JSON encoder mangles and
// Telegram refuses outright — so a long reply in any non-ASCII language
// would fail to send rather than arrive shortened.
func TestChatOutboundTruncatesOnRuneBoundaries(t *testing.T) {
	db := chatTestDB(t)
	// Three-byte runes, so the byte bound lands mid-rune.
	long := strings.Repeat("な", MaxChatBodyBytes)
	seq := mustEnqueue(t, db, ChatOutbound{
		Surface: ButlerSurfaceFamily, Transport: "telegram", Target: "1",
		Body: long, DedupeKey: "long",
	})
	msgs, err := db.ChatOutboundAfter(ButlerSurfaceFamily, "", 0, 0)
	if err != nil {
		t.Fatalf("ChatOutboundAfter: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Seq != seq {
		t.Fatalf("msgs = %+v", msgs)
	}
	if !utf8.ValidString(msgs[0].Body) {
		t.Fatalf("the stored body is not valid UTF-8 (%d bytes)", len(msgs[0].Body))
	}
	if len(msgs[0].Body) > MaxChatBodyBytes {
		t.Fatalf("body = %d bytes, want it bounded to %d", len(msgs[0].Body), MaxChatBodyBytes)
	}
}
