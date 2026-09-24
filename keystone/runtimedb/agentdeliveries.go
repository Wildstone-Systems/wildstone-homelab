package runtimedb

// agent_deliveries (migration 0084, ADR-W039 §3.2 Events & subscriptions,
// §3.6; Phase 3, #339). Phase 3 writes exactly one kind — proposal_outcome,
// the moment a Plan a runtime filed is approved or rejected — because
// that is the one delivery a synchronous decision produces outright,
// with no subscription match to wait on. Phase 4 adds the long-poll
// consumer (cursor, ack, redelivery, expiry) and the other three kinds.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// DefaultAgentDeliveryTTL is how long an unacknowledged delivery stays
// offered before the long-poll's own consumer marks it expired (ADR-W039
// §3.6): a week is long enough that a runtime down over a weekend still
// finds its deliveries Monday, and short enough that "still pending"
// stops meaning "will never be answered".
const DefaultAgentDeliveryTTL = 7 * 24 * time.Hour

// AgentDelivery is one row of agent_deliveries, as read back.
type AgentDelivery struct {
	ID        int64
	Butler    int64
	Kind      agentapi.DeliveryKind
	Cursor    string
	Payload   string
	CreatedAt time.Time
	ExpiresAt time.Time
	AckedAt   time.Time
	ExpiredAt time.Time
}

// RecordAgentDelivery inserts one delivery for the named Agent (by
// Butler seq) and returns its id. payload is opaque JSON, exactly as
// agentapi.Delivery.Payload carries it; this table does not interpret
// it.
func (db *DB) RecordAgentDelivery(butlerSeq int64, kind agentapi.DeliveryKind, payload string) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if !kind.Valid() {
		return 0, fmt.Errorf("runtimedb: unknown delivery kind %q", kind)
	}
	if butlerSeq == 0 {
		return 0, fmt.Errorf("runtimedb: a delivery needs the agent it is for")
	}
	if payload == "" {
		payload = "{}"
	}
	res, err := db.sql.Exec(
		`INSERT INTO agent_deliveries(butler, kind, payload, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		butlerSeq, string(kind), payload, now(), time.Now().Add(DefaultAgentDeliveryTTL).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecordSubscriptionDelivery inserts one `subscription`-kind delivery
// (ADR-W039 §3.6, Phase 4, #341), carrying the per-subscription cursor
// position it advances to alongside the payload — the one field
// RecordAgentDelivery's other three callers (a proposal outcome, a
// re-offered turn) have no cursor for at all.
func (db *DB) RecordSubscriptionDelivery(butlerSeq int64, cursor, payload string) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if butlerSeq == 0 {
		return 0, fmt.Errorf("runtimedb: a delivery needs the agent it is for")
	}
	if payload == "" {
		payload = "{}"
	}
	res, err := db.sql.Exec(
		`INSERT INTO agent_deliveries(butler, kind, cursor, payload, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		butlerSeq, string(agentapi.DeliverySubscription), cursor, payload, now(),
		time.Now().Add(DefaultAgentDeliveryTTL).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const agentDeliveryColumns = `
	SELECT id, butler, kind, cursor, payload, created_at, expires_at, acked_at, expired_at
	  FROM agent_deliveries`

func scanAgentDelivery(s interface{ Scan(...any) error }) (AgentDelivery, error) {
	var d AgentDelivery
	var kind, created, expires, acked, expired string
	if err := s.Scan(&d.ID, &d.Butler, &kind, &d.Cursor, &d.Payload, &created, &expires, &acked, &expired); err != nil {
		return AgentDelivery{}, err
	}
	d.Kind = agentapi.DeliveryKind(kind)
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	for _, f := range []struct {
		text string
		into *time.Time
	}{{expires, &d.ExpiresAt}, {acked, &d.AckedAt}, {expired, &d.ExpiredAt}} {
		if f.text != "" {
			*f.into, _ = time.Parse(time.RFC3339Nano, f.text)
		}
	}
	return d, nil
}

// ListAgentDeliveries returns an Agent's deliveries, oldest first. limit
// <= 0 defaults to 50.
func (db *DB) ListAgentDeliveries(butlerSeq int64, limit int) ([]AgentDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.Query(agentDeliveryColumns+` WHERE butler = ? ORDER BY id ASC LIMIT ?`, butlerSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentDelivery
	for rows.Next() {
		d, err := scanAgentDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkExpiredAgentDeliveries stamps expired_at on every one of an
// Agent's deliveries that is unacknowledged, unexpired, and whose
// expires_at has passed asOf (ADR-W039 §3.6: "an unacknowledged
// delivery is recorded as expired, never dropped"). It returns how many
// it marked.
func (db *DB) MarkExpiredAgentDeliveries(butlerSeq int64, asOf time.Time) (int, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	res, err := db.sql.Exec(
		`UPDATE agent_deliveries SET expired_at = ?
		  WHERE butler = ? AND acked_at = '' AND expired_at = ''
		    AND expires_at <> '' AND expires_at < ?`,
		now(), butlerSeq, asOf.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("runtimedb: marking expired deliveries for butler %d: %w", butlerSeq, err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListUnackedAgentDeliveries is the long-poll's own read (ADR-W039
// §3.6, Phase 4, #341): an Agent's own deliveries, oldest first, with
// id > after, excluding both what it has already acknowledged and what
// MarkExpiredAgentDeliveries has already marked expired. Redelivering
// the same id is the caller's own idempotent contract (§4 item 6) — this
// simply keeps returning a row until it is acked or expires.
func (db *DB) ListUnackedAgentDeliveries(butlerSeq, after int64, limit int) ([]AgentDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.Query(
		agentDeliveryColumns+`
		 WHERE butler = ? AND id > ? AND acked_at = '' AND expired_at = ''
		 ORDER BY id ASC LIMIT ?`,
		butlerSeq, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentDelivery
	for rows.Next() {
		d, err := scanAgentDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AckAgentDelivery sets acked_at for one of an Agent's own deliveries.
// Idempotent: acking an already-acked delivery a second time succeeds
// without changing its timestamp (§4 item 6, "tolerates redelivery of
// the same ID"; the same tolerance applies to the ack itself). It
// refuses to ack a delivery that belongs to another Agent, or does not
// exist, with ErrNotFound.
func (db *DB) AckAgentDelivery(butlerSeq, id int64) error {
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	var exists int64
	err := db.sql.QueryRow(`SELECT 1 FROM agent_deliveries WHERE id = ? AND butler = ?`, id, butlerSeq).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: acking delivery %d: %w", id, err)
	}
	if _, err := db.sql.Exec(
		`UPDATE agent_deliveries SET acked_at = ? WHERE id = ? AND acked_at = ''`, now(), id); err != nil {
		return fmt.Errorf("runtimedb: acking delivery %d: %w", id, err)
	}
	return nil
}
