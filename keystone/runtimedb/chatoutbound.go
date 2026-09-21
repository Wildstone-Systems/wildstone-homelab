package runtimedb

// The outbound chat queue (ADR-W026 A8).
//
// Two things are queued here: a reply to something somebody asked, and a
// push a household rule produced. Both are read the same way — an
// adapter polls its surface and transport with a cursor, delivers, and
// acks — so a rule that notifies over chat and a conversation that
// answers over chat share one delivery path and one failure mode.
//
// A message that nobody collects expires rather than accumulating. An
// adapter that has been down for a day should come back to the last few
// minutes of conversation, not to yesterday's.

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"strings"
	"time"
	"unicode/utf8"
)

// truncateUTF8 bounds s to at most n bytes, cutting on a rune boundary.
// Cutting mid-rune is not a cosmetic problem: the result is invalid
// UTF-8, which a JSON encoder mangles and some transports refuse, so a
// long reply would fail to send rather than arrive shortened.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ChatOutboundRetention is how long a settled message — delivered, or
// expired undelivered — stays in the table before it is swept.
//
// The queue is a queue, not a log: the household stream records what a
// rule did, the butler_sessions table records what was said, and this
// records only what still needs delivering. Keeping every message
// forever would make the busiest table in the deployment the one nothing
// reads, and its dedupe index the thing that grows without bound.
const ChatOutboundRetention = 7 * 24 * time.Hour

// DefaultChatOutboundTTL is how long a queued message stays deliverable.
// A chat reply is worthless long after the question, and a stale one is
// worse than none: it arrives with no context and reads like the house
// talking to itself.
const DefaultChatOutboundTTL = 6 * time.Hour

// MaxChatBodyBytes bounds one queued message. Chat transports have their
// own limits (Telegram's is about 4096 characters); this is the bound
// that holds regardless of transport.
const MaxChatBodyBytes = 3500

// ChatOutbound is one queued message.
type ChatOutbound struct {
	Operation operation.Identity `json:"-"`
	Seq       int64              `json:"seq"`
	// Connection is the bot this message leaves by, and so the Agent it
	// is from. The queue used to be keyed by surface, which handed one
	// bot every Owner Agent's replies (ADR-W033 B7).
	Connection    int64  `json:"connection"`
	Transport     string `json:"transport"`
	Target        string `json:"target"`
	Body          string `json:"body"`
	Kind          string `json:"kind"`
	BindingID     int64  `json:"bindingId,omitempty"`
	CorrelationID string `json:"correlationId,omitempty"`
	DedupeKey     string `json:"dedupeKey"`
	// HoldUntil defers delivery without dropping the message: a
	// non-urgent notice raised during the household's quiet hours is
	// queued now and becomes visible to the adapter when the quiet
	// window ends (ADR-W030 A10). Zero means deliverable immediately.
	HoldUntil   time.Time `json:"holdUntil,omitzero"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	DeliveredAt time.Time `json:"deliveredAt,omitempty"`
}

// ErrDuplicateOutbound reports that this dedupe key is already queued or
// already delivered. It is not a failure: the caller's message exists.
var ErrDuplicateOutbound = errors.New("runtimedb: chat message already queued")

// EnqueueChatOutbound queues one message and returns its seq. A repeated
// dedupe key returns the EXISTING seq with ErrDuplicateOutbound, so a
// retried producer converges on one message rather than two.
func (db *DB) EnqueueChatOutbound(m ChatOutbound) (int64, error) {
	if err := db.CheckHouseholdExecution(); err != nil {
		return 0, err
	}
	if m.Connection == 0 {
		return 0, fmt.Errorf("runtimedb: a chat message leaves by a connection (ADR-W033 B7)")
	}
	if m.Target == "" || m.DedupeKey == "" {
		return 0, fmt.Errorf("runtimedb: chat message requires a target and dedupe key")
	}
	if strings.TrimSpace(m.Body) == "" {
		return 0, fmt.Errorf("runtimedb: refusing to queue an empty chat message")
	}
	m.Body = truncateUTF8(m.Body, MaxChatBodyBytes)
	if m.Kind == "" {
		m.Kind = "reply"
	}
	// Pushes additionally retain their proactive-delivery policy gate. All
	// messages already passed the lease and dispatch-stop boundary above.
	if m.Kind == "push" {
		if err := db.gate(GateDelivery); err != nil {
			return 0, err
		}
	}
	if m.ExpiresAt.IsZero() {
		m.ExpiresAt = time.Now().Add(DefaultChatOutboundTTL)
	}
	db.pruneChatOutbound()
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`INSERT INTO chat_outbound (connection, target, body, kind, binding_id, correlation_id, dedupe_key, created_at, expires_at, hold_until)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Connection, m.Target, m.Body, m.Kind, m.BindingID, m.CorrelationID,
		m.DedupeKey, now(), m.ExpiresAt.UTC().Format(time.RFC3339Nano), timeText(m.HoldUntil))
	if err != nil {
		_ = tx.Rollback()
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			var seq int64
			if qerr := db.sql.QueryRow(
				`SELECT seq FROM chat_outbound WHERE dedupe_key = ?`, m.DedupeKey).Scan(&seq); qerr == nil {
				return seq, ErrDuplicateOutbound
			}
		}
		return 0, fmt.Errorf("runtimedb: queuing chat message: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if m.Operation.TaskID != "" {
		task, err := parseButlerTaskID(m.Operation.TaskID)
		if err != nil {
			return 0, err
		}
		if _, err = tx.Exec("INSERT INTO chat_outbound_effects(outbound,task,step,permission_label,audience,expected_body) VALUES(?,?,?,?,?,?)", seq, task, m.Operation.Step, m.Operation.PermissionLabel, m.Operation.Audience, m.Body); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// ChatOutboundAfter returns undelivered, unexpired messages for one
// Connection after the cursor, oldest first — which is what an adapter
// polls: its own bot's messages, and no other Agent's.
func (db *DB) ChatOutboundAfter(at Connection, after int64, limit int) ([]ChatOutbound, error) {
	if err := db.CheckHouseholdExecution(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	// Quarantine is checked on the READ, not only on the write.
	//
	stamp := now()
	// A held message stays queued and deduplicated; it is simply not
	// offered to the adapter until its hold expires (ADR-W030 A10).
	args := []any{at.Seq, after, stamp, stamp}
	where := `o.connection = ? AND o.seq > ? AND o.delivered_at = '' AND o.expires_at > ? AND (o.hold_until = '' OR o.hold_until <= ?)`
	args = append(args, limit)
	rows, err := db.sql.Query(
		outboundColumns+` WHERE `+where+` ORDER BY o.seq ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading chat outbound: %w", err)
	}
	defer rows.Close()
	return scanChatOutbound(rows)
}

// The transport comes from the Connection, which is the row that owns
// that fact.
const outboundColumns = `SELECT o.seq, o.connection, conn.transport, o.target, o.body, o.kind,
	 o.binding_id, o.correlation_id, o.dedupe_key, o.created_at, o.expires_at, o.delivered_at, o.hold_until
	 FROM chat_outbound o JOIN butler_addresses conn ON conn.seq = o.connection`

// scanChatOutbound reads the shared column list both queue reads select.
func scanChatOutbound(rows *sql.Rows) ([]ChatOutbound, error) {
	var out []ChatOutbound
	for rows.Next() {
		var m ChatOutbound
		var created, expires, delivered, hold string
		if err := rows.Scan(&m.Seq, &m.Connection, &m.Transport, &m.Target, &m.Body, &m.Kind,
			&m.BindingID, &m.CorrelationID, &m.DedupeKey, &created, &expires, &delivered, &hold); err != nil {
			return nil, err
		}
		var err error
		if m.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("runtimedb: bad chat created_at %q: %w", created, err)
		}
		if m.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
			return nil, fmt.Errorf("runtimedb: bad chat expires_at %q: %w", expires, err)
		}
		if hold != "" {
			if m.HoldUntil, err = time.Parse(time.RFC3339Nano, hold); err != nil {
				return nil, fmt.Errorf("runtimedb: bad chat hold_until %q: %w", hold, err)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ChatOutboundHeld returns queued, undelivered messages whose quiet-
// hours hold has not yet expired — what ChatOutboundAfter deliberately
// will not show an adapter.
//
// It exists so that "held until morning" is visible rather than
// indistinguishable from "never queued". A notice nobody can see is
// operationally the same as a notice that was dropped, and the whole
// point of holding rather than dropping is that it will arrive.
func (db *DB) ChatOutboundHeld(at Connection) ([]ChatOutbound, error) {
	stamp := now()
	rows, err := db.sql.Query(
		outboundColumns+`
		 WHERE o.connection = ? AND o.delivered_at = '' AND o.expires_at > ? AND o.hold_until > ?
		 ORDER BY o.seq ASC`, at.Seq, stamp, stamp)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading held chat outbound: %w", err)
	}
	defer rows.Close()
	return scanChatOutbound(rows)
}

// AckChatOutbound marks one message delivered. Acking a message that is
// already acked is not an error — a redelivered message acked twice is
// the normal shape of at-least-once — but acking one that does not exist
// is ErrNotFound, because that means the adapter and the queue disagree
// about reality.
func (db *DB) AckChatOutbound(at Connection, seq int64) error {
	var existing string
	err := db.sql.QueryRow(
		`SELECT delivered_at FROM chat_outbound WHERE seq = ? AND connection = ?`, seq, at.Seq).Scan(&existing)
	if err != nil {
		return ErrNotFound
	}
	if existing != "" {
		return nil
	}
	_, err = db.sql.Exec(`UPDATE chat_outbound SET delivered_at = ? WHERE seq = ?`, now(), seq)
	return err
}

// pruneChatOutbound drops settled messages older than the retention
// window. It runs on enqueue rather than on a timer: the table only
// grows when something is queued, so that is exactly when it is worth
// looking, and a household's message rate makes this a few rows at a
// time over an indexed predicate.
//
// A failure is logged nowhere and returned nowhere on purpose: failing
// to tidy up must never fail the message that was being queued.
func (db *DB) pruneChatOutbound() {
	cutoff := time.Now().Add(-ChatOutboundRetention).UTC().Format(time.RFC3339Nano)
	db.sql.Exec( //nolint:errcheck // best effort: tidying must not fail a send
		`DELETE FROM chat_outbound
		 WHERE created_at < ? AND (delivered_at != '' OR expires_at < ?)`,
		cutoff, now())
	// Inbound claims are swept on the same schedule and for the same
	// reason. A claim only has to outlive the window in which a
	// transport might redeliver its message; keeping them forever would
	// make an old claim point at an outbound row that has itself been
	// pruned, and the adapter would be answered with a seq that no
	// longer exists.
	db.sql.Exec( //nolint:errcheck // best effort
		`DELETE FROM chat_inbound WHERE claimed_at < ?`, cutoff)
}

// ClaimChatInbound reserves one inbound message before anything acts on
// it (ADR-W026 A8). It returns claimed=false when this message has been
// handled already, along with the seq of the reply it produced, so a
// redelivery can be acknowledged with the same answer rather than
// answered twice.
//
// The claim is taken BEFORE the session rather than after, because
// everything expensive and everything with side effects happens in
// between: the provider call, the session record, and on the family
// surface a domestic action that may notify a person.
func (db *DB) ClaimChatInbound(dedupeKey string, surface ButlerSurface) (claimed bool, seq int64, err error) {
	if dedupeKey == "" {
		return false, 0, fmt.Errorf("runtimedb: an inbound claim needs a dedupe key")
	}
	if !ValidButlerSurface(surface) {
		return false, 0, fmt.Errorf("runtimedb: inbound claim surface %q is not admin|family", surface)
	}
	_, err = db.sql.Exec(
		`INSERT INTO chat_inbound (dedupe_key, surface, claimed_at) VALUES (?, ?, ?)`,
		dedupeKey, string(surface), now())
	if err == nil {
		return true, 0, nil
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return false, 0, fmt.Errorf("runtimedb: claiming inbound message: %w", err)
	}
	if qerr := db.sql.QueryRow(
		`SELECT queued_seq FROM chat_inbound WHERE dedupe_key = ?`, dedupeKey).Scan(&seq); qerr != nil {
		return false, 0, nil
	}
	return false, seq, nil
}

// RecordChatInboundReply attaches the queued reply to a claim, so a
// later redelivery can point at the answer that was already given.
func (db *DB) RecordChatInboundReply(dedupeKey string, seq int64) error {
	_, err := db.sql.Exec(`UPDATE chat_inbound SET queued_seq = ? WHERE dedupe_key = ?`, seq, dedupeKey)
	return err
}

// ReleaseChatInbound gives a claim back. A session that failed produced
// no reply and no side effects worth protecting, so the message should
// be retryable — at-most-once is about not repeating what HAPPENED, not
// about refusing to retry what did not.
func (db *DB) ReleaseChatInbound(dedupeKey string) error {
	_, err := db.sql.Exec(`DELETE FROM chat_inbound WHERE dedupe_key = ? AND queued_seq = 0`, dedupeKey)
	return err
}
