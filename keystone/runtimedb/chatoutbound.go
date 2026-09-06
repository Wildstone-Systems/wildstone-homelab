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
	"errors"
	"fmt"
	"strings"
	"time"
)

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
	Seq           int64         `json:"seq"`
	Surface       ButlerSurface `json:"surface"`
	Transport     string        `json:"transport"`
	Target        string        `json:"target"`
	Body          string        `json:"body"`
	Kind          string        `json:"kind"`
	BindingID     int64         `json:"bindingId,omitempty"`
	CorrelationID string        `json:"correlationId,omitempty"`
	DedupeKey     string        `json:"dedupeKey"`
	CreatedAt     time.Time     `json:"createdAt"`
	ExpiresAt     time.Time     `json:"expiresAt"`
	DeliveredAt   time.Time     `json:"deliveredAt,omitempty"`
}

// ErrDuplicateOutbound reports that this dedupe key is already queued or
// already delivered. It is not a failure: the caller's message exists.
var ErrDuplicateOutbound = errors.New("runtimedb: chat message already queued")

// EnqueueChatOutbound queues one message and returns its seq. A repeated
// dedupe key returns the EXISTING seq with ErrDuplicateOutbound, so a
// retried producer converges on one message rather than two.
func (db *DB) EnqueueChatOutbound(m ChatOutbound) (int64, error) {
	if !ValidButlerSurface(m.Surface) {
		return 0, fmt.Errorf("runtimedb: chat message surface %q is not admin|family", m.Surface)
	}
	if m.Transport == "" || m.Target == "" || m.DedupeKey == "" {
		return 0, fmt.Errorf("runtimedb: chat message requires a transport, target and dedupe key")
	}
	if strings.TrimSpace(m.Body) == "" {
		return 0, fmt.Errorf("runtimedb: refusing to queue an empty chat message")
	}
	if len(m.Body) > MaxChatBodyBytes {
		m.Body = m.Body[:MaxChatBodyBytes]
	}
	if m.Kind == "" {
		m.Kind = "reply"
	}
	if m.ExpiresAt.IsZero() {
		m.ExpiresAt = time.Now().Add(DefaultChatOutboundTTL)
	}
	res, err := db.sql.Exec(
		`INSERT INTO chat_outbound (surface, transport, target, body, kind, binding_id, correlation_id, dedupe_key, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(m.Surface), m.Transport, m.Target, m.Body, m.Kind, m.BindingID, m.CorrelationID,
		m.DedupeKey, now(), m.ExpiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			var seq int64
			if qerr := db.sql.QueryRow(
				`SELECT seq FROM chat_outbound WHERE dedupe_key = ?`, m.DedupeKey).Scan(&seq); qerr == nil {
				return seq, ErrDuplicateOutbound
			}
		}
		return 0, fmt.Errorf("runtimedb: queuing chat message: %w", err)
	}
	return res.LastInsertId()
}

// ChatOutboundAfter returns undelivered, unexpired messages for one
// surface after the cursor, oldest first. transport "" reads every
// transport on that surface — useful for an operator looking at the
// queue, and not what an adapter does.
func (db *DB) ChatOutboundAfter(surface ButlerSurface, transport string, after int64, limit int) ([]ChatOutbound, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	args := []any{string(surface), after, now()}
	where := `surface = ? AND seq > ? AND delivered_at = '' AND expires_at > ?`
	if transport != "" {
		where += " AND transport = ?"
		args = append(args, transport)
	}
	args = append(args, limit)
	rows, err := db.sql.Query(
		`SELECT seq, surface, transport, target, body, kind, binding_id, correlation_id, dedupe_key, created_at, expires_at, delivered_at
		 FROM chat_outbound WHERE `+where+` ORDER BY seq ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading chat outbound: %w", err)
	}
	defer rows.Close()
	var out []ChatOutbound
	for rows.Next() {
		var m ChatOutbound
		var surfaceStr, created, expires, delivered string
		if err := rows.Scan(&m.Seq, &surfaceStr, &m.Transport, &m.Target, &m.Body, &m.Kind,
			&m.BindingID, &m.CorrelationID, &m.DedupeKey, &created, &expires, &delivered); err != nil {
			return nil, err
		}
		m.Surface = ButlerSurface(surfaceStr)
		var perr error
		if m.CreatedAt, perr = time.Parse(time.RFC3339Nano, created); perr != nil {
			return nil, perr
		}
		if m.ExpiresAt, perr = time.Parse(time.RFC3339Nano, expires); perr != nil {
			return nil, perr
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AckChatOutbound marks one message delivered. Acking a message that is
// already acked is not an error — a redelivered message acked twice is
// the normal shape of at-least-once — but acking one that does not exist
// is ErrNotFound, because that means the adapter and the queue disagree
// about reality.
func (db *DB) AckChatOutbound(surface ButlerSurface, seq int64) error {
	var existing string
	err := db.sql.QueryRow(
		`SELECT delivered_at FROM chat_outbound WHERE seq = ? AND surface = ?`, seq, string(surface)).Scan(&existing)
	if err != nil {
		return ErrNotFound
	}
	if existing != "" {
		return nil
	}
	_, err = db.sql.Exec(`UPDATE chat_outbound SET delivered_at = ? WHERE seq = ?`, now(), seq)
	return err
}
