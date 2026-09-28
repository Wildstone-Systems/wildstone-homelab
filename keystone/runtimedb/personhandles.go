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

// signedInPerson resolves the member who is ACTING — a signed-in portal
// member, a Butler's subject — by the handle they carry now, never through
// a retired one. A directory account that happens to share somebody's old
// name is not that somebody; only references somebody WROTE are resolved
// through history (resolvePersonRef). "" means no Person on record.
func signedInPerson(q personRower, member string) (string, error) {
	if member == "" {
		return "", nil
	}
	var id string
	err := q.QueryRow(`SELECT person_id FROM butler_members WHERE member = ?`, member).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: resolving the person behind %q: %w", member, err)
	}
	return id, nil
}

// HandleOwners returns every Person who carries or once carried a handle
// equal to login, ignoring case. Radicale lower-cases logins, so a
// calendar principal is claimed by any of them.
func (db *DB) HandleOwners(login string) ([]string, error) {
	rows, err := db.sql.Query(
		`SELECT DISTINCT person_id FROM person_handles WHERE handle = ? COLLATE NOCASE ORDER BY person_id`, login)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading who has been called %q: %w", login, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
// ok is false for a member with no Person on record. The member is matched
// by the handle they carry now: an account named after somebody's old
// handle does not get their folder.
func (db *DB) PersonalFilesDir(member, propose string) (dir string, ok bool, err error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return "", false, fmt.Errorf("runtimedb: reading a personal folder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := signedInPerson(tx, member)
	if err != nil {
		return "", false, err
	}
	if id == "" {
		// No Person: the caller falls back to the folder the login names,
		// unless that folder is already a Person's.
		if propose != "" {
			var taken int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM person_file_dirs WHERE dir = ?`, propose).Scan(&taken); err != nil {
				return "", false, fmt.Errorf("runtimedb: reading a personal folder: %w", err)
			}
			if taken > 0 {
				return "", false, fmt.Errorf("runtimedb: the folder %q belongs to a person on record", propose)
			}
		}
		return "", false, nil
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

// PersonHandles returns every handle the Person currently called member
// has carried: the current one first, then earlier ones, oldest first. A
// member with no Person on record has only their own name.
//
// The member is matched by the handle they carry NOW, not through retired
// handles, so a directory account that happens to share somebody's old
// name is not given that somebody's history.
func (db *DB) PersonHandles(member string) ([]string, error) {
	id, err := signedInPerson(db.sql, member)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return []string{member}, nil
	}
	rows, err := db.sql.Query(
		`SELECT handle FROM person_handles WHERE person_id = ? AND handle <> ? ORDER BY recorded_at, handle`, id, member)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading the handles of %q: %w", member, err)
	}
	defer rows.Close()
	out := []string{member}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
