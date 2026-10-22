package runtimedb

// Personal integration credentials: staged, materialized on the
// placement nodes, replaced, revoked, and provably erased (epic #326
// S02; ADR-W030 A5.1, ADR-W033 D1).
//
// The value never comes here. A Person types it into the protected
// intake (keystone/api credential handlers), Keystone forwards it once
// over mTLS to each placement node's write-only store, and forgets it.
// What this file keeps is the bookkeeping that makes the rest honest:
// which version exists, on which nodes it was SENT (recorded before the
// value leaves, so a crash mid-fan-out still leaves something to erase),
// and whether each copy's erasure has been PROVEN.
//
// Two orders are the design:
//
//   Replacement stages the new version on every node BEFORE fencing and
//   erasing the old one, so the Person is never left with no working
//   credential because a node was slow.
//
//   Revocation FENCES FIRST — lease epoch bumped, authorizations
//   revoked, queued work refused — and erases second. Access ends when
//   somebody decides it should, not when the cleanup finishes. Until
//   every copy's erasure is proven the connection is degraded with an
//   erasure_unverified reason, because a credential that might still be
//   on a machine nobody is watching is worse than one you know about.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/proto/secreterase"
)

// Credential lifecycle states (integration_secrets.state, migration
// 0098). CredentialRecorded is every row from before 0098: a reference
// somebody recorded, with no lifecycle managed here.
const (
	CredentialRecorded   = "recorded"
	CredentialStaged     = "staged"
	CredentialActive     = "active"
	CredentialSuperseded = "superseded"
	CredentialRevoked    = "revoked"
	CredentialFailed     = "failed"
)

// Copy provisioning and erasure states (integration_secret_copies).
const (
	CopySending     = "sending"
	CopyProvisioned = "provisioned"
	CopyFailed      = "failed"

	ErasePending    = "pending"
	EraseErased     = "erased"
	EraseUnverified = "unverified"
)

// ErasureUnverifiedReason begins the degraded reason of a connection
// with any credential copy not yet proven erased. Console and Butler can
// key on it; the rest of the sentence names the nodes.
const ErasureUnverifiedReason = "erasure_unverified"

// MailboxCredentialNeed is the credential need mail-roundcube's adapter
// declares (packages/mail-roundcube, integrates.adapter.needs).
const MailboxCredentialNeed = "mailbox-app-password"

// MailboxFingerprintKeyNeed is the adapter's per-connection key for
// keyed message fingerprints. Keystone generates it (never a Person),
// sends it to every placement node like the app password so fingerprints
// survive failover, and revokes and erases it with the rest. It is kept
// across app-password replacement: rotating it would make every message
// the monitor has already seen look new.
const MailboxFingerprintKeyNeed = "mailbox-fingerprint-key"

// personalCredentialDisclosure is recorded with every Personal
// credential onboarded through the intake. An IMAP app password is not
// read-only upstream — the provider grants it the whole mailbox — so
// A5.1 requires the wider power to be disclosed rather than assumed.
const personalCredentialDisclosure = "An app password carries the whole mailbox's power at the provider " +
	"(send, delete, move). Wildstone's adapter only reads, and its operations are read-only by " +
	"construction, but the provider does not enforce that: revoke the app password at the provider " +
	"if you stop trusting this connection."

// ErrNotCredentialOwner is returned when anybody but the Person who owns
// a Personal connection tries to onboard, replace, inspect or authorize
// its credential. An Owner administering the household is not that
// Person (ADR-W033 D1: administration is not ownership).
var ErrNotCredentialOwner = errors.New("runtimedb: only the Person whose connection this is may do that")

// ErrNoActiveCredential is returned when a connection has no active
// Personal credential for the need asked about.
var ErrNoActiveCredential = errors.New("runtimedb: this connection has no active credential")

var credentialConnectionName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// PersonalCredentialRef is the node secret name of one credential
// version: "pc.<connection>.v<version>" for the mailbox app password and
// "pc.<connection>.<tag>.v<version>" for any other need (the fingerprint
// key's tag is "fk"). The prefix is shared with Wardstone
// (secreterase.PersonalPrefix), which keeps these names out of the
// household recovery bundle.
func PersonalCredentialRef(connection, need string, version int) (string, error) {
	if !credentialConnectionName.MatchString(connection) {
		return "", fmt.Errorf("runtimedb: connection %q cannot name a node secret (lowercase letters, digits, inner dashes)", connection)
	}
	if version < 1 {
		return "", fmt.Errorf("runtimedb: a credential version starts at 1")
	}
	tag := ""
	switch need {
	case "", MailboxCredentialNeed:
	case MailboxFingerprintKeyNeed:
		tag = "fk."
	default:
		if !credentialConnectionName.MatchString(need) {
			return "", fmt.Errorf("runtimedb: credential need %q cannot name a node secret", need)
		}
		tag = need + "."
	}
	ref := fmt.Sprintf("%s%s.%sv%d", secreterase.PersonalPrefix, connection, tag, version)
	if len(ref) > 64 {
		return "", fmt.Errorf("runtimedb: connection %q is too long to name its credential on a node", connection)
	}
	return ref, nil
}

// PersonalCredentialPrefix is the node-secret name prefix every version
// of every one of connection's credentials shares (connection names
// hold no dot, so it is unambiguous). The orphan sweep erases a name
// under it that Keystone does not know as live.
func PersonalCredentialPrefix(connection string) string {
	return secreterase.PersonalPrefix + connection + "."
}

// PersonalCredential is one credential version and where it went. Login
// is the account name, personal data shown only to its Person.
type PersonalCredential struct {
	Seq        int64            `json:"seq"`
	Connection string           `json:"connection"`
	Member     string           `json:"member"`
	Need       string           `json:"need"`
	Version    int              `json:"version"`
	Login      string           `json:"login,omitempty"`
	SecretRef  string           `json:"secretRef"`
	State      string           `json:"state"`
	CreatedAt  time.Time        `json:"createdAt"`
	CreatedBy  string           `json:"createdBy"`
	RevokedAt  time.Time        `json:"revokedAt,omitzero"`
	Copies     []CredentialCopy `json:"copies"`
}

// CredentialCopy is one node's copy of one credential version.
type CredentialCopy struct {
	Node           string    `json:"node"`
	ProvisionState string    `json:"provisionState"`
	NodeVersion    uint32    `json:"nodeVersion,omitempty"`
	EraseState     string    `json:"eraseState,omitempty"`
	FencedEpoch    int64     `json:"fencedEpoch,omitempty"`
	ErasedAt       time.Time `json:"erasedAt,omitzero"`
	EraseDetail    string    `json:"eraseDetail,omitempty"`
}

// personalConnection returns a connection that must be Personal.
func (db *DB) personalConnection(connection string) (IntegrationConnection, error) {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return IntegrationConnection{}, err
	}
	if conn.Ownership != OwnershipPersonal {
		return IntegrationConnection{}, fmt.Errorf("runtimedb: %q is a household connection; a Personal credential "+
			"and its unattended authorization belong to a Personal connection only", connection)
	}
	return conn, nil
}

// requireCredentialOwner is the one ownership check for the Person-only
// operations: the actor must be the connection's owner AND still an
// active member. Nothing substitutes — not an Owner, not an Agent grant.
func (db *DB) requireCredentialOwner(conn IntegrationConnection, actor string) error {
	if actor == "" || actor != conn.Owner {
		return ErrNotCredentialOwner
	}
	m, err := db.GetButlerMember(actor)
	if err != nil || !m.Active() {
		return fmt.Errorf("%w: %s is not an active member", ErrNotCredentialOwner, actor)
	}
	return nil
}

// StagePersonalCredential records a new credential version for the
// connection's owner and the nodes it is about to be sent to, BEFORE the
// value leaves Keystone. The caller then sends it (MarkCredentialCopy
// per node) and activates it (ActivatePersonalCredential) or fails it
// (FailStagedCredential).
func (db *DB) StagePersonalCredential(connection, need, login, actor string, nodes []string) (PersonalCredential, error) {
	conn, err := db.personalConnection(connection)
	if err != nil {
		return PersonalCredential{}, err
	}
	if err := db.requireCredentialOwner(conn, actor); err != nil {
		return PersonalCredential{}, err
	}
	if need == "" {
		need = MailboxCredentialNeed
	}
	login = strings.TrimSpace(login)
	if (login == "" && need == MailboxCredentialNeed) || len(login) > 320 || strings.ContainsAny(login, " \t\r\n") {
		return PersonalCredential{}, fmt.Errorf("runtimedb: a mailbox login is one account name of at most 320 characters")
	}
	if len(nodes) == 0 {
		return PersonalCredential{}, fmt.Errorf("runtimedb: %q has no placement to send a credential to; "+
			"a credential goes only to the connection's approved placement nodes (ADR-W030 A5.1)", connection)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return PersonalCredential{}, fmt.Errorf("runtimedb: staging credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var version int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(version), 0) + 1 FROM integration_secrets WHERE connection = ? AND need = ?`,
		conn.Seq, need).Scan(&version); err != nil {
		return PersonalCredential{}, fmt.Errorf("runtimedb: staging credential: %w", err)
	}
	ref, err := PersonalCredentialRef(connection, need, version)
	if err != nil {
		return PersonalCredential{}, err
	}
	stamp := now()
	res, err := tx.Exec(
		`INSERT INTO integration_secrets (connection, member, secret_ref, upstream_read_only, disclosure,
		     created_at, created_by, need, version, login, state)
		 VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)`,
		conn.Seq, conn.Owner, ref, personalCredentialDisclosure, stamp, actor, need, version, login, CredentialStaged)
	if err != nil {
		return PersonalCredential{}, fmt.Errorf("runtimedb: staging credential: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return PersonalCredential{}, err
	}
	sorted := append([]string(nil), nodes...)
	sort.Strings(sorted)
	for i, node := range sorted {
		if node == "" || (i > 0 && sorted[i-1] == node) {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO integration_secret_copies (secret, node, provision_state, sent_at) VALUES (?, ?, ?, ?)`,
			seq, node, CopySending, stamp); err != nil {
			return PersonalCredential{}, fmt.Errorf("runtimedb: staging credential copy: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return PersonalCredential{}, fmt.Errorf("runtimedb: staging credential: %w", err)
	}
	return db.GetPersonalCredential(seq)
}

// MarkCredentialCopy records what one node said about the value it was
// sent. A copy that was not confirmed stays "sending" (the node may hold
// it) or becomes "failed" (the node refused it); either way it is
// erased if the credential does not become active.
func (db *DB) MarkCredentialCopy(secret int64, node string, provisioned bool, nodeVersion uint32) error {
	state := CopyFailed
	if provisioned {
		state = CopyProvisioned
	}
	res, err := db.sql.Exec(
		`UPDATE integration_secret_copies SET provision_state = ?, node_version = ?
		 WHERE secret = ? AND node = ? AND erase_state = ''`, state, nodeVersion, secret, node)
	if err != nil {
		return fmt.Errorf("runtimedb: recording credential copy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailStagedCredential abandons a staged version that did not reach
// every node. Whatever it reached is fenced for erasure; the previous
// active version, if any, is untouched and keeps working.
func (db *DB) FailStagedCredential(secret int64, actor, reason string) error {
	cred, err := db.GetPersonalCredential(secret)
	if err != nil {
		return err
	}
	if cred.State != CredentialStaged {
		return fmt.Errorf("runtimedb: credential %d is %s, not staged", secret, cred.State)
	}
	conn, err := db.GetConnection(cred.Connection)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: failing staged credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE integration_secrets SET state = ?, revoked_at = ? WHERE seq = ?`,
		CredentialFailed, now(), secret); err != nil {
		return fmt.Errorf("runtimedb: failing staged credential: %w", err)
	}
	if err := fenceCredentialCopiesTx(tx, secret, conn.LeaseEpoch); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: failing staged credential: %w", err)
	}
	if err := db.journal("secret", cred.SecretRef, "revoked", actor, "staging failed: "+reason); err != nil {
		return err
	}
	return db.settleCredentialState(cred.Connection)
}

// ActivatePersonalCredential makes a staged version the connection's
// credential. Every node it was sent to must have confirmed it. The
// previous active version is SUPERSEDED and fenced in the same
// transaction: the lease epoch moves (the adapter that holds the old
// value is refused from here on), every authorization bound to the old
// version is revoked (a credential is a material field), and each old
// copy becomes an erasure to prove.
func (db *DB) ActivatePersonalCredential(secret int64, actor string) (PersonalCredential, []int64, error) {
	cred, err := db.GetPersonalCredential(secret)
	if err != nil {
		return PersonalCredential{}, nil, err
	}
	if cred.State != CredentialStaged {
		return PersonalCredential{}, nil, fmt.Errorf("runtimedb: credential %d is %s, not staged", secret, cred.State)
	}
	for _, c := range cred.Copies {
		if c.ProvisionState != CopyProvisioned {
			return PersonalCredential{}, nil, fmt.Errorf("runtimedb: credential %d is not yet on %s; "+
				"it becomes active only once every placement node holds it", secret, c.Node)
		}
	}
	conn, err := db.GetConnection(cred.Connection)
	if err != nil {
		return PersonalCredential{}, nil, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return PersonalCredential{}, nil, fmt.Errorf("runtimedb: activating credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var superseded []int64
	var refs []string
	rows, err := tx.Query(
		`SELECT seq, secret_ref FROM integration_secrets
		 WHERE connection = ? AND need = ? AND state IN (?, ?) AND seq <> ?`,
		conn.Seq, cred.Need, CredentialActive, CredentialStaged, secret)
	if err != nil {
		return PersonalCredential{}, nil, fmt.Errorf("runtimedb: activating credential: %w", err)
	}
	for rows.Next() {
		var s int64
		var ref string
		if err := rows.Scan(&s, &ref); err != nil {
			rows.Close()
			return PersonalCredential{}, nil, err
		}
		superseded = append(superseded, s)
		refs = append(refs, ref)
	}
	rows.Close()

	epoch := conn.LeaseEpoch
	if len(superseded) > 0 {
		epoch++
		if _, err := tx.Exec(`UPDATE integration_connections SET lease_epoch = ?, updated_at = ? WHERE seq = ?`,
			epoch, now(), conn.Seq); err != nil {
			return PersonalCredential{}, nil, fmt.Errorf("runtimedb: fencing the previous credential: %w", err)
		}
	}
	stamp := now()
	for _, s := range superseded {
		if _, err := tx.Exec(`UPDATE integration_secrets SET state = ?, revoked_at = ? WHERE seq = ?`,
			CredentialSuperseded, stamp, s); err != nil {
			return PersonalCredential{}, nil, fmt.Errorf("runtimedb: superseding credential: %w", err)
		}
		if err := fenceCredentialCopiesTx(tx, s, epoch); err != nil {
			return PersonalCredential{}, nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE integration_secrets SET state = ? WHERE seq = ?`, CredentialActive, secret); err != nil {
		return PersonalCredential{}, nil, fmt.Errorf("runtimedb: activating credential: %w", err)
	}
	for _, c := range cred.Copies {
		if _, err := tx.Exec(`UPDATE integration_placements SET secret_provisioned = 1
		     WHERE connection = ? AND node = ? AND removing_at = ''`, conn.Seq, c.Node); err != nil {
			return PersonalCredential{}, nil, fmt.Errorf("runtimedb: recording provisioning: %w", err)
		}
	}
	revoked, err := revokeAuthorizationsTx(tx, conn.Seq, "credential", superseded, actor,
		"the credential it was bound to was replaced")
	if err != nil {
		return PersonalCredential{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return PersonalCredential{}, nil, fmt.Errorf("runtimedb: activating credential: %w", err)
	}
	for _, ref := range refs {
		if err := db.journal("secret", ref, "revoked", actor, "replaced by a newer credential"); err != nil {
			return PersonalCredential{}, nil, err
		}
	}
	for _, seq := range revoked {
		if err := db.journal("grant", backgroundAuthorizationJournalID(seq), "revoked", actor, "credential replaced"); err != nil {
			return PersonalCredential{}, nil, err
		}
	}
	if err := db.settleCredentialState(cred.Connection); err != nil {
		return PersonalCredential{}, nil, err
	}
	out, err := db.GetPersonalCredential(secret)
	return out, superseded, err
}

// RevokePersonalCredentials withdraws every live credential version of
// a Personal connection: fence first, erase second.
//
// The fence is one transaction: every staged or active version becomes
// revoked, every copy becomes an erasure to prove, the lease epoch
// moves (so an adapter holding the value can neither claim nor report),
// queued and claimed work is cancelled (unprocessed mail is not read on
// a revoked authority), every live background authorization is revoked,
// and the placements stop counting the secret as provisioned. The
// connection is then degraded with an erasure_unverified reason until
// each copy's erasure is proven (RecordCredentialErasure).
//
// The Person may revoke; so may an Owner administering, and offboarding
// and restore replay: withdrawing access never needs the owner's
// consent, only granting it does (ADR-W033 D1).
func (db *DB) RevokePersonalCredentials(connection, actor, reason string) (int, error) {
	if actor == "" || reason == "" {
		return 0, fmt.Errorf("runtimedb: revoking a credential needs who and why")
	}
	conn, err := db.personalConnection(connection)
	if err != nil {
		return 0, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: revoking credentials: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, refs, authz, err := fenceConnectionCredentialsTx(tx, conn, actor, reason, "")
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: revoking credentials: %w", err)
	}
	for _, ref := range refs {
		if err := db.journal("secret", ref, "revoked", actor, reason); err != nil {
			return n, err
		}
	}
	for _, seq := range authz {
		if err := db.journal("grant", backgroundAuthorizationJournalID(seq), "revoked", actor, reason); err != nil {
			return n, err
		}
	}
	return n, db.settleCredentialState(connection)
}

// fenceConnectionCredentialsTx is the fence half of revocation. onlyRef
// narrows it to one credential (restore replay); empty means every live
// version.
func fenceConnectionCredentialsTx(tx *sql.Tx, conn IntegrationConnection, actor, reason, onlyRef string) (int, []string, []int64, error) {
	query := `SELECT seq, secret_ref, state FROM integration_secrets WHERE connection = ? AND state <> ?`
	args := []any{conn.Seq, CredentialRecorded}
	if onlyRef != "" {
		query += ` AND secret_ref = ?`
		args = append(args, onlyRef)
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("runtimedb: fencing credentials: %w", err)
	}
	type row struct {
		seq   int64
		ref   string
		state string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.ref, &r.state); err != nil {
			rows.Close()
			return 0, nil, nil, err
		}
		all = append(all, r)
	}
	rows.Close()

	epoch := conn.LeaseEpoch + 1
	if _, err := tx.Exec(`UPDATE integration_connections SET lease_epoch = ?, updated_at = ? WHERE seq = ?`,
		epoch, now(), conn.Seq); err != nil {
		return 0, nil, nil, fmt.Errorf("runtimedb: fencing credentials: %w", err)
	}
	stamp := now()
	var revoked []int64
	var refs []string
	for _, r := range all {
		if r.state == CredentialActive || r.state == CredentialStaged {
			if _, err := tx.Exec(`UPDATE integration_secrets SET state = ?, revoked_at = ? WHERE seq = ?`,
				CredentialRevoked, stamp, r.seq); err != nil {
				return 0, nil, nil, fmt.Errorf("runtimedb: revoking credential: %w", err)
			}
			revoked = append(revoked, r.seq)
			refs = append(refs, r.ref)
		}
		// Every copy that is still wanted becomes an erasure, whatever
		// the version's state: a restored snapshot can hold a superseded
		// version whose copies were never fenced.
		if err := fenceCredentialCopiesTx(tx, r.seq, epoch); err != nil {
			return 0, nil, nil, err
		}
	}
	// Unprocessed work is not read on a revoked authority.
	if _, err := tx.Exec(
		`UPDATE integration_work SET state = ?, finished_at = ?, error = ?
		 WHERE connection = ? AND state IN (?, ?)`,
		WorkCancelled, stamp, "the credential was revoked: "+reason, conn.Seq, WorkQueued, WorkClaimed); err != nil {
		return 0, nil, nil, fmt.Errorf("runtimedb: cancelling work on a revoked credential: %w", err)
	}
	if onlyRef == "" || len(revoked) > 0 {
		if _, err := tx.Exec(`UPDATE integration_placements SET secret_provisioned = 0, container_running = 0,
		     healthy_since = '' WHERE connection = ?`, conn.Seq); err != nil {
			return 0, nil, nil, fmt.Errorf("runtimedb: unprovisioning placements: %w", err)
		}
	}
	var authz []int64
	if onlyRef == "" {
		authz, err = revokeAuthorizationsTx(tx, conn.Seq, "", nil, actor, reason)
	} else {
		authz, err = revokeAuthorizationsTx(tx, conn.Seq, "credential", revoked, actor, reason)
	}
	if err != nil {
		return 0, nil, nil, err
	}
	return len(revoked), refs, authz, nil
}

// fenceCredentialCopiesTx marks every still-wanted copy of one credential
// as an erasure to prove, recording the epoch that fenced it.
func fenceCredentialCopiesTx(tx *sql.Tx, secret, epoch int64) error {
	if _, err := tx.Exec(
		`UPDATE integration_secret_copies SET erase_state = ?, fenced_epoch = ?, erase_requested_at = ?
		 WHERE secret = ? AND erase_state = ''`, ErasePending, epoch, now(), secret); err != nil {
		return fmt.Errorf("runtimedb: fencing credential copies: %w", err)
	}
	return nil
}

// fenceCredentialCopiesOnNodeTx fences every copy of a connection's
// credentials on one node — what a placement removal does before it can
// prove the node's secret erased.
func fenceCredentialCopiesOnNodeTx(tx *sql.Tx, connSeq int64, node string, epoch int64) error {
	if _, err := tx.Exec(
		`UPDATE integration_secret_copies SET erase_state = ?, fenced_epoch = ?, erase_requested_at = ?
		 WHERE node = ? AND erase_state = '' AND secret IN (SELECT seq FROM integration_secrets WHERE connection = ?)`,
		ErasePending, epoch, now(), node, connSeq); err != nil {
		return fmt.Errorf("runtimedb: fencing credential copies on %s: %w", node, err)
	}
	return nil
}

// FencePersonalCredentialRef revokes one credential by its node secret
// name — the restore replay of a journalled revocation.
func (db *DB) FencePersonalCredentialRef(ref, actor, reason string) error {
	var connName string
	err := db.sql.QueryRow(
		`SELECT c.name FROM integration_secrets s JOIN integration_connections c ON c.seq = s.connection
		 WHERE s.secret_ref = ? AND s.state <> ?`, ref, CredentialRecorded).Scan(&connName)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: finding credential %s: %w", ref, err)
	}
	conn, err := db.GetConnection(connName)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: fencing credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, _, err := fenceConnectionCredentialsTx(tx, conn, actor, reason, ref); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: fencing credential: %w", err)
	}
	return db.settleCredentialState(connName)
}

// CredentialErasure is one copy whose erasure has not been proven.
type CredentialErasure struct {
	Secret     int64  `json:"secret"`
	Connection string `json:"connection"`
	Adapter    string `json:"adapter"`
	Node       string `json:"node"`
	SecretRef  string `json:"secretRef"`
	// MaxVersion fences the node-side erase to the store version this
	// copy was written as; 0 when the node never confirmed one.
	MaxVersion uint32 `json:"maxVersion"`
	EraseState string `json:"eraseState"`
	// WasActive reports whether this version was ever the connection's
	// credential, and so whether an adapter container may hold it in
	// memory. A version that failed staging never reached one.
	WasActive bool `json:"wasActive"`
}

// OutstandingCredentialErasures lists every copy that is fenced and not
// yet proven erased, for one connection or (empty) all of them.
func (db *DB) OutstandingCredentialErasures(connection string) ([]CredentialErasure, error) {
	query := `SELECT s.seq, c.name, c.adapter, cp.node, s.secret_ref, cp.node_version, cp.erase_state, s.state
		 FROM integration_secret_copies cp
		 JOIN integration_secrets s ON s.seq = cp.secret
		 JOIN integration_connections c ON c.seq = s.connection
		 WHERE cp.erase_state IN (?, ?)`
	args := []any{ErasePending, EraseUnverified}
	if connection != "" {
		query += ` AND c.name = ?`
		args = append(args, connection)
	}
	rows, err := db.sql.Query(query+` ORDER BY c.name, cp.node, s.seq`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing outstanding erasures: %w", err)
	}
	defer rows.Close()
	var out []CredentialErasure
	for rows.Next() {
		var e CredentialErasure
		var v int64
		var state string
		if err := rows.Scan(&e.Secret, &e.Connection, &e.Adapter, &e.Node, &e.SecretRef, &v, &e.EraseState, &state); err != nil {
			return nil, err
		}
		e.MaxVersion = uint32(v)
		e.WasActive = state != CredentialFailed
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordCredentialErasure records the outcome of one node-side erase.
// verified must mean the node's evidence verified (secreterase.Verify)
// AND the adapter container that could hold the value in memory is gone;
// anything less is unverified, with detail saying what is missing.
func (db *DB) RecordCredentialErasure(secret int64, node string, verified bool, evidence, detail string) error {
	state, erasedAt := EraseUnverified, ""
	if verified {
		if evidence == "" {
			return fmt.Errorf("runtimedb: a verified erasure needs the node's evidence")
		}
		state, erasedAt = EraseErased, now()
	} else if detail == "" {
		return fmt.Errorf("runtimedb: an unverified erasure must say what could not be shown")
	}
	res, err := db.sql.Exec(
		`UPDATE integration_secret_copies SET erase_state = ?, erased_at = ?, evidence = ?, erase_detail = ?
		 WHERE secret = ? AND node = ? AND erase_state IN (?, ?)`,
		state, erasedAt, evidence, detail, secret, node, ErasePending, EraseUnverified)
	if err != nil {
		return fmt.Errorf("runtimedb: recording erasure: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: no fenced copy of credential %d on %s", ErrNotFound, secret, node)
	}
	var connection string
	if err := db.sql.QueryRow(
		`SELECT c.name FROM integration_secrets s JOIN integration_connections c ON c.seq = s.connection WHERE s.seq = ?`,
		secret).Scan(&connection); err != nil {
		return fmt.Errorf("runtimedb: recording erasure: %w", err)
	}
	return db.settleCredentialState(connection)
}

// KnownPersonalCredentialRefs maps each node secret name Keystone knows
// for a connection to its state. The orphan sweep erases a name under
// PersonalCredentialPrefix that is absent here or no longer live — the
// version a restored snapshot has never heard of.
func (db *DB) KnownPersonalCredentialRefs(connection string) (map[string]string, error) {
	rows, err := db.sql.Query(
		`SELECT s.secret_ref, s.state FROM integration_secrets s JOIN integration_connections c ON c.seq = s.connection
		 WHERE c.name = ? AND s.state <> ?`, connection, CredentialRecorded)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing credential names: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var ref, state string
		if err := rows.Scan(&ref, &state); err != nil {
			return nil, err
		}
		out[ref] = state
	}
	return out, rows.Err()
}

// credentialCopiesOutstandingOnNode counts a connection's credential
// copies on node that are wanted-or-fenced but not proven erased.
func (db *DB) credentialCopiesOutstandingOnNode(connSeq int64, node string) (int, error) {
	var n int
	err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM integration_secret_copies cp JOIN integration_secrets s ON s.seq = cp.secret
		 WHERE s.connection = ? AND cp.node = ? AND cp.erase_state <> ?`, connSeq, node, EraseErased).Scan(&n)
	return n, err
}

// settleCredentialState makes the connection's state tell the truth
// about its credential copies. Anything fenced and not proven erased is
// erasure_unverified, degraded, naming the nodes. Once every copy is
// proven, only a degradation this file caused is cleared.
func (db *DB) settleCredentialState(connection string) error {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	rows, err := db.sql.Query(
		`SELECT DISTINCT cp.node FROM integration_secret_copies cp JOIN integration_secrets s ON s.seq = cp.secret
		 WHERE s.connection = ? AND cp.erase_state IN (?, ?) ORDER BY cp.node`, conn.Seq, ErasePending, EraseUnverified)
	if err != nil {
		return fmt.Errorf("runtimedb: settling credential state: %w", err)
	}
	var nodes []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if len(nodes) > 0 {
		return db.SetConnectionState(connection, ConnectionDegraded, fmt.Sprintf(
			"%s: a revoked or replaced credential on %s is not proven erased", ErasureUnverifiedReason, strings.Join(nodes, ", ")))
	}
	if conn.State != ConnectionDegraded || !strings.HasPrefix(conn.DegradedReason, ErasureUnverifiedReason) {
		return nil
	}
	// A placement removal still in progress keeps its own degradation:
	// the credential copies being proven gone is only part of it.
	if removing, err := db.UnprovenRemovals(connection); err != nil {
		return err
	} else if len(removing) > 0 {
		return db.SetConnectionState(connection, ConnectionDegraded, fmt.Sprintf(
			"removing the adapter on %s is not proven complete", strings.Join(removing, ", ")))
	}
	if m, err := db.GetButlerMember(conn.Owner); err != nil || !m.Active() {
		return db.SetConnectionState(connection, ConnectionUnavailable,
			fmt.Sprintf("%s is no longer an active member; this was their personal connection", conn.Owner))
	}
	if _, err := db.ActivePersonalCredential(connection, MailboxCredentialNeed); err == nil {
		placements, err := db.ListPlacements(connection)
		if err != nil {
			return err
		}
		for _, p := range placements {
			if p.Holder && p.Prepared() {
				_, err := db.MarkReadyUnlessRemoving(connection)
				return err
			}
		}
	}
	return db.SetConnectionState(connection, ConnectionConfigured, "")
}

// ActivePersonalCredential returns the connection's active credential
// for need. An empty need means "the only active one".
func (db *DB) ActivePersonalCredential(connection, need string) (PersonalCredential, error) {
	creds, err := db.ListPersonalCredentials(connection)
	if err != nil {
		return PersonalCredential{}, err
	}
	var found []PersonalCredential
	for _, c := range creds {
		if c.State == CredentialActive && (need == "" || c.Need == need) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return PersonalCredential{}, ErrNoActiveCredential
	case 1:
		return found[0], nil
	default:
		return PersonalCredential{}, fmt.Errorf("runtimedb: %q has several active credentials; name the need", connection)
	}
}

const personalCredentialColumns = `SELECT s.seq, c.name, s.member, s.need, s.version, s.login, s.secret_ref,
	s.state, s.created_at, s.created_by, s.revoked_at
	FROM integration_secrets s JOIN integration_connections c ON c.seq = s.connection`

// GetPersonalCredential returns one managed credential and its copies.
func (db *DB) GetPersonalCredential(seq int64) (PersonalCredential, error) {
	c, err := scanPersonalCredential(db.sql.QueryRow(personalCredentialColumns+
		` WHERE s.seq = ? AND s.state <> ?`, seq, CredentialRecorded))
	if err != nil {
		return PersonalCredential{}, err
	}
	if c.Copies, err = db.credentialCopies(seq); err != nil {
		return PersonalCredential{}, err
	}
	return c, nil
}

// ListPersonalCredentials returns every managed credential version of a
// connection, oldest first.
func (db *DB) ListPersonalCredentials(connection string) ([]PersonalCredential, error) {
	rows, err := db.sql.Query(personalCredentialColumns+
		` WHERE c.name = ? AND s.state <> ? ORDER BY s.seq`, connection, CredentialRecorded)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing credentials: %w", err)
	}
	var out []PersonalCredential
	for rows.Next() {
		c, err := scanPersonalCredential(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Copies, err = db.credentialCopies(out[i].Seq); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanPersonalCredential(row rowScanner) (PersonalCredential, error) {
	var c PersonalCredential
	var created, revoked string
	err := row.Scan(&c.Seq, &c.Connection, &c.Member, &c.Need, &c.Version, &c.Login, &c.SecretRef,
		&c.State, &created, &c.CreatedBy, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return PersonalCredential{}, ErrNotFound
	}
	if err != nil {
		return PersonalCredential{}, fmt.Errorf("runtimedb: reading credential: %w", err)
	}
	if c.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return PersonalCredential{}, err
	}
	if revoked != "" {
		if c.RevokedAt, err = time.Parse(time.RFC3339Nano, revoked); err != nil {
			return PersonalCredential{}, err
		}
	}
	return c, nil
}

func (db *DB) credentialCopies(secret int64) ([]CredentialCopy, error) {
	rows, err := db.sql.Query(
		`SELECT node, provision_state, node_version, erase_state, fenced_epoch, erased_at, erase_detail
		 FROM integration_secret_copies WHERE secret = ? ORDER BY node`, secret)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading credential copies: %w", err)
	}
	defer rows.Close()
	out := []CredentialCopy{}
	for rows.Next() {
		var c CredentialCopy
		var v int64
		var erased string
		if err := rows.Scan(&c.Node, &c.ProvisionState, &v, &c.EraseState, &c.FencedEpoch, &erased, &c.EraseDetail); err != nil {
			return nil, err
		}
		c.NodeVersion = uint32(v)
		if erased != "" {
			if c.ErasedAt, err = time.Parse(time.RFC3339Nano, erased); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkPlacementContainerStopped records that a placement's adapter
// container was removed — it no longer counts as running, so the node is
// not eligible for the lease until its placement is re-rendered.
func (db *DB) MarkPlacementContainerStopped(connection, node string) error {
	conn, err := db.GetConnection(connection)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(
		`UPDATE integration_placements SET container_running = 0, healthy_since = '', holder = 0
		 WHERE connection = ? AND node = ?`, conn.Seq, node)
	if err != nil {
		return fmt.Errorf("runtimedb: recording adapter removal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdapterPlacementEnv is what a placement renderer passes to
// capability.Engine.ExpandAdapter for a Personal mail connection: the
// non-secret per-connection environment (the login and the authorized
// source scope) and each credential need's node secret name. Neither
// carries a value. The filters come from the LIVE background
// authorization, so an adapter is only ever rendered with the scope its
// Person authorized; with none live, the filters are absent and the
// adapter has nothing it may read.
func (db *DB) AdapterPlacementEnv(connection string) (env, secrets map[string]string, err error) {
	creds, err := db.ListPersonalCredentials(connection)
	if err != nil {
		return nil, nil, err
	}
	env, secrets = map[string]string{}, map[string]string{}
	for _, c := range creds {
		if c.State != CredentialActive {
			continue
		}
		secrets[c.Need] = c.SecretRef
		if c.Need == MailboxCredentialNeed {
			env[AdapterEnvMailboxUsername] = c.Login
		}
	}
	if a, err := db.LiveBackgroundAuthorization(connection); err == nil {
		filters, err := json.Marshal(struct {
			Folders         []string `json:"folders"`
			Senders         []string `json:"senders,omitempty"`
			SubjectContains []string `json:"subjectContains,omitempty"`
		}{a.Scope.Folders, a.Scope.Senders, a.Scope.Subjects})
		if err != nil {
			return nil, nil, err
		}
		env[AdapterEnvMailboxFilters] = string(filters)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	return env, secrets, nil
}

// The per-connection adapter environment the mail adapter reads
// (keystone/cmd/wild-mail-adapter). Not secret; never a credential.
const (
	AdapterEnvMailboxUsername = "WILD_MAILBOX_USERNAME"
	AdapterEnvMailboxFilters  = "WILD_MAILBOX_FILTERS"
)
