package personref_test

// The classification is only worth having if it cannot silently fall
// behind the schema. These tests reconcile it against a REAL migrated
// database — sqlite_master and PRAGMA table_info — rather than against
// the migration text, so a column added by any means is caught.

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb/personref"

	_ "modernc.org/sqlite"
)

// candidate is the same name heuristic the survey used. It is
// deliberately WIDE: it over-matches, and every over-match is then
// given an explicit not-a-person verdict. A heuristic that missed a
// column would leave a person reference unclassified, which is the
// failure that matters.
var candidate = map[string]bool{
	"member": true, "owner": true, "author": true, "requested_by": true,
	"granted_by": true, "revoked_by": true, "created_by": true, "admitted_by": true,
	"disabled_by": true, "offboarded_by": true, "decided_by": true, "guardian": true,
	"actor": true, "updated_by": true, "acting_for": true, "principal": true,
	"subject": true, "enabled_by": true, "retired_by": true, "used_by": true,
	"added_by": true, "bound_by": true, "claimed_by": true, "answered_by": true,
	"resolved_by": true, "promoted_by": true, "suppressed_by": true,
	"forgotten_by": true, "released_by": true, "superseded_by": true,
	"journal_asserted_by": true,
}

// liveColumns opens a fully migrated database and returns every
// candidate column in it.
func liveColumns(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := runtimedb.Open(path)
	if err != nil {
		t.Fatalf("runtimedb.Open: %v", err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open for inspection: %v", err)
	}
	defer raw.Close()

	tables, err := raw.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err := tables.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	tables.Close()
	if len(names) == 0 {
		t.Fatal("a migrated database reported no tables")
	}

	out := map[string]bool{}
	for _, table := range names {
		cols, err := raw.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("table_info(%s): %v", table, err)
		}
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				t.Fatal(err)
			}
			if candidate[strings.ToLower(c)] {
				out[table+"."+c] = true
			}
		}
		cols.Close()
	}
	return out
}

func classified() map[string]personref.Column {
	out := map[string]personref.Column{}
	for _, c := range personref.Columns {
		out[c.Table+"."+c.Column] = c
	}
	return out
}

// The point of the whole package: a column that could name a person and
// has no verdict is a column slice 2b would have to guess about.
func TestEveryCandidateColumnHasAVerdict(t *testing.T) {
	live, have := liveColumns(t), classified()
	var missing []string
	for col := range live {
		if _, ok := have[col]; !ok {
			missing = append(missing, col)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d column(s) could name a person and carry no verdict:\n  %s\n\n"+
			"Add each to personref.Columns with the evidence — what the write path passes, "+
			"or what the schema comment says. Do not guess from the name.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// And the converse: a verdict for a column that no longer exists is
// stale, and a stale verdict is worse than none because it reads as
// current.
func TestNoVerdictOutlivesItsColumn(t *testing.T) {
	live, have := liveColumns(t), classified()
	var gone []string
	for col := range have {
		if !live[col] {
			gone = append(gone, col)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Fatalf("%d verdict(s) name a column that is not in the schema:\n  %s",
			len(gone), strings.Join(gone, "\n  "))
	}
}

// Every verdict carries its evidence. "Why" is what a reviewer reads
// instead of re-deriving the classification, and what tells a later
// reader whether a changed write path has invalidated it.
func TestEveryVerdictCarriesItsEvidence(t *testing.T) {
	for _, c := range personref.Columns {
		if strings.TrimSpace(c.Why) == "" {
			t.Errorf("%s.%s is classified %s with no evidence", c.Table, c.Column, c.Kind)
		}
		switch c.Kind {
		case personref.Human, personref.Actor, personref.NotAPerson:
		default:
			t.Errorf("%s.%s has kind %q, which is not one of the three", c.Table, c.Column, c.Kind)
		}
	}
}

func TestNoColumnIsClassifiedTwice(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range personref.Columns {
		key := c.Table + "." + c.Column
		if seen[key] {
			t.Errorf("%s has two verdicts; the later one silently wins", key)
		}
		seen[key] = true
	}
}
