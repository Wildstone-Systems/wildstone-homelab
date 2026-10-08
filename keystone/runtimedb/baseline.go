package runtimedb

import (
	_ "embed"
	"fmt"
	"io/fs"
	"strings"
)

// baselineMigration is the whole schema as migrations 0001 through 0104
// left it, in one file (ADR-W007 A4, "Baseline"). Replaying 112 files
// into every new database had become the slowest thing the test suite
// did (#231), and none of those intermediate states exists anywhere but
// on databases that already finished them.
const baselineMigration = "migrations/0000_baseline.sql"

// squashedMigrations are the migrations the baseline replaced, by the
// names schema_migrations recorded them under. A database that applied
// every one of them already has the baseline's schema, so it adopts the
// baseline instead of running it. The list lives in a text file because
// it is data about history, not SQL this package runs.
var squashedMigrations = parseSquashed(squashedList)

//go:embed baseline_squashed.txt
var squashedList string

func parseSquashed(list string) []string {
	var names []string
	for _, line := range strings.Split(list, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, line)
		}
	}
	return names
}

// adoptBaseline settles, before any migration runs, whether this database
// runs the baseline or has already reached it the long way.
//
//   - Fresh (none of the squashed migrations recorded): nothing to do;
//     the baseline is pending like any other migration and runs first.
//   - Finished (all of them recorded): the database is exactly what the
//     baseline builds, so the baseline is recorded as applied and never
//     run. Nothing in the schema or the rows changes.
//   - Part-way (some of them recorded): refused. The steps it still
//     needs no longer ship, and running the baseline on top of a
//     half-built schema would fail or, worse, half-succeed. A build from
//     before the baseline finishes the chain; this one then adopts it.
//
// Only a migration source that carries the baseline is checked, so the
// runner's own tests, which bring their own migrations, are unaffected.
func (db *DB) adoptBaseline(src fs.FS) error {
	if _, err := fs.Stat(src, baselineMigration); err != nil {
		return nil
	}
	recorded := map[string]bool{}
	rows, err := db.sql.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("runtimedb: reading applied migrations: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("runtimedb: reading applied migrations: %w", err)
		}
		recorded[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("runtimedb: reading applied migrations: %w", err)
	}
	if recorded[baselineMigration] {
		return nil
	}
	var missing []string
	for _, name := range squashedMigrations {
		if !recorded[name] {
			missing = append(missing, name)
		}
	}
	switch len(missing) {
	case len(squashedMigrations):
		return nil
	case 0:
		if _, err := db.sql.Exec(
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, baselineMigration, now()); err != nil {
			return fmt.Errorf("runtimedb: adopting %s: %w", baselineMigration, err)
		}
		return nil
	default:
		return fmt.Errorf("runtimedb: this database stopped part-way through the migrations squashed into %s (%d of %d missing, first %s); "+
			"open it once with a Keystone build from before the baseline, which finishes them, then upgrade",
			baselineMigration, len(missing), len(squashedMigrations), strings.TrimPrefix(missing[0], "migrations/"))
	}
}
