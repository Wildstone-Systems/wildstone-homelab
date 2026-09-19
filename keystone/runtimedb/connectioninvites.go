package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ConnectionInvite is a Person invited to a chat Connection (ADR-W033
// B7.1). It carries no key; see migration 0056.
type ConnectionInvite struct {
	Seq        int64  `json:"seq"`
	Connection int64  `json:"connection"`
	Butler     string `json:"butler"`
	Transport  string `json:"transport"`
	Capability string `json:"capability"`
	// Surface is the Agent's class ceiling, which decides where the
	// invite is shown: the household dashboard for a family Agent, the
	// Console for an Owner one.
	Surface   ButlerSurface `json:"surface"`
	Member    string        `json:"member"`
	InvitedBy string        `json:"invitedBy"`
	CreatedAt string        `json:"createdAt"`
}

// ErrNoInvite is an invite that does not exist, is closed, or is not the
// asking Person's.
var ErrNoInvite = errors.New("runtimedb: no open invite")

// InviteToConnection records that member is invited to the Connection,
// or returns the invite already open for them there.
func (db *DB) InviteToConnection(connection int64, member, invitedBy string) (ConnectionInvite, error) {
	if connection == 0 || strings.TrimSpace(member) == "" || invitedBy == "" {
		return ConnectionInvite{}, fmt.Errorf("runtimedb: an invite names a connection, a person and who invited them")
	}
	person, err := resolvePersonID(db.sql, member)
	if err != nil {
		return ConnectionInvite{}, fmt.Errorf("runtimedb: inviting %q: %w", member, err)
	}
	if _, err := db.sql.Exec(
		`INSERT INTO connection_invites (connection, person_id, invited_by, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (connection, person_id) WHERE closed_at = '' DO NOTHING`,
		connection, person, invitedBy, now()); err != nil {
		return ConnectionInvite{}, fmt.Errorf("runtimedb: inviting %q: %w", member, err)
	}
	invites, err := db.openInvites(`i.connection = ? AND i.person_id = ?`, connection, person)
	if err != nil {
		return ConnectionInvite{}, err
	}
	if len(invites) != 1 {
		return ConnectionInvite{}, fmt.Errorf("runtimedb: invite for %q not found after recording it", member)
	}
	return invites[0], nil
}

// OpenInvitesFor is every open invite for a member, on Connections that
// are still live.
func (db *DB) OpenInvitesFor(member string) ([]ConnectionInvite, error) {
	person, err := resolvePersonID(db.sql, member)
	if err != nil {
		// Somebody with no Person record has no invites, which is not a
		// failure of the page asking.
		return nil, nil
	}
	return db.openInvites(`i.person_id = ?`, person)
}

// OpenInvite is one open invite, if it is member's.
func (db *DB) OpenInvite(seq int64, member string) (ConnectionInvite, error) {
	person, err := resolvePersonID(db.sql, member)
	if err != nil {
		return ConnectionInvite{}, ErrNoInvite
	}
	invites, err := db.openInvites(`i.seq = ? AND i.person_id = ?`, seq, person)
	if err != nil {
		return ConnectionInvite{}, err
	}
	if len(invites) == 0 {
		return ConnectionInvite{}, ErrNoInvite
	}
	return invites[0], nil
}

// WithdrawInvite closes an open invite without a binding.
func (db *DB) WithdrawInvite(seq int64, reason string) error {
	res, err := db.sql.Exec(
		`UPDATE connection_invites SET closed_at = ?, closed_reason = ? WHERE seq = ? AND closed_at = ''`,
		now(), reason, seq)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoInvite
	}
	return nil
}

func (db *DB) openInvites(where string, args ...any) ([]ConnectionInvite, error) {
	rows, err := db.sql.Query(
		`SELECT i.seq, i.connection, b.name, a.transport, a.external_id, b.surface, m.member, i.invited_by, i.created_at
		 FROM connection_invites i
		 JOIN butler_addresses a ON a.seq = i.connection
		 JOIN butlers b ON b.seq = a.butler
		 JOIN butler_members m ON m.person_id = i.person_id
		 WHERE i.closed_at = '' AND a.revoked_at = '' AND `+where+`
		 ORDER BY i.seq`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing invites: %w", err)
	}
	defer rows.Close()
	var out []ConnectionInvite
	for rows.Next() {
		var i ConnectionInvite
		if err := rows.Scan(&i.Seq, &i.Connection, &i.Butler, &i.Transport, &i.Capability, &i.Surface,
			&i.Member, &i.InvitedBy, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// closeInvitesOnBinding closes the invite a redemption fulfils, inside
// the redemption's own transaction.
func closeInvitesOnBinding(tx *sql.Tx, connection int64, person string) error {
	_, err := tx.Exec(
		`UPDATE connection_invites SET closed_at = ?, closed_reason = 'bound'
		 WHERE connection = ? AND person_id = ? AND closed_at = ''`,
		now(), connection, person)
	return err
}
