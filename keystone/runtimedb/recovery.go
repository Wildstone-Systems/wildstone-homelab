package runtimedb

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// BackupTo creates a transactionally consistent SQLite snapshot while the
// live WAL-mode database remains online. VACUUM INTO is SQLite's online
// backup mechanism; unlike copying keystone.db, it includes committed WAL
// content and cannot produce a torn database.
func (db *DB) BackupTo(destination string) error {
	if destination == "" || destination == ":memory:" {
		return fmt.Errorf("runtimedb: backup destination must be a file")
	}
	absDestination, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("runtimedb: resolving backup destination: %w", err)
	}
	if _, err := os.Stat(absDestination); err == nil {
		return fmt.Errorf("runtimedb: backup destination already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("runtimedb: checking backup destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absDestination), 0o700); err != nil {
		return fmt.Errorf("runtimedb: creating backup directory: %w", err)
	}
	if _, err := db.sql.Exec(`VACUUM INTO ?`, absDestination); err != nil {
		return fmt.Errorf("runtimedb: online backup: %w", err)
	}
	return nil
}

// RestoreBackup installs a consistent, already-offline snapshot into a fresh
// path and enters restore quarantine before returning it to the caller. The
// live database was captured through BackupTo; copying this immutable artifact
// is safe and preserves the source recovery set byte-for-byte.
func RestoreBackup(source, destination, reason string, snapshotAt time.Time) (*DB, error) {
	if reason == "" {
		return nil, fmt.Errorf("runtimedb: restore reason is required")
	}
	if _, err := os.Stat(destination); err == nil {
		return nil, fmt.Errorf("runtimedb: restore destination already exists")
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("runtimedb: checking restore destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return nil, fmt.Errorf("runtimedb: creating restore directory: %w", err)
	}
	in, err := os.Open(source)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: opening recovery snapshot: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: creating restored database: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		if copyErr != nil {
			return nil, fmt.Errorf("runtimedb: copying recovery snapshot: %w", copyErr)
		}
		return nil, fmt.Errorf("runtimedb: closing restored database: %w", closeErr)
	}
	restored, err := Open(destination)
	if err != nil {
		_ = os.Remove(destination)
		return nil, err
	}
	if err := restored.EnterRestoreQuarantine(reason, snapshotAt); err != nil {
		restored.Close()
		_ = os.Remove(destination)
		return nil, err
	}
	return restored, nil
}
