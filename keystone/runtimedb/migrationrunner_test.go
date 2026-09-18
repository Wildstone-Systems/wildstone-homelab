package runtimedb

// The migration runner's foreign-keys-off directive (see
// rebuildDirective in runtimedb.go).
//
// Turning foreign key enforcement off is the most dangerous thing the
// runner can do, so each property that makes it safe is pinned here
// rather than left to the one migration that currently uses it:
//
//   opt-in       — a migration gets FK-off only by asking, on its first
//                  line, and a file that merely mentions the directive
//                  does not qualify
//   isolated     — it runs on its own handle, so no pooled connection is
//                  ever left with enforcement off
//   verified     — foreign_key_check runs before commit, always
//   fails closed — any violation fails the migration and records nothing
//   restored     — the next migration is back under enforcement
//
// The fixtures are synthetic on purpose. Proving "the migration AFTER an
// FK-off one is enforced again" needs a migration after one, and the
// real set cannot grow fixtures without them becoming schema history
// that every deployment then replays forever.

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// base sets up a parent with a child that holds a foreign key into it —
// the shape that makes a naive rebuild impossible.
const runnerBase = `
CREATE TABLE parent (id TEXT PRIMARY KEY, note TEXT NOT NULL DEFAULT '');
CREATE TABLE child (seq INTEGER PRIMARY KEY, parent TEXT NOT NULL REFERENCES parent(id));
INSERT INTO parent (id, note) VALUES ('a', 'kept'), ('b', 'kept');
INSERT INTO child (parent) VALUES ('a'), ('b');
`

// rebuildBody is a create-copy-drop-rename that only succeeds with
// foreign keys off, and preserves every parent row.
const runnerRebuild = `
CREATE TABLE parent_new (id TEXT PRIMARY KEY, note TEXT NOT NULL DEFAULT '', CHECK (note <> 'banned'));
INSERT INTO parent_new (id, note) SELECT id, note FROM parent;
DROP TABLE parent;
ALTER TABLE parent_new RENAME TO parent;
`

// The same rebuild, but dropping a parent row that a child still
// references. The SQL itself is valid; only foreign_key_check notices.
const runnerRebuildOrphaning = `
CREATE TABLE parent_new (id TEXT PRIMARY KEY, note TEXT NOT NULL DEFAULT '');
INSERT INTO parent_new (id, note) SELECT id, note FROM parent WHERE id <> 'a';
DROP TABLE parent;
ALTER TABLE parent_new RENAME TO parent;
`

func runnerDB(t *testing.T) *DB {
	t.Helper()
	db, err := openUnmigrated(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func migrationSet(files map[string]string) fstest.MapFS {
	set := fstest.MapFS{}
	for name, body := range files {
		set["migrations/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return set
}

func applied(t *testing.T, db *DB, name string) bool {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, "migrations/"+name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// foreignKeysOn reports the pragma as seen by one pooled connection.
func foreignKeysOn(t *testing.T, db *DB) bool {
	t.Helper()
	var on int
	if err := db.sql.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatal(err)
	}
	return on == 1
}

// A migration gets foreign keys off only by asking for it, on its first
// line. Without the directive the identical rebuild is refused, which
// is what makes the directive meaningful rather than decorative.
func TestRebuildDirectiveIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantApply bool
	}{
		{
			name:      "with the directive",
			body:      rebuildDirective + "\n" + runnerRebuild,
			wantApply: true,
		},
		{
			name:      "without it",
			body:      "-- an ordinary migration\n" + runnerRebuild,
			wantApply: false,
		},
		{
			// Not on the first line: a migration that merely discusses
			// the directive in a comment has not asked for it.
			name:      "mentioned but not declared",
			body:      "-- this is NOT " + rebuildDirective + "\n" + runnerRebuild,
			wantApply: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := runnerDB(t)
			set := migrationSet(map[string]string{
				"0001_base.sql":    runnerBase,
				"0002_rebuild.sql": tc.body,
			})
			err := db.migrateFrom(set, "")
			if tc.wantApply {
				if err != nil {
					t.Fatalf("the declared rebuild was refused: %v", err)
				}
				if !applied(t, db, "0002_rebuild.sql") {
					t.Fatal("the rebuild was not recorded")
				}
				return
			}
			if err == nil {
				t.Fatal("a rebuild ran with foreign keys on")
			}
			if !strings.Contains(err.Error(), "FOREIGN KEY") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
			if applied(t, db, "0002_rebuild.sql") {
				t.Fatal("a failed migration was recorded as applied")
			}
		})
	}
}

// The rebuild runs on a handle the runner owns and closes. No
// connection in the caller's pool may be left with enforcement off —
// that would silently disable foreign keys for whatever ran on it next.
func TestRebuildMigrationUsesAnIsolatedConnection(t *testing.T) {
	db := runnerDB(t)
	set := migrationSet(map[string]string{
		"0001_base.sql":    runnerBase,
		"0002_rebuild.sql": rebuildDirective + "\n" + runnerRebuild,
	})
	if err := db.migrateFrom(set, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Sample several pooled connections at once. Holding transactions
	// open forces the pool to hand out distinct connections rather than
	// the same one repeatedly, so this would catch a pragma left off on
	// any of them.
	const conns = 4
	db.sql.SetMaxOpenConns(conns)
	var wg sync.WaitGroup
	bad := make([]string, conns)
	release := make(chan struct{})
	for i := range conns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := db.sql.Begin()
			if err != nil {
				bad[i] = fmt.Sprintf("begin: %v", err)
				return
			}
			defer tx.Rollback()
			var on int
			if err := tx.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
				bad[i] = fmt.Sprintf("pragma: %v", err)
				return
			}
			if on != 1 {
				bad[i] = "foreign keys are off on a pooled connection"
			}
			<-release
		}(i)
	}
	close(release)
	wg.Wait()
	for i, msg := range bad {
		if msg != "" {
			t.Errorf("connection %d: %s", i, msg)
		}
	}

	// And enforcement genuinely bites on the rebuilt table.
	if _, err := db.sql.Exec(`INSERT INTO child (parent) VALUES ('nobody')`); err == nil {
		t.Error("the rebuilt table no longer enforces its foreign key")
	}
}

// foreign_key_check is not advisory. A rebuild that orphans a row is
// refused, records nothing, and leaves the database as it was — the
// whole point of running the check before COMMIT rather than after.
func TestRebuildMigrationFailsOnForeignKeyViolation(t *testing.T) {
	db := runnerDB(t)
	set := migrationSet(map[string]string{
		"0001_base.sql":    runnerBase,
		"0002_rebuild.sql": rebuildDirective + "\n" + runnerRebuildOrphaning,
	})
	err := db.migrateFrom(set, "")
	if err == nil {
		t.Fatal("a rebuild that orphaned a child row was committed")
	}
	if !strings.Contains(err.Error(), "orphaned foreign key rows") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
	if applied(t, db, "0002_rebuild.sql") {
		t.Fatal("a failed rebuild was recorded as applied")
	}
	// Rolled back whole: the parent row the rebuild would have dropped
	// is still there.
	var note string
	if err := db.sql.QueryRow(`SELECT note FROM parent WHERE id = 'a'`).Scan(&note); err != nil {
		t.Fatalf("the failed rebuild was not rolled back: %v", err)
	}
	if note != "kept" {
		t.Fatalf("parent row = %q, want kept", note)
	}
	// The old table is the one still in place, so the aborted rebuild
	// left no half-built table behind either.
	var leftovers int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'parent_new'`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Error("the aborted rebuild left parent_new behind")
	}
}

// The regression that matters most: the migration immediately after an
// FK-off one must run with foreign keys ON again. If the runner ever
// stops isolating the rebuild handle, this is what catches it.
func TestMigrationAfterRebuildRunsWithForeignKeysEnabled(t *testing.T) {
	db := runnerDB(t)
	// 0003 does something only a foreign key would stop. If enforcement
	// leaked off, it would succeed and this test would fail.
	set := migrationSet(map[string]string{
		"0001_base.sql":    runnerBase,
		"0002_rebuild.sql": rebuildDirective + "\n" + runnerRebuild,
		"0003_after.sql":   `INSERT INTO child (parent) VALUES ('nobody');`,
	})
	err := db.migrateFrom(set, "")
	if err == nil {
		t.Fatal("the migration after a rebuild inserted an orphan: foreign keys were still off")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
	if !strings.Contains(err.Error(), "0003_after.sql") {
		t.Fatalf("the wrong migration failed: %v", err)
	}
	// The rebuild before it did commit, so this is genuinely "the next
	// migration" and not a cascade from a failed one.
	if !applied(t, db, "0002_rebuild.sql") {
		t.Fatal("the rebuild did not commit")
	}
	if applied(t, db, "0003_after.sql") {
		t.Fatal("the failed migration was recorded as applied")
	}
	if !foreignKeysOn(t, db) {
		t.Error("foreign keys are off on the caller's handle")
	}
}

// The happy path of the same sequence: a migration after a rebuild runs
// normally, and the rebuilt schema is what it writes against.
func TestMigrationAfterRebuildStillApplies(t *testing.T) {
	db := runnerDB(t)
	set := migrationSet(map[string]string{
		"0001_base.sql":    runnerBase,
		"0002_rebuild.sql": rebuildDirective + "\n" + runnerRebuild,
		"0003_after.sql":   `INSERT INTO parent (id, note) VALUES ('c', 'later'); INSERT INTO child (parent) VALUES ('c');`,
	})
	if err := db.migrateFrom(set, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, name := range []string{"0001_base.sql", "0002_rebuild.sql", "0003_after.sql"} {
		if !applied(t, db, name) {
			t.Errorf("%s was not applied", name)
		}
	}
	// 0002's new CHECK is live, so the migration after it really is
	// writing against the rebuilt table.
	if _, err := db.sql.Exec(`INSERT INTO parent (id, note) VALUES ('d', 'banned')`); err == nil {
		t.Error("the rebuilt table's CHECK constraint is not enforced")
	}
}

// A rebuild needs a file it can reopen. Rather than silently running
// against a different in-memory database, it says so.
func TestRebuildMigrationRefusesAnUnnamedDatabase(t *testing.T) {
	db := runnerDB(t)
	db.path = ":memory:"
	set := migrationSet(map[string]string{
		"0001_base.sql":    runnerBase,
		"0002_rebuild.sql": rebuildDirective + "\n" + runnerRebuild,
	})
	err := db.migrateFrom(set, "")
	if err == nil {
		t.Fatal("a rebuild ran against an in-memory database")
	}
	if !strings.Contains(err.Error(), "file-backed") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
}

// The real migration set: 0039 is the only migration that declares the
// directive, and the database it produces enforces foreign keys.
func TestRealMigrationSetDeclaresTheDirectiveOnlyWhereIntended(t *testing.T) {
	names, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, entry := range names {
		body, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(body), rebuildDirective) {
			declared = append(declared, entry.Name())
		}
	}
	// Both rebuild a table other tables reference, and nothing else may:
	// the directive turns enforcement off for a whole migration, so a
	// file that declares it without needing it is a hole nobody notices.
	want := []string{"0039_person_core.sql", "0043_person_id_references.sql"}
	if len(declared) != len(want) {
		t.Fatalf("migrations declaring foreign_keys=off = %v, want %v", declared, want)
	}
	for i, name := range want {
		if declared[i] != name {
			t.Errorf("migrations declaring foreign_keys=off = %v, want %v", declared, want)
		}
	}

	// And after the real set runs, enforcement is on.
	db := open(t)
	if !foreignKeysOn(t, db) {
		t.Error("foreign keys are off after the real migration set")
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_identity_links (link_type, external_id, person_id, generation, created_at, created_by)
		 VALUES ('api-principal', 'x', 'psn_nobody', 1, 't', 'root-admin')`); err == nil {
		t.Error("butler_identity_links no longer enforces its foreign key")
	}
}
