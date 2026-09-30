package runtimedb

// Named connections and the integration-work contract (ADR-W030 A5/A5.1,
// P10).
//
// Keystone stays application-neutral. Nothing in this file knows what
// Home Assistant is; it knows that a connection has capabilities, that
// work is queued against one, that an adapter claims work for the
// connections its credential is bound to, and that a result is one of
// four kinds.
//
// Those four kinds carry most of the value. A valid EMPTY answer is not
// the same as UNAVAILABLE or STALE, and a caller that cannot tell them
// apart will report an outage as an empty shopping list — which is the
// specific failure A5 names when it says responses must distinguish
// valid results, including empty lists, from unavailable or stale
// information.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/integration"
)

// Connection ownership and the defaults it decides (A5).
const (
	OwnershipPersonal  = "personal"
	OwnershipHousehold = "household"
)

// Connection states.
const (
	ConnectionConfigured  = "configured"
	ConnectionReady       = "ready"
	ConnectionDegraded    = "degraded"
	ConnectionUnavailable = "unavailable"
)

// Capability kinds an integration may declare.
const (
	CapabilityEvent  = "event"
	CapabilityQuery  = "query"
	CapabilityAction = "action"
)

// Work kinds and states.
const (
	WorkQuery     = "query"
	WorkAction    = "action"
	WorkSubscribe = "subscribe"

	WorkQueued    = "queued"
	WorkClaimed   = "claimed"
	WorkDone      = "done"
	WorkCancelled = "cancelled"
	WorkExpired   = "expired"
)

// Result types. The set is closed and the distinctions are the contract.
const (
	// ResultOK is a real answer.
	ResultOK = "ok"
	// ResultEmpty is a real answer that happens to be nothing — the list
	// exists and has no items on it.
	ResultEmpty = "empty"
	// ResultUnavailable means the app could not be reached or refused.
	// Dependent work waits; it does not conclude.
	ResultUnavailable = "unavailable"
	// ResultStale is what the source said earlier, offered because
	// current state could not be read. It carries an observation time
	// and must be reported as of then.
	ResultStale = "stale"
)

// DefaultQueryDeadline is the interactive end-to-end budget, queue delay
// included (A5.1). It is absolute rather than a duration the adapter
// starts counting on receipt: a query that spent four seconds waiting to
// be claimed has one left, not five.
const DefaultQueryDeadline = 5 * time.Second

// ErrNotClaimable is returned when an adapter asks for work it is not
// bound to, or work whose epoch has moved on.
var ErrNotClaimable = errors.New("runtimedb: this adapter cannot claim that work")

// IntegrationConnection is one named connection to one application.
type IntegrationConnection struct {
	Seq             int64              `json:"seq"`
	Name            string             `json:"name"`
	Adapter         string             `json:"adapter"`
	Endpoint        string             `json:"endpoint,omitempty"`
	Ownership       string             `json:"ownership"`
	Owner           string             `json:"owner"`
	Audience        ButlerTaskAudience `json:"audience"`
	ActionsEnabled  bool               `json:"actionsEnabled"`
	IndexingEnabled bool               `json:"indexingEnabled"`
	CloudAllowed    bool               `json:"cloudAllowed"`
	State           string             `json:"state"`
	LeaseEpoch      int64              `json:"leaseEpoch"`
	CreatedAt       time.Time          `json:"createdAt"`
	CreatedBy       string             `json:"createdBy"`
	DegradedReason  string             `json:"degradedReason,omitempty"`
}

// CreateConnection registers a named connection.
//
// The defaults follow ownership and are NOT parameters. A personal
// connection starts private, read-only, live-query-only and
// local-processing-only; a household one starts shared and read-only
// (A5). Widening any of those is a later, explicit act, so that
// "connect my calendar" cannot also mean "index my calendar and send it
// to a cloud model".
func (db *DB) CreateConnection(c IntegrationConnection) (int64, error) {
	if c.Name == "" || c.Adapter == "" || c.Owner == "" || c.CreatedBy == "" {
		return 0, fmt.Errorf("runtimedb: a connection needs a name, an adapter, an owner, and who created it")
	}
	switch c.Ownership {
	case OwnershipPersonal:
		c.Audience = ButlerAudiencePrivate
	case OwnershipHousehold:
		c.Audience = ButlerAudienceShared
	default:
		return 0, fmt.Errorf("runtimedb: connection ownership must be personal|household, got %q", c.Ownership)
	}
	res, err := db.sql.Exec(
		`INSERT INTO integration_connections (name, adapter, endpoint, ownership, owner, audience,
		     actions_enabled, indexing_enabled, cloud_allowed, state, lease_epoch,
		     created_at, created_by, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, ?, 1, ?, ?, ?)`,
		c.Name, c.Adapter, c.Endpoint, c.Ownership, c.Owner, string(c.Audience),
		ConnectionConfigured, now(), c.CreatedBy, now())
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("runtimedb: a connection named %q already exists", c.Name)
		}
		return 0, fmt.Errorf("runtimedb: creating connection: %w", err)
	}
	return res.LastInsertId()
}

// GetConnection returns one connection by name.
func (db *DB) GetConnection(name string) (IntegrationConnection, error) {
	return scanConnection(db.sql.QueryRow(connectionColumns+` WHERE name = ?`, name))
}

const connectionColumns = `SELECT seq, name, adapter, endpoint, ownership, owner, audience,
	 actions_enabled, indexing_enabled, cloud_allowed, state, lease_epoch, created_at,
	 created_by, degraded_reason FROM integration_connections`

func scanConnection(row rowScanner) (IntegrationConnection, error) {
	var c IntegrationConnection
	var audience, created string
	var actions, indexing, cloud int
	err := row.Scan(&c.Seq, &c.Name, &c.Adapter, &c.Endpoint, &c.Ownership, &c.Owner, &audience,
		&actions, &indexing, &cloud, &c.State, &c.LeaseEpoch, &created, &c.CreatedBy, &c.DegradedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return IntegrationConnection{}, ErrNotFound
	}
	if err != nil {
		return IntegrationConnection{}, fmt.Errorf("runtimedb: scanning connection: %w", err)
	}
	c.Audience = ButlerTaskAudience(audience)
	c.ActionsEnabled, c.IndexingEnabled, c.CloudAllowed = actions != 0, indexing != 0, cloud != 0
	if c.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return IntegrationConnection{}, err
	}
	return c, nil
}

// ListConnections returns every connection, by name.
func (db *DB) ListConnections() ([]IntegrationConnection, error) {
	rows, err := db.sql.Query(connectionColumns + ` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing connections: %w", err)
	}
	defer rows.Close()
	var out []IntegrationConnection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetConnectionState records readiness, with a reason when it is not
// good news. A connection that is degraded for a reason nobody can read
// is a connection nobody fixes.
func (db *DB) SetConnectionState(name, state, reason string) error {
	switch state {
	case ConnectionConfigured, ConnectionReady, ConnectionDegraded, ConnectionUnavailable:
	default:
		return fmt.Errorf("runtimedb: connection state must be configured|ready|degraded|unavailable, got %q", state)
	}
	if (state == ConnectionDegraded || state == ConnectionUnavailable) && reason == "" {
		return fmt.Errorf("runtimedb: a connection reported %s must say why", state)
	}
	res, err := db.sql.Exec(
		`UPDATE integration_connections SET state = ?, degraded_reason = ?, updated_at = ? WHERE name = ?`,
		state, reason, now(), name)
	if err != nil {
		return fmt.Errorf("runtimedb: setting connection state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// WidenConnection is the explicit act that turns on actions, indexing or
// cloud routing. Each is separate, and each starts off (A5).
func (db *DB) WidenConnection(name string, actions, indexing, cloud bool) error {
	res, err := db.sql.Exec(
		`UPDATE integration_connections SET actions_enabled = ?, indexing_enabled = ?,
		     cloud_allowed = ?, updated_at = ? WHERE name = ?`,
		boolInt(actions), boolInt(indexing), boolInt(cloud), now(), name)
	if err != nil {
		return fmt.Errorf("runtimedb: widening connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BumpConnectionEpoch fences the current adapter. Work already claimed
// under the old epoch is refused when its result arrives.
func (db *DB) BumpConnectionEpoch(name string) (int64, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: fencing connection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var epoch int64
	err = tx.QueryRow(`SELECT lease_epoch FROM integration_connections WHERE name = ?`, name).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("runtimedb: fencing connection: %w", err)
	}
	epoch++
	if _, err := tx.Exec(
		`UPDATE integration_connections SET lease_epoch = ?, updated_at = ? WHERE name = ?`,
		epoch, now(), name); err != nil {
		return 0, fmt.Errorf("runtimedb: fencing connection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: fencing connection: %w", err)
	}
	return epoch, nil
}

// BindAdapterPrincipal binds an adapter credential to a connection. This
// is what makes "an adapter cannot claim another connection's work"
// structural: the claim query joins through this table.
func (db *DB) BindAdapterPrincipal(principal, connection, boundBy string) error {
	return db.BindAdapterPrincipalOnNode(principal, connection, "", boundBy)
}

// BindAdapterPrincipalOnNode binds an adapter credential to a connection
// AND to the placement node its container runs on (migration 0097). Such
// a principal claims only while that node holds the connection's lease,
// so a prepared fallback stays ready but inactive until failover moves
// the lease to it (ADR-W030 A5). An empty node is the unplaced binding
// BindAdapterPrincipal makes.
func (db *DB) BindAdapterPrincipalOnNode(principal, connection, node, boundBy string) error {
	if principal == "" || boundBy == "" {
		return fmt.Errorf("runtimedb: binding an adapter needs the principal and who bound it")
	}
	c, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	var existing string
	err = db.sql.QueryRow(`SELECT node FROM integration_principals WHERE principal = ? AND connection = ?`,
		principal, c.Seq).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("runtimedb: binding adapter principal: %w", err)
	case existing != node:
		// Moving a credential between nodes is a removal and a new
		// placement, each proven; it is never a quiet rebind.
		return fmt.Errorf("runtimedb: adapter %q is already bound to %q on node %q", principal, connection, existing)
	default:
		return nil
	}
	_, err = db.sql.Exec(
		`INSERT INTO integration_principals (principal, connection, bound_at, bound_by, node)
		 VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, principal, c.Seq, now(), boundBy, node)
	if err != nil {
		return fmt.Errorf("runtimedb: binding adapter principal: %w", err)
	}
	return nil
}

// UnbindAdapterPrincipal withdraws a binding and fences the connection,
// so work in flight under that adapter is refused when it answers.
func (db *DB) UnbindAdapterPrincipal(principal, connection string) error {
	c, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	if _, err := db.sql.Exec(
		`DELETE FROM integration_principals WHERE principal = ? AND connection = ?`,
		principal, c.Seq); err != nil {
		return fmt.Errorf("runtimedb: unbinding adapter principal: %w", err)
	}
	_, err = db.BumpConnectionEpoch(connection)
	return err
}

// IntegrationCapability is one verified thing a connection can do.
type IntegrationCapability struct {
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Version    string    `json:"version,omitempty"`
	Available  bool      `json:"available"`
	VerifiedAt time.Time `json:"verifiedAt"`
	Detail     string    `json:"detail,omitempty"`
}

// RecordCapability stores what a connection was verified to support.
//
// Recording a capability says "this works". It never says "this is
// allowed": discovery does not authorize newly available permissions
// (A5), and an action stays disabled until the connection's actions
// switch is turned on and a grant covers it.
func (db *DB) RecordCapability(connection string, c IntegrationCapability) error {
	switch c.Kind {
	case CapabilityEvent, CapabilityQuery, CapabilityAction:
	default:
		return fmt.Errorf("runtimedb: capability kind must be event|query|action, got %q", c.Kind)
	}
	conn, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(
		`INSERT INTO integration_capabilities (connection, kind, name, version, available, verified_at, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(connection, kind, name) DO UPDATE SET
		   version = excluded.version, available = excluded.available,
		   verified_at = excluded.verified_at, detail = excluded.detail`,
		conn.Seq, c.Kind, c.Name, c.Version, boolInt(c.Available), now(), c.Detail)
	if err != nil {
		return fmt.Errorf("runtimedb: recording capability: %w", err)
	}
	return nil
}

// ListCapabilities returns what a connection supports.
func (db *DB) ListCapabilities(connection string) ([]IntegrationCapability, error) {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT kind, name, version, available, verified_at, detail
		 FROM integration_capabilities WHERE connection = ? ORDER BY kind, name`, conn.Seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing capabilities: %w", err)
	}
	defer rows.Close()
	var out []IntegrationCapability
	for rows.Next() {
		var c IntegrationCapability
		var available int
		var verified string
		if err := rows.Scan(&c.Kind, &c.Name, &c.Version, &available, &verified, &c.Detail); err != nil {
			return nil, err
		}
		c.Available = available != 0
		if c.VerifiedAt, err = time.Parse(time.RFC3339Nano, verified); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// IntegrationWork is one queued request.
type IntegrationWork struct {
	Seq             int64     `json:"seq"`
	Connection      string    `json:"connection"`
	Kind            string    `json:"kind"`
	Capability      string    `json:"capability"`
	CorrelationID   string    `json:"correlationId,omitempty"`
	RequestJSON     string    `json:"requestJson,omitempty"`
	EnvelopeVersion int       `json:"envelopeVersion"`
	State           string    `json:"state"`
	Epoch           int64     `json:"epoch"`
	CreatedAt       time.Time `json:"createdAt"`
	DeadlineAt      time.Time `json:"deadlineAt"`
	ClaimedBy       string    `json:"claimedBy,omitempty"`
	ResultType      string    `json:"resultType,omitempty"`
	ResultJSON      string    `json:"resultJson,omitempty"`
	ObservedAt      time.Time `json:"observedAt,omitzero"`
	Error           string    `json:"error,omitempty"`
}

// EnqueueIntegrationWork queues one request against a connection.
//
// An action against a connection whose actions are not enabled is
// refused here rather than dispatched and rejected downstream: the
// adapter should never see work the household has not turned on.
func (db *DB) EnqueueIntegrationWork(connection string, w IntegrationWork, deadline time.Duration) (int64, error) {
	switch w.Kind {
	case WorkQuery, WorkAction, WorkSubscribe:
	default:
		return 0, fmt.Errorf("runtimedb: work kind must be query|action|subscribe, got %q", w.Kind)
	}
	if w.Capability == "" {
		return 0, fmt.Errorf("runtimedb: queued work must name a capability")
	}
	// Inside a declared integration domain the vocabulary is closed
	// (keystone/integration), and it is closed at DISPATCH as well as at
	// declaration: a query must be a declared query and an action a
	// declared action. A read-only domain such as mail declares no action
	// at all, so no mutating mail work can be queued, whatever a caller
	// or a connection's switches say.
	if domain, known := integration.DomainOf(w.Capability); known {
		switch {
		case w.Kind == WorkQuery && !integration.IsQuery(w.Capability):
			return 0, fmt.Errorf("runtimedb: %q is not a %s query, so it was not queued", w.Capability, domain.Name)
		case w.Kind == WorkAction && !integration.IsAction(w.Capability):
			return 0, fmt.Errorf("runtimedb: %q is not a %s action, so it was not queued", w.Capability, domain.Name)
		}
	}
	conn, err := db.GetConnection(connection)
	if err != nil {
		return 0, err
	}
	if w.Kind == WorkAction && !conn.ActionsEnabled {
		return 0, fmt.Errorf("runtimedb: connection %q has actions disabled, so %q was not queued (ADR-W030 A5)",
			connection, w.Capability)
	}
	request := w.RequestJSON
	if request == "" {
		request = "{}"
	}
	if !json.Valid([]byte(request)) {
		return 0, fmt.Errorf("runtimedb: integration request is not valid JSON")
	}
	if deadline <= 0 {
		deadline = DefaultQueryDeadline
	}
	res, err := db.sql.Exec(
		`INSERT INTO integration_work (connection, kind, capability, correlation_id, request_json,
		     envelope_version, state, epoch, created_at, deadline_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conn.Seq, w.Kind, w.Capability, w.CorrelationID, request, TaskEnvelopeVersion,
		WorkQueued, conn.LeaseEpoch, now(),
		time.Now().UTC().Add(deadline).Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("runtimedb: queuing integration work: %w", err)
	}
	return res.LastInsertId()
}

// ClaimIntegrationWork returns the next item for the connections this
// adapter principal is bound to, or ok=false when there is nothing.
//
// The adapter names no connection. It gets what its credential is bound
// to and nothing else, so a compromised adapter cannot reach another
// connection's work by asking for it.
func (db *DB) ClaimIntegrationWork(principal string) (IntegrationWork, bool, error) {
	// An adapter claiming work is dispatch to an application, so it
	// waits out quarantine like everything else. An empty answer would
	// be wrong here: the adapter should learn that the control plane is
	// holding, not conclude the queue is quiet.
	if err := db.CheckHouseholdExecution(); err != nil {
		return IntegrationWork{}, false, err
	}
	if principal == "" {
		return IntegrationWork{}, false, ErrNotClaimable
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return IntegrationWork{}, false, fmt.Errorf("runtimedb: claiming integration work: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stamp := now()
	// Expire anything past its deadline before claiming, so an adapter
	// that has been away does not wake up and answer a question nobody
	// is still waiting for.
	if _, err := tx.Exec(
		`UPDATE integration_work SET state = ?, finished_at = ?, result_type = ?, error = ?
		 WHERE state = ? AND deadline_at <= ?`,
		WorkExpired, stamp, ResultUnavailable, "deadline passed before it was claimed",
		WorkQueued, stamp); err != nil {
		return IntegrationWork{}, false, fmt.Errorf("runtimedb: expiring integration work: %w", err)
	}

	var w IntegrationWork
	var created, deadline string
	err = tx.QueryRow(
		`SELECT w.seq, c.name, w.kind, w.capability, w.correlation_id, w.request_json,
		        w.envelope_version, w.epoch, w.created_at, w.deadline_at
		 FROM integration_work w
		 JOIN integration_connections c ON c.seq = w.connection
		 JOIN integration_principals p ON p.connection = w.connection
		 WHERE p.principal = ? AND w.state = ? AND w.epoch = c.lease_epoch
		   AND (p.node = '' OR EXISTS (
		         SELECT 1 FROM integration_placements pl
		          WHERE pl.connection = w.connection AND pl.node = p.node
		            AND pl.holder = 1 AND pl.removing_at = ''))
		 ORDER BY w.seq LIMIT 1`, principal, WorkQueued).
		Scan(&w.Seq, &w.Connection, &w.Kind, &w.Capability, &w.CorrelationID, &w.RequestJSON,
			&w.EnvelopeVersion, &w.Epoch, &created, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to do is the normal case for a long poll, not a
		// failure. Commit so the expiry sweep above is not rolled back.
		if err := tx.Commit(); err != nil {
			return IntegrationWork{}, false, err
		}
		return IntegrationWork{}, false, nil
	}
	if err != nil {
		return IntegrationWork{}, false, fmt.Errorf("runtimedb: claiming integration work: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE integration_work SET state = ?, claimed_at = ?, claimed_by = ? WHERE seq = ?`,
		WorkClaimed, stamp, principal, w.Seq); err != nil {
		return IntegrationWork{}, false, fmt.Errorf("runtimedb: claiming integration work: %w", err)
	}
	if w.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return IntegrationWork{}, false, err
	}
	if w.DeadlineAt, err = time.Parse(time.RFC3339Nano, deadline); err != nil {
		return IntegrationWork{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return IntegrationWork{}, false, fmt.Errorf("runtimedb: claiming integration work: %w", err)
	}
	w.State, w.ClaimedBy = WorkClaimed, principal
	return w, true, nil
}

// ReportIntegrationResult records an adapter's answer.
//
// It refuses three things: work this adapter did not claim, work whose
// connection epoch has moved on, and a second result for work that is
// already finished. The third is what makes duplicate delivery safe —
// an adapter that dies between answering and acknowledging can retry,
// and the retry is recognised rather than applied twice.
func (db *DB) ReportIntegrationResult(principal string, seq int64, resultType, resultJSON string, observedAt time.Time, errText string) error {
	switch resultType {
	case ResultOK, ResultEmpty, ResultUnavailable, ResultStale:
	default:
		return fmt.Errorf("runtimedb: result type must be ok|empty|unavailable|stale, got %q", resultType)
	}
	if resultType == ResultStale && observedAt.IsZero() {
		return fmt.Errorf("runtimedb: a stale result must say when it was observed (ADR-W030 A5)")
	}
	if resultJSON == "" {
		resultJSON = "{}"
	}
	if !json.Valid([]byte(resultJSON)) {
		return fmt.Errorf("runtimedb: integration result is not valid JSON")
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: reporting integration result: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state, claimedBy string
	var workEpoch, leaseEpoch int64
	err = tx.QueryRow(
		`SELECT w.state, w.claimed_by, w.epoch, c.lease_epoch
		 FROM integration_work w JOIN integration_connections c ON c.seq = w.connection
		 WHERE w.seq = ?`, seq).Scan(&state, &claimedBy, &workEpoch, &leaseEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: reporting integration result: %w", err)
	}
	if claimedBy != principal {
		// Not "wrong adapter": to a caller probing the surface, work it
		// did not claim and work that does not exist are the same thing.
		return ErrNotClaimable
	}
	if workEpoch != leaseEpoch {
		return fmt.Errorf("%w: this connection has moved to epoch %d", ErrAttemptFenced, leaseEpoch)
	}
	if state != WorkClaimed {
		return fmt.Errorf("runtimedb: integration work %d is %s and already has an answer", seq, state)
	}
	if _, err := tx.Exec(
		`UPDATE integration_work SET state = ?, result_type = ?, result_json = ?, result_at = ?,
		     observed_at = ?, error = ?, finished_at = ? WHERE seq = ?`,
		WorkDone, resultType, resultJSON, now(), timeText(observedAt), errText, now(), seq); err != nil {
		return fmt.Errorf("runtimedb: reporting integration result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: reporting integration result: %w", err)
	}
	return nil
}

// CancelIntegrationWork withdraws queued or claimed work. An adapter
// that later answers cancelled work finds it finished and is refused.
func (db *DB) CancelIntegrationWork(seq int64, reason string) error {
	res, err := db.sql.Exec(
		`UPDATE integration_work SET state = ?, finished_at = ?, error = ?
		 WHERE seq = ? AND state IN (?, ?)`,
		WorkCancelled, now(), reason, seq, WorkQueued, WorkClaimed)
	if err != nil {
		return fmt.Errorf("runtimedb: cancelling integration work: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetIntegrationWork returns one item, including its answer.
func (db *DB) GetIntegrationWork(seq int64) (IntegrationWork, error) {
	var w IntegrationWork
	var created, deadline, observed string
	err := db.sql.QueryRow(
		`SELECT w.seq, c.name, w.kind, w.capability, w.correlation_id, w.request_json,
		        w.envelope_version, w.state, w.epoch, w.created_at, w.deadline_at, w.claimed_by,
		        w.result_type, w.result_json, w.observed_at, w.error
		 FROM integration_work w JOIN integration_connections c ON c.seq = w.connection
		 WHERE w.seq = ?`, seq).
		Scan(&w.Seq, &w.Connection, &w.Kind, &w.Capability, &w.CorrelationID, &w.RequestJSON,
			&w.EnvelopeVersion, &w.State, &w.Epoch, &created, &deadline, &w.ClaimedBy,
			&w.ResultType, &w.ResultJSON, &observed, &w.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return IntegrationWork{}, ErrNotFound
	}
	if err != nil {
		return IntegrationWork{}, fmt.Errorf("runtimedb: reading integration work: %w", err)
	}
	if w.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return IntegrationWork{}, err
	}
	if w.DeadlineAt, err = time.Parse(time.RFC3339Nano, deadline); err != nil {
		return IntegrationWork{}, err
	}
	if observed != "" {
		if w.ObservedAt, err = time.Parse(time.RFC3339Nano, observed); err != nil {
			return IntegrationWork{}, err
		}
	}
	return w, nil
}

// ExpireIntegrationWork marks everything past its deadline unavailable.
// It runs on claim as well; this is the sweep for work nobody polled for.
func (db *DB) ExpireIntegrationWork() (int, error) {
	stamp := now()
	res, err := db.sql.Exec(
		`UPDATE integration_work SET state = ?, finished_at = ?, result_type = ?, error = ?
		 WHERE state IN (?, ?) AND deadline_at <= ?`,
		WorkExpired, stamp, ResultUnavailable, "deadline passed", WorkQueued, WorkClaimed, stamp)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: expiring integration work: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
