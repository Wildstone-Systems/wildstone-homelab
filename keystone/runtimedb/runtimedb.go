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
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB wraps the SQLite handle. All accessors are safe for concurrent use;
// SQLite serializes writers and WAL keeps readers unblocked.
type DB struct {
	coordinatorMu         sync.Mutex
	coordinatorLock       *os.File
	coordinatorLockHolder string
	sql                   *sql.DB
	executionLease        atomic.Pointer[CoordinatorLease]
	// path is kept so a table rebuild can reopen the same file on its
	// own handle with foreign keys off. See applyRebuildMigration.
	path string

	// registrationTestHook is nil in production. Tests use
	// it to hold a validated, uncommitted registration while an authority
	// withdrawal queues on SQLite's immediate write lock.
	registrationTestHook func()
	// challengeClock is nil in production. Challenge tests use it to prove
	// expiry after transaction admission without changing the process clock.
	challengeClock func() time.Time
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
	// those DEFERRED. FULL synchronous WAL commits make publication journal
	// acknowledgements durable independently of filesystem object fsyncs (W047).
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: open %s: %w", path, err)
	}
	db := &DB{sql: sqlDB, path: path}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// openUnmigrated opens the handle without applying migrations: test
// support for exercising a migration against data written by the schema
// before it.
func openUnmigrated(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: open %s: %w", path, err)
	}
	return &DB{sql: sqlDB, path: path}, nil
}

func (db *DB) Close() error {
	db.coordinatorMu.Lock()
	defer db.coordinatorMu.Unlock()
	err := db.sql.Close()
	db.executionLease.Store(nil)
	if db.coordinatorLock != nil {
		lockErr := db.coordinatorLock.Close()
		db.coordinatorLock = nil
		if err == nil {
			err = lockErr
		}
	}
	return err
}

// migrate applies embedded migrations in filename order, each inside its
// own transaction, tracked in schema_migrations. Forward-only (ADR-W007
// A4): a bad migration is fixed by a new one, never by editing history.
func (db *DB) migrate() error { return db.migrateThrough("") }

// migrateThrough applies pending migrations up to and including the named
// one (a "migrations/NNNN_x.sql" path); "" applies them all.
func (db *DB) migrateThrough(last string) error { return db.migrateFrom(migrationFS, last) }

// migrateFrom is migrateThrough against an arbitrary migration source.
// Only the runner's own tests pass anything but migrationFS: proving a
// property of the RUNNER — that the foreign-keys-off directive is
// opt-in, that a violation fails, that the migration after one is back
// under enforcement — needs migrations written for that purpose, and
// the real set cannot grow fixtures without becoming schema history.
func (db *DB) migrateFrom(src fs.FS, last string) error {
	if _, err := db.sql.Exec(
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("runtimedb: creating schema_migrations: %w", err)
	}
	if err := db.adoptBaseline(src); err != nil {
		return err
	}
	names, err := fs.Glob(src, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("runtimedb: listing migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		if last != "" && name > last {
			break
		}
		var done int
		if err := db.sql.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&done); err != nil {
			return fmt.Errorf("runtimedb: checking migration %s: %w", name, err)
		}
		if done > 0 {
			continue
		}
		body, err := fs.ReadFile(src, name)
		if err != nil {
			return fmt.Errorf("runtimedb: reading migration %s: %w", name, err)
		}
		if strings.HasPrefix(string(body), rebuildDirective) {
			if err := db.applyRebuildMigration(name, string(body)); err != nil {
				return err
			}
			continue
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

// rebuildDirective marks a migration that rebuilds a table other tables
// hold foreign keys into. Such a migration must run with foreign keys
// OFF, and it must be the first line of the file.
//
// SQLite cannot drop or widen a CHECK constraint in place, so changing
// one means create-copy-drop-rename. With foreign keys ON that is
// impossible for a table anything references: DROP TABLE performs an
// implicit DELETE FROM, which orphans the children and fails with
// SQLITE_CONSTRAINT_FOREIGNKEY. Deferring does not help — the deferred
// counter is incremented by the drop and is not decremented by
// recreating a table with the same rows, so the violation surfaces at
// COMMIT instead.
//
// The alternative the earlier rebuilds took (0005, 0008, 0012, 0026) is
// to rebuild the children in the same migration, which works when the
// child is a leaf. It stops working when a child is itself a parent:
// butler_conversations is referenced by butler_conversation_sessions
// and butler_handoffs, so rebuilding butler_members that way would drag
// in five tables to change one.
//
// So this follows SQLite's own documented procedure for "other kinds of
// table schema changes" instead: foreign keys off, rebuild, verify with
// foreign_key_check, commit. Nothing is repointed and the children's
// REFERENCES clauses keep naming the same table.
const rebuildDirective = "-- +wildstone: foreign_keys=off"

// applyRebuildMigration runs one rebuild migration on a dedicated handle
// opened with foreign keys off, then closes it.
//
// It is a separate handle rather than a pinned connection from the pool
// because PRAGMA foreign_keys is a no-op inside a transaction and a
// per-connection setting outside one: a pooled connection whose pragma
// we failed to restore would silently stop enforcing foreign keys for
// whatever ran on it next. A handle we own and close cannot leak that
// way.
func (db *DB) applyRebuildMigration(name, body string) error {
	if db.path == "" || db.path == ":memory:" {
		return fmt.Errorf("runtimedb: migration %s rebuilds a referenced table and needs a file-backed database, not %q", name, db.path)
	}
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(0)", db.path)
	rebuild, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("runtimedb: opening rebuild handle for %s: %w", name, err)
	}
	defer rebuild.Close()
	// One connection: the whole point is that every statement here sees
	// the same foreign_keys setting.
	rebuild.SetMaxOpenConns(1)

	tx, err := rebuild.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: begin migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(body); err != nil {
		return fmt.Errorf("runtimedb: applying migration %s: %w", name, err)
	}
	// The gate. With enforcement off for the duration, nothing else
	// would notice a rebuild that orphaned a row, so the migration does
	// not get to commit until the database says it is still whole.
	violations, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("runtimedb: checking foreign keys after %s: %w", name, err)
	}
	var orphans int
	for violations.Next() {
		orphans++
	}
	violations.Close()
	if err := violations.Err(); err != nil {
		return fmt.Errorf("runtimedb: checking foreign keys after %s: %w", name, err)
	}
	if orphans > 0 {
		return fmt.Errorf("runtimedb: migration %s left %d orphaned foreign key rows; refusing to commit", name, orphans)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, now()); err != nil {
		return fmt.Errorf("runtimedb: recording migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: commit migration %s: %w", name, err)
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

// OpenUnmigratedForTest opens the database without applying any
// migration.
//
// It exists for tests in other packages that must exercise a schema this
// build no longer produces. personref is the one that needs it: its
// legacy classification describes columns 0043 removed, and the only way
// to survey those is a database that has not reached 0043, which is
// exactly the deployment the pre-flight report is for. That chain is
// squashed into the baseline now, so the test loads a frozen copy of the
// old schema into the empty file this returns. Nothing in production
// calls it.
func OpenUnmigratedForTest(path string) (*DB, error) {
	return openUnmigrated(path)
}

// ExecForTest runs a raw statement. It exists so tests in other
// packages can prove a schema constraint holds against a writer that
// bypasses this package's own methods — which is the only interesting
// way to test a trigger. Nothing in production calls it.
func (db *DB) ExecForTest(query string, args ...any) (sql.Result, error) {
	return db.sql.Exec(query, args...)
}

// CountForTest runs a raw single-integer query (a COUNT). It exists so
// tests in other packages can assert that a table holds nothing it must
// not — the capture checks' "no task trace was opened". Nothing in
// production calls it.
func (db *DB) CountForTest(query string, args ...any) (int, error) {
	var n int
	err := db.sql.QueryRow(query, args...).Scan(&n)
	return n, err
}
