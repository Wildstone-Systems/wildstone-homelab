package runtimedb

// agent_subscriptions (migration 0086, ADR-W039 §3.2 Events &
// subscriptions, §3.6; Phase 4, #341): an Agent's standing, Owner-
// approved request to be told about a household event again and again.
// 0099 (epic #326 S03) adds a scheduled trigger beside the event one;
// its run, schedule and checkpoint state lives in
// agentsubscriptionschedule.go.
//
// Nothing here enables a subscription. ProposeAgentSubscription writes
// it disabled and unapproved; only the approve functions — called from
// the SAME Plan-approval path every other proposal goes through — ever
// set enabled, approved_version, approved_by and approved_at. A caller
// that wants a subscription live has no other door.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Subscription trigger kinds (0099, epic #326 S03). An event
// subscription is matched against the household stream; a scheduled one
// is due every Cadence and never sees the stream at all.
const (
	SubscriptionTriggerEvent    = "event"
	SubscriptionTriggerSchedule = "schedule"
)

// Scheduled-subscription cadence bounds. Coarse on purpose: this is
// mailbox polling, not a timer service, and a floor keeps one
// subscription from turning an external provider into a hot loop.
const (
	DefaultSubscriptionCadence = 15 * time.Minute
	MinSubscriptionCadence     = 5 * time.Minute
	MaxSubscriptionCadence     = 24 * time.Hour
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
	// TriggerKind is SubscriptionTriggerEvent or
	// SubscriptionTriggerSchedule (0099).
	TriggerKind string
	// Cadence is how often a scheduled subscription is due; zero for an
	// event subscription.
	Cadence time.Duration
	// SourceConnection names the integration connection a scheduled run
	// reads, or "" for none.
	SourceConnection string
	// PersonID is the canonical Person a Personal-source subscription
	// runs for and is authored by, or "" when it has no Personal source.
	PersonID string
	// Version counts edits. Dispatch requires ApprovedVersion == Version:
	// an edited subscription is inert until that exact version is
	// approved.
	Version int
}

// Dispatchable reports whether an Owner has approved exactly the
// version this row now holds and it is switched on.
func (s AgentSubscription) Dispatchable() bool {
	return s.Enabled && s.ApprovedVersion > 0 && s.ApprovedVersion == s.Version
}

const agentSubscriptionColumns = `
	SELECT id, butler, match_kind, match_json, audience, capability_set, budget_json,
	       approved_version, approved_by, approved_at, enabled, cursor, created_at, updated_at,
	       trigger_kind, cadence_seconds, source_connection, person_id, version
	  FROM agent_subscriptions`

func scanAgentSubscription(s interface{ Scan(...any) error }) (AgentSubscription, error) {
	var sub AgentSubscription
	var capsJSON, approvedAt, created, updated string
	var enabled int
	var cadence int64
	var person sql.NullString
	if err := s.Scan(&sub.ID, &sub.Butler, &sub.MatchKind, &sub.MatchJSON, &sub.Audience, &capsJSON,
		&sub.BudgetJSON, &sub.ApprovedVersion, &sub.ApprovedBy, &approvedAt, &enabled, &sub.Cursor,
		&created, &updated, &sub.TriggerKind, &cadence, &sub.SourceConnection, &person, &sub.Version); err != nil {
		return AgentSubscription{}, err
	}
	sub.Enabled = enabled != 0
	sub.Cadence = time.Duration(cadence) * time.Second
	sub.PersonID = person.String
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

// AgentSubscriptionSpec is everything a proposed subscription carries.
// TriggerKind "" means SubscriptionTriggerEvent, the only kind 0086
// knew; Cadence zero on a scheduled subscription means
// DefaultSubscriptionCadence.
type AgentSubscriptionSpec struct {
	Butler           int64
	TriggerKind      string
	MatchKind        string
	MatchJSON        string
	Audience         string
	Capabilities     []string
	BudgetJSON       string
	Cadence          time.Duration
	SourceConnection string
}

// NormalizeSubscriptionTrigger validates a trigger kind, cadence and
// source together and fills the defaults. It is exported so the
// proposal path refuses a bad schedule before an Owner is ever shown it,
// with the same bounds the store enforces.
func NormalizeSubscriptionTrigger(kind string, cadence time.Duration, source string) (string, time.Duration, error) {
	switch kind {
	case "", SubscriptionTriggerEvent:
		if cadence != 0 || source != "" {
			return "", 0, fmt.Errorf("runtimedb: an event subscription takes no cadence or source; use a scheduled trigger")
		}
		return SubscriptionTriggerEvent, 0, nil
	case SubscriptionTriggerSchedule:
		if cadence == 0 {
			cadence = DefaultSubscriptionCadence
		}
		if cadence < MinSubscriptionCadence || cadence > MaxSubscriptionCadence {
			return "", 0, fmt.Errorf("runtimedb: a scheduled subscription's cadence must be between %s and %s, got %s",
				MinSubscriptionCadence, MaxSubscriptionCadence, cadence)
		}
		if cadence%time.Second != 0 {
			return "", 0, fmt.Errorf("runtimedb: a cadence is whole seconds, got %s", cadence)
		}
		return SubscriptionTriggerSchedule, cadence, nil
	default:
		return "", 0, fmt.Errorf("runtimedb: subscription trigger must be event|schedule, got %q", kind)
	}
}

// sourcePerson resolves who a source connection binds a subscription
// to: the owning Person of a Personal connection; nobody (NULL) for a
// household connection or for no source at all.
func sourcePerson(q personRower, source string) (any, error) {
	if source == "" {
		return nil, nil
	}
	var ownership, owner string
	err := q.QueryRow(`SELECT ownership, owner FROM integration_connections WHERE name = ?`, source).
		Scan(&ownership, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("runtimedb: subscription source %q is not a connection", source)
	}
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading subscription source %q: %w", source, err)
	}
	if ownership != OwnershipPersonal {
		return nil, nil
	}
	id, err := resolvePersonID(q, owner)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: subscription source %q belongs to nobody on record: %w", source, err)
	}
	return id, nil
}

// InsertAgentSubscription proposes an event subscription, disabled and
// unapproved — the 0086 shape, kept for its callers.
func (db *DB) InsertAgentSubscription(butlerSeq int64, matchKind, matchJSON, audience string, capabilities []string, budgetJSON string) (int64, error) {
	return db.ProposeAgentSubscription(AgentSubscriptionSpec{
		Butler: butlerSeq, MatchKind: matchKind, MatchJSON: matchJSON, Audience: audience,
		Capabilities: capabilities, BudgetJSON: budgetJSON,
	})
}

// ProposeAgentSubscription records a subscription, disabled and
// unapproved. It refuses an empty match kind or capability set: there is
// nothing to watch for and nothing it could ever be allowed to use. A
// Personal source binds the row to that connection's owning Person here,
// from the connection itself — never from whoever proposed it.
func (db *DB) ProposeAgentSubscription(spec AgentSubscriptionSpec) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if spec.Butler == 0 {
		return 0, fmt.Errorf("runtimedb: a subscription needs the agent it is for")
	}
	if spec.MatchKind == "" {
		return 0, fmt.Errorf("runtimedb: a subscription needs a match kind")
	}
	if len(spec.Capabilities) == 0 {
		return 0, fmt.Errorf("runtimedb: a subscription needs at least one capability")
	}
	trigger, cadence, err := NormalizeSubscriptionTrigger(spec.TriggerKind, spec.Cadence, spec.SourceConnection)
	if err != nil {
		return 0, err
	}
	if spec.MatchJSON == "" {
		spec.MatchJSON = "{}"
	}
	if spec.BudgetJSON == "" {
		spec.BudgetJSON = "{}"
	}
	capsJSON, err := json.Marshal(spec.Capabilities)
	if err != nil {
		return 0, err
	}
	person, err := sourcePerson(db.sql, spec.SourceConnection)
	if err != nil {
		return 0, err
	}
	stamp := now()
	res, err := db.sql.Exec(
		`INSERT INTO agent_subscriptions
		   (butler, match_kind, match_json, audience, capability_set, budget_json, created_at, updated_at,
		    trigger_kind, cadence_seconds, source_connection, person_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.Butler, spec.MatchKind, spec.MatchJSON, spec.Audience, string(capsJSON), spec.BudgetJSON, stamp, stamp,
		trigger, int64(cadence/time.Second), spec.SourceConnection, person)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: proposing a subscription: %w", err)
	}
	return res.LastInsertId()
}

// ApproveAgentSubscription is the ONLY way a new subscription is ever
// enabled: called from the Plan-approval path once an Owner has approved
// it (ADR-W039 §3.6: "takes effect only when an Owner approves it"). It
// refuses to approve an already-approved row a second time. It approves
// version 1, the version every proposal starts at; an edited row is
// approved by ApproveAgentSubscriptionVersion naming the exact version
// the Owner was shown.
func (db *DB) ApproveAgentSubscription(id int64, approvedBy string) error {
	return db.approveAgentSubscription(id, 1, approvedBy, true)
}

// ApproveAgentSubscriptionVersion approves exactly one version of an
// edited subscription (0099). It refuses when the row has been edited
// again since — approving version 2 must never enable version 3 — and
// when that version is already approved.
func (db *DB) ApproveAgentSubscriptionVersion(id int64, version int, approvedBy string) error {
	return db.approveAgentSubscription(id, version, approvedBy, false)
}

func (db *DB) approveAgentSubscription(id int64, version int, approvedBy string, first bool) error {
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	if approvedBy == "" {
		return fmt.Errorf("runtimedb: approving a subscription needs who approved it")
	}
	if version < 1 {
		return fmt.Errorf("runtimedb: approving a subscription needs the version shown")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cond := `approved_version < version`
	if first {
		cond = `approved_version = 0`
	}
	res, err := tx.Exec(
		`UPDATE agent_subscriptions
		    SET enabled = 1, approved_version = version, approved_by = ?, approved_at = ?, updated_at = ?
		  WHERE id = ? AND version = ? AND `+cond,
		approvedBy, now(), now(), id, version)
	if err != nil {
		return fmt.Errorf("runtimedb: approving subscription %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("runtimedb: subscription %d does not exist, is already approved, or is no longer at version %d", id, version)
	}
	if err := ensureSubscriptionSchedule(tx, id); err != nil {
		return err
	}
	return tx.Commit()
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
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`UPDATE agent_subscriptions SET enabled = ?, updated_at = ?
		  WHERE id = ? AND approved_version > 0`, v, now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: setting subscription %d enabled=%v: %w", id, enabled, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("runtimedb: subscription %d is not an approved subscription", id)
	}
	if !enabled {
		// Disabling fences the run in flight too: its result may still
		// arrive, and it must not commit a checkpoint for a subscription
		// somebody has just switched off.
		if err := fenceRunningSubscriptionRuns(tx, id, "subscription disabled by "+by); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DisableAgentSubscriptionsForRuntime withdraws every enabled subscription
// belonging to one Agent when its runtime is explicitly revoked. It preserves
// the rows, approved versions, cursors, schedules and history; only dispatch
// authority is removed, and every running subscription checkpoint is fenced in
// the same transaction. This is intentionally not restore-gated: withdrawing
// authority must remain possible while a restored database is quarantined.
func (db *DB) DisableAgentSubscriptionsForRuntime(butler int64, reason string) (int, error) {
	if butler == 0 {
		return 0, fmt.Errorf("runtimedb: disabling runtime subscriptions needs an agent")
	}
	if reason == "" {
		reason = "runtime revoked"
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT id FROM agent_subscriptions WHERE butler = ? AND enabled = 1`, butler)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: listing runtime subscriptions for agent %d: %w", butler, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`UPDATE agent_subscriptions SET enabled = 0, updated_at = ? WHERE butler = ? AND enabled = 1`,
		now(), butler); err != nil {
		return 0, fmt.Errorf("runtimedb: disabling runtime subscriptions for agent %d: %w", butler, err)
	}
	for _, id := range ids {
		if err := fenceRunningSubscriptionRuns(tx, id, reason); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// AgentSubscriptionRevision is an edit to a subscription. A nil field
// keeps its current value. Every edit is material: it bumps Version, so
// the row stops dispatching until an Owner approves the new version, and
// any run in flight is fenced.
type AgentSubscriptionRevision struct {
	MatchJSON        *string
	Audience         *string
	Capabilities     []string
	BudgetJSON       *string
	SourceConnection *string
}

// ReviseAgentSubscription applies an edit and returns the new version.
// The Person binding is recomputed from the (possibly new) source, never
// carried over: pointing a subscription at another Person's mailbox
// binds it to that Person, whose own authorization the dispatch gate
// then requires.
func (db *DB) ReviseAgentSubscription(id int64, rev AgentSubscriptionRevision, by string) (int, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if by == "" {
		return 0, fmt.Errorf("runtimedb: editing a subscription needs who edited it")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	sub, err := scanAgentSubscription(tx.QueryRow(agentSubscriptionColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("runtimedb: reading subscription %d: %w", id, err)
	}
	if rev.MatchJSON != nil {
		sub.MatchJSON = *rev.MatchJSON
		if sub.MatchJSON == "" {
			sub.MatchJSON = "{}"
		}
	}
	if rev.Audience != nil {
		sub.Audience = *rev.Audience
	}
	if rev.Capabilities != nil {
		if len(rev.Capabilities) == 0 {
			return 0, fmt.Errorf("runtimedb: a subscription needs at least one capability")
		}
		sub.CapabilitySet = rev.Capabilities
	}
	if rev.BudgetJSON != nil {
		sub.BudgetJSON = *rev.BudgetJSON
		if sub.BudgetJSON == "" {
			sub.BudgetJSON = "{}"
		}
	}
	if rev.SourceConnection != nil {
		sub.SourceConnection = *rev.SourceConnection
	}
	if _, _, err := NormalizeSubscriptionTrigger(sub.TriggerKind, sub.Cadence, sub.SourceConnection); err != nil {
		return 0, err
	}
	person, err := sourcePerson(tx, sub.SourceConnection)
	if err != nil {
		return 0, err
	}
	capsJSON, err := json.Marshal(sub.CapabilitySet)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`UPDATE agent_subscriptions
		    SET match_json = ?, audience = ?, capability_set = ?, budget_json = ?, source_connection = ?,
		        person_id = ?, version = version + 1, updated_at = ?
		  WHERE id = ?`,
		sub.MatchJSON, sub.Audience, string(capsJSON), sub.BudgetJSON, sub.SourceConnection,
		person, now(), id); err != nil {
		return 0, fmt.Errorf("runtimedb: editing subscription %d: %w", id, err)
	}
	if err := fenceRunningSubscriptionRuns(tx, id, "subscription edited by "+by); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return sub.Version + 1, nil
}

// GetAgentSubscription reads one subscription by id.
func (db *DB) GetAgentSubscription(id int64) (AgentSubscription, error) {
	sub, err := scanAgentSubscription(db.sql.QueryRow(agentSubscriptionColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentSubscription{}, fmt.Errorf("runtimedb: reading subscription %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return AgentSubscription{}, fmt.Errorf("runtimedb: reading subscription %d: %w", id, err)
	}
	return sub, nil
}

// SubscriptionCapabilities is the narrowing check's own read (ADR-W039
// §3.6, agentgateway.Invoke): the LIVE capability set and enabled bit
// for one subscription, re-read at every use rather than trusted from
// whatever a bounded Task cached when it was spawned. An edited row
// awaiting approval of its new version reads as not enabled: the Task
// was spawned under a version nobody approved any more.
func (db *DB) SubscriptionCapabilities(id int64) (capabilities []string, enabled bool, err error) {
	sub, err := db.GetAgentSubscription(id)
	if err != nil {
		return nil, false, err
	}
	return sub.CapabilitySet, sub.Dispatchable(), nil
}

// ListAgentSubscriptions returns one Agent's subscriptions, oldest
// first — the Console's own listing.
func (db *DB) ListAgentSubscriptions(butlerSeq int64) ([]AgentSubscription, error) {
	rows, err := db.sql.Query(agentSubscriptionColumns+` WHERE butler = ? ORDER BY id`, butlerSeq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing subscriptions for butler %d: %w", butlerSeq, err)
	}
	return collectAgentSubscriptions(rows)
}

// ListEnabledAgentSubscriptions returns every enabled EVENT subscription,
// across every Agent, oldest first — the evaluator's own tick (ADR-W039
// §3.6, keystone/automation/subscriptions.go), the same "one indexed
// scan" shape automation.Engine's own Rules() reads. A scheduled
// subscription is never matched against the event stream.
func (db *DB) ListEnabledAgentSubscriptions() ([]AgentSubscription, error) {
	return db.listAgentSubscriptions(` WHERE enabled = 1 AND trigger_kind = 'event' ORDER BY id`)
}

// ListScheduledAgentSubscriptions returns every enabled scheduled
// subscription, oldest first — the scheduled evaluator's own read. An
// enabled row is not necessarily dispatchable (an edit may await
// approval); the evaluator decides that and records why.
func (db *DB) ListScheduledAgentSubscriptions() ([]AgentSubscription, error) {
	return db.listAgentSubscriptions(` WHERE enabled = 1 AND trigger_kind = 'schedule' ORDER BY id`)
}

// ListAllAgentSubscriptions returns every subscription across every
// Agent, oldest first — the public listing, which its caller filters to
// what the reader may see.
func (db *DB) ListAllAgentSubscriptions() ([]AgentSubscription, error) {
	return db.listAgentSubscriptions(` ORDER BY id`)
}

func (db *DB) listAgentSubscriptions(where string) ([]AgentSubscription, error) {
	rows, err := db.sql.Query(agentSubscriptionColumns + where)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing subscriptions: %w", err)
	}
	return collectAgentSubscriptions(rows)
}

func collectAgentSubscriptions(rows *sql.Rows) ([]AgentSubscription, error) {
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
