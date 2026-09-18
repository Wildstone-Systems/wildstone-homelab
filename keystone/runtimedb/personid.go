package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
)

// The handle/identity boundary.
//
// Since 0040 every provably-human column stores a person_id rather than
// a handle, so a rename moves the label and detaches nothing. The
// callers above this package still speak handles — that is what a
// person types, what the Console shows, and what an API request names —
// so the translation happens here, at the edge of storage, and nowhere
// further out.
//
// Which columns those are is not decided here either. The classification
// in runtimedb/personref holds one verdict per column with its evidence,
// and 0040 moved exactly the ones it marks `human` or `scoped`. Actor
// columns still hold recorded text, because the value there may be a
// machine principal and an id that resolves to nothing is not evidence.

// personRower is the part of *sql.DB and *sql.Tx this file needs, so a
// resolution inside a transaction sees that transaction's writes. A
// resolve on the pool and a write in a transaction would be two
// snapshots, which is how a row gets attached to a person who was
// renamed in between.
type personRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// ErrNoSuchPerson is returned when a handle names nobody on record.
// Callers that accept a handle from outside should surface it as a
// 404 rather than a 500: an unknown handle is a bad request, not a
// broken database.
var ErrNoSuchPerson = errors.New("runtimedb: no person on record with that handle")

// personID resolves a handle to the stable identity behind it.
//
// The match is exact, which is the same rule migration 0040 gates on
// and the same one slice 2a adopted: handles are unique
// case-insensitively, and a case variant is reported and never merged.
// Resolving `Sam` to `sam` here would quietly undo that.
func resolvePersonID(q personRower, member string) (string, error) {
	if member == "" {
		return "", fmt.Errorf("%w: (empty handle)", ErrNoSuchPerson)
	}
	var id string
	err := q.QueryRow(`SELECT person_id FROM butler_members WHERE member = ?`, member).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %q", ErrNoSuchPerson, member)
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: resolving the person behind %q: %w", member, err)
	}
	return id, nil
}

// personHandle renders an identity back as the handle it currently
// carries. Storage joins to butler_members for this in the same query
// wherever it can; this is for the places that cannot.
func personHandle(q personRower, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	var member string
	err := q.QueryRow(`SELECT member FROM butler_members WHERE person_id = ?`, id).Scan(&member)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: person %q", ErrNoSuchPerson, id)
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: reading the handle for person %q: %w", id, err)
	}
	return member, nil
}

// HouseholdOwner is the owner value that means the household itself
// rather than any one person: the literal the `scoped` columns have
// always taken, and what callers still pass and still read back.
//
// In storage it is NULL. A reserved person_id would have been a row in
// butler_members that is not a person, and every query that joins
// people would need to know to leave it out.
const HouseholdOwner = "household"

// ownerPersonID resolves an owner for a `scoped` column: the household
// becomes NULL, anybody else resolves as usual.
func ownerPersonID(q personRower, owner string) (any, error) {
	if owner == HouseholdOwner {
		return nil, nil
	}
	id, err := resolvePersonID(q, owner)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// ownerFromPersonID is ownerPersonID backwards, for reading a `scoped`
// column out: a NULL owner is the household.
func ownerFromPersonID(handle sql.NullString) string {
	if !handle.Valid {
		return HouseholdOwner
	}
	return handle.String
}

// personIDForRead resolves a handle for a READ and tolerates a name
// that is nobody on record, returning "".
//
// Reads are deliberately more forgiving than writes. A reader may be a
// machine principal — the Butler asking on somebody's behalf, an
// operator token calling the API — and before 0040 a name like that
// simply matched no owner and saw only shared material. Erroring
// instead would turn a read that used to work into a failure, so an
// unresolvable reader keeps meaning "owns nothing here".
//
// Writes go through resolvePersonID, which refuses: storing a reference
// to somebody who does not exist is how the free-text model produced
// orphans in the first place.
func personIDForRead(q personRower, member string) (string, error) {
	id, err := resolvePersonID(q, member)
	if errors.Is(err, ErrNoSuchPerson) {
		return "", nil
	}
	return id, err
}
