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

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
)

// DefaultAgentDeliveryTTL is how long an unacknowledged delivery stays
// offered before the long-poll's own consumer marks it expired (ADR-W039
// §3.6): a week is long enough that a runtime down over a weekend still
// finds its deliveries Monday, and short enough that "still pending"
// stops meaning "will never be answered".
const DefaultAgentDeliveryTTL = 7 * 24 * time.Hour

// ErrRuntimeDeliveryDenied means the principal or credential no longer
// carries the live runtime authority required to receive or acknowledge an
// Agent delivery. Keep the reason opaque to callers: whether a principal was
// disabled, superseded, retired or revoked is administrative state, not
// information a stale runtime credential may enumerate.
var ErrRuntimeDeliveryDenied = errors.New("runtimedb: runtime delivery authority denied")

// RuntimeDeliveryCredential is the request-local proof used at the final
// delivery boundary. TokenHash is set by HTTP callers after authenticating the
// exact presented bearer; in-process callers leave it empty and are still
// checked against the live principal, current runtime and Agent lifecycle.
// The bearer itself never crosses into the store.
type RuntimeDeliveryCredential struct {
	Principal string
	TokenHash string
}

// AgentDelivery is one row of agent_deliveries, as read back.
type AgentDelivery struct {
	ID         int64
	Butler     int64
	Kind       agentapi.DeliveryKind
	Cursor     string
	Payload    string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AckedAt    time.Time
	ExpiredAt  time.Time
	Provenance ExternalDeliveryProvenance
}

// ExternalDeliveryProvenance is the immutable tuple required before a row may
// cross the OAuth delivery boundary. Zero is the explicit static/legacy
// profile; it is never inferred later from payload content or Agent state.
type ExternalDeliveryProvenance struct {
	RegistrationID   string
	PersonID         string
	Connection       int64
	RuntimePrincipal string
	CredentialEpoch  int64
	Task             int64
	Audience         ButlerTaskAudience
}

// ExternalDeliveryIdentity is the request-local result of fresh OAuth
// introspection, reduced to fields the store can fence without retaining the
// bearer.
type ExternalDeliveryIdentity struct {
	RegistrationID   string
	PersonID         string
	Butler           int64
	Connection       int64
	RuntimePrincipal string
	CredentialEpoch  int64
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
	SELECT id, butler, kind, cursor, payload, created_at, expires_at, acked_at, expired_at,
	       registration_id, person_id, connection, runtime_principal, credential_epoch, task, audience
	  FROM agent_deliveries`

func scanAgentDelivery(s interface{ Scan(...any) error }) (AgentDelivery, error) {
	var d AgentDelivery
	var kind, created, expires, acked, expired string
	var audience string
	if err := s.Scan(&d.ID, &d.Butler, &kind, &d.Cursor, &d.Payload, &created, &expires, &acked, &expired,
		&d.Provenance.RegistrationID, &d.Provenance.PersonID, &d.Provenance.Connection,
		&d.Provenance.RuntimePrincipal, &d.Provenance.CredentialEpoch, &d.Provenance.Task, &audience); err != nil {
		return AgentDelivery{}, err
	}
	d.Provenance.Audience = ButlerTaskAudience(audience)
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

// RecordAgentDeliveryForTask records a normal static row unless taskID is an
// OAuth-admitted task. In that case the exact immutable admission tuple is
// copied into the row in the same transaction as insertion.
func (db *DB) RecordAgentDeliveryForTask(butlerSeq int64, kind agentapi.DeliveryKind, taskID, payload string) (int64, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return 0, err
	}
	var external int
	err = db.sql.QueryRow(`SELECT 1 FROM external_task_admissions WHERE task=?`, seq).Scan(&external)
	if errors.Is(err, sql.ErrNoRows) {
		return db.RecordAgentDelivery(butlerSeq, kind, payload)
	}
	if err != nil {
		return 0, err
	}
	return db.recordDeliveryWithOrigin(butlerSeq, kind, "", payload, seq)
}

// LinkExternalPlanOrigin remembers only a proven OAuth task origin. Static
// plans remain unlinked and therefore cannot become OAuth-visible later.
func (db *DB) LinkExternalPlanOrigin(planID, taskID string) error {
	plan, err := parsePlanID(planID)
	if err != nil {
		return err
	}
	task, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(`INSERT OR IGNORE INTO external_plan_origins(plan,task)
		SELECT ?,? WHERE EXISTS (SELECT 1 FROM external_task_admissions WHERE task=?)`, plan, task, task)
	return err
}

// RecordAgentDeliveryForPlan preserves a proven originating OAuth task for a
// proposal outcome. Plans without that explicit link remain static-only.
func (db *DB) RecordAgentDeliveryForPlan(butlerSeq int64, kind agentapi.DeliveryKind, planID, payload string) (int64, error) {
	plan, err := parsePlanID(planID)
	if err != nil {
		return 0, err
	}
	var task int64
	err = db.sql.QueryRow(`SELECT task FROM external_plan_origins WHERE plan=?`, plan).Scan(&task)
	if errors.Is(err, sql.ErrNoRows) {
		return db.RecordAgentDelivery(butlerSeq, kind, payload)
	}
	if err != nil {
		return 0, err
	}
	return db.recordDeliveryWithOrigin(butlerSeq, kind, "", payload, task)
}

func (db *DB) recordDeliveryWithOrigin(butlerSeq int64, kind agentapi.DeliveryKind, cursor, payload string, task int64) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if !kind.Valid() || butlerSeq == 0 || task <= 0 {
		return 0, fmt.Errorf("runtimedb: invalid task delivery")
	}
	if payload == "" {
		payload = "{}"
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var p ExternalDeliveryProvenance
	var taskButler int64
	var audience string
	err = tx.QueryRow(`SELECT a.registration_id,r.person_id,r.connection,r.runtime_principal,
		r.credential_epoch,a.task,t.audience,b.seq
		FROM external_task_admissions a
		JOIN external_client_registrations r ON r.id=a.registration_id AND r.state='active'
		JOIN butler_tasks t ON t.seq=a.task
		JOIN butlers b ON b.name=t.butler
		WHERE a.task=?`, task).Scan(&p.RegistrationID, &p.PersonID, &p.Connection,
		&p.RuntimePrincipal, &p.CredentialEpoch, &p.Task, &audience, &taskButler)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotAdmitted
	}
	if err != nil {
		return 0, err
	}
	if taskButler != butlerSeq {
		return 0, ErrNotAdmitted
	}
	p.Audience = ButlerTaskAudience(audience)
	res, err := tx.Exec(`INSERT INTO agent_deliveries
		(butler,kind,cursor,payload,created_at,expires_at,registration_id,person_id,connection,
		 runtime_principal,credential_epoch,task,audience) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		butlerSeq, string(kind), cursor, payload, now(), time.Now().Add(DefaultAgentDeliveryTTL).UTC().Format(time.RFC3339Nano),
		p.RegistrationID, p.PersonID, p.Connection, p.RuntimePrincipal, p.CredentialEpoch, p.Task, string(p.Audience))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
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
		  WHERE butler = ? AND registration_id = '' AND acked_at = '' AND expired_at = ''
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
		 WHERE butler = ? AND registration_id = '' AND id > ? AND acked_at = '' AND expired_at = ''
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

// PollRuntimeDeliveries performs expiry, final runtime-authority admission and
// delivery selection under one immediate SQLite transaction. That transaction
// is the disclosure linearization point: a completed disable/revoke/rotation
// wins before it; a selection that commits first has already admitted those
// rows for disclosure and cannot recall bytes from the caller. The clock is
// evaluated after transaction admission, never before a potentially blocked Begin.
func (db *DB) PollRuntimeDeliveries(cred RuntimeDeliveryCredential, after int64, limit int, clock func() time.Time) ([]AgentDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("runtimedb: beginning runtime delivery poll: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := gateTx(tx, GateDelivery); err != nil {
		return nil, err
	}
	// Sample only after Begin has acquired the immediate transaction lock.
	// A credential may expire while waiting for a connection or writer.
	asOf := clock()
	butler, err := authorizeRuntimeDeliveryTx(tx, cred, asOf)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`UPDATE agent_deliveries SET expired_at = ?
		  WHERE butler = ? AND registration_id = '' AND acked_at = '' AND expired_at = ''
		    AND expires_at <> '' AND expires_at < ?`,
		now(), butler, asOf.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("runtimedb: marking expired deliveries for butler %d: %w", butler, err)
	}
	rows, err := tx.Query(
		agentDeliveryColumns+`
		 WHERE butler = ? AND registration_id = '' AND id > ? AND acked_at = '' AND expired_at = ''
		 ORDER BY id ASC LIMIT ?`,
		butler, after, limit)
	if err != nil {
		return nil, err
	}
	var out []AgentDelivery
	for rows.Next() {
		d, err := scanAgentDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("runtimedb: committing runtime delivery poll: %w", err)
	}
	return out, nil
}

// PollExternalDeliveries selects only rows carrying the exact freshly
// introspected OAuth identity. The local registration, Person, Connection,
// Agent, runtime, epoch, task and audience fences are rechecked under the same
// transaction that selects bytes for disclosure.
func (db *DB) PollExternalDeliveries(id ExternalDeliveryIdentity, after int64, limit int, clock func() time.Time) ([]AgentDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := gateTx(tx, GateDelivery); err != nil {
		return nil, err
	}
	reg, err := externalDeliveryRegistration(tx, id)
	if err != nil {
		return nil, err
	}
	asOf := clock()
	if _, err := tx.Exec(`UPDATE agent_deliveries SET expired_at=?
		WHERE registration_id=? AND acked_at='' AND expired_at='' AND expires_at<>'' AND expires_at<?`,
		now(), reg.ID, asOf.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	rows, err := tx.Query(agentDeliveryColumns+`
		WHERE registration_id=? AND person_id=? AND butler=? AND connection=? AND runtime_principal=?
		  AND credential_epoch=? AND id>? AND acked_at='' AND expired_at=''
		  AND task IN (SELECT a.task FROM external_task_admissions a
		               JOIN butler_tasks t ON t.seq=a.task
		              WHERE a.registration_id=? AND t.owner_person_id=? AND t.butler=(SELECT name FROM butlers WHERE seq=?)
		                AND t.audience=agent_deliveries.audience)
		ORDER BY id ASC LIMIT ?`, reg.ID, reg.PersonID, reg.Butler, reg.Connection,
		reg.RuntimePrincipal, reg.CredentialEpoch, after, reg.ID, reg.PersonID, reg.Butler, limit)
	if err != nil {
		return nil, err
	}
	var out []AgentDelivery
	for rows.Next() {
		d, err := scanAgentDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
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
	err := db.sql.QueryRow(`SELECT 1 FROM agent_deliveries WHERE id = ? AND butler = ? AND registration_id = ''`, id, butlerSeq).Scan(&exists)
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

// AckRuntimeDelivery is AckAgentDelivery with the runtime's live authority
// checked and fenced with the write. The immediate transaction makes either
// this acknowledgment or a concurrent disable/revoke the first committed
// writer; the second observes the first. Repeated authorized acknowledgment
// remains idempotent because the update only fills an empty acked_at.
func (db *DB) AckRuntimeDelivery(cred RuntimeDeliveryCredential, id int64, clock func() time.Time) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: beginning runtime delivery acknowledgment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := gateTx(tx, GateDelivery); err != nil {
		return err
	}
	// Sample only after Begin has acquired the immediate transaction lock.
	// A credential may expire while waiting for a connection or writer.
	asOf := clock()
	butler, err := authorizeRuntimeDeliveryTx(tx, cred, asOf)
	if err != nil {
		return err
	}
	var exists int64
	err = tx.QueryRow(`SELECT 1 FROM agent_deliveries WHERE id = ? AND butler = ? AND registration_id = ''`, id, butler).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: acking delivery %d: %w", id, err)
	}
	if _, err := tx.Exec(
		`UPDATE agent_deliveries SET acked_at = ? WHERE id = ? AND butler = ? AND acked_at = ''`,
		now(), id, butler); err != nil {
		return fmt.Errorf("runtimedb: acking delivery %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: committing runtime delivery acknowledgment: %w", err)
	}
	return nil
}

// AckExternalDelivery fences acknowledgment by the same exact immutable tuple
// used for polling. A valid duplicate remains idempotent; a wrong, legacy,
// cross-registration or stale row is indistinguishable from not found.
func (db *DB) AckExternalDelivery(identity ExternalDeliveryIdentity, deliveryID int64) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := gateTx(tx, GateDelivery); err != nil {
		return err
	}
	reg, err := externalDeliveryRegistration(tx, identity)
	if err != nil {
		return err
	}
	var exists int
	err = tx.QueryRow(`SELECT 1 FROM agent_deliveries d
		JOIN external_task_admissions a ON a.task=d.task AND a.registration_id=d.registration_id
		JOIN butler_tasks t ON t.seq=d.task AND t.owner_person_id=d.person_id AND t.audience=d.audience
		WHERE d.id=? AND d.registration_id=? AND d.person_id=? AND d.butler=? AND d.connection=?
		  AND d.runtime_principal=? AND d.credential_epoch=?`, deliveryID, reg.ID, reg.PersonID,
		reg.Butler, reg.Connection, reg.RuntimePrincipal, reg.CredentialEpoch).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agent_deliveries SET acked_at=? WHERE id=? AND acked_at=''`, now(), deliveryID); err != nil {
		return err
	}
	return tx.Commit()
}

func externalDeliveryRegistration(tx *sql.Tx, id ExternalDeliveryIdentity) (ExternalClientRegistration, error) {
	if id.RegistrationID == "" || id.PersonID == "" || id.Butler <= 0 || id.Connection <= 0 ||
		id.RuntimePrincipal == "" || id.CredentialEpoch <= 0 {
		return ExternalClientRegistration{}, ErrRuntimeDeliveryDenied
	}
	reg, err := scanExternalRegistration(tx.QueryRow(externalRegistrationColumns+
		` WHERE id=? AND person_id=? AND butler=? AND connection=? AND runtime_principal=?
		  AND credential_epoch=? AND state='active'`, id.RegistrationID, id.PersonID, id.Butler,
		id.Connection, id.RuntimePrincipal, id.CredentialEpoch))
	if err != nil {
		return ExternalClientRegistration{}, ErrRuntimeDeliveryDenied
	}
	if err := validateExternalRegistrationAuthority(tx, reg); err != nil {
		return ExternalClientRegistration{}, ErrRuntimeDeliveryDenied
	}
	return reg, nil
}

// authorizeRuntimeDeliveryTx is the shared HTTP/in-process final authority
// check. The immutable binding is necessary but not sufficient: the principal
// must still be enabled and current, the runtime must not be revoked, and the
// Agent must remain live. HTTP callers additionally prove that the exact token
// which entered the request is still live and unexpired.
func authorizeRuntimeDeliveryTx(tx *sql.Tx, cred RuntimeDeliveryCredential, asOf time.Time) (int64, error) {
	if cred.Principal == "" {
		return 0, ErrRuntimeDeliveryDenied
	}
	var butler int64
	err := tx.QueryRow(`
		SELECT rp.butler
		  FROM runtime_principals rp
		  JOIN principals p ON p.name = rp.principal
		  JOIN butler_runtimes rt ON rt.butler = rp.butler
		  JOIN butlers b ON b.seq = rp.butler
		 WHERE rp.principal = ?
		   AND p.role = ? AND p.disabled = 0
		   AND rt.principal = rp.principal AND rt.health <> ?
		   AND b.enabled = 1 AND b.retired_at = ''`,
		cred.Principal, string(RoleButler), string(agentapi.HealthRevoked)).Scan(&butler)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrRuntimeDeliveryDenied
	}
	if err != nil {
		return 0, fmt.Errorf("runtimedb: checking runtime delivery authority: %w", err)
	}
	if cred.TokenHash != "" {
		var expiry string
		err = tx.QueryRow(`
			SELECT expires_at FROM tokens
			 WHERE hash = ? AND principal = ? AND revoked_at = ''`,
			cred.TokenHash, cred.Principal).Scan(&expiry)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrRuntimeDeliveryDenied
		}
		if err != nil {
			return 0, fmt.Errorf("runtimedb: checking runtime delivery credential: %w", err)
		}
		if expiry != "" {
			// RFC3339Nano omits trailing fractional zeros, so TEXT ordering
			// is not chronological ordering (a whole-second Z sorts after .).
			expiresAt, err := time.Parse(time.RFC3339Nano, expiry)
			if err != nil || !expiresAt.After(asOf) {
				return 0, ErrRuntimeDeliveryDenied
			}
		}
	}
	return butler, nil
}
