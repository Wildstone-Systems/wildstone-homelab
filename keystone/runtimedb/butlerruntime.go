package runtimedb

// The Agent's runtime record and its runtime principal (ADR-W039 §3.3,
// §3.7, §4.1).
//
// A runtime is what reasons for an Agent: the in-process runner
// (`native`), or an external agent framework registered against this
// Agent. The record says which, where, as whom, and how it is doing.
// It is one field of the Agent and replacing it replaces nothing else,
// which is why it is a side table and its own accessors rather than
// columns on Butler.
//
// The runtime principal is a normal Keystone principal with the
// `butler` role, minted by the platform and bound to exactly one Agent
// for its life. The binding is what turns "a butler-role token" into
// "this Agent's runtime": the gateway that later serves capability
// calls resolves the caller to its Agent through it, and a principal
// bound to Agent A can never be handed a task that belongs to Agent B.
// The runtime record's Principal field names the CURRENT credential;
// runtime_principals keeps every credential the Agent ever held.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// ErrRuntimePrincipalBound reports an attempt to bind a runtime
// principal to a second Agent. The binding is immutable (§4.1): a
// runtime that should act for another Agent gets a new credential.
var ErrRuntimePrincipalBound = errors.New("runtimedb: that runtime principal is already bound to another agent")

// ButlerRuntime is one Agent's runtime record. The enumerated fields
// use keystone/agentapi's types so the database and the contract cannot
// drift apart without one of them failing to compile.
type ButlerRuntime struct {
	Butler int64                `json:"butler"`
	Type   agentapi.RuntimeType `json:"type"`
	// Endpoint is empty for native; a Route host or
	// container/<name>:<port> for a packaged runtime.
	Endpoint string `json:"endpoint,omitempty"`
	// Principal names the current credential — §3.3's credentialRef.
	// Empty until the first one is minted.
	Principal     string                 `json:"principal,omitempty"`
	Health        agentapi.RuntimeHealth `json:"health"`
	LastHeartbeat time.Time              `json:"lastHeartbeat,omitzero"`
	// Delivery and ProcessingClass are the negotiated pair. The class
	// is what the Owner declared, never what the runtime claims; empty
	// means undeclared.
	Delivery        agentapi.DeliveryMode    `json:"delivery"`
	ProcessingClass agentapi.ProcessingClass `json:"processingClass,omitempty"`
	// ExecutionProfile is #334's vocabulary, carried as a reference.
	ExecutionProfile string    `json:"executionProfile"`
	UpdatedAt        time.Time `json:"updatedAt"`
	UpdatedBy        string    `json:"updatedBy,omitempty"`
}

// Record is the runtime as the Agent API presents it (§3.3): the same
// facts with the contract's field names, and the principal as a
// credentialRef. Negotiated capabilities are not stored — they are
// whatever the Toolset offers on the next manifest fetch.
func (rt ButlerRuntime) Record() agentapi.RuntimeRecord {
	return agentapi.RuntimeRecord{
		Type:          rt.Type,
		Endpoint:      rt.Endpoint,
		CredentialRef: rt.Principal,
		Health:        rt.Health,
		LastHeartbeat: rt.LastHeartbeat,
		Negotiated: agentapi.Negotiated{
			Delivery:        rt.Delivery,
			ProcessingClass: rt.ProcessingClass,
		},
		ExecutionProfileRef: rt.ExecutionProfile,
	}
}

const butlerRuntimeColumns = `
	SELECT butler, type, endpoint, principal, health, last_heartbeat,
	       delivery, processing_class, execution_profile, updated_at, updated_by
	  FROM butler_runtimes`

func scanButlerRuntime(s interface{ Scan(...any) error }) (ButlerRuntime, error) {
	var rt ButlerRuntime
	var typ, health, delivery, class, heartbeat, updated string
	if err := s.Scan(&rt.Butler, &typ, &rt.Endpoint, &rt.Principal, &health, &heartbeat,
		&delivery, &class, &rt.ExecutionProfile, &updated, &rt.UpdatedBy); err != nil {
		return ButlerRuntime{}, err
	}
	rt.Type = agentapi.RuntimeType(typ)
	rt.Health = agentapi.RuntimeHealth(health)
	rt.Delivery = agentapi.DeliveryMode(delivery)
	rt.ProcessingClass = agentapi.ProcessingClass(class)
	for _, f := range []struct {
		text string
		into *time.Time
	}{{heartbeat, &rt.LastHeartbeat}, {updated, &rt.UpdatedAt}} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return ButlerRuntime{}, fmt.Errorf("runtimedb: bad runtime timestamp %q: %w", f.text, err)
		}
		*f.into = parsed
	}
	return rt, nil
}

// DeriveNativeProcessingClass maps a native runtime's assigned model
// kind onto the Owner-declared processing class the gateway-side
// disclosure filter keys on (ADR-W039 §3.3, §7.2 Phase 5, #345). The
// native runtime is never asked to declare itself the way an external
// one is (§3.5 rule 3): Wildstone already knows which model it calls
// (A2), so the class is derived, not registered — ModelLocal maps to
// local_only and everything else (ModelCloud, and any future hosted
// kind) maps to cloud.
func DeriveNativeProcessingClass(modelKind string) agentapi.ProcessingClass {
	if modelKind == ModelLocal {
		return agentapi.ProcessingLocalOnly
	}
	return agentapi.ProcessingCloud
}

// SetNativeRuntimeProcessingClass backfills butler_runtimes.processing_class
// for one Agent's NATIVE runtime row from its currently assigned model
// kind, without disturbing type, endpoint, delivery mode or execution
// profile, and without touching health or the principal (SetButlerRuntime
// never does either). It is a no-op for a non-native runtime — an
// external runtime's class is the Owner's own declaration at
// registration, never inferred from anything Wildstone can see (§3.5) —
// and for a native row whose class already matches, so callers may run
// it on every convergence (Butler creation, restore, and the startup
// backfill) without spurious writes.
func (db *DB) SetNativeRuntimeProcessingClass(butlerSeq int64, modelKind, by string) error {
	rt, err := db.GetButlerRuntime(butlerSeq)
	if err != nil {
		return err
	}
	if rt.Type != agentapi.RuntimeNative {
		return nil
	}
	class := DeriveNativeProcessingClass(modelKind)
	if rt.ProcessingClass == class {
		return nil
	}
	rt.ProcessingClass = class
	return db.SetButlerRuntime(butlerSeq, rt, by)
}

// GetButlerRuntime reads one Agent's runtime record by Butler seq.
// ErrNotFound when the Agent has none, which after 0081 means the Agent
// itself does not exist.
func (db *DB) GetButlerRuntime(butler int64) (ButlerRuntime, error) {
	rt, err := scanButlerRuntime(db.sql.QueryRow(butlerRuntimeColumns+` WHERE butler = ?`, butler))
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerRuntime{}, ErrNotFound
	}
	if err != nil {
		return ButlerRuntime{}, fmt.Errorf("runtimedb: reading runtime for butler %d: %w", butler, err)
	}
	return rt, nil
}

// GetButlerRuntimeByName is GetButlerRuntime for callers that hold the
// Butler's name, which is what every API route does.
func (db *DB) GetButlerRuntimeByName(name string) (ButlerRuntime, error) {
	rt, err := scanButlerRuntime(db.sql.QueryRow(
		butlerRuntimeColumns+` WHERE butler = (SELECT seq FROM butlers WHERE name = ?)`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerRuntime{}, ErrNotFound
	}
	if err != nil {
		return ButlerRuntime{}, fmt.Errorf("runtimedb: reading runtime for butler %q: %w", name, err)
	}
	return rt, nil
}

// SetButlerRuntime writes the Owner-decided half of an Agent's runtime:
// type, endpoint, delivery mode, processing class and execution
// profile. It creates the row when the Agent has none and otherwise
// replaces those fields, stamping who changed them.
//
// It deliberately does NOT touch health, the heartbeat or the
// principal. Those are observed or minted, not configured, and a
// configuration write that could set health to `connected` would let
// the record claim a runtime is there before it ever authenticated.
// A caller replacing the runtime (§3.7) revokes the old credential and
// registers the new one as separate acts, each with its own guard.
func (db *DB) SetButlerRuntime(butler int64, rt ButlerRuntime, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: setting an agent's runtime needs who set it")
	}
	if !rt.Type.Valid() {
		return fmt.Errorf("runtimedb: unknown runtime type %q", rt.Type)
	}
	if rt.Delivery == "" {
		rt.Delivery = agentapi.DeliveryPush
	}
	if !rt.Delivery.Valid() {
		return fmt.Errorf("runtimedb: unknown delivery mode %q", rt.Delivery)
	}
	if rt.ProcessingClass != "" && !rt.ProcessingClass.Valid() {
		return fmt.Errorf("runtimedb: unknown processing class %q", rt.ProcessingClass)
	}
	if rt.ExecutionProfile == "" {
		rt.ExecutionProfile = "platform-only"
	}
	if rt.Type == agentapi.RuntimeNative && rt.Endpoint != "" {
		return fmt.Errorf("runtimedb: the native runtime is in-process and has no endpoint")
	}
	_, err := db.sql.Exec(
		`INSERT INTO butler_runtimes (butler, type, endpoint, delivery, processing_class,
		     execution_profile, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(butler) DO UPDATE SET
		     type = excluded.type, endpoint = excluded.endpoint,
		     delivery = excluded.delivery, processing_class = excluded.processing_class,
		     execution_profile = excluded.execution_profile,
		     updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		butler, string(rt.Type), rt.Endpoint, string(rt.Delivery), string(rt.ProcessingClass),
		rt.ExecutionProfile, now(), by)
	if err != nil {
		return fmt.Errorf("runtimedb: setting runtime for butler %d: %w", butler, err)
	}
	return nil
}

// SetRuntimeHealth records the Agent's runtime health (§3.7). It is a
// state on the Agent, not a verdict on anything else. ErrNotFound when
// the Agent has no runtime record.
func (db *DB) SetRuntimeHealth(butler int64, health agentapi.RuntimeHealth) error {
	if !health.Valid() {
		return fmt.Errorf("runtimedb: unknown runtime health %q", health)
	}
	res, err := db.sql.Exec(
		`UPDATE butler_runtimes SET health = ?, updated_at = ? WHERE butler = ?`,
		string(health), now(), butler)
	if err != nil {
		return fmt.Errorf("runtimedb: setting runtime health for butler %d: %w", butler, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchRuntimeHeartbeat stamps the runtime's last heartbeat. It carries
// no health claim (Phase 0: a heartbeat is a sign of life, not a
// self-assessment); whoever observes the beat decides what health it
// implies. A revoked runtime cannot heartbeat — its principal is
// disabled before any call reaches here — so the stamp is refused
// rather than recording a beat from a credential that should be dead.
func (db *DB) TouchRuntimeHeartbeat(butler int64) error {
	res, err := db.sql.Exec(
		`UPDATE butler_runtimes SET last_heartbeat = ? WHERE butler = ? AND health <> ?`,
		now(), butler, string(agentapi.HealthRevoked))
	if err != nil {
		return fmt.Errorf("runtimedb: recording heartbeat for butler %d: %w", butler, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepStaleRuntimeHeartbeats flips connected → offline for every
// non-native runtime whose last heartbeat is older than cutoff
// (ADR-W039 §3.7, Phase 4, #340). Native runtimes are never touched —
// nothing runs out of process to lose a heartbeat over — and a runtime
// that is anything other than connected (unregistered, degraded,
// offline, revoked already) is left exactly where it is: this is a
// one-way, health-driven demotion, not a general health-setter. It
// returns the Butler seqs it flipped, oldest first, so the caller can
// decide whether anything of theirs needs re-offering later.
func (db *DB) SweepStaleRuntimeHeartbeats(cutoff time.Time) ([]int64, error) {
	rows, err := db.sql.Query(
		`SELECT butler FROM butler_runtimes
		  WHERE type <> ? AND health = ? AND last_heartbeat <> '' AND last_heartbeat < ?
		  ORDER BY butler`,
		string(agentapi.RuntimeNative), string(agentapi.HealthConnected), cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("runtimedb: finding stale runtimes: %w", err)
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return nil, err
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, seq := range seqs {
		if err := db.SetRuntimeHealth(seq, agentapi.HealthOffline); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return seqs, nil
}

// BindRuntimePrincipal binds a principal to an Agent as its runtime
// credential and makes it the Agent's current one.
//
// The binding is immutable (§4.1). Binding a principal already bound to
// the same Agent is a no-op that still makes it current, so a retried
// mint converges; binding one bound to a different Agent is refused
// with ErrRuntimePrincipalBound, and the trigger in 0081 refuses the
// same thing to anyone who reaches for the row directly.
//
// The principal must exist and hold the butler role: a runtime never
// holds more than an external agent may (ADR-W024 A5), and a binding
// that named an admin credential would hand a runtime the Console.
func (db *DB) BindRuntimePrincipal(principal string, butler int64) error {
	if principal == "" {
		return fmt.Errorf("runtimedb: binding a runtime principal needs its name")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: binding runtime principal: %w", err)
	}
	defer tx.Rollback()

	var role string
	err = tx.QueryRow(`SELECT role FROM principals WHERE name = ?`, principal).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no principal named %q", ErrNotFound, principal)
	}
	if err != nil {
		return fmt.Errorf("runtimedb: binding runtime principal %s: %w", principal, err)
	}
	if Role(role) != RoleButler {
		return fmt.Errorf("runtimedb: a runtime principal holds the %s role, and %q holds %s (ADR-W024 A5)",
			RoleButler, principal, role)
	}

	var bound int64
	err = tx.QueryRow(`SELECT butler FROM runtime_principals WHERE principal = ?`, principal).Scan(&bound)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.Exec(
			`INSERT INTO runtime_principals (principal, butler, created_at) VALUES (?, ?, ?)`,
			principal, butler, now()); err != nil {
			return fmt.Errorf("runtimedb: binding runtime principal %s to butler %d: %w", principal, butler, err)
		}
	case err != nil:
		return fmt.Errorf("runtimedb: binding runtime principal %s: %w", principal, err)
	case bound != butler:
		return fmt.Errorf("%w: %s is bound to butler %d, not %d", ErrRuntimePrincipalBound, principal, bound, butler)
	}

	res, err := tx.Exec(
		`UPDATE butler_runtimes SET principal = ?, updated_at = ? WHERE butler = ?`,
		principal, now(), butler)
	if err != nil {
		return fmt.Errorf("runtimedb: recording runtime credential for butler %d: %w", butler, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: butler %d has no runtime record", ErrNotFound, butler)
	}
	return tx.Commit()
}

// RuntimePrincipalButler resolves a principal to the Agent it is bound
// to. ok is false for a principal that is not a runtime principal at
// all — an admin token, a chat adapter, a name nobody minted — which is
// the answer every capability call checks first.
func (db *DB) RuntimePrincipalButler(principal string) (butler int64, ok bool, err error) {
	err = db.sql.QueryRow(`SELECT butler FROM runtime_principals WHERE principal = ?`, principal).Scan(&butler)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("runtimedb: resolving runtime principal %s: %w", principal, err)
	}
	return butler, true, nil
}

// ListRuntimePrincipals returns every principal ever bound to the
// Agent, oldest first. The last one is the current credential unless it
// has been revoked; the count is what rotation numbers the next one by.
func (db *DB) ListRuntimePrincipals(butler int64) ([]string, error) {
	rows, err := db.sql.Query(
		`SELECT principal FROM runtime_principals WHERE butler = ? ORDER BY created_at, principal`, butler)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing runtime principals for butler %d: %w", butler, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
