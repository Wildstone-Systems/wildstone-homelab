package runtimedb

import (
	"fmt"
	"time"
)

// Node is the durable record that a node exists (#30). It deliberately
// carries no "connected" field: liveness belongs to the gateway's session
// registry, and a persisted connected flag would be wrong from the moment
// the process restarts. The API joins this record with live sessions at
// read time.
type Node struct {
	NodeID        string    `json:"id"`
	FirstJoinedAt time.Time `json:"firstJoinedAt"`
	LastSeenAt    time.Time `json:"lastSeenAt"`
	AgentVersion  string    `json:"agentVersion,omitempty"`
	ProtocolMinor uint32    `json:"protocolMinor,omitempty"`
}

// RecordNodeJoin records a completed join. first_joined_at is preserved on
// re-join: a node that is rebuilt and re-paired keeps the date it first
// entered the fleet, which is what a reader is asking for.
func (db *DB) RecordNodeJoin(nodeID string) error {
	if nodeID == "" {
		return fmt.Errorf("runtimedb: node id required")
	}
	_, err := db.sql.Exec(
		`INSERT INTO nodes (node_id, first_joined_at, last_seen_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT (node_id) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		nodeID, now(), now())
	if err != nil {
		return fmt.Errorf("runtimedb: recording node join %s: %w", nodeID, err)
	}
	return nil
}

// RecordNodeSeen updates last-seen and the agent's self-reported build,
// called when a session opens and again when it closes. A node whose
// session opens without a prior join row still gets one: the gateway only
// admits a node whose certificate it issued, so the row is a record of
// something that already happened, not an authorization decision.
func (db *DB) RecordNodeSeen(nodeID, agentVersion string, protocolMinor uint32) error {
	if nodeID == "" {
		return fmt.Errorf("runtimedb: node id required")
	}
	_, err := db.sql.Exec(
		`INSERT INTO nodes (node_id, first_joined_at, last_seen_at, agent_version, protocol_minor)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (node_id) DO UPDATE SET
		   last_seen_at = excluded.last_seen_at,
		   agent_version = excluded.agent_version,
		   protocol_minor = excluded.protocol_minor`,
		nodeID, now(), now(), agentVersion, protocolMinor)
	if err != nil {
		return fmt.Errorf("runtimedb: recording node seen %s: %w", nodeID, err)
	}
	return nil
}

// ListNodes returns every node that has ever joined, id-ordered.
func (db *DB) ListNodes() ([]Node, error) {
	rows, err := db.sql.Query(
		`SELECT node_id, first_joined_at, last_seen_at, agent_version, protocol_minor
		   FROM nodes ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing nodes: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var joined, seen string
		if err := rows.Scan(&n.NodeID, &joined, &seen, &n.AgentVersion, &n.ProtocolMinor); err != nil {
			return nil, err
		}
		if n.FirstJoinedAt, err = time.Parse(time.RFC3339Nano, joined); err != nil {
			return nil, fmt.Errorf("runtimedb: bad node first_joined_at %q: %w", joined, err)
		}
		if n.LastSeenAt, err = time.Parse(time.RFC3339Nano, seen); err != nil {
			return nil, fmt.Errorf("runtimedb: bad node last_seen_at %q: %w", seen, err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
