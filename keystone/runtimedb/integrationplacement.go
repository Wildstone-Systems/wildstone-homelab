package runtimedb

// Adapter placement, credentials, and failover (ADR-W030 A5/A5.1, P11).
//
// The one rule that everything here serves: automatic failover may move
// a LEASE and may do nothing else.
//
// Not create a container. Not select a node nobody approved. Not copy a
// credential. Every one of those is what you would reach for at 3am when
// the primary is down and the household wants its calendar, and every
// one of them turns a temporary outage into a credential on a machine
// nobody meant to put it on. So the eligible set is prepared IN ADVANCE,
// by an approving Plan, and failover picks from it or waits.
//
// "Prepared" is three separate facts — the container is running, the
// secret is provisioned, readiness was verified — because they fail
// separately and a node with a container and no secret is a different
// problem from one with a secret and no container.

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Placement roles.
const (
	PlacementPrimary  = "primary"
	PlacementFallback = "fallback"
)

// DefaultStabilization is how long a recovered primary must stay healthy
// before the lease moves back. Failback that does not wait turns a
// flapping node into a lease that ping-pongs, and every bounce is an
// in-flight query answered by nobody.
const DefaultStabilization = 5 * time.Minute

// ErrNoEligibleNode is returned when nothing is prepared to take the
// lease. Dependent work WAITS on this; it does not fail, and nothing
// improvises a placement (ADR-W030 A5).
var ErrNoEligibleNode = errors.New("runtimedb: no prepared node is eligible to hold this connection")

// ErrSecretReferenced is returned when erasing a secret that a live
// placement still uses.
var ErrSecretReferenced = errors.New("runtimedb: that credential is still referenced by a live placement")

// Placement is one node's standing for one connection.
type Placement struct {
	Seq               int64     `json:"seq"`
	Connection        string    `json:"connection"`
	Node              string    `json:"node"`
	Role              string    `json:"role"`
	Rank              int       `json:"rank"`
	ContainerRunning  bool      `json:"containerRunning"`
	SecretProvisioned bool      `json:"secretProvisioned"`
	ReadinessVerified bool      `json:"readinessVerified"`
	HealthySince      time.Time `json:"healthySince,omitzero"`
	LastHealth        time.Time `json:"lastHealth,omitzero"`
	Holder            bool      `json:"holder"`
	Removing          bool      `json:"removing,omitempty"`
	ContainerRemoved  bool      `json:"containerRemoved,omitempty"`
	SecretErased      bool      `json:"secretErased,omitempty"`
	CredentialRevoked bool      `json:"credentialRevoked,omitempty"`
	AddedBy           string    `json:"addedBy"`
}

// Prepared reports whether this node may take the lease. All three
// facts, and not in the middle of being removed.
func (p Placement) Prepared() bool {
	return p.ContainerRunning && p.SecretProvisioned && p.ReadinessVerified && !p.Removing
}

// AddPlacement declares a node for a connection. It is NOT eligible yet:
// adding a fallback is a Plan, and it is incomplete until the container
// is running, the secret is provisioned, and readiness is verified
// (A5.1).
func (db *DB) AddPlacement(connection, node, role string, rank int, addedBy string) error {
	switch role {
	case PlacementPrimary, PlacementFallback:
	default:
		return fmt.Errorf("runtimedb: placement role must be primary|fallback, got %q", role)
	}
	if node == "" || addedBy == "" {
		return fmt.Errorf("runtimedb: a placement needs a node and who approved it")
	}
	conn, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	if role == PlacementPrimary {
		rank = 0
	}
	_, err = db.sql.Exec(
		`INSERT INTO integration_placements (connection, node, role, rank, added_at, added_by)
		 VALUES (?, ?, ?, ?, ?, ?)`, conn.Seq, node, role, rank, now(), addedBy)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("runtimedb: %s is already declared for connection %q", node, connection)
		}
		return fmt.Errorf("runtimedb: adding placement: %w", err)
	}
	return nil
}

// MarkPlacementPrepared records the three facts that make a node
// eligible. Passing false for any of them is how a node becomes
// ineligible again — a container that stopped, a secret that was
// rotated, a readiness probe that failed.
func (db *DB) MarkPlacementPrepared(connection, node string, containerRunning, secretProvisioned, readinessVerified bool) error {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	healthy := ""
	if containerRunning && secretProvisioned && readinessVerified {
		// Only start the stabilization clock when it was not already
		// running: a node that has been healthy for an hour must not
		// look freshly recovered because someone re-reported it.
		var existing string
		_ = db.sql.QueryRow(
			`SELECT healthy_since FROM integration_placements WHERE connection = ? AND node = ?`,
			conn.Seq, node).Scan(&existing)
		healthy = existing
		if healthy == "" {
			healthy = now()
		}
	}
	res, err := db.sql.Exec(
		`UPDATE integration_placements SET container_running = ?, secret_provisioned = ?,
		     readiness_verified = ?, healthy_since = ?, last_health = ?
		 WHERE connection = ? AND node = ?`,
		boolInt(containerRunning), boolInt(secretProvisioned), boolInt(readinessVerified),
		healthy, now(), conn.Seq, node)
	if err != nil {
		return fmt.Errorf("runtimedb: marking placement prepared: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPlacements returns a connection's nodes, primary first then
// fallbacks in rank order.
func (db *DB) ListPlacements(connection string) ([]Placement, error) {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, node, role, rank, container_running, secret_provisioned, readiness_verified,
		        healthy_since, last_health, holder, removing_at, container_removed, secret_erased,
		        credential_revoked, added_by
		 FROM integration_placements WHERE connection = ?
		 ORDER BY role <> 'primary', rank, seq`, conn.Seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing placements: %w", err)
	}
	defer rows.Close()
	var out []Placement
	for rows.Next() {
		p := Placement{Connection: connection}
		var running, provisioned, verified, holder, removed, erased, revoked int
		var healthy, last, removing string
		if err := rows.Scan(&p.Seq, &p.Node, &p.Role, &p.Rank, &running, &provisioned, &verified,
			&healthy, &last, &holder, &removing, &removed, &erased, &revoked, &p.AddedBy); err != nil {
			return nil, err
		}
		p.ContainerRunning, p.SecretProvisioned, p.ReadinessVerified = running != 0, provisioned != 0, verified != 0
		p.Holder, p.Removing = holder != 0, removing != ""
		p.ContainerRemoved, p.SecretErased, p.CredentialRevoked = removed != 0, erased != 0, revoked != 0
		for _, f := range []struct {
			text string
			into *time.Time
		}{{healthy, &p.HealthySince}, {last, &p.LastHealth}} {
			if f.text == "" {
				continue
			}
			parsed, err := time.Parse(time.RFC3339Nano, f.text)
			if err != nil {
				return nil, fmt.Errorf("runtimedb: bad placement timestamp %q: %w", f.text, err)
			}
			*f.into = parsed
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlaceConnection moves the lease to the best prepared node, or reports
// that none is eligible.
//
// "Best" is the primary when it is prepared and has been healthy for the
// stabilization period, otherwise the lowest-ranked prepared fallback.
// The stabilization wait is what stops a flapping primary dragging the
// lease back and forth, and every bounce is an in-flight query answered
// by nobody.
//
// Moving the lease bumps the connection epoch, so an adapter that was
// mid-answer on the old node is fenced rather than allowed to land its
// result beside the new node's.
func (db *DB) PlaceConnection(connection string, stabilization time.Duration) (Placement, bool, error) {
	if stabilization <= 0 {
		stabilization = DefaultStabilization
	}
	placements, err := db.ListPlacements(connection)
	if err != nil {
		return Placement{}, false, err
	}
	var current Placement
	var eligible []Placement
	for _, p := range placements {
		if p.Holder {
			current = p
		}
		if p.Prepared() {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 0 {
		// Nothing is prepared. Dependent work waits; nothing is created.
		return Placement{}, false, ErrNoEligibleNode
	}

	want := eligible[0]
	// Prefer the primary only once it has settled. eligible is already
	// in primary-then-rank order, so this is only about the wait.
	if want.Role == PlacementPrimary && current.Node != "" && current.Node != want.Node {
		if want.HealthySince.IsZero() || time.Since(want.HealthySince) < stabilization {
			// Not settled yet: stay where we are if where we are is
			// still prepared.
			for _, p := range eligible {
				if p.Node == current.Node {
					return current, false, nil
				}
			}
			// The current holder is no longer prepared, so move to the
			// best fallback rather than back to an unsettled primary.
			for _, p := range eligible {
				if p.Role == PlacementFallback {
					want = p
					break
				}
			}
		}
	}
	if current.Node == want.Node {
		return current, false, nil
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return Placement{}, false, fmt.Errorf("runtimedb: placing connection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	conn, err := db.GetConnection(connection)
	if err != nil {
		return Placement{}, false, err
	}
	if _, err := tx.Exec(
		`UPDATE integration_placements SET holder = 0 WHERE connection = ?`, conn.Seq); err != nil {
		return Placement{}, false, fmt.Errorf("runtimedb: placing connection: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE integration_placements SET holder = 1 WHERE connection = ? AND node = ?`,
		conn.Seq, want.Node); err != nil {
		return Placement{}, false, fmt.Errorf("runtimedb: placing connection: %w", err)
	}
	// The lease moves, so the previous holder is fenced: a result it was
	// mid-way through will be refused rather than landing beside the new
	// node's.
	if _, err := tx.Exec(
		`UPDATE integration_connections SET lease_epoch = lease_epoch + 1, updated_at = ? WHERE seq = ?`,
		now(), conn.Seq); err != nil {
		return Placement{}, false, fmt.Errorf("runtimedb: placing connection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Placement{}, false, fmt.Errorf("runtimedb: placing connection: %w", err)
	}
	want.Holder = true
	return want, true, nil
}

// BeginPlacementRemoval fences a node immediately and starts the removal
// it must then PROVE.
//
// Fencing first is the point: removal is incomplete until the container
// is gone, the secret erased, and the credential revoked where the
// remote service permits separate ones — but the adapter must stop being
// able to act the moment someone decides it should go, not when the
// cleanup finishes.
func (db *DB) BeginPlacementRemoval(connection, node string) error {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: removing placement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`UPDATE integration_placements SET removing_at = ?, holder = 0
		 WHERE connection = ? AND node = ? AND removing_at = ''`, now(), conn.Seq, node)
	if err != nil {
		return fmt.Errorf("runtimedb: removing placement: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(
		`UPDATE integration_connections SET lease_epoch = lease_epoch + 1, state = ?,
		     degraded_reason = ?, updated_at = ? WHERE seq = ?`,
		ConnectionDegraded,
		fmt.Sprintf("removing the adapter on %s: its container, secret and credential are not yet proven gone", node),
		now(), conn.Seq); err != nil {
		return fmt.Errorf("runtimedb: removing placement: %w", err)
	}
	// Every Personal credential copy on the node becomes an erasure to
	// prove (migration 0098). ProvePlacementRemoval will not accept
	// "secret erased" for this node until the node's evidence has.
	if err := fenceCredentialCopiesOnNodeTx(tx, conn.Seq, node, conn.LeaseEpoch+1); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: removing placement: %w", err)
	}
	return nil
}

// UnprovenRemovals lists the nodes whose removal was started and has not
// been proven complete.
//
// It exists because "Ready" is a claim about the whole connection, not
// about whichever node just took the lease. A failover that places the
// adapter somewhere healthy while an old node is still holding a
// credential nobody has revoked is not a ready connection — it is a
// degraded one that happens to be answering. Anything about to report
// Ready asks this first.
func (db *DB) UnprovenRemovals(connection string) ([]string, error) {
	placements, err := db.ListPlacements(connection)
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, p := range placements {
		if p.Removing {
			nodes = append(nodes, p.Node)
		}
	}
	return nodes, nil
}

// MarkReadyUnlessRemoving sets the connection Ready only when no removal
// is still outstanding, and otherwise leaves it degraded with a reason
// that names the nodes. It returns whether it reported Ready.
func (db *DB) MarkReadyUnlessRemoving(connection string) (ready bool, err error) {
	pending, err := db.UnprovenRemovals(connection)
	if err != nil {
		return false, err
	}
	if len(pending) > 0 {
		return false, db.SetConnectionState(connection, ConnectionDegraded,
			fmt.Sprintf("the adapter is placed, but removal on %s is not proven complete",
				strings.Join(pending, ", ")))
	}
	return true, db.SetConnectionState(connection, ConnectionReady, "")
}

// ProvePlacementRemoval records what has actually been verified, and
// finishes the removal only when all of it has.
//
// Partial proof leaves the row and the degraded state in place. A
// credential that might still exist on a machine you have stopped
// watching is worse than one you know about, so "we think it's gone" is
// not a state this function can produce.
func (db *DB) ProvePlacementRemoval(connection, node string, containerRemoved, secretErased, credentialRevoked bool) (complete bool, err error) {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return false, err
	}
	// The proof only applies to a placement that is actually being
	// removed. Without this guard a "proof" for a node that was never
	// fenced would fall through to the DELETE below and drop a LIVE
	// placement — no epoch bump, so the node it was running on is
	// still holding a credential nothing has revoked, and the
	// connection would be reported Ready.
	// A caller's word that the secret is gone is not evidence when
	// Keystone sent a Personal credential there itself: those copies are
	// proven erased only by the node's own erase evidence
	// (RecordCredentialErasure), and until then this proof is partial.
	credentialEvidenceMissing := false
	if secretErased {
		outstanding, err := db.credentialCopiesOutstandingOnNode(conn.Seq, node)
		if err != nil {
			return false, fmt.Errorf("runtimedb: proving placement removal: %w", err)
		}
		if outstanding > 0 {
			secretErased, credentialEvidenceMissing = false, true
		}
	}
	res, err := db.sql.Exec(
		`UPDATE integration_placements SET container_removed = ?, secret_erased = ?, credential_revoked = ?
		 WHERE connection = ? AND node = ? AND removing_at <> ''`,
		boolInt(containerRemoved), boolInt(secretErased), boolInt(credentialRevoked),
		conn.Seq, node)
	if err != nil {
		return false, fmt.Errorf("runtimedb: proving placement removal: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n == 0 {
		return false, fmt.Errorf("%w: %q on %q is not being removed, so there is nothing to prove — fence it first",
			ErrNotFound, connection, node)
	}
	if !containerRemoved || !secretErased || !credentialRevoked {
		var missing []string
		if !containerRemoved {
			missing = append(missing, "the container is still there")
		}
		if credentialEvidenceMissing {
			missing = append(missing, ErasureUnverifiedReason+": the node has not returned erase evidence for every credential copy")
		} else if !secretErased {
			missing = append(missing, "the local secret is not proven erased")
		}
		if !credentialRevoked {
			missing = append(missing, "the credential is not proven revoked")
		}
		if err := db.SetConnectionState(connection, ConnectionDegraded,
			fmt.Sprintf("removing the adapter on %s is incomplete: %s", node, strings.Join(missing, "; "))); err != nil {
			return false, err
		}
		return false, nil
	}
	if _, err := db.sql.Exec(
		`DELETE FROM integration_placements WHERE connection = ? AND node = ? AND removing_at <> ''`,
		conn.Seq, node); err != nil {
		return false, fmt.Errorf("runtimedb: completing placement removal: %w", err)
	}
	// The removal is proven. The connection is healthy again only if
	// something else can actually hold the lease — otherwise it is
	// unavailable, which is a true statement about a connection with
	// nowhere to run.
	if _, _, err := db.PlaceConnection(connection, 0); err != nil {
		if errors.Is(err, ErrNoEligibleNode) {
			return true, db.SetConnectionState(connection, ConnectionUnavailable,
				"no prepared node is left to run this adapter")
		}
		return true, err
	}
	if _, err := db.MarkReadyUnlessRemoving(connection); err != nil {
		return true, err
	}
	return true, nil
}

// ConnectionSecret is a reference to a credential, never the credential.
type ConnectionSecret struct {
	Seq              int64     `json:"seq"`
	Connection       string    `json:"connection"`
	Member           string    `json:"member,omitempty"`
	SecretRef        string    `json:"secretRef"`
	UpstreamReadOnly bool      `json:"upstreamReadOnly"`
	Disclosure       string    `json:"disclosure,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	CreatedBy        string    `json:"createdBy"`
	RevokedAt        time.Time `json:"revokedAt,omitzero"`
	// Need, Version and State are set for a credential onboarded through
	// the protected intake (migration 0098); a reference recorded the
	// older way is state "recorded". The login and the value are never
	// here.
	Need    string `json:"need,omitempty"`
	Version int    `json:"version,omitempty"`
	State   string `json:"state"`
}

// Managed reports whether this credential's lifecycle — staging, copies,
// verified erasure — is managed by personalcredentials.go rather than
// being a bare recorded reference.
func (s ConnectionSecret) Managed() bool { return s.State != "" && s.State != CredentialRecorded }

// RecordConnectionSecret records that a credential exists and where its
// reference lives.
//
// The value never comes here — there is no column for it and no
// parameter that could carry one. Connection setup fans the secret
// directly to the eligible nodes over mTLS without persisting it in
// Keystone (A5.1); this row is the bookkeeping that says which
// connection, whose account, and what the credential can actually do
// upstream.
func (db *DB) RecordConnectionSecret(s ConnectionSecret) (int64, error) {
	if s.SecretRef == "" || s.CreatedBy == "" {
		return 0, fmt.Errorf("runtimedb: a connection secret needs a reference and who onboarded it")
	}
	// A reference that looks like a value is refused. It is a crude
	// check and it is the right one: the failure it prevents — a
	// password pasted into the reference field during setup — is a
	// realistic mistake with permanent consequences.
	if looksLikeSecretValue(s.SecretRef) {
		return 0, fmt.Errorf("runtimedb: %q looks like a credential rather than a reference to one — "+
			"the value goes to the node's secret path, never into Keystone (ADR-W012, ADR-W030 A5.1)", "<redacted>")
	}
	if !s.UpstreamReadOnly && s.Disclosure == "" {
		return 0, fmt.Errorf("runtimedb: this credential is not read-only upstream, so it needs a disclosure " +
			"saying what else it can do (ADR-W030 A5.1)")
	}
	conn, err := db.GetConnection(s.Connection)
	if err != nil {
		return 0, err
	}
	res, err := db.sql.Exec(
		`INSERT INTO integration_secrets (connection, member, secret_ref, upstream_read_only,
		     disclosure, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		conn.Seq, s.Member, s.SecretRef, boolInt(s.UpstreamReadOnly), s.Disclosure, now(), s.CreatedBy)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("runtimedb: that credential reference is already recorded for this connection")
		}
		return 0, fmt.Errorf("runtimedb: recording connection secret: %w", err)
	}
	return res.LastInsertId()
}

// looksLikeSecretValue is a deliberately blunt heuristic for a reference
// field that has been given a credential.
//
// A secret REFERENCE is a name — short, no spaces, no scheme, no colon-
// separated user:pass. Anything with whitespace, an @ before a dot, or
// a URL scheme is far more likely to be the thing itself.
func looksLikeSecretValue(ref string) bool {
	if strings.ContainsAny(ref, " \t\n") {
		return true
	}
	lower := strings.ToLower(ref)
	for _, scheme := range []string{"http://", "https://", "imap://", "imaps://", "caldav://"} {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	// user:pass@host, the shape a hurried person pastes.
	if at := strings.Index(ref, "@"); at > 0 && strings.Contains(ref[:at], ":") {
		return true
	}
	return len(ref) > 200
}

// ListConnectionSecrets returns the references for a connection.
func (db *DB) ListConnectionSecrets(connection string) ([]ConnectionSecret, error) {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, member, secret_ref, upstream_read_only, disclosure, created_at, created_by, revoked_at,
		        need, version, state
		 FROM integration_secrets WHERE connection = ? ORDER BY seq`, conn.Seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing connection secrets: %w", err)
	}
	defer rows.Close()
	var out []ConnectionSecret
	for rows.Next() {
		s := ConnectionSecret{Connection: connection}
		var readOnly int
		var created, revoked string
		if err := rows.Scan(&s.Seq, &s.Member, &s.SecretRef, &readOnly, &s.Disclosure,
			&created, &s.CreatedBy, &revoked, &s.Need, &s.Version, &s.State); err != nil {
			return nil, err
		}
		s.UpstreamReadOnly = readOnly != 0
		if s.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		if revoked != "" {
			if s.RevokedAt, err = time.Parse(time.RFC3339Nano, revoked); err != nil {
				return nil, err
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeConnectionSecret marks a credential revoked, refusing while a
// live placement still uses it.
//
// The refusal matters: erasing a secret out from under a running adapter
// produces a connection that fails in a way nobody can diagnose, and the
// correct order — fence the placement, then erase — is the one the
// removal path already takes.
func (db *DB) RevokeConnectionSecret(connection string, seq int64) error {
	var state string
	if err := db.sql.QueryRow(`SELECT state FROM integration_secrets WHERE seq = ?`, seq).Scan(&state); err == nil &&
		state != CredentialRecorded {
		// A managed Personal credential is revoked by fencing it and then
		// proving each copy erased, never by stamping the row.
		return fmt.Errorf("runtimedb: credential %d is managed; revoke it with RevokePersonalCredentials", seq)
	}
	placements, err := db.ListPlacements(connection)
	if err != nil {
		return err
	}
	for _, p := range placements {
		if p.SecretProvisioned && !p.Removing {
			return fmt.Errorf("%w: %s still has it provisioned", ErrSecretReferenced, p.Node)
		}
	}
	res, err := db.sql.Exec(
		`UPDATE integration_secrets SET revoked_at = ? WHERE seq = ? AND revoked_at = ''`, now(), seq)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking connection secret: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
