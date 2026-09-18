package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Connections (ADR-W033 B7, design §8.1).
//
// A Connection is one way a Person reaches one Agent: a bot on a
// transport, with its own credential, admission and lifecycle. It
// belongs to exactly one Agent, and an Agent may have many, including
// several on the same transport.
//
// The row is butler_addresses. That table predates the name — it was
// written as "where a Butler answers" (ADR-W031 A7, "the bot a message
// arrives at IS the address") — and it is the same thing: one live
// address per (transport, external_id), naming one Butler. So this file
// adds the vocabulary and the lookups rather than a second table.
//
// Not to be confused with integration_connections, which is an account
// on somebody else's service — an IMAP mailbox, a CalDAV calendar. The
// design keeps the two apart (§12.2 separates the household secret
// store from the infrastructure one) and the word is unfortunately the
// same. This one is how a Person reaches an Agent.
//
// What replaced what: admission, claim codes, threads and the outbound
// queue used to key on a SURFACE, and so did the question ledger. A
// surface is a class of Agents, not an Agent, so admitting an account
// to `admin` admitted it to every Owner Agent at once — including ones
// created later. Migration 0042 pointed all five at the Connection
// instead.

// ErrNoConnection is returned when no live Connection answers at an
// address. A message arriving at a bot nothing is assigned to has no
// Agent to run as, and inventing one would be answering as whichever
// Agent happened to sort first.
var ErrNoConnection = errors.New("runtimedb: no connection answers at that address")

// Connection is one Agent's reach on one transport.
//
// The accessors below are Chat-prefixed because this package already
// has ListConnections for integration_connections — an account on
// somebody else's service, which is a different thing wearing the same
// word. The type keeps the ADR's name; the methods say which kind.
type Connection struct {
	Seq        int64  `json:"seq"`
	Butler     string `json:"butler"`
	Transport  string `json:"transport"`
	ExternalID string `json:"externalId"`
	Label      string `json:"label,omitempty"`
	// Surface is the Agent's class ceiling, carried here because
	// admission depends on it: an Owner Agent's Connection admits
	// Owners only (B9). It is the Agent's property, not the
	// Connection's — slice 9 (#210) renames it to the agent class the
	// design uses.
	Surface ButlerSurface `json:"surface"`
}

// ChatConnectionAt resolves the live Connection at one address — which is
// what an inbound message has to hand: the bot it arrived at.
//
// BOTH halves are required, and a lookup on external_id alone would be
// wrong: butler_addresses_live is unique on (transport, external_id),
// not on external_id, and AddButlerAddress takes transport-local ids on
// purpose. Two live addresses on different transports may share one, so
// an external id alone can resolve an adapter to the wrong Agent — and
// therefore to another Agent's queue.
func (db *DB) ChatConnectionAt(transport, externalID string) (Connection, error) {
	return scanChatConnection(db.sql.QueryRow(chatConnectionColumns+`
		 WHERE a.transport = ? AND a.external_id = ? AND a.revoked_at = ''`,
		transport, externalID))
}

// ChatConnectionBySeq reads one by its seq, for a caller holding a stored
// reference rather than an address.
func (db *DB) ChatConnectionBySeq(seq int64) (Connection, error) {
	return scanChatConnection(db.sql.QueryRow(chatConnectionColumns+` WHERE a.seq = ?`, seq))
}

const chatConnectionColumns = `SELECT a.seq, b.name, a.transport, a.external_id, a.label, b.surface
	 FROM butler_addresses a JOIN butlers b ON b.seq = a.butler`

func scanChatConnection(row rowScanner) (Connection, error) {
	var c Connection
	var surface string
	err := row.Scan(&c.Seq, &c.Butler, &c.Transport, &c.ExternalID, &c.Label, &surface)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrNoConnection
	}
	if err != nil {
		return Connection{}, fmt.Errorf("runtimedb: reading a connection: %w", err)
	}
	c.Surface = ButlerSurface(surface)
	return c, nil
}

// AdmitsOwnersOnly reports whether this Connection belongs to an Owner
// Agent, which admits Owners and nobody else (ADR-W033 B9).
func (c Connection) AdmitsOwnersOnly() bool { return c.Surface == ButlerSurfaceAdmin }

// ErrAmbiguousConnection is returned when an Agent has more than one
// Connection on a transport and the caller named neither.
//
// B7 allows exactly this — "multiple Connections of the same transport
// type" — so the answer is to ask rather than to take the first. A code
// issued for the wrong one of somebody's two Telegram bots is an invite
// that works and admits them somewhere they did not mean to go.
var ErrAmbiguousConnection = errors.New("runtimedb: that agent has more than one connection on that transport")

// ChatConnectionForButler resolves the live Connection an Agent answers on
// for one transport.
func (db *DB) ChatConnectionForButler(butler, transport string) (Connection, error) {
	rows, err := db.sql.Query(chatConnectionColumns+`
		 WHERE b.name = ? AND a.transport = ? AND a.revoked_at = ''
		 ORDER BY a.seq`, butler, transport)
	if err != nil {
		return Connection{}, fmt.Errorf("runtimedb: reading connections for %q: %w", butler, err)
	}
	defer rows.Close()
	var found []Connection
	for rows.Next() {
		c, err := scanChatConnection(rows)
		if err != nil {
			return Connection{}, err
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return Connection{}, err
	}
	switch len(found) {
	case 0:
		return Connection{}, ErrNoConnection
	case 1:
		return found[0], nil
	default:
		// The error NAMES the choices, because a caller that cannot see
		// them cannot resolve the ambiguity: an operator told only
		// "there is more than one" has been refused with no way
		// forward. Every caller of this takes an external id to pick
		// with, and these are the ids that work.
		ids := make([]string, 0, len(found))
		for _, c := range found {
			ids = append(ids, c.ExternalID)
		}
		return Connection{}, fmt.Errorf("%w: %s answers on %d %s bots (%s) -- name which one",
			ErrAmbiguousConnection, butler, len(found), transport, strings.Join(ids, ", "))
	}
}

// ListChatConnections returns every live Connection, newest last — the
// operator's view of which Agent is reachable where.
func (db *DB) ListChatConnections() ([]Connection, error) {
	rows, err := db.sql.Query(chatConnectionColumns + ` WHERE a.revoked_at = '' ORDER BY b.name, a.transport, a.seq`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing connections: %w", err)
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, err := scanChatConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
