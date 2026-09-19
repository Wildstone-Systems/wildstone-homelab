package runtimedb

// A person's chosen theme, and the themes a household pasted in (0058).
//
// THE STORE HOLDS A NAME, NOT A LOOK. Resolving a name to colours is
// keystone/theme's job and happens on read, so a later fix to the
// contrast derivation reaches every theme already chosen. What is
// stored here is the choice and the pasted source, which are the two
// things that cannot be recomputed.
//
// AN UNKNOWN NAME IS NO CHOICE. A household may remove a theme people
// are using — that is the Owner's call, and it should not fail. So the
// column is not a foreign key onto household_themes, and the caller
// that resolves a name treats one it cannot find as "this person has
// chosen nothing", which renders as the design's own look rather than
// as an error.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PersonTheme is one person's choice.
type PersonTheme struct {
	Theme     string    `json:"theme"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// HouseholdTheme is one theme a household pasted in.
type HouseholdTheme struct {
	Name      string    `json:"name"`
	Colors    string    `json:"colors"`
	AddedBy   string    `json:"addedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// GetPersonTheme returns the theme a person chose, or the empty string
// when they have chosen none.
//
// A handle that names nobody returns the empty string rather than
// ErrNoSuchPerson. This is read on the way to painting a page: a person
// whose record is mid-migration should see the default theme, not a
// broken dashboard.
func (db *DB) GetPersonTheme(member string) (string, error) {
	id, err := resolvePersonID(db.sql, member)
	if errors.Is(err, ErrNoSuchPerson) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var name string
	err = db.sql.QueryRow(`SELECT theme FROM person_theme WHERE person_id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: reading the theme for %q: %w", member, err)
	}
	return name, nil
}

// SetPersonTheme records a choice. An empty name clears it, which is
// how a person goes back to the household's own look — a DELETE rather
// than a sentinel row, so "chose the default" and "never chose" are the
// same state and cannot drift apart.
func (db *DB) SetPersonTheme(member, name string) error {
	id, err := resolvePersonID(db.sql, member)
	if err != nil {
		return err
	}
	if name == "" {
		if _, err := db.sql.Exec(`DELETE FROM person_theme WHERE person_id = ?`, id); err != nil {
			return fmt.Errorf("runtimedb: clearing the theme for %q: %w", member, err)
		}
		return nil
	}
	_, err = db.sql.Exec(
		`INSERT INTO person_theme (person_id, theme, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (person_id) DO UPDATE SET theme = excluded.theme, updated_at = excluded.updated_at`,
		id, name, now())
	if err != nil {
		return fmt.Errorf("runtimedb: setting the theme for %q: %w", member, err)
	}
	return nil
}

// ListHouseholdThemes returns the pasted themes, by name.
func (db *DB) ListHouseholdThemes() ([]HouseholdTheme, error) {
	rows, err := db.sql.Query(
		`SELECT name, colors, added_by, updated_at FROM household_themes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing themes: %w", err)
	}
	defer rows.Close()
	var out []HouseholdTheme
	for rows.Next() {
		var t HouseholdTheme
		var updated string
		if err := rows.Scan(&t.Name, &t.Colors, &t.AddedBy, &updated); err != nil {
			return nil, fmt.Errorf("runtimedb: reading a theme: %w", err)
		}
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveHouseholdTheme adds a pasted theme, or replaces one of the same
// name.
//
// REPLACING IS THE POINT of upsert here rather than a refusal: the
// normal reason to paste a theme whose name is already taken is that
// its author published a revision, and making a household delete first
// would break every person currently on it for as long as it took them
// to paste the new one.
//
// The name and the palette are validated by the caller (the API layer,
// against keystone/theme) before they reach this function. That
// asymmetry is deliberate and matches the rest of this package: the
// store enforces shape, the layer above enforces meaning.
func (db *DB) SaveHouseholdTheme(name, colors, by string) error {
	switch {
	case name == "":
		return fmt.Errorf("runtimedb: a theme needs a name")
	case colors == "":
		return fmt.Errorf("runtimedb: a theme needs colours")
	}
	_, err := db.sql.Exec(
		`INSERT INTO household_themes (name, colors, added_by, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET
		   colors = excluded.colors, added_by = excluded.added_by, updated_at = excluded.updated_at`,
		name, colors, by, now())
	if err != nil {
		return fmt.Errorf("runtimedb: saving the theme %q: %w", name, err)
	}
	return nil
}

// DeleteHouseholdTheme removes a pasted theme.
//
// The people who chose it are deliberately left alone rather than
// migrated to something else: their row now names a theme nobody can
// resolve, which reads as no choice, which is the design's own look. If
// the Owner pastes the theme back tomorrow, everybody who had it has it
// again — which is what somebody who removed a theme by mistake wants.
func (db *DB) DeleteHouseholdTheme(name string) (bool, error) {
	res, err := db.sql.Exec(`DELETE FROM household_themes WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("runtimedb: removing the theme %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("runtimedb: removing the theme %q: %w", name, err)
	}
	return n > 0, nil
}
