package runtimedb

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// chatTestDB gives a database two Agents with a bot each, and returns
// the household Agent's Connection and the Owner Agent's.
//
// Two of them, because since 0045 the queue is per Connection and the
// interesting assertion is that one Agent's bot does not collect
// another's messages. A surface could not express that: both Owner
// Agents are `admin`.
func uncoordinatedChatTestDB(t *testing.T) (*DB, Connection, Connection) {
	t.Helper()
	db, err := Open(t.TempDir() + "/keystone.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.UpsertModel(Model{
		Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	record(t, db, "mika")
	return db,
		connect(t, db, "sandy", "telegram", ButlerSurfaceFamily),
		connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin)
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
	db, family, admin := chatTestDB(t)
	first := mustEnqueue(t, db, ChatOutbound{
		Connection: family.Seq, Target: "1",
		Body: "one", DedupeKey: "k1",
	})
	second := mustEnqueue(t, db, ChatOutbound{
		Connection: family.Seq, Target: "1",
		Body: "two", DedupeKey: "k2",
	})
	// Another AGENT's message is not this Agent's bot to deliver. The
	// two are both on telegram, which is the case a surface key could
	// not separate at all.
	mustEnqueue(t, db, ChatOutbound{
		Connection: admin.Seq, Target: "9",
		Body: "alfred", DedupeKey: "k3",
	})
	// Already expired: queued, never collected, and now past its life.
	mustEnqueue(t, db, ChatOutbound{
		Connection: family.Seq, Target: "1",
		Body: "stale", DedupeKey: "k4", ExpiresAt: time.Now().Add(-time.Minute),
	})

	msgs, err := db.ChatOutboundAfter(family, 0, 0)
	if err != nil {
		t.Fatalf("ChatOutboundAfter: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Seq != first || msgs[1].Seq != second {
		t.Fatalf("msgs = %+v, want the two live family messages oldest first", msgs)
	}

	if err := db.AckChatOutbound(family, first); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	// Acking twice is fine: at-least-once delivery means an adapter may
	// re-ack a message it re-read.
	if err := db.AckChatOutbound(family, first); err != nil {
		t.Fatalf("second Ack: %v", err)
	}
	// Acking another surface's message is not this adapter's to do.
	if err := db.AckChatOutbound(family, 3); err == nil {
		t.Fatalf("a family adapter acked an admin message")
	}
	msgs, _ = db.ChatOutboundAfter(family, 0, 0)
	if len(msgs) != 1 || msgs[0].Seq != second {
		t.Fatalf("after ack, msgs = %+v", msgs)
	}
}

// A repeated dedupe key returns the EXISTING message rather than
// queueing a second one, so a retried producer converges on one message.
func TestChatOutboundDedupe(t *testing.T) {
	db, family, _ := chatTestDB(t)
	first := mustEnqueue(t, db, ChatOutbound{
		Connection: family.Seq, Target: "1",
		Body: "hello", DedupeKey: "same",
	})
	again, err := db.EnqueueChatOutbound(ChatOutbound{
		Connection: family.Seq, Target: "1",
		Body: "hello", DedupeKey: "same",
	})
	if err != ErrDuplicateOutbound || again != first {
		t.Fatalf("re-enqueue = %d, %v; want the existing seq with ErrDuplicateOutbound", again, err)
	}
	msgs, _ := db.ChatOutboundAfter(family, 0, 0)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %+v, want one", msgs)
	}
}

// The queue is a queue, not a log: settled messages are swept, and a
// message still waiting is never swept out from under the adapter that
// has not collected it yet.
func TestChatOutboundPrunesSettledMessages(t *testing.T) {
	db, family, _ := chatTestDB(t)
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
			`INSERT INTO chat_outbound (connection, target, body, kind, dedupe_key, created_at, expires_at, delivered_at)
			 VALUES (?,'1','body','reply',?,?,?,?)`,
			family.Seq, row.key, old, row.expires, row.delivered); err != nil {
			t.Fatalf("seed %s: %v", row.key, err)
		}
	}

	// The sweep runs on enqueue.
	mustEnqueue(t, db, ChatOutbound{
		Connection: family.Seq, Target: "1",
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
	db, family, _ := chatTestDB(t)
	if _, err := db.RecordChatQuestion(ChatQuestion{
		CorrelationID: "corr-1", Connection: family.Seq,
		Target: "12345", Rule: "mail-check", Subject: "sam",
	}); err != nil {
		t.Fatalf("RecordChatQuestion: %v", err)
	}

	q, err := db.ClaimChatAnswer(family, "12345", 0)
	if err != nil {
		t.Fatalf("ClaimChatAnswer: %v", err)
	}
	if q.CorrelationID != "corr-1" || q.Rule != "mail-check" || q.Subject != "sam" {
		t.Fatalf("claimed %+v", q)
	}
	if _, err := db.ClaimChatAnswer(family, "12345", 0); err != ErrNotFound {
		t.Fatalf("second claim = %v, want ErrNotFound", err)
	}

	// A different conversation's question is not this one's to answer.
	if _, err := db.RecordChatQuestion(ChatQuestion{
		CorrelationID: "corr-2", Connection: family.Seq, Target: "99999",
	}); err != nil {
		t.Fatalf("RecordChatQuestion: %v", err)
	}
	if _, err := db.ClaimChatAnswer(family, "12345", 0); err != ErrNotFound {
		t.Fatalf("a question for another conversation was claimed")
	}

	// And an old question expires rather than swallowing a later message.
	if _, err := db.ClaimChatAnswer(family, "99999", time.Nanosecond); err != ErrNotFound {
		t.Fatalf("a question outside the window was claimed")
	}
}

// An answer to one Agent's bot does not close another Agent's question
// (ADR-W033 B7).
//
// The case the old key could not express at all. A Telegram direct
// chat's target is the SAME user id at every bot, so a ledger keyed on
// (transport, target) had one row set for both bots: sam's "yes" to
// Robert would claim Alfred's newer question and file
// household.answered under the rule Alfred asked for. The rule that did
// ask never hears back, and the one that did not gets an answer it
// cannot have wanted — both silently.
func TestAnAnswerToOneBotDoesNotCloseAnothersQuestion(t *testing.T) {
	db := chatQuestionDB(t)
	alfred := connect(t, db, "alfred", "telegram", ButlerSurfaceAdmin, "chat-telegram-admin")
	robert := connect(t, db, "robert", "telegram", ButlerSurfaceAdmin, "chat-telegram-second")

	// The same person, in a direct chat, at both bots.
	const target = "12345"
	for _, q := range []ChatQuestion{
		{CorrelationID: "corr-robert", Connection: robert.Seq, Target: target, Rule: "bins"},
		{CorrelationID: "corr-alfred", Connection: alfred.Seq, Target: target, Rule: "mail-check"},
	} {
		if _, err := db.RecordChatQuestion(q); err != nil {
			t.Fatalf("RecordChatQuestion(%s): %v", q.Rule, err)
		}
	}

	// Alfred's is the NEWER row, which is what a ledger keyed on
	// (transport, target) would hand back to either bot.
	got, err := db.ClaimChatAnswer(robert, target, 0)
	if err != nil {
		t.Fatalf("ClaimChatAnswer at robert: %v", err)
	}
	if got.CorrelationID != "corr-robert" {
		t.Fatalf("an answer at robert's bot claimed %q, want corr-robert", got.CorrelationID)
	}

	// And Alfred's question is still open, waiting for its own reply.
	still, err := db.ClaimChatAnswer(alfred, target, 0)
	if err != nil {
		t.Fatalf("ClaimChatAnswer at alfred: %v", err)
	}
	if still.CorrelationID != "corr-alfred" {
		t.Fatalf("alfred's question claimed %q, want corr-alfred", still.CorrelationID)
	}
}

// chatQuestionDB is chatTestDB without its two connections, for a test
// that makes its own.
func chatQuestionDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir() + "/keystone.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.UpsertModel(Model{
		Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	record(t, db, "mika")
	return db
}

// A body longer than the bound is cut on a rune boundary. Cutting
// mid-rune leaves invalid UTF-8, which a JSON encoder mangles and
// Telegram refuses outright — so a long reply in any non-ASCII language
// would fail to send rather than arrive shortened.
func TestChatOutboundTruncatesOnRuneBoundaries(t *testing.T) {
	db, family, _ := chatTestDB(t)
	// Three-byte runes, so the byte bound lands mid-rune.
	long := strings.Repeat("な", MaxChatBodyBytes)
	seq := mustEnqueue(t, db, ChatOutbound{
		Connection: family.Seq, Target: "1",
		Body: long, DedupeKey: "long",
	})
	msgs, err := db.ChatOutboundAfter(family, 0, 0)
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

func chatTestDB(t *testing.T) (*DB, Connection, Connection) {
	t.Helper()
	db, family, admin := uncoordinatedChatTestDB(t)
	if _, err := db.AcquireCoordinator("effect-fixture"); err != nil {
		t.Fatal(err)
	}
	return db, family, admin
}
