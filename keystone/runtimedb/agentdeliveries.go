package runtimedb

// agent_deliveries (migration 0084, ADR-W039 §3.2 Events & subscriptions,
// §3.6; Phase 3, #339). Phase 3 writes exactly one kind — proposal_outcome,
// the moment a Plan a runtime filed is approved or rejected — because
// that is the one delivery a synchronous decision produces outright,
// with no subscription match to wait on. Phase 4 adds the long-poll
// consumer (cursor, ack, redelivery, expiry) and the other three kinds.

import (
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

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
		`INSERT INTO agent_deliveries(butler, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		butlerSeq, string(kind), payload, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAgentDeliveries returns an Agent's deliveries, oldest first. limit
// <= 0 defaults to 50. No caller exists yet (Phase 4 adds the long-poll
// consumer); this is here so a test — and Phase 4 itself — can read
// back what Phase 3 wrote without a second migration.
func (db *DB) ListAgentDeliveries(butlerSeq int64, limit int) ([]AgentDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.Query(
		`SELECT id, butler, kind, cursor, payload, created_at, expires_at, acked_at
		   FROM agent_deliveries WHERE butler = ? ORDER BY id ASC LIMIT ?`,
		butlerSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentDelivery
	for rows.Next() {
		var d AgentDelivery
		var kind, created, expires, acked string
		if err := rows.Scan(&d.ID, &d.Butler, &kind, &d.Cursor, &d.Payload, &created, &expires, &acked); err != nil {
			return nil, err
		}
		d.Kind = agentapi.DeliveryKind(kind)
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if expires != "" {
			d.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
		}
		if acked != "" {
			d.AckedAt, _ = time.Parse(time.RFC3339Nano, acked)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
