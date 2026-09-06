package runtimedb

// Chat bindings and claim codes (ADR-W026 A4): how a chat account
// becomes a person Wildstone will talk to.
//
// The rule this file exists to enforce is that an adapter never asserts
// an identity. It presents a transport and an account id; a binding row
// — written by a human redeeming a code — is what turns that into a
// member or a principal. There is no path here that creates a binding
// from a message, an allow-list, or a config file.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ChatBinding is one chat account bound to one identity on one surface.
type ChatBinding struct {
	ID          int64         `json:"id"`
	Transport   string        `json:"transport"`
	Account     string        `json:"account"`
	Surface     ButlerSurface `json:"surface"`
	Identity    string        `json:"identity"`
	DisplayName string        `json:"displayName,omitempty"`
	CreatedAt   time.Time     `json:"createdAt"`
	CreatedBy   string        `json:"createdBy"`
	LastUsedAt  time.Time     `json:"lastUsedAt,omitempty"`
	RevokedAt   time.Time     `json:"revokedAt,omitempty"`
}

// Revoked reports whether this binding has been withdrawn.
func (b ChatBinding) Revoked() bool { return !b.RevokedAt.IsZero() }

// ChatClaimCode is an issued, not-yet-redeemed invitation for one
// identity to bind one chat account on one surface. The secret itself is
// never stored — only its hash — so this struct describes a code, it
// does not contain one.
type ChatClaimCode struct {
	Surface    ButlerSurface `json:"surface"`
	Identity   string        `json:"identity"`
	Transport  string        `json:"transport,omitempty"`
	CreatedAt  time.Time     `json:"createdAt"`
	CreatedBy  string        `json:"createdBy"`
	ExpiresAt  time.Time     `json:"expiresAt"`
	RedeemedAt time.Time     `json:"redeemedAt,omitempty"`
	BindingID  int64         `json:"bindingId,omitempty"`
}

// ErrClaimCode is returned when a presented code is not redeemable. Its
// reasons deliberately collapse into one error: unknown, expired,
// already redeemed and wrong-transport are all "this code does not work"
// to whoever is holding it, and distinguishing them for the caller is
// free help for someone guessing.
var ErrClaimCode = errors.New("runtimedb: claim code is not redeemable")

// InsertChatClaimCode records an issued code by its hash.
func (db *DB) InsertChatClaimCode(hash string, c ChatClaimCode) error {
	if hash == "" || c.Identity == "" || c.CreatedBy == "" {
		return fmt.Errorf("runtimedb: claim code requires a hash, identity and issuer")
	}
	if !ValidButlerSurface(c.Surface) {
		return fmt.Errorf("runtimedb: claim code surface %q is not admin|family", c.Surface)
	}
	if c.ExpiresAt.IsZero() {
		return fmt.Errorf("runtimedb: claim code requires an expiry (ADR-W026 A4: short-lived, single-use)")
	}
	_, err := db.sql.Exec(
		`INSERT INTO chat_claim_codes (hash, surface, identity, transport, created_at, created_by, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hash, string(c.Surface), c.Identity, c.Transport, now(), c.CreatedBy,
		c.ExpiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("runtimedb: inserting claim code: %w", err)
	}
	return nil
}

// RedeemChatClaimCode turns a presented code hash into a binding, in one
// transaction: the code must be unredeemed, unexpired, and for this
// transport; the binding is written; the code is burned. Redeeming twice
// fails the second time, which is what single-use means.
//
// An existing live binding for the same (transport, account, surface) is
// revoked first, so re-binding an account — a new phone, a re-installed
// app — is a normal act rather than a unique-constraint failure.
func (db *DB) RedeemChatClaimCode(hash, transport, account, displayName string) (ChatBinding, error) {
	if transport == "" || account == "" {
		return ChatBinding{}, fmt.Errorf("runtimedb: redeeming requires a transport and account")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return ChatBinding{}, err
	}
	defer tx.Rollback() //nolint:errcheck // rolled back unless committed

	var surface, identity, codeTransport, redeemedAt, expiresAt, createdBy string
	err = tx.QueryRow(
		`SELECT surface, identity, transport, redeemed_at, expires_at, created_by
		 FROM chat_claim_codes WHERE hash = ?`, hash).
		Scan(&surface, &identity, &codeTransport, &redeemedAt, &expiresAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatBinding{}, ErrClaimCode
	}
	if err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: reading claim code: %w", err)
	}
	if redeemedAt != "" {
		return ChatBinding{}, ErrClaimCode
	}
	if codeTransport != "" && codeTransport != transport {
		return ChatBinding{}, ErrClaimCode
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: parsing claim code expiry: %w", err)
	}
	if time.Now().After(exp) {
		return ChatBinding{}, ErrClaimCode
	}

	stamp := now()
	if _, err := tx.Exec(
		`UPDATE chat_bindings SET revoked_at = ?
		 WHERE transport = ? AND account = ? AND surface = ? AND revoked_at = ''`,
		stamp, transport, account, surface); err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: replacing binding: %w", err)
	}
	res, err := tx.Exec(
		`INSERT INTO chat_bindings (transport, account, surface, identity, display_name, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		transport, account, surface, identity, displayName, stamp, createdBy)
	if err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: inserting binding: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ChatBinding{}, err
	}
	if _, err := tx.Exec(
		`UPDATE chat_claim_codes SET redeemed_at = ?, binding_id = ? WHERE hash = ? AND redeemed_at = ''`,
		stamp, id, hash); err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: burning claim code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ChatBinding{}, err
	}
	created, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return ChatBinding{}, err
	}
	return ChatBinding{
		ID: id, Transport: transport, Account: account, Surface: ButlerSurface(surface),
		Identity: identity, DisplayName: displayName, CreatedAt: created, CreatedBy: createdBy,
	}, nil
}

// LookupChatBinding resolves a live binding for one account on one
// surface. A revoked binding is not found, which is what makes revoking
// take effect on the next message.
func (db *DB) LookupChatBinding(transport, account string, surface ButlerSurface) (ChatBinding, error) {
	row := db.sql.QueryRow(
		`SELECT id, transport, account, surface, identity, display_name, created_at, created_by, last_used_at, revoked_at
		 FROM chat_bindings WHERE transport = ? AND account = ? AND surface = ? AND revoked_at = ''`,
		transport, account, string(surface))
	b, err := scanChatBinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatBinding{}, ErrNotFound
	}
	return b, err
}

// ListChatBindings returns bindings newest first. Revoked ones are
// included only when withRevoked is set: an operator auditing who can
// talk to their homelab wants the live list, and an operator asking what
// happened wants the history.
func (db *DB) ListChatBindings(withRevoked bool) ([]ChatBinding, error) {
	where := ""
	if !withRevoked {
		where = " WHERE revoked_at = ''"
	}
	rows, err := db.sql.Query(
		`SELECT id, transport, account, surface, identity, display_name, created_at, created_by, last_used_at, revoked_at
		 FROM chat_bindings` + where + ` ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing chat bindings: %w", err)
	}
	defer rows.Close()
	var out []ChatBinding
	for rows.Next() {
		b, err := scanChatBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RevokeChatBinding withdraws one binding. Revoking an already-revoked
// or unknown binding reports ErrNotFound rather than pretending to have
// done something.
func (db *DB) RevokeChatBinding(id int64) error {
	res, err := db.sql.Exec(`UPDATE chat_bindings SET revoked_at = ? WHERE id = ? AND revoked_at = ''`, now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking chat binding: %w", err)
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

// TouchChatBinding stamps last use, so the operator's list shows which
// bindings are alive and which are forgotten phones.
func (db *DB) TouchChatBinding(id int64) error {
	_, err := db.sql.Exec(`UPDATE chat_bindings SET last_used_at = ? WHERE id = ?`, now(), id)
	return err
}

func scanChatBinding(r rowScanner) (ChatBinding, error) {
	var b ChatBinding
	var surface, created, lastUsed, revoked string
	if err := r.Scan(&b.ID, &b.Transport, &b.Account, &surface, &b.Identity, &b.DisplayName,
		&created, &b.CreatedBy, &lastUsed, &revoked); err != nil {
		return ChatBinding{}, err
	}
	b.Surface = ButlerSurface(surface)
	var err error
	if b.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ChatBinding{}, fmt.Errorf("runtimedb: parsing binding created_at: %w", err)
	}
	if lastUsed != "" {
		if b.LastUsedAt, err = time.Parse(time.RFC3339Nano, lastUsed); err != nil {
			return ChatBinding{}, err
		}
	}
	if revoked != "" {
		if b.RevokedAt, err = time.Parse(time.RFC3339Nano, revoked); err != nil {
			return ChatBinding{}, err
		}
	}
	return b, nil
}
