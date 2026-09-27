package runtimedb

// Household lists (packages/lists-wildstone): shopping, chores, packing.
//
// Every list belongs to the household. Who added or ticked an item is
// recorded for people to read, and is never an access rule — the access
// rules are the Lists card's directory group and the household surface
// an Agent is on, both decided before this store is reached.
//
// Every write reports what it actually changed. An Agent answers from
// that report ("milk was already on the list", "there is no eggs on it")
// rather than from what it meant to do, and a write that changed
// something is what its receipt vouches for.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds. A household list is short; one that is not has become a
// database, and an Agent reading 5,000 items back to somebody is a bug.
const (
	MaxHouseholdLists     = 50
	MaxHouseholdListItems = 200
	MaxListNameRunes      = 60
	MaxListItemRunes      = 200
)

// ErrListNotFound is "there is no list called that".
var ErrListNotFound = fmt.Errorf("%w: no such list", ErrNotFound)

// HouseholdList is one list and, when asked for, its items.
type HouseholdList struct {
	Seq       int64               `json:"seq"`
	Name      string              `json:"name"`
	CreatedAt time.Time           `json:"createdAt"`
	CreatedBy string              `json:"createdBy,omitempty"`
	UpdatedAt time.Time           `json:"updatedAt"`
	Open      int                 `json:"open"`
	Done      int                 `json:"done"`
	Items     []HouseholdListItem `json:"items,omitempty"`
}

// HouseholdListItem is one line of a list.
type HouseholdListItem struct {
	Seq       int64     `json:"seq"`
	Text      string    `json:"text"`
	Checked   bool      `json:"checked"`
	AddedAt   time.Time `json:"addedAt"`
	AddedBy   string    `json:"addedBy,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
	CheckedBy string    `json:"checkedBy,omitempty"`
}

// ListChange is what one write did, item by item, in the spelling the
// caller used.
type ListChange struct {
	List string `json:"list"`
	// Created is true when this write made the list.
	Created bool `json:"created,omitempty"`
	// Changed are the items the write actually changed.
	Changed []string `json:"changed,omitempty"`
	// Unchanged are items that were already in the requested state
	// (added but already there and open; ticked but already ticked).
	Unchanged []string `json:"unchanged,omitempty"`
	// Missing are items the write was asked about that the list does not
	// have.
	Missing []string `json:"missing,omitempty"`
}

// listKey is the spelling lists and items are matched on: trimmed,
// inner whitespace collapsed, case-folded.
func listKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// cleanListText is the spelling kept: trimmed and whitespace-collapsed,
// case as typed.
func cleanListText(s string) string { return strings.Join(strings.Fields(s), " ") }

func validListName(name string) (string, error) {
	name = cleanListText(name)
	switch {
	case name == "":
		return "", fmt.Errorf("runtimedb: a list needs a name")
	case utf8.RuneCountInString(name) > MaxListNameRunes:
		return "", fmt.Errorf("runtimedb: a list name is at most %d characters", MaxListNameRunes)
	}
	return name, nil
}

// cleanListItems cleans, validates and de-duplicates the items one write
// names, keeping the first spelling of each.
func cleanListItems(items []string) ([]string, error) {
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		it = cleanListText(it)
		if it == "" {
			continue
		}
		if utf8.RuneCountInString(it) > MaxListItemRunes {
			return nil, fmt.Errorf("runtimedb: a list item is at most %d characters", MaxListItemRunes)
		}
		if k := listKey(it); !seen[k] {
			seen[k] = true
			out = append(out, it)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("runtimedb: name at least one item")
	}
	return out, nil
}

func parseListTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// HouseholdLists returns every list with its open and done counts,
// most recently changed first. Items are not included.
func (db *DB) HouseholdLists() ([]HouseholdList, error) {
	if err := db.gate(GateRetrieval); err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`SELECT l.seq, l.name, l.created_at, l.created_by, l.updated_at,
		COALESCE(SUM(CASE WHEN i.checked = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN i.checked = 1 THEN 1 ELSE 0 END), 0)
		FROM household_lists l LEFT JOIN household_list_items i ON i.list = l.seq
		GROUP BY l.seq ORDER BY l.updated_at DESC, l.seq DESC`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing household lists: %w", err)
	}
	defer rows.Close()
	var out []HouseholdList
	for rows.Next() {
		var l HouseholdList
		var created, updated string
		if err := rows.Scan(&l.Seq, &l.Name, &created, &l.CreatedBy, &updated, &l.Open, &l.Done); err != nil {
			return nil, err
		}
		l.CreatedAt, l.UpdatedAt = parseListTime(created), parseListTime(updated)
		out = append(out, l)
	}
	return out, rows.Err()
}

// HouseholdList returns one list with its items: open items first, in
// the order they were added, then ticked ones.
func (db *DB) HouseholdList(name string) (HouseholdList, error) {
	if err := db.gate(GateRetrieval); err != nil {
		return HouseholdList{}, err
	}
	l, err := getListTx(db.sql, name)
	if err != nil {
		return HouseholdList{}, err
	}
	rows, err := db.sql.Query(`SELECT seq, text, checked, added_at, added_by, checked_at, checked_by
		FROM household_list_items WHERE list = ? ORDER BY checked, seq`, l.Seq)
	if err != nil {
		return HouseholdList{}, fmt.Errorf("runtimedb: reading list %q: %w", l.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var it HouseholdListItem
		var checked int
		var added, checkedAt string
		if err := rows.Scan(&it.Seq, &it.Text, &checked, &added, &it.AddedBy, &checkedAt, &it.CheckedBy); err != nil {
			return HouseholdList{}, err
		}
		it.Checked = checked == 1
		it.AddedAt, it.CheckedAt = parseListTime(added), parseListTime(checkedAt)
		if it.Checked {
			l.Done++
		} else {
			l.Open++
		}
		l.Items = append(l.Items, it)
	}
	return l, rows.Err()
}

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func getListTx(q queryer, name string) (HouseholdList, error) {
	var l HouseholdList
	var created, updated string
	err := q.QueryRow(`SELECT seq, name, created_at, created_by, updated_at FROM household_lists WHERE name_key = ?`,
		listKey(name)).Scan(&l.Seq, &l.Name, &created, &l.CreatedBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return HouseholdList{}, ErrListNotFound
	}
	if err != nil {
		return HouseholdList{}, fmt.Errorf("runtimedb: finding list %q: %w", name, err)
	}
	l.CreatedAt, l.UpdatedAt = parseListTime(created), parseListTime(updated)
	return l, nil
}

// AddListItems puts items on a list, creating the list if it does not
// exist. An item already on the list and open is left as it is; one that
// was ticked is un-ticked, because asking for milk again means milk is
// needed again.
func (db *DB) AddListItems(list string, items []string, by string) (ListChange, error) {
	if err := db.gate(GateIngestion); err != nil {
		return ListChange{}, err
	}
	name, err := validListName(list)
	if err != nil {
		return ListChange{}, err
	}
	clean, err := cleanListItems(items)
	if err != nil {
		return ListChange{}, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return ListChange{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stamp := now()
	change := ListChange{}
	l, err := getListTx(tx, name)
	if errors.Is(err, ErrListNotFound) {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM household_lists`).Scan(&n); err != nil {
			return ListChange{}, err
		}
		if n >= MaxHouseholdLists {
			return ListChange{}, fmt.Errorf("runtimedb: the household already has %d lists; delete one first", MaxHouseholdLists)
		}
		res, err := tx.Exec(`INSERT INTO household_lists (name, name_key, created_at, created_by, updated_at) VALUES (?, ?, ?, ?, ?)`,
			name, listKey(name), stamp, by, stamp)
		if err != nil {
			return ListChange{}, fmt.Errorf("runtimedb: creating list %q: %w", name, err)
		}
		l.Seq, _ = res.LastInsertId()
		l.Name = name
		change.Created = true
	} else if err != nil {
		return ListChange{}, err
	}
	change.List = l.Name
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM household_list_items WHERE list = ?`, l.Seq).Scan(&count); err != nil {
		return ListChange{}, err
	}
	for _, it := range clean {
		var seq int64
		var checked int
		err := tx.QueryRow(`SELECT seq, checked FROM household_list_items WHERE list = ? AND text_key = ?`,
			l.Seq, listKey(it)).Scan(&seq, &checked)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if count >= MaxHouseholdListItems {
				return ListChange{}, fmt.Errorf("runtimedb: %s already has %d items; clear some first", l.Name, MaxHouseholdListItems)
			}
			if _, err := tx.Exec(`INSERT INTO household_list_items (list, text, text_key, added_at, added_by) VALUES (?, ?, ?, ?, ?)`,
				l.Seq, it, listKey(it), stamp, by); err != nil {
				return ListChange{}, fmt.Errorf("runtimedb: adding %q to %s: %w", it, l.Name, err)
			}
			count++
			change.Changed = append(change.Changed, it)
		case err != nil:
			return ListChange{}, err
		case checked == 1:
			if _, err := tx.Exec(`UPDATE household_list_items SET checked = 0, checked_at = '', checked_by = '' WHERE seq = ?`, seq); err != nil {
				return ListChange{}, err
			}
			change.Changed = append(change.Changed, it)
		default:
			change.Unchanged = append(change.Unchanged, it)
		}
	}
	if err := touchList(tx, l.Seq, stamp, len(change.Changed) > 0 || change.Created); err != nil {
		return ListChange{}, err
	}
	return change, tx.Commit()
}

// SetListItemsChecked ticks (checked) or un-ticks items on an existing
// list.
func (db *DB) SetListItemsChecked(list string, items []string, checked bool, by string) (ListChange, error) {
	return db.changeListItems(list, items, func(tx *sql.Tx, seq int64, isChecked bool, stamp string) (bool, error) {
		if isChecked == checked {
			return false, nil
		}
		var err error
		if checked {
			_, err = tx.Exec(`UPDATE household_list_items SET checked = 1, checked_at = ?, checked_by = ? WHERE seq = ?`, stamp, by, seq)
		} else {
			_, err = tx.Exec(`UPDATE household_list_items SET checked = 0, checked_at = '', checked_by = '' WHERE seq = ?`, seq)
		}
		return err == nil, err
	})
}

// RemoveListItems takes items off an existing list altogether.
func (db *DB) RemoveListItems(list string, items []string) (ListChange, error) {
	return db.changeListItems(list, items, func(tx *sql.Tx, seq int64, _ bool, _ string) (bool, error) {
		_, err := tx.Exec(`DELETE FROM household_list_items WHERE seq = ?`, seq)
		return err == nil, err
	})
}

// changeListItems applies apply to each named item of an existing list.
func (db *DB) changeListItems(list string, items []string, apply func(tx *sql.Tx, seq int64, checked bool, stamp string) (bool, error)) (ListChange, error) {
	if err := db.gate(GateIngestion); err != nil {
		return ListChange{}, err
	}
	clean, err := cleanListItems(items)
	if err != nil {
		return ListChange{}, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return ListChange{}, err
	}
	defer func() { _ = tx.Rollback() }()
	l, err := getListTx(tx, list)
	if err != nil {
		return ListChange{}, err
	}
	stamp := now()
	change := ListChange{List: l.Name}
	for _, it := range clean {
		var seq int64
		var checked int
		err := tx.QueryRow(`SELECT seq, checked FROM household_list_items WHERE list = ? AND text_key = ?`,
			l.Seq, listKey(it)).Scan(&seq, &checked)
		if errors.Is(err, sql.ErrNoRows) {
			change.Missing = append(change.Missing, it)
			continue
		}
		if err != nil {
			return ListChange{}, err
		}
		did, err := apply(tx, seq, checked == 1, stamp)
		if err != nil {
			return ListChange{}, fmt.Errorf("runtimedb: changing %q on %s: %w", it, l.Name, err)
		}
		if did {
			change.Changed = append(change.Changed, it)
		} else {
			change.Unchanged = append(change.Unchanged, it)
		}
	}
	if err := touchList(tx, l.Seq, stamp, len(change.Changed) > 0); err != nil {
		return ListChange{}, err
	}
	return change, tx.Commit()
}

// ClearCheckedListItems removes every ticked item from a list and
// reports how many went.
func (db *DB) ClearCheckedListItems(list string) (string, int, error) {
	if err := db.gate(GateIngestion); err != nil {
		return "", 0, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback() }()
	l, err := getListTx(tx, list)
	if err != nil {
		return "", 0, err
	}
	res, err := tx.Exec(`DELETE FROM household_list_items WHERE list = ? AND checked = 1`, l.Seq)
	if err != nil {
		return "", 0, fmt.Errorf("runtimedb: clearing %s: %w", l.Name, err)
	}
	n, _ := res.RowsAffected()
	if err := touchList(tx, l.Seq, now(), n > 0); err != nil {
		return "", 0, err
	}
	return l.Name, int(n), tx.Commit()
}

// DeleteHouseholdList removes a list and everything on it, and reports
// the list's name as stored and how many items it held.
func (db *DB) DeleteHouseholdList(list string) (string, int, error) {
	if err := db.gate(GateIngestion); err != nil {
		return "", 0, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback() }()
	l, err := getListTx(tx, list)
	if err != nil {
		return "", 0, err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM household_list_items WHERE list = ?`, l.Seq).Scan(&n); err != nil {
		return "", 0, err
	}
	if _, err := tx.Exec(`DELETE FROM household_lists WHERE seq = ?`, l.Seq); err != nil {
		return "", 0, fmt.Errorf("runtimedb: deleting %s: %w", l.Name, err)
	}
	return l.Name, n, tx.Commit()
}

func touchList(tx *sql.Tx, seq int64, stamp string, changed bool) error {
	if !changed {
		return nil
	}
	_, err := tx.Exec(`UPDATE household_lists SET updated_at = ? WHERE seq = ?`, stamp, seq)
	return err
}

// RetireMovedListItems finishes moving a list's items somewhere else —
// PantryOS's grocery list, in practice (ADR-W042 A3.1, #387). It takes
// the moved items off the list and, once nothing open is left on it,
// removes the list altogether so the household is not shown two grocery
// lists. Ticked items are the old list's history and go with it.
//
// One transaction, and the emptiness is judged inside it: an item added
// to the old list while the move ran keeps the list, and is left for the
// next move rather than lost.
//
// It reports whether the list was removed and how many open items are
// still on it.
func (db *DB) RetireMovedListItems(list string, moved []string) (removed bool, open int, err error) {
	if err := db.gate(GateIngestion); err != nil {
		return false, 0, err
	}
	clean, err := cleanListItems(moved)
	if err != nil {
		return false, 0, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	l, err := getListTx(tx, list)
	if err != nil {
		return false, 0, err
	}
	for _, it := range clean {
		if _, err := tx.Exec(`DELETE FROM household_list_items WHERE list = ? AND text_key = ?`, l.Seq, listKey(it)); err != nil {
			return false, 0, fmt.Errorf("runtimedb: taking %q off %s: %w", it, l.Name, err)
		}
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM household_list_items WHERE list = ? AND checked = 0`, l.Seq).Scan(&open); err != nil {
		return false, 0, err
	}
	if open == 0 {
		if _, err := tx.Exec(`DELETE FROM household_lists WHERE seq = ?`, l.Seq); err != nil {
			return false, 0, fmt.Errorf("runtimedb: removing %s: %w", l.Name, err)
		}
	} else if err := touchList(tx, l.Seq, now(), len(clean) > 0); err != nil {
		return false, 0, err
	}
	return open == 0, open, tx.Commit()
}
