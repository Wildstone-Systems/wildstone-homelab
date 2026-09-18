package personref

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Querier is the read surface a survey needs. It is an interface, and
// the survey never holds anything wider, so no amount of later editing
// can turn the dry run into a write. The CLI hands it a handle opened
// with mode=ro, which makes that structural rather than a promise.
type Querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// Verdict is what a handle turned out to be.
type Verdict string

const (
	// Resolved: exactly one Person owns this handle.
	Resolved Verdict = "resolved"
	// CaseVariant: it matches a Person when case is folded, but not
	// exactly. Reported for a decision, never merged.
	CaseVariant Verdict = "case-variant"
	// Ambiguous: more than one Person answers to it once case is
	// folded. Never merged, never picked between.
	Ambiguous Verdict = "ambiguous"
	// MachinePrincipal: no Person, but a principal of that name exists.
	// Expected, and not an error: an actor column holds these by
	// design.
	MachinePrincipal Verdict = "machine-principal"
	// AlsoAPrincipal: a Person AND a machine principal answer to this
	// name. Nothing can tell which one wrote a given row, so it is
	// never resolved.
	AlsoAPrincipal Verdict = "also-a-principal"
	// Scope: a well-known non-person owner, in a column that takes one.
	// Valid, permanent, and never resolved to anybody.
	Scope Verdict = "scope"
	// Orphan: no Person and no principal.
	Orphan Verdict = "orphan"
)

// Use is one handle found in one column, in rows that agree about
// whether they are scoped.
//
// A Scoped column can produce TWO uses for one handle — `household` in
// shared memory rows is the scope, and the same word in private rows is
// not — because the two answer the migration's question differently.
type Use struct {
	Table  string
	Column string
	Kind   Kind
	Rows   int
	// Scope is true when these rows hold the column's non-person scope
	// rather than a reference to a party.
	Scope bool
}

// Handle is one distinct value and everywhere it appears.
type Handle struct {
	Value   string
	Verdict Verdict
	// PersonID is set when Verdict is Resolved.
	PersonID string
	// Candidates lists the Persons that answered to it, for
	// CaseVariant and Ambiguous.
	Candidates []string
	Uses       []Use
}

// scopes are the non-person owners a Scoped column may legitimately
// hold. They are values, not names, and no Person is ever resolved from
// one.
var scopes = map[string]bool{"household": true}

// scopeMode says WHICH ROWS of a Scoped column hold the scope rather
// than a person: the companion column recording the ownership mode, and
// the value of it that licenses the scope.
//
// Asking only "is this value the word household" is a different and
// weaker question. A PRIVATE memory record owned by `household` is not
// a scoped row, it is a broken one, and the difference between a report
// that is right per row and one that is right on average is exactly the
// kind of thing a migration gate must not get wrong.
var scopeMode = map[string]struct{ by, when string }{
	"butler_memory.owner": {"audience", "shared"},
	"butler_index.owner":  {"audience", "shared"},
}

// InHumanColumn reports whether this handle appears anywhere slice 2b
// would repoint. A handle used only in actor columns is not a Person
// question at all. Scoped columns count: they hold a Person in one
// ownership mode, and that mode is exactly what 2b repoints.
func (h Handle) InHumanColumn() bool {
	for _, u := range h.Uses {
		if u.Kind == Human || u.Kind == Scoped {
			return true
		}
	}
	return false
}

// Blocking reports whether this handle must stop the real migration.
//
// Decided PER USE, not per handle. A handle is one string, and the same
// string can be legitimate in one place and unresolvable in another:
// `household` owning a shared memory record is the scope, and the same
// word in butler_tasks.author is a person reference to nobody.
// Collapsing the two — as an earlier version did, by asking whether ANY
// use was scoped — let the scope excuse the orphan, which is precisely
// the silent pass this report exists to prevent.
//
// A machine principal in an actor column is still the normal case:
// every deployment has rows written by root-admin, and treating those
// as unresolved is what would abort every real migration.
func (h Handle) Blocking() bool {
	if h.Verdict == Resolved {
		return false
	}
	for _, u := range h.Uses {
		if u.Kind != Human && u.Kind != Scoped {
			continue
		}
		if u.Scope {
			// Legitimately the scope in these rows.
			continue
		}
		return true
	}
	return false
}

// Report is the whole dry run.
type Report struct {
	Handles []Handle
	// People is how many Person rows the survey resolved against.
	People int
}

// Blocking returns the handles that would abort the real migration,
// which is the list an Owner has to clear before slice 2b runs.
func (r Report) Blocking() []Handle {
	var out []Handle
	for _, h := range r.Handles {
		if h.Blocking() {
			out = append(out, h)
		}
	}
	return out
}

// allScoped reports whether every use that bears on the migration is
// the column's scope. A use in an actor column does not count either
// way — 2b does not touch those.
func allScoped(us []Use) bool {
	any := false
	for _, u := range us {
		if u.Kind != Human && u.Kind != Scoped {
			continue
		}
		if !u.Scope {
			return false
		}
		any = true
	}
	return any
}

// person is one row of the people table.
type person struct{ id, handle string }

// Survey reads every classified column and reports what it found. It
// writes nothing.
func Survey(q Querier) (Report, error) {
	people, err := loadPeople(q)
	if err != nil {
		return Report{}, err
	}
	principals, err := loadPrincipals(q)
	if err != nil {
		return Report{}, err
	}

	// byFold groups Persons by case-folded handle, which is the whole
	// point of the case rule: two Persons under one folded handle is an
	// ambiguity nothing may resolve automatically.
	byFold := map[string][]person{}
	for _, p := range people {
		f := fold(p.handle)
		byFold[f] = append(byFold[f], p)
	}
	// Principals are folded too. A Person called Root-Admin beside a
	// principal called root-admin is the same collision wearing a
	// different hat, and the case rule this slice adopts says those are
	// one name.
	principalsByFold := map[string]bool{}
	for name := range principals {
		principalsByFold[fold(name)] = true
	}

	// Both tables, filtered by what this database actually has. On a
	// deployment still at 0039 that is LegacyColumns; on one past 0040
	// it is the person_id columns, which hold no free text and are
	// skipped below. Nothing is surveyed twice, because 0040 replaced
	// each legacy column rather than adding beside it.
	uses := map[string][]Use{}
	for _, c := range append(append([]Column{}, Columns...), LegacyColumns...) {
		if c.Kind == NotAPerson {
			continue
		}
		// A person_id column holds an identity, not a handle, so there
		// is nothing here to resolve: a value naming nobody is a broken
		// foreign key, which the database refuses outright rather than
		// leaving for a report to notice.
		if c.Kind == PersonID {
			continue
		}
		present, err := columnExists(q, c.Table, c.Column)
		if err != nil {
			return Report{}, err
		}
		if !present {
			continue
		}
		// Two columns hold a person's handle without REFERRING to one,
		// and surveying them would report every person as a handle to
		// resolve against themselves.
		//
		// butler_members.member is the people table's own handle.
		// butler_role_changes.member is retained audit text that
		// already carries person_id beside it — kept precisely so an
		// audit row stays readable after the Person is purged, which is
		// the case where resolving it is guaranteed to fail.
		if c.Column == "member" && (c.Table == "butler_members" || c.Table == "butler_role_changes") {
			continue
		}
		found, err := distinct(q, c)
		if err != nil {
			return Report{}, err
		}
		for k, rows := range found {
			uses[k.value] = append(uses[k.value], Use{
				Table: c.Table, Column: c.Column, Kind: c.Kind,
				Rows: rows, Scope: k.scoped && scopes[fold(k.value)],
			})
		}
	}

	rep := Report{People: len(people)}
	for value, us := range uses {
		sort.Slice(us, func(i, j int) bool {
			if us[i].Table != us[j].Table {
				return us[i].Table < us[j].Table
			}
			return us[i].Column < us[j].Column
		})
		h := Handle{Value: value, Uses: us}
		matches := byFold[fold(value)]
		switch {
		// Every place this value means anything to the migration is a
		// scope. Checked first because a scope is a VALUE with a fixed
		// meaning, not a name to look up: household-shared memory is
		// owned by the household and always will be, so resolving it —
		// or blocking on it — would refuse to migrate every deployment
		// that has any.
		//
		// "Every place", not "any place". The same word elsewhere, in a
		// column that takes only a person, is a reference to nobody and
		// keeps its own verdict.
		case scopes[fold(value)] && allScoped(us):
			h.Verdict = Scope
		// Checked BEFORE resolution, and that order is the whole point.
		//
		// butler_members.member is unique only within butler_members,
		// and principals.name only within principals, so nothing stops
		// a Person from being called root-admin. Resolving first would
		// then attach a machine-written row to an unrelated human — and
		// machine-written rows in human columns are not hypothetical:
		// the legacy ask path puts them there (#240), which is the
		// exact case this report exists to catch. Attributing one to a
		// Person who happens to share the name is the silent wrong
		// merge the whole slice is built to prevent.
		case len(matches) > 0 && principalsByFold[fold(value)]:
			h.Verdict = AlsoAPrincipal
			for _, m := range matches {
				h.Candidates = append(h.Candidates, m.handle)
			}
			sort.Strings(h.Candidates)
		case len(matches) > 1:
			h.Verdict = Ambiguous
			for _, m := range matches {
				h.Candidates = append(h.Candidates, m.handle)
			}
			sort.Strings(h.Candidates)
		case len(matches) == 1 && matches[0].handle == value:
			h.Verdict, h.PersonID = Resolved, matches[0].id
		case len(matches) == 1:
			h.Verdict = CaseVariant
			h.Candidates = []string{matches[0].handle}
		case principals[value]:
			h.Verdict = MachinePrincipal
		default:
			h.Verdict = Orphan
		}
		rep.Handles = append(rep.Handles, h)
	}
	sort.Slice(rep.Handles, func(i, j int) bool { return rep.Handles[i].Value < rep.Handles[j].Value })
	return rep, nil
}

// loadPeople reads every Person, active or not.
//
// Deliberately not "active only", which is what #203's rule 1 said. An
// offboarded person still owns the memory records, tasks and grants
// they created, and those rows still have to repoint at them. Resolving
// only against active people would make every offboarded person's
// history an orphan and abort the migration on exactly the deployments
// that have been running longest.
func loadPeople(q Querier) ([]person, error) {
	rows, err := q.Query(`SELECT person_id, member FROM butler_members`)
	if err != nil {
		return nil, fmt.Errorf("personref: reading people: %w", err)
	}
	defer rows.Close()
	var out []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.id, &p.handle); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// loadPrincipals reads the machine/API principals, so an actor column
// holding root-admin is recognised rather than reported as a missing
// person.
func loadPrincipals(q Querier) (map[string]bool, error) {
	rows, err := q.Query(`SELECT name FROM principals`)
	if err != nil {
		return nil, fmt.Errorf("personref: reading principals: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// group is one value of a column, split by whether its rows are in the
// column's scoped ownership mode.
type group struct {
	value  string
	scoped bool
}

// distinct counts the rows behind each non-empty value of one column,
// splitting a Scoped column's rows by their ownership mode so the two
// can be judged separately.
//
// The identifiers are interpolated because a column name cannot be a
// bound parameter in SQL. They are safe: every one comes from Columns
// or scopeMode, both compile-time tables in this package, and Columns
// is reconciled against the live schema by the tests. Nothing here ever
// sees a caller's string.
func distinct(q Querier, c Column) (map[group]int, error) {
	mode, scoped := scopeMode[c.Table+"."+c.Column]
	sel := fmt.Sprintf(`SELECT %s, COUNT(*), '' FROM %s WHERE %s <> '' GROUP BY %s`,
		c.Column, c.Table, c.Column, c.Column)
	if scoped {
		sel = fmt.Sprintf(`SELECT %s, COUNT(*), %s FROM %s WHERE %s <> '' GROUP BY %s, %s`,
			c.Column, mode.by, c.Table, c.Column, c.Column, mode.by)
	}
	rows, err := q.Query(sel)
	if err != nil {
		return nil, fmt.Errorf("personref: scanning %s.%s: %w", c.Table, c.Column, err)
	}
	defer rows.Close()
	out := map[group]int{}
	for rows.Next() {
		var v, m string
		var n int
		if err := rows.Scan(&v, &n, &m); err != nil {
			return nil, err
		}
		out[group{value: v, scoped: scoped && m == mode.when}] += n
	}
	return out, rows.Err()
}

// fold is the case rule, in one place.
//
// Handles are unique case-insensitively (#237, closing ADR-W032 A4).
// Surrounding whitespace is stripped too: a handle stored as "sam " is
// the same person as "sam" to everyone except a byte comparison, and
// leaving that to chance is how a rename orphans a row.
func fold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// columnExists asks the live schema rather than assuming it.
//
// The classification spans both sides of migration 0040, and this is
// what keeps that honest: a column the database does not have is not
// surveyed, so the same binary reports correctly against a deployment
// that has migrated and one that has not.
func columnExists(q Querier, table, column string) (bool, error) {
	rows, err := q.Query(`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("personref: checking for %s.%s: %w", table, column, err)
	}
	defer rows.Close()
	found := rows.Next()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("personref: checking for %s.%s: %w", table, column, err)
	}
	return found, nil
}
