package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Role is a public-plane principal role (ADR-W007 A2). The set is closed;
// the CHECK constraint in the schema and this list must agree.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleButler   Role = "butler"
	// RolePublisher may emit household events and nothing else
	// (ADR-W024 A3): an integration's shim can lie about the house; it
	// cannot read state, propose, or approve.
	RolePublisher Role = "publisher"
	// RoleConcierge is a family-surface chat adapter (ADR-W026 A3): it
	// holds the `converse` verb and nothing else. It can open a family
	// session for a chat account bound to a member and carry the answer
	// back; it cannot read the household stream itself, so a compromised
	// adapter cannot enumerate the house.
	RoleConcierge Role = "concierge"
	// RoleValet is an admin-surface chat adapter (ADR-W026 A3). Like a
	// concierge it holds `converse` and nothing else.
	//
	// It is deliberately NOT the butler role. ADR-W024 A5 makes every
	// external agent a butler-role principal, so putting the chat verb
	// there would hand each of them the operator's private chat queue —
	// a capability appearing on credentials minted before it existed.
	RoleValet Role = "valet"
)

// ValidRole reports whether r is one of the closed role set.
func ValidRole(r Role) bool {
	return r == RoleAdmin || r == RoleOperator || r == RoleButler ||
		r == RolePublisher || r == RoleConcierge || r == RoleValet
}

// Principal is who calls the public API. ActingFor is the ADR-W001
// acting-identity foundation: stored and audited from day one, no behavior
// branches on it until v2.
type Principal struct {
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	ActingFor string    `json:"actingFor,omitempty"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// CreatePrincipal inserts a new principal. Names follow the resource-name
// discipline (validated by the caller in keystone/principal, which owns
// the model; this layer only enforces role closure and uniqueness).
func (db *DB) CreatePrincipal(name string, role Role, actingFor string) error {
	if name == "" {
		return fmt.Errorf("runtimedb: principal name required")
	}
	if !ValidRole(role) {
		return fmt.Errorf("runtimedb: unknown role %q", role)
	}
	_, err := db.sql.Exec(
		`INSERT INTO principals (name, role, acting_for, created_at) VALUES (?, ?, ?, ?)`,
		name, string(role), actingFor, now())
	if err != nil {
		return fmt.Errorf("runtimedb: creating principal %s: %w", name, err)
	}
	return nil
}

// GetPrincipal looks a principal up by name.
func (db *DB) GetPrincipal(name string) (Principal, error) {
	row := db.sql.QueryRow(
		`SELECT name, role, acting_for, disabled, created_at FROM principals WHERE name = ?`, name)
	return scanPrincipal(row)
}

// ListPrincipals returns every principal, name-ordered.
func (db *DB) ListPrincipals() ([]Principal, error) {
	rows, err := db.sql.Query(
		`SELECT name, role, acting_for, disabled, created_at FROM principals ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing principals: %w", err)
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPrincipalDisabled flips a principal's disabled flag. Disabling does
// not delete tokens; authn checks the flag on every request.
func (db *DB) SetPrincipalDisabled(name string, disabled bool) error {
	res, err := db.sql.Exec(`UPDATE principals SET disabled = ? WHERE name = ?`, boolInt(disabled), name)
	if err != nil {
		return fmt.Errorf("runtimedb: disabling principal %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanPrincipal(row rowScanner) (Principal, error) {
	var p Principal
	var role, created string
	var disabled int
	err := row.Scan(&p.Name, &role, &p.ActingFor, &disabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrNotFound
	}
	if err != nil {
		return Principal{}, fmt.Errorf("runtimedb: scanning principal: %w", err)
	}
	p.Role = Role(role)
	p.Disabled = disabled != 0
	if p.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Principal{}, fmt.Errorf("runtimedb: bad principal timestamp %q: %w", created, err)
	}
	return p, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- tokens

// Token is a stored bearer-token record. Hash is hex sha256 of the secret;
// the secret itself is never stored (ADR-W007 A3).
type Token struct {
	Hash      string    `json:"-"` // never serialized: even the hash stays server-side
	Principal string    `json:"principal"`
	ActingFor string    `json:"actingFor,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	CreatedAt time.Time `json:"createdAt"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

// InsertToken stores a token record. expiresAt zero means no expiry.
func (db *DB) InsertToken(hash, principal, actingFor string, expiresAt time.Time) error {
	if hash == "" || principal == "" {
		return fmt.Errorf("runtimedb: token hash and principal required")
	}
	exp := ""
	if !expiresAt.IsZero() {
		exp = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := db.sql.Exec(
		`INSERT INTO tokens (hash, principal, acting_for, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		hash, principal, actingFor, exp, now())
	if err != nil {
		return fmt.Errorf("runtimedb: inserting token: %w", err)
	}
	return nil
}

// GetToken looks a token record up by secret hash.
func (db *DB) GetToken(hash string) (Token, error) {
	row := db.sql.QueryRow(
		`SELECT hash, principal, acting_for, expires_at, created_at, last_used, revoked_at
		 FROM tokens WHERE hash = ?`, hash)
	var t Token
	var exp, created, lastUsed, revoked string
	err := row.Scan(&t.Hash, &t.Principal, &t.ActingFor, &exp, &created, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, fmt.Errorf("runtimedb: scanning token: %w", err)
	}
	for _, f := range []struct {
		s   string
		dst *time.Time
	}{{exp, &t.ExpiresAt}, {created, &t.CreatedAt}, {lastUsed, &t.LastUsed}, {revoked, &t.RevokedAt}} {
		if f.s == "" {
			continue
		}
		if *f.dst, err = time.Parse(time.RFC3339Nano, f.s); err != nil {
			return Token{}, fmt.Errorf("runtimedb: bad token timestamp %q: %w", f.s, err)
		}
	}
	return t, nil
}

// TouchToken stamps last_used. Best-effort from authn's perspective, but
// errors still surface so a read-only disk doesn't fail silently.
func (db *DB) TouchToken(hash string) error {
	_, err := db.sql.Exec(`UPDATE tokens SET last_used = ? WHERE hash = ?`, now(), hash)
	if err != nil {
		return fmt.Errorf("runtimedb: touching token: %w", err)
	}
	return nil
}

// RevokeToken marks a token revoked. Revoking an already-revoked token is
// a no-op, not an error (idempotent for the admin API).
// LiveTokensFor counts the usable tokens a principal holds: not
// revoked, not expired.
//
// "The principal exists" is not the same question, and the difference is
// a real state the system produces: credentialing a chat adapter creates
// the principal, mints a token, and places it on the node — and when the
// placement fails it revokes the token and leaves the principal behind.
// A caller asking "is this adapter credentialed" and getting yes from an
// existing principal would be reading a failed step as a finished one.
func (db *DB) LiveTokensFor(principal string) (int, error) {
	var n int
	err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM tokens
		 WHERE principal = ? AND revoked_at = '' AND (expires_at = '' OR expires_at > ?)`,
		principal, now()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: counting tokens for %s: %w", principal, err)
	}
	return n, nil
}

func (db *DB) RevokeToken(hash string) error {
	res, err := db.sql.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE hash = ? AND revoked_at = ''`, now(), hash)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking token: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Distinguish "absent" from "already revoked" for the caller.
		if _, err := db.GetToken(hash); errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
	}
	return nil
}
