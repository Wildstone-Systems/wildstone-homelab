package runtimedb

// Assignment and addressing (ADR-W031 A5/A7, work packages B3/B4/B5).
//
// Everything a Butler does is something that named it beforehand. A
// routine names its Butler when it is written; a task snapshots one
// when it is created; a conversation reaches one by the address it
// arrived on.
//
// None of that is a decision made by inspection at run time, and the
// reason is A2: anything that reads the material in order to decide
// where it should go has already sent it somewhere before the decision
// that was supposed to protect it.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoButlerForAddress reports that a message arrived somewhere no
// Butler answers.
//
// It is not a routing failure to be recovered from by picking a Butler.
// Picking one would be the dispatcher A7 refuses; an unaddressed
// message is a configuration gap, and saying so is the whole remedy.
var ErrNoButlerForAddress = errors.New("runtimedb: no butler answers at that address")

// AssignRoutineButler names the Butler a routine runs as.
//
// An empty name means the routine needs no model — a deterministic rule
// with no reasoning step, which most routines are. That is a real
// answer and not a missing one, so it is allowed and recorded.
func (db *DB) AssignRoutineButler(routine, butler, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: assigning a routine's butler needs who assigned it")
	}
	if butler != "" {
		b, err := db.GetButler(butler)
		if err != nil {
			return err
		}
		if !b.Live() {
			return fmt.Errorf("%w: %s has been retired or disabled, so a routine cannot be assigned to it",
				ErrButlerUnreachable, butler)
		}
	}
	res, err := db.sql.Exec(
		`UPDATE household_routines SET butler = ?, updated_at = ?, updated_by = ?
		 WHERE name = ? AND disabled_at = ''`, butler, now(), by, routine)
	if err != nil {
		return fmt.Errorf("runtimedb: assigning a routine's butler: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RoutineButler returns the Butler a routine runs as, and whether it
// has one at all.
func (db *DB) RoutineButler(routine string) (butler string, assigned bool, err error) {
	err = db.sql.QueryRow(
		`SELECT butler FROM household_routines WHERE name = ?`, routine).Scan(&butler)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("runtimedb: reading a routine's butler: %w", err)
	}
	return butler, butler != "", nil
}

// ResolveRoutineButler is what a routine runner calls before a step
// that needs a model.
//
// A routine that reaches a reasoning step with no Butler assigned does
// not get one chosen for it. That would be the run-time decision this
// design removed, and it would pick a model nobody agreed to for
// material nobody classified.
func (db *DB) ResolveRoutineButler(routine string) (Butler, error) {
	name, assigned, err := db.RoutineButler(routine)
	if err != nil {
		return Butler{}, err
	}
	if !assigned {
		return Butler{}, fmt.Errorf(
			"%w: routine %q needs a model but has no butler assigned — assign one rather than having a model chosen for it (ADR-W031 A2)",
			ErrNotFound, routine)
	}
	b, err := db.GetButler(name)
	if err != nil {
		return Butler{}, err
	}
	if !b.Reachable() {
		// A6: the work waits. It does not move to another Butler,
		// because the substitution is the disclosure this exists to
		// prevent.
		return Butler{}, fmt.Errorf("%w: %s runs on %s, which is unavailable — %q waits rather than running on another model (ADR-W031 A6)",
			ErrButlerUnreachable, b.Name, b.Route, routine)
	}
	return b, nil
}

// SetTaskButler snapshots the Butler a task runs as.
//
// Snapshotted, like the task's surface and profile: which model
// answered a piece of work is a fact about that work, and a lookup that
// changes when somebody reassigns the Butler next week does not answer
// it.
func (db *DB) SetTaskButler(taskID, butler string) error {
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(
		`UPDATE butler_tasks SET butler = ?, updated_at = ? WHERE seq = ?`, b.Name, now(), seq)
	if err != nil {
		return fmt.Errorf("runtimedb: setting a task's butler: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TaskButler returns the Butler a task ran as.
func (db *DB) TaskButler(taskID string) (string, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return "", err
	}
	var butler string
	err = db.sql.QueryRow(`SELECT butler FROM butler_tasks WHERE seq = ?`, seq).Scan(&butler)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: reading a task's butler: %w", err)
	}
	return butler, nil
}

// ButlerAddress is one place a Butler answers.
type ButlerAddress struct {
	Seq        int64     `json:"seq"`
	Butler     string    `json:"butler"`
	Transport  string    `json:"transport"`
	ExternalID string    `json:"externalId"`
	Label      string    `json:"label,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	CreatedBy  string    `json:"createdBy"`
}

// AddButlerAddress gives a Butler somewhere to be reached.
//
// The external id is whatever the transport uses to tell one
// destination from another. On Telegram that is a bot id if each Butler
// has its own bot, or a topic id if they share one — this function does
// not care, and that is deliberate. Both are addresses; which one a
// deployment uses is its choice, and neither requires reading a message
// to work out where it should go.
func (db *DB) AddButlerAddress(butler, transport, externalID, label, by string) (int64, error) {
	switch {
	case transport == "" || externalID == "":
		return 0, fmt.Errorf("runtimedb: a butler address needs a transport and the transport's own id for it")
	case by == "":
		return 0, fmt.Errorf("runtimedb: adding a butler address needs who added it")
	}
	b, err := db.GetButler(butler)
	if err != nil {
		return 0, err
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_addresses (butler, transport, external_id, label, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		b.Seq, transport, externalID, label, now(), by)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("%s/%s already answers for another butler — one address, one butler, or the same message could be answered by either (ADR-W031 A7)",
				transport, externalID)
		}
		return 0, fmt.Errorf("runtimedb: adding a butler address: %w", err)
	}
	return res.LastInsertId()
}

// RevokeButlerAddressFor stops ONE named Butler answering at an
// address.
//
// The name is part of the match rather than decoration: an address is
// unique across Butlers, so revoking by (transport, id) alone succeeds
// against whichever Butler happens to hold it — which is the wrong one
// whenever the caller named a different Butler.
func (db *DB) RevokeButlerAddressFor(butler, transport, externalID, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: revoking a butler address needs who revoked it")
	}
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(
		`UPDATE butler_addresses SET revoked_at = ?, revoked_by = ?
		 WHERE butler = ? AND transport = ? AND external_id = ? AND revoked_at = ''`,
		now(), by, b.Seq, transport, externalID)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking a butler address: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeButlerAddress stops whichever Butler answers at an address.
//
// Prefer RevokeButlerAddressFor where the caller knows the Butler.
func (db *DB) RevokeButlerAddress(transport, externalID, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: revoking a butler address needs who revoked it")
	}
	res, err := db.sql.Exec(
		`UPDATE butler_addresses SET revoked_at = ?, revoked_by = ?
		 WHERE transport = ? AND external_id = ? AND revoked_at = ''`,
		now(), by, transport, externalID)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking a butler address: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListButlerAddresses returns where a Butler currently answers.
func (db *DB) ListButlerAddresses(butler string) ([]ButlerAddress, error) {
	b, err := db.GetButler(butler)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT seq, transport, external_id, label, created_at, created_by
		 FROM butler_addresses WHERE butler = ? AND revoked_at = ''
		 ORDER BY transport, external_id`, b.Seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler addresses: %w", err)
	}
	defer rows.Close()
	var out []ButlerAddress
	for rows.Next() {
		a := ButlerAddress{Butler: b.Name}
		var created string
		if err := rows.Scan(&a.Seq, &a.Transport, &a.ExternalID, &a.Label, &created, &a.CreatedBy); err != nil {
			return nil, err
		}
		if a.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveButlerAtAddress answers "which Butler is this message for",
// from where it arrived and from nothing else.
//
// The member is checked too, because arriving at a Butler's address is
// not permission to talk to it: A10's access list still decides, and a
// person who was never granted this Butler is refused here exactly as
// they would be in the Console.
func (db *DB) ResolveButlerAtAddress(transport, externalID, member string) (Butler, error) {
	var name string
	err := db.sql.QueryRow(
		`SELECT b.name FROM butler_addresses a JOIN butlers b ON b.seq = a.butler
		 WHERE a.transport = ? AND a.external_id = ? AND a.revoked_at = ''`,
		transport, externalID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return Butler{}, fmt.Errorf("%w: %s/%s", ErrNoButlerForAddress, transport, externalID)
	}
	if err != nil {
		return Butler{}, fmt.Errorf("runtimedb: resolving a butler address: %w", err)
	}
	return db.ResolveButlerFor(name, member)
}

// PoolDisclosure is what promoting a record into the shared pool would
// expose, and to whom.
type PoolDisclosure struct {
	Memory int64 `json:"memory"`
	// Butlers gain read access. Named, because "some Butlers" is not
	// something a person can weigh.
	Butlers []string `json:"butlers"`
	// LeavesHouse is true when any of them runs on a cloud model. It is
	// the fact that decides whether this is a small act or a large one.
	LeavesHouse bool `json:"leavesHouse"`
	// CloudButlers names which ones, so the warning can be specific
	// rather than ominous.
	CloudButlers []string `json:"cloudButlers,omitempty"`
}

// PreviewPoolPromotion answers what promoting this record WOULD expose,
// without promoting it.
//
// A5 says promotion is a disclosure decision presented as one, and a
// decision can only be weighed before it is taken. Returning the
// consequence afterwards, as PromoteMemoryToSharedPool does, is a
// record; this is the part a person reads first.
func (db *DB) PreviewPoolPromotion(seq int64) (PoolDisclosure, error) {
	m, err := db.GetMemory(seq)
	if err != nil {
		return PoolDisclosure{}, err
	}
	d := PoolDisclosure{Memory: m.Seq}
	all, err := db.ListButlers()
	if err != nil {
		return PoolDisclosure{}, err
	}
	for _, b := range all {
		if !b.Live() {
			continue
		}
		d.Butlers = append(d.Butlers, b.Name)
		if b.RouteKind == RouteCloud {
			d.LeavesHouse = true
			d.CloudButlers = append(d.CloudButlers, b.Name)
		}
	}
	return d, nil
}

// RecordPoolDisclosure writes down what a promotion exposed.
//
// The Butler names and the verdict are stored as text, as they were at
// the time. A join would answer today's question; this answers the one
// that was actually decided, which is what somebody asking a month
// later needs.
func (db *DB) RecordPoolDisclosure(d PoolDisclosure, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: recording a disclosure needs who made it")
	}
	// JSON rather than a joined string. A comma in a Butler's name
	// would split one name into two fabricated ones on read-back, and
	// an audit record that invents names is worse than none.
	names, err := json.Marshal(struct {
		Butlers []string `json:"butlers"`
		Cloud   []string `json:"cloud"`
	}{d.Butlers, d.CloudButlers})
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(
		`INSERT INTO butler_pool_disclosures (memory, butlers, left_house, promoted_at, promoted_by)
		 VALUES (?, ?, ?, ?, ?)`,
		d.Memory, string(names), boolInt(d.LeavesHouse), now(), by)
	if err != nil {
		return fmt.Errorf("runtimedb: recording a pool disclosure: %w", err)
	}
	return nil
}

// PoolDisclosuresFor returns what was exposed about one record, newest
// first.
func (db *DB) PoolDisclosuresFor(memory int64) ([]PoolDisclosure, error) {
	rows, err := db.sql.Query(
		`SELECT memory, butlers, left_house FROM butler_pool_disclosures
		 WHERE memory = ? ORDER BY seq DESC`, memory)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading pool disclosures: %w", err)
	}
	defer rows.Close()
	var out []PoolDisclosure
	for rows.Next() {
		var d PoolDisclosure
		var butlers string
		var left int
		if err := rows.Scan(&d.Memory, &butlers, &left); err != nil {
			return nil, err
		}
		if butlers != "" {
			var stored struct {
				Butlers []string `json:"butlers"`
				Cloud   []string `json:"cloud"`
			}
			if err := json.Unmarshal([]byte(butlers), &stored); err != nil {
				return nil, fmt.Errorf("runtimedb: bad disclosure record for memory %d: %w", d.Memory, err)
			}
			d.Butlers, d.CloudButlers = stored.Butlers, stored.Cloud
		}
		d.LeavesHouse = left != 0
		out = append(out, d)
	}
	return out, rows.Err()
}
