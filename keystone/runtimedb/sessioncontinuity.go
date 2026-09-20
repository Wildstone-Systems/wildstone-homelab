package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SessionCandidate is metadata that may be considered for continuation.
// It deliberately contains no transcript: candidate selection happens before
// any candidate's context is loaded (design section 9).
type SessionCandidate struct {
	SessionID  string
	Subject    string
	LastTurnAt time.Time
	Provenance []SessionConnection
}

// SessionConnection retains each inbound origin, even after continuation on a
// different Connection. Later turns must not relax earlier disclosure limits.
type SessionConnection struct {
	ConnectionRef  int64
	ConnectionName string
	Connection     *Connection
}

// RecentSessionCandidates returns sessions for exactly one canonical Person
// and Agent. Revoked, legacy-unkeyed and inactive sessions are not candidates.
func (db *DB) RecentSessionCandidates(identity, agent string, since, after time.Time) ([]SessionCandidate, error) {
	person, err := resolvePersonID(db.sql, identity)
	if err != nil {
		return nil, err
	}
	var agentID int64
	if err := db.sql.QueryRow(`SELECT seq FROM butlers WHERE name = ?`, agent).Scan(&agentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("runtimedb: resolving session agent: %w", err)
	}
	cutoff := since
	if after.After(cutoff) {
		cutoff = after
	}
	rows, err := db.sql.Query(`
		SELECT s.seq,
		       COALESCE((SELECT text FROM butler_session_turns
		                  WHERE session = s.seq AND role = 'user'
		                  ORDER BY ordinal LIMIT 1), ''),
		       COALESCE((SELECT MAX(created_at) FROM butler_session_turns
		                  WHERE session = s.seq), ''),
		       COALESCE(p.connection_ref, -1), COALESCE(p.connection_name, '')
		  FROM butler_sessions s
		  LEFT JOIN butler_session_connections p ON p.session = s.seq AND p.direction = 'inbound'
		 WHERE s.person_id = ? AND s.agent_id = ? AND s.revoked_at = ''
		   AND EXISTS (SELECT 1 FROM butler_session_turns t
		                WHERE t.session = s.seq AND t.created_at > ?)
		 GROUP BY s.seq, p.connection_ref, p.connection_name
		 ORDER BY (SELECT MAX(created_at) FROM butler_session_turns
		            WHERE session = s.seq) DESC, s.seq DESC`,
		person, agentID, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing session candidates: %w", err)
	}
	defer rows.Close()
	var out []SessionCandidate
	indices := make(map[int64]int)
	for rows.Next() {
		var c SessionCandidate
		var source SessionConnection
		var seq int64
		var last string
		if err := rows.Scan(&seq, &c.Subject, &last, &source.ConnectionRef, &source.ConnectionName); err != nil {
			return nil, err
		}
		c.SessionID = butlerSessionID(seq)
		if c.LastTurnAt, err = time.Parse(time.RFC3339Nano, last); err != nil {
			return nil, fmt.Errorf("runtimedb: parsing candidate activity: %w", err)
		}
		if index, exists := indices[seq]; exists {
			out[index].Provenance = append(out[index].Provenance, source)
		} else {
			indices[seq] = len(out)
			c.Provenance = []SessionConnection{source}
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		for j := range out[i].Provenance {
			source := &out[i].Provenance[j]
			if source.ConnectionRef > 0 {
				conn, err := db.ChatConnectionBySeq(source.ConnectionRef)
				if err == nil {
					source.Connection = &conn
				}
			}
		}
	}
	return out, nil
}

// NoteButlerSessionConnection appends the Connection on which a completed
// turn entered or left. It never rewrites prior provenance.
func (db *DB) NoteButlerSessionConnection(sessionID, direction, name string, ref int64) error {
	var seq int64
	if _, err := fmt.Sscanf(sessionID, "bs-%d", &seq); err != nil {
		return ErrNotFound
	}
	if direction != "inbound" && direction != "outbound" {
		return fmt.Errorf("runtimedb: session connection direction must be inbound or outbound")
	}
	if name == "" {
		return fmt.Errorf("runtimedb: session connection needs a name")
	}
	_, err := db.sql.Exec(`INSERT INTO butler_session_connections
		(session, direction, connection_ref, connection_name, recorded_at)
		VALUES (?, ?, ?, ?, ?)`, seq, direction, ref, name, now())
	if err != nil {
		return fmt.Errorf("runtimedb: recording session connection: %w", err)
	}
	return nil
}

func insertSessionConnection(tx *sql.Tx, session int64, direction, name string, ref int64) error {
	if name == "" {
		return nil // legacy/imported sessions have no Connection provenance
	}
	if direction != "inbound" && direction != "outbound" {
		return fmt.Errorf("runtimedb: session connection direction must be inbound or outbound")
	}
	if _, err := tx.Exec(`INSERT INTO butler_session_connections
		(session, direction, connection_ref, connection_name, recorded_at)
		VALUES (?, ?, ?, ?, ?)`, session, direction, ref, name, now()); err != nil {
		return fmt.Errorf("runtimedb: recording session connection: %w", err)
	}
	return nil
}

// BreakChatThread implements /new. Candidate discovery ignores everything at
// or before the marker, so clearing a pointer cannot immediately rediscover it.
func (db *DB) BreakChatThread(at Connection, conversation, identity string) error {
	person, err := resolvePersonID(db.sql, identity)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`DELETE FROM chat_threads WHERE connection = ? AND conversation = ? AND person_id = ?`, at.Seq, conversation, person); err != nil {
		return fmt.Errorf("runtimedb: clearing chat thread: %w", err)
	}
	if _, err = tx.Exec(`INSERT INTO chat_thread_breaks (connection, conversation, person_id, broken_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(connection, conversation, person_id)
		DO UPDATE SET broken_at = excluded.broken_at`, at.Seq, conversation, person, now()); err != nil {
		return fmt.Errorf("runtimedb: recording chat thread break: %w", err)
	}
	return tx.Commit()
}

func (db *DB) ChatThreadBrokenAt(at Connection, conversation, identity string) (time.Time, error) {
	var stamp string
	err := db.sql.QueryRow(`SELECT b.broken_at FROM chat_thread_breaks b
		JOIN butler_members m ON m.person_id = b.person_id
		WHERE b.connection = ? AND b.conversation = ? AND m.member = ? COLLATE NOCASE`,
		at.Seq, conversation, identity).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, stamp)
}

func (db *DB) CreateSessionResumeOffer(tokenHash, identity, agent string, connection int64, sessionID string, expires time.Time) error {
	person, err := resolvePersonID(db.sql, identity)
	if err != nil {
		return err
	}
	var agentID, session int64
	if err := db.sql.QueryRow(`SELECT seq FROM butlers WHERE name = ?`, agent).Scan(&agentID); err != nil {
		return ErrNotFound
	}
	if _, err := fmt.Sscanf(sessionID, "bs-%d", &session); err != nil {
		return ErrNotFound
	}
	// Offers are short-lived capabilities. Prune spent and expired rows on
	// the write path so repeated /sessions calls cannot grow this table
	// without bound.
	if _, err := db.sql.Exec(`DELETE FROM session_resume_offers WHERE consumed_at <> '' OR expires_at <= ?`, now()); err != nil {
		return fmt.Errorf("runtimedb: pruning session resume offers: %w", err)
	}
	res, err := db.sql.Exec(`INSERT INTO session_resume_offers
		(token_hash, person_id, agent_id, connection, session, created_at, expires_at)
		SELECT ?, ?, ?, ?, s.seq, ?, ? FROM butler_sessions s
		WHERE s.seq = ? AND s.person_id = ? AND s.agent_id = ? AND s.revoked_at = ''`,
		tokenHash, person, agentID, connection, now(), expires.UTC().Format(time.RFC3339Nano),
		session, person, agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// ConsumeSessionResumeOffer atomically spends an opaque choice. A token is
// valid only in the exact Person + Agent + Connection context that received it.
func (db *DB) ConsumeSessionResumeOffer(tokenHash, identity, agent string, connection int64) (string, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var session int64
	err = tx.QueryRow(`SELECT o.session FROM session_resume_offers o
		JOIN butler_members m ON m.person_id = o.person_id
		JOIN butlers b ON b.seq = o.agent_id
		JOIN butler_sessions s ON s.seq = o.session
		WHERE o.token_hash = ? AND m.member = ? COLLATE NOCASE AND b.name = ?
		  AND o.connection = ? AND o.consumed_at = '' AND o.expires_at > ?
		  AND s.revoked_at = '' AND s.person_id = o.person_id AND s.agent_id = o.agent_id`,
		tokenHash, identity, agent, connection, now()).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	res, err := tx.Exec(`UPDATE session_resume_offers SET consumed_at = ? WHERE token_hash = ? AND consumed_at = ''`, now(), tokenHash)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return butlerSessionID(session), nil
}
