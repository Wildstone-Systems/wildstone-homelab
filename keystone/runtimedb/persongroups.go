package runtimedb

// The household's one principal namespace (ADR-W033 design §4, slice 6).
//
// Three coming slices need to say "adults": service entitlements (8),
// memory audiences (11) and secret grants (12). §4 requires that when
// all three say it, they mean the same people — and names the cost of
// letting each keep its own list:
//
//	The alternative — a separate audience vocabulary beside the
//	entitlement one — produces two group systems that drift, and a
//	household that cannot say why a person is in one and not the other.
//
// WHAT THIS FILE DOES NOT DO is the more important half. Membership
// authorizes nothing. There is no grants table, no "can" method, and
// no consumer of a group in this slice at all — §4 calls membership an
// *identity primitive*, and what it permits is each resource's own
// decision. A helper here that answered "may they?" would make this
// package the place permissions accumulate, which is exactly the
// coupling the namespace exists to avoid.
//
// ONE RESOLVER. ResolveGroupMembers is the only implementation of "who
// is in this group", and everything else in this file is written in
// terms of it rather than beside it. Two resolvers is not a style
// problem: it is two answers to the same question, differing on some
// edge nobody notices until an offboarded person is still counted as an
// adult by one of the three consumers.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Seed keys, the one thing code may look a group up by.
//
// These name the ROLE a group plays, not what the household calls it.
// Rename `Adults` to `Grown-ups` and SeedAdults still finds it, because
// what changed is the word for the group rather than which group it is.
const (
	SeedFamily = "family"
	SeedAdults = "adults"
	SeedKids   = "kids"
)

// Group is a named set of People.
type Group struct {
	ID string `json:"id"`
	// Name is the household's word for this group, and may change.
	// Nothing should store it; store ID.
	Name string `json:"name"`
	// SeedKey is set on the three groups the design names and empty on
	// every group a household made itself.
	SeedKey   string    `json:"seedKey,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func newGroupID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("runtimedb: generating a group id: %w", err)
	}
	return "grp_" + hex.EncodeToString(raw), nil
}

// ResolveGroupMembers returns the person_ids in a group.
//
// THE one membership resolver (§4, §18.1 "the same group resolves to
// the same people whether asked as an entitlement, an audience or a
// secret grant"). Every other question about membership in this package
// is answered by calling this, so there is one place where the answer
// is decided and one place to change it.
//
// It reads live on every call. There is no cache, and that is a
// requirement rather than an omission: removing somebody from a group
// has to take effect at the next authorization evaluation, not when
// something expires. A cache here would mean a person removed from
// `Adults` keeps adult access for as long as the entry lives, which is
// the kind of window nobody can reason about from the outside.
//
// ACTIVE PEOPLE ONLY. Someone disabled, offboarded or never admitted is
// not returned, even though their row remains — the row is the
// household's record that they were in the group, which offboarding
// (slice 23) will want, while every caller of this function is about to
// make an authorization decision. Counting an offboarded person as an
// adult is the failure this ordering avoids, and it fails closed: the
// mistake this cannot make is admitting somebody it should not.
//
// Sorted, so a caller that compares two resolutions is comparing sets
// rather than whatever order SQLite happened to return.
func (db *DB) ResolveGroupMembers(groupID string) ([]string, error) {
	return resolveGroupMembers(db.sql, groupID)
}

func resolveGroupMembers(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, groupID string) ([]string, error) {
	rows, err := q.Query(
		`SELECT m.person_id
		   FROM person_group_members m
		   JOIN butler_members b ON b.person_id = m.person_id
		  WHERE m.group_id = ?
		    AND b.admitted = 1
		    AND b.disabled_at = ''
		    AND b.offboarded_at = ''
		  ORDER BY m.person_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: resolving group %q: %w", groupID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("runtimedb: resolving group %q: %w", groupID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtimedb: resolving group %q: %w", groupID, err)
	}
	return out, nil
}

// PersonIsInGroup answers membership for one Person.
//
// Built ON ResolveGroupMembers rather than as its own query, which is
// the point: a second SELECT here is a second definition of membership,
// and the two would agree right up until one of them was edited. The
// cost is reading a household-sized list to answer a yes/no, which is
// not a cost worth a second source of truth.
func (db *DB) PersonIsInGroup(personID, groupID string) (bool, error) {
	people, err := db.ResolveGroupMembers(groupID)
	if err != nil {
		return false, err
	}
	for _, p := range people {
		if p == personID {
			return true, nil
		}
	}
	return false, nil
}

// GroupsForPerson returns every group a Person is in.
//
// The union, not a winner: §4's own examples put somebody in `Adults`
// and `Media` at once, and a resolver that picked one would be
// answering a question nobody asked.
//
// Also built on the one resolver, for the same reason PersonIsInGroup
// is. A household has a handful of groups, so the loop is cheaper than
// the drift.
func (db *DB) GroupsForPerson(personID string) ([]Group, error) {
	groups, err := db.ListGroups()
	if err != nil {
		return nil, err
	}
	var out []Group
	for _, g := range groups {
		in, err := db.PersonIsInGroup(personID, g.ID)
		if err != nil {
			return nil, err
		}
		if in {
			out = append(out, g)
		}
	}
	return out, nil
}

// ListGroups returns every group, seeded and household-made, by name.
func (db *DB) ListGroups() ([]Group, error) {
	rows, err := db.sql.Query(
		`SELECT group_id, name, COALESCE(seed_key, ''), created_at
		   FROM person_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing groups: %w", err)
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		var created string
		if err := rows.Scan(&g.ID, &g.Name, &g.SeedKey, &created); err != nil {
			return nil, fmt.Errorf("runtimedb: listing groups: %w", err)
		}
		g.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtimedb: listing groups: %w", err)
	}
	return out, nil
}

// GroupBySeedKey finds one of the groups the design names.
//
// The only supported way for code to reach a specific group, and it
// keeps working across a rename because seed_key does not follow the
// display name. Code that looked a group up by the word "Adults" would
// break the first time a household preferred another one.
func (db *DB) GroupBySeedKey(seedKey string) (Group, error) {
	var g Group
	var created string
	err := db.sql.QueryRow(
		`SELECT group_id, name, COALESCE(seed_key, ''), created_at
		   FROM person_groups WHERE seed_key = ?`, seedKey).
		Scan(&g.ID, &g.Name, &g.SeedKey, &created)
	// A lookup that FAILED is not a lookup that found nothing. Folding
	// both into ErrNotFound would tell a caller the adults group does
	// not exist because the database was briefly unreadable — and the
	// caller's reasonable response to "not found" is to create one,
	// which would leave a household with two.
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("runtimedb: looking up the %q group: %w", seedKey, err)
	}
	g.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return g, nil
}

// CreateGroup adds a group the household defines itself.
//
// It gets no seed_key: seed keys name roles this design assigned
// meaning to, and a household group means whatever the household means
// by it. Nothing in Wildstone should have an opinion about a group it
// did not define.
func (db *DB) CreateGroup(name string) (Group, error) {
	id, err := newGroupID()
	if err != nil {
		return Group{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.sql.Exec(
		`INSERT INTO person_groups (group_id, name, seed_key, created_at) VALUES (?, ?, NULL, ?)`,
		id, name, now); err != nil {
		return Group{}, fmt.Errorf("runtimedb: creating group %q: %w", name, err)
	}
	return Group{ID: id, Name: name, CreatedAt: time.Now().UTC()}, nil
}

// RenameGroup changes the household's word for a group.
//
// The id does not move, so every rule that referenced this group still
// refers to it — which is the whole reason rules reference an id
// (§18.1, "rename `Adults`; every rule that referenced it still refers
// to the same group"). A seeded group may be renamed like any other:
// what a household calls its adults is theirs to decide, and seed_key
// goes on naming the role regardless.
func (db *DB) RenameGroup(groupID, name string) error {
	res, err := db.sql.Exec(`UPDATE person_groups SET name = ? WHERE group_id = ?`, name, groupID)
	if err != nil {
		return fmt.Errorf("runtimedb: renaming group %q: %w", groupID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("runtimedb: renaming group %q: %w", groupID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddPersonToGroup puts a Person in a group. Idempotent.
//
// Grants them nothing by doing so, which is worth saying at the one
// call site that might be mistaken for a permission change.
func (db *DB) AddPersonToGroup(groupID, personID, actor string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.sql.Exec(
		`INSERT INTO person_group_members (group_id, person_id, added_at, added_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (group_id, person_id) DO NOTHING`,
		groupID, personID, now, actor); err != nil {
		return fmt.Errorf("runtimedb: adding %q to group %q: %w", personID, groupID, err)
	}
	return nil
}

// RemovePersonFromGroup takes a Person out of a group.
//
// Effective at the next resolution, because ResolveGroupMembers reads
// live and holds no cache. Idempotent: removing somebody who is not in
// the group is not an error, since the caller's intent — that they not
// be in it — already holds.
func (db *DB) RemovePersonFromGroup(groupID, personID string) error {
	if _, err := db.sql.Exec(
		`DELETE FROM person_group_members WHERE group_id = ? AND person_id = ?`,
		groupID, personID); err != nil {
		return fmt.Errorf("runtimedb: removing %q from group %q: %w", personID, groupID, err)
	}
	return nil
}
