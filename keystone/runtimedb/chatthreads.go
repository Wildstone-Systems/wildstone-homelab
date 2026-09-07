package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ChatThread is the Butler session a chat conversation is currently
// talking to (ADR-W027). One row per (transport, conversation, surface);
// see migration 0015 for why the surface is part of the key.
type ChatThread struct {
	Transport    string
	Conversation string
	Surface      ButlerSurface
	SessionID    string
	UpdatedAt    time.Time
}

// ChatThreadSession returns the session a conversation should continue,
// and whether there is one. A thread older than idle is not returned:
// picking up yesterday's conversation because someone said "good
// morning" is worse than starting fresh, and the model would be handed
// a context the person has long forgotten.
//
// The stale row is left in place rather than deleted — the next message
// overwrites it, and a read has no business writing.
func (db *DB) ChatThreadSession(transport, conversation string, surface ButlerSurface, idle time.Duration) (string, bool, error) {
	var id, updated string
	err := db.sql.QueryRow(
		`SELECT session_id, updated_at FROM chat_threads
		 WHERE transport = ? AND conversation = ? AND surface = ?`,
		transport, conversation, string(surface),
	).Scan(&id, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("runtimedb: reading chat thread: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return "", false, fmt.Errorf("runtimedb: parsing chat thread updated_at: %w", err)
	}
	if idle > 0 && time.Since(at) > idle {
		return "", false, nil
	}
	return id, id != "", nil
}

// SetChatThreadSession points a conversation at a session, replacing
// whatever it pointed at before.
func (db *DB) SetChatThreadSession(transport, conversation string, surface ButlerSurface, sessionID string) error {
	if transport == "" || conversation == "" || sessionID == "" {
		return fmt.Errorf("runtimedb: chat thread needs a transport, conversation and session")
	}
	if !ValidButlerSurface(surface) {
		return fmt.Errorf("runtimedb: chat thread needs a valid surface, got %q", surface)
	}
	_, err := db.sql.Exec(
		`INSERT INTO chat_threads (transport, conversation, surface, session_id, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(transport, conversation, surface)
		 DO UPDATE SET session_id = excluded.session_id, updated_at = excluded.updated_at`,
		transport, conversation, string(surface), sessionID, now(),
	)
	if err != nil {
		return fmt.Errorf("runtimedb: recording chat thread: %w", err)
	}
	return nil
}

// ClearChatThread forgets which session a conversation was on, so the
// next message starts a new one. The conversation itself is untouched:
// its turns stay recorded and readable in the Console.
func (db *DB) ClearChatThread(transport, conversation string, surface ButlerSurface) error {
	_, err := db.sql.Exec(
		`DELETE FROM chat_threads WHERE transport = ? AND conversation = ? AND surface = ?`,
		transport, conversation, string(surface),
	)
	if err != nil {
		return fmt.Errorf("runtimedb: clearing chat thread: %w", err)
	}
	return nil
}
