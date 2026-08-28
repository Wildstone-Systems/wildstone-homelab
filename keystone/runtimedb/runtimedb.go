// Package runtimedb is Keystone's SQLite runtime database (ADR-W007 A4):
// observed state, events, plan records, principals/tokens, and audit — the
// mutable operational half of the split whose intentional half is the
// git-backed Statefile. Pure-Go driver (modernc.org/sqlite) so the no-cgo
// build and cross-compile CI checks keep holding.
package runtimedb

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB wraps the SQLite handle. All accessors are safe for concurrent use;
// SQLite serializes writers and WAL keeps readers unblocked.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if needed) the runtime database at path and applies
// any pending migrations. Use ":memory:" only in tests that don't exercise
// concurrency; real callers pass <DataDir>/keystone.db.
func Open(path string) (*DB, error) {
	// _pragma busy_timeout: a second writer waits instead of failing with
	// SQLITE_BUSY; foreign_keys is off by default in SQLite and our tokens
	// table relies on it.
	//
	// _txlock=immediate takes the write lock when a transaction BEGINs
	// rather than when it first writes. Our transactions are all read-
	// modify-write (TransitionPlan reads the current state to check the
	// transition table, then updates), and under the default DEFERRED
	// mode the read only takes a snapshot — so the write has to upgrade,
	// and any writer that committed in between makes that upgrade fail
	// with SQLITE_BUSY/BUSY_SNAPSHOT *immediately*. busy_timeout cannot
	// help there: retrying an upgrade in place can never succeed, so
	// SQLite deliberately skips the busy handler. Taking the lock up
	// front puts the contention back under busy_timeout, where it waits.
	// Read-only transactions can still opt out with
	// BeginTx(ctx, &sql.TxOptions{ReadOnly: true}) — the driver leaves
	// those DEFERRED.
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: open %s: %w", path, err)
	}
	db := &DB{sql: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error { return db.sql.Close() }

// migrate applies embedded migrations in filename order, each inside its
// own transaction, tracked in schema_migrations. Forward-only (ADR-W007
// A4): a bad migration is fixed by a new one, never by editing history.
func (db *DB) migrate() error {
	if _, err := db.sql.Exec(
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("runtimedb: creating schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("runtimedb: listing migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		var done int
		if err := db.sql.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&done); err != nil {
			return fmt.Errorf("runtimedb: checking migration %s: %w", name, err)
		}
		if done > 0 {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("runtimedb: reading migration %s: %w", name, err)
		}
		tx, err := db.sql.Begin()
		if err != nil {
			return fmt.Errorf("runtimedb: begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("runtimedb: applying migration %s: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, now()); err != nil {
			tx.Rollback()
			return fmt.Errorf("runtimedb: recording migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("runtimedb: commit migration %s: %w", name, err)
		}
	}
	return nil
}

// AppliedMigrations returns the names of applied migrations in order —
// used by tests and `keystone` diagnostics.
func (db *DB) AppliedMigrations() ([]string, error) {
	rows, err := db.sql.Query(`SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing applied migrations: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// now returns the canonical timestamp representation: RFC3339Nano in UTC.
// TEXT timestamps keep the database greppable with the sqlite3 CLI — the
// escape-hatch principle applies to our own storage too.
func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
