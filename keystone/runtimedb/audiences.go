package runtimedb

// Who a record is disclosed to (ADR-W033 design §10 layer 3, §11;
// slice 7).
//
// An audience is a row now, not one of two words, and it names people
// through the one group namespace slice 6 built. `Adults` here and
// `Adults` in a service entitlement are the same set of people, which
// is the whole reason §4 insists on one namespace.
//
// LAYER 3, AND ONLY LAYER 3. §10 stacks authority: layer 1 is whether
// the Butler may act at all, layer 2 is the resource-specific grant,
// layer 3 is disclosure. This file is the third and never widens the
// first two — a record whose audience includes somebody is still out of
// reach if they lack the grant. Nothing here answers "may they?"; it
// answers "who is this for?", and the difference is what keeps the
// audience from quietly becoming a permission model.
//
// ONE RESOLVER, for the same reason slice 6 has one: two answers to
// "who does this disclose to" differ on some edge nobody notices until
// a record reaches somebody it was not meant for. ResolveAudience is
// it, and AudiencesVisibleTo — which the retrieval filters use — is
// written in terms of it.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

// The two audiences every record had before this slice. They are rows
// in `audiences`, seeded by 0052, and the ids are stable.
const (
	// AudienceOwner discloses to the record's owner alone. It is
	// resolved against the RECORD, which is why one row serves every
	// table: "the owner" means a different person for each row.
	AudienceOwner = "aud_owner"
	// AudienceHousehold discloses to every active person.
	AudienceHousehold = "aud_household"
)

// Audience kinds, as stored.
const (
	audienceKindOwner     = "owner"
	audienceKindHousehold = "household"
	audienceKindSet       = "set"
)

// Audience is a disclosure rule.
type Audience struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Label is what a household calls this audience, empty on the two
	// seeded rows: "the owner" and "the household" are roles rather
	// than names somebody chose.
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// ResolveAudience returns the person_ids a record is disclosed to.
//
// THE one audience resolver. ownerPersonID is the record's own owner
// and is used only by the 'owner' kind — passing it for the others is
// harmless, and passing it wrongly for 'owner' is the one way to widen
// disclosure by accident, which is why it is a required argument
// rather than something this function looks up for itself. The caller
// has the row; this function must not guess which row it meant.
//
// Reads live, holds no cache: removing somebody from a group has to
// take effect at the next evaluation, exactly as in slice 6.
//
// FAILS CLOSED. An audience that resolves to nobody discloses to
// nobody. That is not a degenerate case to smooth over — a 'set'
// audience whose only group has been emptied is precisely the moment
// when guessing "well, somebody must be able to see it" would hand the
// record to everyone.
func (db *DB) ResolveAudience(audienceID, ownerPersonID string) ([]string, error) {
	a, err := db.GetAudience(audienceID)
	if err != nil {
		return nil, err
	}
	switch a.Kind {
	case audienceKindOwner:
		// A record owned by nobody — the household-owned case, where
		// owner_person_id is NULL — discloses to nobody under an owner
		// audience. There is no owner to be, so there is no one it is
		// for, and inventing a reader here is the widening this whole
		// file exists to refuse.
		if ownerPersonID == "" {
			return nil, nil
		}
		return []string{ownerPersonID}, nil

	case audienceKindHousehold:
		return db.activePeople()

	case audienceKindSet:
		return db.resolveAudienceSet(audienceID)
	}
	return nil, fmt.Errorf("runtimedb: audience %q has unknown kind %q", audienceID, a.Kind)
}

// resolveAudienceSet is the union of the audience's groups and people.
//
// Groups go through slice 6's ResolveGroupMembers rather than a join
// written here, which is what makes `Adults` mean the same thing in an
// audience as it does anywhere else (§18.1). A second join would be a
// second definition, agreeing right up until one was edited.
func (db *DB) resolveAudienceSet(audienceID string) ([]string, error) {
	rows, err := db.sql.Query(
		`SELECT kind, ref FROM audience_members WHERE audience_id = ? ORDER BY kind, ref`,
		audienceID)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: resolving audience %q: %w", audienceID, err)
	}
	defer rows.Close()
	var groups, people []string
	for rows.Next() {
		var kind, ref string
		if err := rows.Scan(&kind, &ref); err != nil {
			return nil, fmt.Errorf("runtimedb: resolving audience %q: %w", audienceID, err)
		}
		if kind == "group" {
			groups = append(groups, ref)
		} else {
			people = append(people, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtimedb: resolving audience %q: %w", audienceID, err)
	}

	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, g := range groups {
		members, err := db.ResolveGroupMembers(g)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			add(m)
		}
	}
	// A person named directly still has to be active, or an offboarded
	// person named in an audience would keep reading it — the same
	// standing rule ResolveGroupMembers applies, applied here too so
	// the two halves of a set cannot disagree.
	active, err := db.activePeople()
	if err != nil {
		return nil, err
	}
	isActive := map[string]bool{}
	for _, p := range active {
		isActive[p] = true
	}
	for _, p := range people {
		if isActive[p] {
			add(p)
		}
	}
	sortStrings(out)
	return out, nil
}

// AudiencesVisibleTo returns the audience ids this reader may see,
// EXCLUDING the owner audience.
//
// The owner audience is deliberately left out, and that is the whole
// subtlety of this function. It resolves against the record rather than
// against the audience — "the owner" is a different person per row — so
// it cannot be pre-computed for a query. Callers filter with
//
//	audience IN (<these>) OR (audience = 'aud_owner' AND owner_person_id = <reader>)
//
// which is the only shape that keeps one resolver while still filtering
// in SQL, before any content loads.
//
// Built on ResolveAudience, so there is one definition of who an
// audience discloses to and one place to change it.
func (db *DB) AudiencesVisibleTo(readerPersonID string) ([]string, error) {
	all, err := db.ListAudiences()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range all {
		if a.Kind == audienceKindOwner {
			continue
		}
		// The household audience is visible to every reader, without
		// asking whether this one is an active member.
		//
		// That is what 'shared' meant, and keeping it is deliberate.
		// Resolving it through activePeople instead would refuse
		// household material to anybody not admitted — a change to WHO
		// MAY READ, which is the authorization model. #208 is explicit
		// that this slice changes the vocabulary and not that: "Do not
		// add resource-specific grant logic here." Narrowing who counts
		// as the household is the memory slice's decision to make, with
		// its own tests, not a side effect of renaming two words.
		if a.Kind == audienceKindHousehold {
			out = append(out, a.ID)
			continue
		}
		// ownerPersonID is irrelevant for the kinds that reach here,
		// and passing the reader would be a lie the next edit believes.
		people, err := db.ResolveAudience(a.ID, "")
		if err != nil {
			return nil, err
		}
		for _, p := range people {
			if p == readerPersonID {
				out = append(out, a.ID)
				break
			}
		}
	}
	return out, nil
}

// GetAudience reads one audience.
func (db *DB) GetAudience(audienceID string) (Audience, error) {
	var a Audience
	var created string
	err := db.sql.QueryRow(
		`SELECT audience_id, kind, label, created_at FROM audiences WHERE audience_id = ?`,
		audienceID).Scan(&a.ID, &a.Kind, &a.Label, &created)
	// A lookup that failed is not a lookup that found nothing: a caller
	// told an audience does not exist may reasonably create one, and
	// two audiences for one rule is worse than an error.
	if errors.Is(err, sql.ErrNoRows) {
		return Audience{}, ErrNotFound
	}
	if err != nil {
		return Audience{}, fmt.Errorf("runtimedb: reading audience %q: %w", audienceID, err)
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return a, nil
}

// ListAudiences returns every audience.
func (db *DB) ListAudiences() ([]Audience, error) {
	rows, err := db.sql.Query(
		`SELECT audience_id, kind, label, created_at FROM audiences ORDER BY audience_id`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing audiences: %w", err)
	}
	defer rows.Close()
	var out []Audience
	for rows.Next() {
		var a Audience
		var created string
		if err := rows.Scan(&a.ID, &a.Kind, &a.Label, &created); err != nil {
			return nil, fmt.Errorf("runtimedb: listing audiences: %w", err)
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtimedb: listing audiences: %w", err)
	}
	return out, nil
}

// CreateAudienceForGroups makes a 'set' audience naming groups.
//
// The shape §208 asks for that the two seeded rows cannot express:
// "adults only". Groups by id, so a rename does not re-point it.
func (db *DB) CreateAudienceForGroups(label string, groupIDs ...string) (Audience, error) {
	if len(groupIDs) == 0 {
		// An audience naming nothing discloses to nobody, which is a
		// legal state to arrive at but never a sensible one to create
		// on purpose — it is far more often a caller that meant to pass
		// something and passed nothing.
		return Audience{}, fmt.Errorf("runtimedb: an audience must name at least one group")
	}
	id, err := newAudienceID()
	if err != nil {
		return Audience{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.sql.Begin()
	if err != nil {
		return Audience{}, fmt.Errorf("runtimedb: creating audience: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO audiences (audience_id, kind, label, created_at) VALUES (?, 'set', ?, ?)`,
		id, label, now); err != nil {
		return Audience{}, fmt.Errorf("runtimedb: creating audience: %w", err)
	}
	for _, g := range groupIDs {
		if _, err := tx.Exec(
			`INSERT INTO audience_members (audience_id, kind, ref) VALUES (?, 'group', ?)`,
			id, g); err != nil {
			return Audience{}, fmt.Errorf("runtimedb: adding group %q to audience: %w", g, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Audience{}, fmt.Errorf("runtimedb: creating audience: %w", err)
	}
	return Audience{ID: id, Kind: audienceKindSet, Label: label, CreatedAt: time.Now().UTC()}, nil
}

// activePeople is every person who may be disclosed to at all.
//
// The same standing rule as slice 6's resolver — admitted, not
// disabled, not offboarded — so "the household" and "everyone in
// Adults" cannot disagree about whether a departed person counts.
func (db *DB) activePeople() ([]string, error) {
	rows, err := db.sql.Query(
		`SELECT person_id FROM butler_members
		  WHERE admitted = 1 AND disabled_at = '' AND offboarded_at = ''
		  ORDER BY person_id`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing active people: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("runtimedb: listing active people: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func newAudienceID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("runtimedb: generating an audience id: %w", err)
	}
	return "aud_" + hex.EncodeToString(raw), nil
}

// sortStrings keeps a resolved set comparable between calls. SQLite
// returns whatever order it likes across a union of several queries,
// and a caller comparing two resolutions would see sets "differ" that
// hold the same people.
func sortStrings(s []string) { sort.Strings(s) }

// visibleAudienceClause builds the SQL a retrieval filter ANDs on, and
// the arguments it takes.
//
// THE one place the audience rule is expressed in SQL. Before slice 7
// three queries each carried `audience = 'shared' OR owner = ?`, which
// was three copies of a rule — fine while the rule was one comparison,
// and three places to forget a group the moment it was not. #208 asks
// for exactly this: "no table keeps a private notion of who a group
// means."
//
// The shape is two halves because an audience is two kinds of thing.
// Everything the reader can see is pre-resolved into a list; the owner
// audience cannot be, because it resolves against the ROW. Filtering
// still happens entirely in SQL, before any content loads, which is
// what the old predicates were protecting.
//
// A reader who is nobody on record gets an empty list and no owner
// match, so the clause admits nothing. Fails closed, as it must.
func (db *DB) visibleAudienceClause(col, ownerCol, readerPersonID string) (string, []any, error) {
	visible, err := db.AudiencesVisibleTo(readerPersonID)
	if err != nil {
		return "", nil, err
	}
	args := make([]any, 0, len(visible)+2)
	in := ""
	for i, a := range visible {
		if i > 0 {
			in += ", "
		}
		in += "?"
		args = append(args, a)
	}
	// "IN ()" is not valid SQL, and a reader who can see no audience at
	// all is a real state — somebody offboarded, or a name nobody
	// recognises. `NULL` is never equal to anything, so an empty list
	// becomes a term that cannot match rather than a syntax error.
	if in == "" {
		in = "NULL"
	}
	args = append(args, AudienceOwner, readerPersonID)
	return fmt.Sprintf("(%s IN (%s) OR (%s = ? AND %s = ?))", col, in, col, ownerCol), args, nil
}

// StrictestAudience picks the narrowest of several audiences, and
// refuses when there is no narrowest one.
//
// "Strictest" was one comparison while there were two values: private
// beat shared. With sets it is a subset test, and the honest version
// admits that sets need not be comparable at all. `Adults` and `Kids`
// overlap in nobody; neither is narrower; a derivative of both has no
// correct audience, and inventing one is how a summary of two people's
// material reaches both of them.
//
// So this returns the candidate that is a subset of every other, and an
// error when no candidate is. Refusing is the same answer the
// two-owner case has always given for the same reason (ADR-W030 A2/A3)
// — there is no audience the result could correctly have.
//
// The owner audience is NOT handled here. It resolves against a row
// rather than against itself, and it is already the narrowest thing
// there is, so the caller short-circuits on it before asking this.
func (db *DB) StrictestAudience(audienceIDs []string) (string, error) {
	if len(audienceIDs) == 0 {
		return "", fmt.Errorf("runtimedb: no audiences to compare")
	}
	sets := make(map[string]map[string]bool, len(audienceIDs))
	for _, id := range audienceIDs {
		if _, done := sets[id]; done {
			continue
		}
		people, err := db.ResolveAudience(id, "")
		if err != nil {
			return "", err
		}
		set := make(map[string]bool, len(people))
		for _, p := range people {
			set[p] = true
		}
		sets[id] = set
	}
	for _, candidate := range audienceIDs {
		subsetOfAll := true
		for other, otherSet := range sets {
			if other == candidate {
				continue
			}
			for p := range sets[candidate] {
				if !otherSet[p] {
					subsetOfAll = false
					break
				}
			}
			if !subsetOfAll {
				break
			}
		}
		if subsetOfAll {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"runtimedb: refusing to derive one record from sources whose audiences overlap without one being narrowest (%v) — the result would have no correct audience (ADR-W030 A2/A3, design §11)",
		audienceIDs)
}
