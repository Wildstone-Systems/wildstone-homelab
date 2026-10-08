package runtimedb

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// asIfMigratedTheLongWay rewrites schema_migrations so the database at
// path looks like one that ran the first n squashed migrations and never
// heard of the baseline. The schema itself is the baseline's, which is
// what the full chain produced, so for n == len(squashedMigrations) this
// is exactly a deployment that upgraded before the baseline existed.
func asIfMigratedTheLongWay(t *testing.T, path string, n int) {
	t.Helper()
	db, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.sql.Exec(`DELETE FROM schema_migrations WHERE name = ?`, baselineMigration); err != nil {
		t.Fatal(err)
	}
	for _, name := range squashedMigrations[:n] {
		if _, err := db.sql.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, 't')`, name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestANewDatabaseRunsTheBaselineAndNothingSquashed(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	applied, err := db.AppliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 || applied[0] != baselineMigration {
		t.Fatalf("applied = %v, want the baseline first", applied)
	}
	for _, name := range squashedMigrations {
		if slices.Contains(applied, name) {
			t.Fatalf("a new database recorded squashed migration %s", name)
		}
	}
	// The seed rows the old chain wrote arrive with the baseline.
	var groups int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM person_groups WHERE seed_key IN ('family','adults','kids')`).Scan(&groups); err != nil || groups != 3 {
		t.Fatalf("seeded person groups = %d, %v; want 3", groups, err)
	}
}

// A deployment that finished the old chain adopts the baseline without
// running it, and keeps every row it had.
func TestAFinishedDatabaseAdoptsTheBaselineUntouched(t *testing.T) {
	path := migrated.Path(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record(t, db, "sam")
	db.Close()
	asIfMigratedTheLongWay(t, path, len(squashedMigrations))

	for i := 0; i < 2; i++ { // adopting is once; reopening changes nothing
		db, err = Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		applied, err := db.AppliedMigrations()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(applied, baselineMigration) {
			t.Fatalf("open %d: the baseline was not adopted: %v", i+1, applied)
		}
		if got, want := len(applied), len(squashedMigrations)+1; got != want {
			t.Fatalf("open %d: %d migrations recorded, want %d", i+1, got, want)
		}
		if _, err := db.GetButlerMember("sam"); err != nil {
			t.Fatalf("open %d: adopting lost a row: %v", i+1, err)
		}
		db.Close()
	}
}

// A deployment that stopped part-way through the old chain is refused:
// the steps it is missing no longer ship, and the error says how to get
// past it.
func TestAPartWayDatabaseIsRefused(t *testing.T) {
	path := migrated.Path(t)
	asIfMigratedTheLongWay(t, path, len(squashedMigrations)-1)

	_, err := Open(path)
	if err == nil {
		t.Fatal("a database missing a squashed migration opened")
	}
	if !strings.Contains(err.Error(), "part-way") || !strings.Contains(err.Error(), "before the baseline") {
		t.Fatalf("the refusal does not say what to do: %v", err)
	}
}

// The squashed list is data in a text file; a line lost from it would
// make every finished deployment look part-way, and a line added would
// make a part-way one look finished.
func TestTheSquashedListIsTheWholeChain(t *testing.T) {
	if got := len(squashedMigrations); got != 112 {
		t.Fatalf("%d squashed migrations listed, want 112", got)
	}
	seen := map[string]bool{}
	for _, name := range squashedMigrations {
		if seen[name] || !strings.HasPrefix(name, "migrations/") || !strings.HasSuffix(name, ".sql") || name == baselineMigration {
			t.Fatalf("bad squashed entry %q", name)
		}
		seen[name] = true
	}
}
