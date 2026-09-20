package runtimedb

// Withdrawing a session's authority (design §10.1, migration 0062).
//
// §10.1 says a Task loses authority at its next boundary when "the
// Person, session, Connection or grant behind it" loses theirs.
// butler_sessions has never had a lifecycle — a row is written when a
// turn finishes and is never withdrawn — so the session leg of that
// sentence had nothing to read.
//
// This is the REVOCATION half and nothing else. What a session IS —
// keyed to Person + Agent + session_id rather than to a free-text
// caller — is slice 12 (#213), and attempting it here would be this
// slice taking a dependency on the one after it.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrSessionRevoked reports that a session may no longer have work done
// under it.
var ErrSessionRevoked = errors.New("runtimedb: that session was withdrawn")

// RevokeButlerSession withdraws a session. Work derived from it stops
// at its next authorization boundary rather than being hunted down:
// §10.1 evaluates authority at use time, and a revocation that had to
// find every task it affected would be a revocation that could miss one.
func (db *DB) RevokeButlerSession(id, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: revoking a butler session needs who revoked it")
	}
	var seq int64
	if _, err := fmt.Sscanf(id, "bs-%d", &seq); err != nil {
		return ErrNotFound
	}
	res, err := db.sql.Exec(
		`UPDATE butler_sessions SET revoked_at = ?, revoked_by = ?
		 WHERE seq = ? AND revoked_at = ''`, now(), by, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking a butler session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either no such session, or it was already withdrawn. Both are
		// "nothing changed", and neither is a state a caller can fix by
		// retrying.
		return ErrNotFound
	}
	return nil
}

// ButlerSessionActive reports whether a session may still have work
// done under it.
//
// A session that does not exist is NOT active. That matters for derived
// authority: a task naming a session nobody can find is a task whose
// origin cannot be confirmed, and §10.1 answers an unconfirmable origin
// with no.
func (db *DB) ButlerSessionActive(id string) error {
	var seq int64
	if _, err := fmt.Sscanf(id, "bs-%d", &seq); err != nil {
		return fmt.Errorf("%w: %q is not a session id", ErrSessionRevoked, id)
	}
	var revoked string
	err := db.sql.QueryRow(
		`SELECT revoked_at FROM butler_sessions WHERE seq = ?`, seq).Scan(&revoked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: it no longer exists", ErrSessionRevoked)
	case err != nil:
		return fmt.Errorf("runtimedb: reading a butler session's state: %w", err)
	case revoked != "":
		at, perr := time.Parse(time.RFC3339Nano, revoked)
		if perr != nil {
			return ErrSessionRevoked
		}
		return fmt.Errorf("%w: it was withdrawn at %s", ErrSessionRevoked, at.Format(time.RFC3339))
	}
	return nil
}
