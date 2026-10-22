package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
)

// The task handle's digest (migration 0082, ADR-W039 Phase 2a).
//
// Only the two column accessors live here. What a handle IS — how its
// secret is generated, how a presented one is checked, what a resolved
// one entitles — is butlerstore's, one layer up, where the task, the
// Butler it runs as and the ledger meet. This file cannot mint or
// verify anything: it stores a digest and reads it back.

// SetButlerTaskHandleHash records the digest of the task's live handle.
// An empty hash withdraws the handle: nothing resolves until a new one
// is issued.
func (db *DB) SetButlerTaskHandleHash(taskID, hash string) error {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(
		`UPDATE butler_tasks SET handle_hash = ?, updated_at = ? WHERE seq = ?`, hash, now(), seq)
	if err != nil {
		return fmt.Errorf("runtimedb: recording a task handle: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ButlerTaskHandleHash returns the digest of the task's live handle, or
// "" when none is issued.
func (db *DB) ButlerTaskHandleHash(taskID string) (string, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return "", err
	}
	var hash string
	err = db.sql.QueryRow(`SELECT handle_hash FROM butler_tasks WHERE seq = ?`, seq).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: reading a task handle: %w", err)
	}
	return hash, nil
}
