package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LastTurnAt is when a session last had anything said in it, on any
// surface. Zero when it has no turns.
func (s ButlerSession) LastTurnAt() time.Time {
	if len(s.Turns) == 0 {
		return time.Time{}
	}
	return s.Turns[len(s.Turns)-1].CreatedAt
}

// ChatThread is the Butler session a chat conversation is currently
// talking to (ADR-W027). One row per (transport, conversation, surface);
// see migration 0015 for why the surface is part of the key.
type ChatThread struct {
	Transport    string
	Conversation string
	Surface      ButlerSurface
	Identity     string
	SessionID    string
	UpdatedAt    time.Time
}

// ChatThreadSession returns the session a conversation is pointing at,
// and whether there is one.
//
// It says nothing about whether that session is still FRESH. Staleness
// is a property of the conversation, not of this row: the same
// conversation can be carried on in the Console, and measuring idleness
// by when Telegram last spoke would call a thread dead that was alive
// two minutes ago somewhere else. The caller judges age from the
// session's newest turn, which is the one place every surface records
// activity.
func (db *DB) ChatThreadSession(at Connection, conversation, identity string) (string, bool, error) {
	var id string
	err := db.sql.QueryRow(
		`SELECT t.session_id FROM chat_threads t
		 JOIN butler_members m ON m.person_id = t.person_id
		 WHERE t.connection = ? AND t.conversation = ? AND m.member = ?`,
		at.Seq, conversation, identity,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("runtimedb: reading chat thread: %w", err)
	}
	return id, id != "", nil
}

// SetChatThreadSession points a conversation at a session, replacing
// whatever it pointed at before.
func (db *DB) SetChatThreadSession(at Connection, conversation, identity, sessionID string) error {
	if at.Seq == 0 || conversation == "" || identity == "" || sessionID == "" {
		return fmt.Errorf("runtimedb: chat thread needs a connection, conversation, identity and session")
	}
	// The thread names the Person whose session it is (ADR-W033 B10).
	// A thread for somebody who is not on record is not a thread whose
	// owner we could name later, so it is refused rather than written.
	person, err := resolvePersonID(db.sql, identity)
	if err != nil {
		return fmt.Errorf("runtimedb: recording chat thread: %w", err)
	}
	_, err = db.sql.Exec(
		`INSERT INTO chat_threads (connection, conversation, person_id, session_id, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(connection, conversation, person_id)
		 DO UPDATE SET session_id = excluded.session_id, updated_at = excluded.updated_at`,
		at.Seq, conversation, person, sessionID, now(),
	)
	if err != nil {
		return fmt.Errorf("runtimedb: recording chat thread: %w", err)
	}
	return nil
}

// ClearChatThread forgets which session a conversation was on, so the
// next message starts a new one. The conversation itself is untouched:
// its turns stay recorded and readable in the Console.
func (db *DB) ClearChatThread(at Connection, conversation, identity string) error {
	_, err := db.sql.Exec(
		`DELETE FROM chat_threads
		 WHERE connection = ? AND conversation = ?
		   AND person_id = (SELECT person_id FROM butler_members WHERE member = ?)`,
		at.Seq, conversation, identity,
	)
	if err != nil {
		return fmt.Errorf("runtimedb: clearing chat thread: %w", err)
	}
	return nil
}
