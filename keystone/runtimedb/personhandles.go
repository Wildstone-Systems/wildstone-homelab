package runtimedb

// Resolving references that live outside this database (#372, 0093).
//
// Desired state and the disk name people by the handle they had when the
// reference was written: a calendar grant, a commitment's
// `household/<member>`, a Members/<name> folder. person_handles keeps
// every handle a Person has carried, so those references keep reaching
// the same Person after a rename. See the migration for why a retired
// handle is never given to anybody else.

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

// personIDPattern is the shape newPersonID mints.
var personIDPattern = regexp.MustCompile(`^psn_[0-9a-f]{16}$`)

// IsPersonID reports whether ref is written as a Person's identity rather
// than as a handle.
func IsPersonID(ref string) bool { return personIDPattern.MatchString(ref) }

// ResolvePersonRef finds the Person a reference names: a person_id, the
// handle they carry now, or a handle they carried before. "" with a nil
// error means nobody on record, which a caller making an authorization
// decision must treat as "matches nobody".
//
// Lifecycle is NOT checked here. A disabled Person is still the Person a
// grant names; whether they may act is the caller's separate question,
// and the portal door has already answered it for a signed-in member.
func (db *DB) ResolvePersonRef(ref string) (string, error) {
	return resolvePersonRef(db.sql, ref)
}

func resolvePersonRef(q personRower, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	var id string
	query := `SELECT person_id FROM person_handles WHERE handle = ?`
	if IsPersonID(ref) {
		query = `SELECT person_id FROM butler_members WHERE person_id = ?`
	}
	err := q.QueryRow(query, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: resolving the person behind %q: %w", ref, err)
	}
	return id, nil
}

// PersonHandle returns the handle a Person carries now, or ErrNoSuchPerson.
func (db *DB) PersonHandle(personID string) (string, error) {
	var handle string
	err := db.sql.QueryRow(`SELECT member FROM butler_members WHERE person_id = ?`, personID).Scan(&handle)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %q", ErrNoSuchPerson, personID)
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: reading the handle of %q: %w", personID, err)
	}
	return handle, nil
}

// PersonalFilesDir returns the folder name a Person's own Files space
// lives in (ADR-W037). The folder is recorded the first time it is asked
// for and kept for good, so a rename does not move the person to an empty
// folder. propose is the name to record when none is yet, normally the
// lower-cased handle; the caller has already checked it is a safe folder
// name.
//
// A proposal another Person already holds is refused rather than shared:
// two Persons in one folder is the leak this exists to prevent. The only
// shared folders are the ones 0093 found already shared by handles that
// differ in case, which the household had before this change.
//
// ok is false for a member with no Person on record.
func (db *DB) PersonalFilesDir(member, propose string) (dir string, ok bool, err error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return "", false, fmt.Errorf("runtimedb: reading a personal folder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := resolvePersonRef(tx, member)
	if err != nil || id == "" {
		return "", false, err
	}
	err = tx.QueryRow(`SELECT dir FROM person_file_dirs WHERE person_id = ?`, id).Scan(&dir)
	if err == nil {
		return dir, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("runtimedb: reading a personal folder: %w", err)
	}
	if propose == "" {
		return "", false, nil
	}
	var taken int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM person_file_dirs WHERE dir = ?`, propose).Scan(&taken); err != nil {
		return "", false, fmt.Errorf("runtimedb: reading a personal folder: %w", err)
	}
	if taken > 0 {
		return "", false, fmt.Errorf("runtimedb: the folder %q already belongs to another person", propose)
	}
	if _, err := tx.Exec(`INSERT INTO person_file_dirs (person_id, dir, created_at) VALUES (?, ?, ?)`,
		id, propose, now()); err != nil {
		return "", false, fmt.Errorf("runtimedb: recording a personal folder: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("runtimedb: recording a personal folder: %w", err)
	}
	return propose, true, nil
}
