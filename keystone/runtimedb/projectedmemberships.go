package runtimedb

// What Wildstone put in the directory (ADR-W033 design §4; slice 8).
//
// The record that makes removal safe. Projection may take out a
// membership recorded here, because it is the one that put it there.
// Anything absent from this table was added by a person in LLDAP's own
// UI, and is reported rather than deleted — which is how the failure
// LdapGroupSpec named stays impossible after the additive-only posture
// is reversed:
//
//	a system that thinks it does is one that removes your sister from
//	photos_users because a YAML file did not mention her.
//
// Wildstone never claimed the sister, so it never removes her.

import (
	"fmt"
	"strings"
	"time"
)

// Membership is one directory membership Wildstone asserted.
type Membership struct {
	PersonID string `json:"personId"`
	// LdapGroup is the DIRECTORY group's name, e.g. "docs_users" — not
	// a slice-6 group_id. This is a record of what exists in LLDAP, and
	// LLDAP has never heard of a group_id.
	LdapGroup string    `json:"ldapGroup"`
	PlacedAt  time.Time `json:"placedAt"`
}

// PlacedMemberships returns every membership Wildstone has asserted,
// as person_id -> the directory groups it claimed for them.
func (db *DB) PlacedMemberships() (map[string][]string, error) {
	rows, err := db.sql.Query(
		`SELECT person_id, ldap_group FROM projected_memberships ORDER BY person_id, ldap_group`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading placed memberships: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var person, group string
		if err := rows.Scan(&person, &group); err != nil {
			return nil, fmt.Errorf("runtimedb: reading placed memberships: %w", err)
		}
		out[person] = append(out[person], group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtimedb: reading placed memberships: %w", err)
	}
	return out, nil
}

// ClaimMemberships records what projection now asserts for one Person,
// replacing whatever it asserted before.
//
// Replace rather than merge, and that is the point: the claim is "these
// and no others", so a membership dropped from the set stops being
// Wildstone's the moment it is no longer wanted. The caller has already
// asked the directory to remove it; forgetting the claim afterwards is
// what stops a later pass from removing it a second time, or — worse —
// from treating a membership somebody re-added by hand as still
// Wildstone's to take away.
func (db *DB) ClaimMemberships(personID string, ldapGroups []string) error {
	if personID == "" {
		return fmt.Errorf("runtimedb: claiming memberships needs a Person")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: claiming memberships: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`DELETE FROM projected_memberships WHERE person_id = ?`, personID); err != nil {
		return fmt.Errorf("runtimedb: claiming memberships: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, g := range ldapGroups {
		if strings.TrimSpace(g) == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO projected_memberships (person_id, ldap_group, placed_at)
			 VALUES (?, ?, ?)`, personID, g, now); err != nil {
			return fmt.Errorf("runtimedb: claiming %q for %q: %w", g, personID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: claiming memberships: %w", err)
	}
	return nil
}

// PersonForDirectoryAccount resolves a directory username to a Person,
// and refuses when that is ambiguous.
//
// THE HANDLE IS NOT AN IDENTITY, and this is the one place in projection
// that has to pretend otherwise. `LdapUser.Username` and
// `butler_members.member` are two registries joined by nothing but a
// string, and auth_console.go says what that is worth:
//
//	equal names are not proof of identity, and may only be a reason to
//	keep somebody OUT, never to let them in.
//
// Projection PLACES memberships, so it is using the match in the
// direction that comment warns about. It compensates by refusing every
// case the comment is about: a handle matching two Person records, or
// matching one who is not in good standing, resolves to nothing and
// projection does nothing for that account. An account with no Person
// at all is not an error either — the installer creates directory
// accounts that Wildstone does not manage, and they keep whatever
// memberships a human gave them.
//
// The second return value distinguishes "no such Person" from "more
// than one", because the caller reports them differently: the first is
// ordinary, and the second is a household that has a problem.
func (db *DB) PersonForDirectoryAccount(username string) (personID string, ambiguous bool, err error) {
	members, err := db.ButlerMembersByHandleFold(username)
	if err != nil {
		return "", false, err
	}
	var active []ButlerMember
	for _, m := range members {
		if m.Active() {
			active = append(active, m)
		}
	}
	switch len(active) {
	case 0:
		return "", false, nil
	case 1:
		return active[0].PersonID, false, nil
	default:
		return "", true, nil
	}
}
