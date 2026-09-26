package runtimedb

// agent_subscriptions (migration 0086, ADR-W039 §3.2 Events &
// subscriptions, §3.6; Phase 4, #341): an Agent's standing, Owner-
// approved request to be told about a household event again and again.
//
// Nothing here enables a subscription. InsertAgentSubscription writes it
// disabled and unapproved; only ApproveAgentSubscription — called from
// the SAME Plan-approval path every other proposal goes through — ever
// sets enabled, approved_version, approved_by and approved_at. A caller
// that wants a subscription live has no other door.

import (
	"encoding/json"
	"fmt"
	"time"
)

// AgentSubscription is one row, as read back.
type AgentSubscription struct {
	ID              int64
	Butler          int64
	MatchKind       string
	MatchJSON       string
	Audience        string
	CapabilitySet   []string
	BudgetJSON      string
	ApprovedVersion int
	ApprovedBy      string
	ApprovedAt      time.Time
	Enabled         bool
	Cursor          int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const agentSubscriptionColumns = `
	SELECT id, butler, match_kind, match_json, audience, capability_set, budget_json,
	       approved_version, approved_by, approved_at, enabled, cursor, created_at, updated_at
	  FROM agent_subscriptions`

func scanAgentSubscription(s interface{ Scan(...any) error }) (AgentSubscription, error) {
	var sub AgentSubscription
	var capsJSON, approvedAt, created, updated string
	var enabled int
	if err := s.Scan(&sub.ID, &sub.Butler, &sub.MatchKind, &sub.MatchJSON, &sub.Audience, &capsJSON,
		&sub.BudgetJSON, &sub.ApprovedVersion, &sub.ApprovedBy, &approvedAt, &enabled, &sub.Cursor,
		&created, &updated); err != nil {
		return AgentSubscription{}, err
	}
	sub.Enabled = enabled != 0
	if capsJSON != "" {
		if err := json.Unmarshal([]byte(capsJSON), &sub.CapabilitySet); err != nil {
			return AgentSubscription{}, fmt.Errorf("runtimedb: bad subscription capability set %q: %w", capsJSON, err)
		}
	}
	for _, f := range []struct {
		text string
		into *time.Time
	}{{approvedAt, &sub.ApprovedAt}, {created, &sub.CreatedAt}, {updated, &sub.UpdatedAt}} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return AgentSubscription{}, fmt.Errorf("runtimedb: bad subscription timestamp %q: %w", f.text, err)
		}
		*f.into = parsed
	}
	return sub, nil
}

// InsertAgentSubscription proposes a subscription, disabled and
// unapproved. It refuses an empty match kind or capability set: there is
// nothing to watch for and nothing it could ever be allowed to use.
func (db *DB) InsertAgentSubscription(butlerSeq int64, matchKind, matchJSON, audience string, capabilities []string, budgetJSON string) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if butlerSeq == 0 {
		return 0, fmt.Errorf("runtimedb: a subscription needs the agent it is for")
	}
	if matchKind == "" {
		return 0, fmt.Errorf("runtimedb: a subscription needs a match kind")
	}
	if len(capabilities) == 0 {
		return 0, fmt.Errorf("runtimedb: a subscription needs at least one capability")
	}
	if matchJSON == "" {
		matchJSON = "{}"
	}
	if budgetJSON == "" {
		budgetJSON = "{}"
	}
	capsJSON, err := json.Marshal(capabilities)
	if err != nil {
		return 0, err
	}
	stamp := now()
	res, err := db.sql.Exec(
		`INSERT INTO agent_subscriptions
		   (butler, match_kind, match_json, audience, capability_set, budget_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		butlerSeq, matchKind, matchJSON, audience, string(capsJSON), budgetJSON, stamp, stamp)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: proposing a subscription: %w", err)
	}
	return res.LastInsertId()
}

// ApproveAgentSubscription is the ONLY way a subscription is ever
// enabled: called from the Plan-approval path once an Owner has
// approved it (ADR-W039 §3.6: "takes effect only when an Owner approves it"). It refuses to
// approve an already-approved row a second time — a changed subscription
// is a new proposal against a new row, not a re-approval of this one.
func (db *DB) ApproveAgentSubscription(id int64, approvedBy string) error {
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	if approvedBy == "" {
		return fmt.Errorf("runtimedb: approving a subscription needs who approved it")
	}
	res, err := db.sql.Exec(
		`UPDATE agent_subscriptions
		    SET enabled = 1, approved_version = 1, approved_by = ?, approved_at = ?, updated_at = ?
		  WHERE id = ? AND approved_version = 0`,
		approvedBy, now(), now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: approving subscription %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("runtimedb: subscription %d does not exist or is already approved", id)
	}
	return nil
}

// SetAgentSubscriptionEnabled lets an Owner turn an approved subscription
// off (or back on) without deleting it or losing its cursor. It refuses
// a subscription that was never approved: there is nothing to switch.
func (db *DB) SetAgentSubscriptionEnabled(id int64, enabled bool, by string) error {
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	v := 0
	if enabled {
		v = 1
	}
	res, err := db.sql.Exec(
		`UPDATE agent_subscriptions SET enabled = ?, updated_at = ?
		  WHERE id = ? AND approved_version > 0`, v, now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: setting subscription %d enabled=%v: %w", id, enabled, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("runtimedb: subscription %d is not an approved subscription", id)
	}
	return nil
}

// GetAgentSubscription reads one subscription by id.
func (db *DB) GetAgentSubscription(id int64) (AgentSubscription, error) {
	sub, err := scanAgentSubscription(db.sql.QueryRow(agentSubscriptionColumns+` WHERE id = ?`, id))
	if err != nil {
		return AgentSubscription{}, fmt.Errorf("runtimedb: reading subscription %d: %w", id, err)
	}
	return sub, nil
}

// SubscriptionCapabilities is the narrowing check's own read (ADR-W039
// §3.6, agentgateway.Invoke): the LIVE capability set and enabled bit
// for one subscription, re-read at every use rather than trusted from
// whatever a bounded Task cached when it was spawned.
func (db *DB) SubscriptionCapabilities(id int64) (capabilities []string, enabled bool, err error) {
	sub, err := db.GetAgentSubscription(id)
	if err != nil {
		return nil, false, err
	}
	return sub.CapabilitySet, sub.Enabled, nil
}

// ListAgentSubscriptions returns one Agent's subscriptions, oldest
// first — the Console's own listing.
func (db *DB) ListAgentSubscriptions(butlerSeq int64) ([]AgentSubscription, error) {
	rows, err := db.sql.Query(agentSubscriptionColumns+` WHERE butler = ? ORDER BY id`, butlerSeq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing subscriptions for butler %d: %w", butlerSeq, err)
	}
	defer rows.Close()
	var out []AgentSubscription
	for rows.Next() {
		sub, err := scanAgentSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// ListEnabledAgentSubscriptions returns every enabled subscription,
// across every Agent, oldest first — the evaluator's own tick (ADR-W039
// §3.6, keystone/automation/subscriptions.go), the same "one indexed
// scan" shape automation.Engine's own Rules() reads.
func (db *DB) ListEnabledAgentSubscriptions() ([]AgentSubscription, error) {
	rows, err := db.sql.Query(agentSubscriptionColumns + ` WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing enabled subscriptions: %w", err)
	}
	defer rows.Close()
	var out []AgentSubscription
	for rows.Next() {
		sub, err := scanAgentSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// SetAgentSubscriptionCursor advances a subscription's own position in
// the household event stream (ADR-W039 §3.6 "per-subscription cursor").
func (db *DB) SetAgentSubscriptionCursor(id, cursor int64) error {
	res, err := db.sql.Exec(
		`UPDATE agent_subscriptions SET cursor = ?, updated_at = ? WHERE id = ?`, cursor, now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: advancing subscription %d's cursor: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("runtimedb: subscription %d does not exist", id)
	}
	return nil
}
