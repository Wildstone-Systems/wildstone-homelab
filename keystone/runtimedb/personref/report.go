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
	// Orphan: no Person and no principal.
	Orphan Verdict = "orphan"
)

// Use is one handle found in one column.
type Use struct {
	Table  string
	Column string
	Kind   Kind
	Rows   int
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

// InHumanColumn reports whether this handle appears anywhere slice 2b
// would repoint. A handle used only in actor columns is not a Person
// question at all.
func (h Handle) InHumanColumn() bool {
	for _, u := range h.Uses {
		if u.Kind == Human {
			return true
		}
	}
	return false
}

// Blocking reports whether this handle must stop the real migration.
//
// Only a handle in a HUMAN column can block, and only when it does not
// resolve. A machine principal in an actor column is the normal case —
// every deployment has rows written by root-admin — and treating it as
// an unresolved reference is the mistake that would abort every real
// migration.
func (h Handle) Blocking() bool {
	if !h.InHumanColumn() {
		return false
	}
	switch h.Verdict {
	case Resolved:
		return false
	default:
		return true
	}
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

	uses := map[string][]Use{}
	for _, c := range Columns {
		if c.Kind == NotAPerson {
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
		for value, rows := range found {
			uses[value] = append(uses[value],
				Use{Table: c.Table, Column: c.Column, Kind: c.Kind, Rows: rows})
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

// distinct counts the rows behind each non-empty value of one column.
//
// The identifiers are interpolated because a column name cannot be a
// bound parameter in SQL. They are safe: both come from Columns, which
// is a compile-time table in this package, and every entry is
// reconciled against the live schema by the tests. Nothing here ever
// sees a caller's string.
func distinct(q Querier, c Column) (map[string]int, error) {
	rows, err := q.Query(fmt.Sprintf(
		`SELECT %s, COUNT(*) FROM %s WHERE %s <> '' GROUP BY %s`,
		c.Column, c.Table, c.Column, c.Column))
	if err != nil {
		return nil, fmt.Errorf("personref: scanning %s.%s: %w", c.Table, c.Column, err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var v string
		var n int
		if err := rows.Scan(&v, &n); err != nil {
			return nil, err
		}
		out[v] = n
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
