package runtimedb

// Household routine authoring and review (ADR-W030 A6/A8/A8.1, P13).
//
// Two things this file keeps apart, and the whole design follows from
// them.
//
// Native and Wildstone ownership. A native routine runs inside an
// application; Wildstone records that it exists, by the application's
// own id, so a household can see the whole picture without Wildstone
// pretending to own it. A Wildstone routine is a cross-app document
// Wildstone runs. Blurring the two produces a Console offering to edit
// something it cannot, or two systems both firing on one departure —
// so ownership is immutable at the schema.
//
// Approval. Activating a routine version is a Plan, and only a human
// Owner approves it, at its exact version, in the Console (ADR-W033
// E2/E3). The household_review_tokens table from 0028 remains in the
// schema for its history; nothing issues or redeems a token any more.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Routine ownership.
const (
	RoutineNative    = "native"
	RoutineWildstone = "wildstone"
)

// Routine states. Trial is a routine that runs but performs no actions —
// the preview a household wants before letting something loose.
const (
	RoutineDraft    = "draft"
	RoutineTrial    = "trial"
	RoutineEnabled  = "enabled"
	RoutineDisabled = "disabled"
)

// HouseholdRoutine is one routine, native or Wildstone-run.
type HouseholdRoutine struct {
	OutputAudience   string    `json:"outputAudience"`
	ApprovedRevision int       `json:"approvedRevision"`
	RequiredSecrets  []string  `json:"requiredSecrets,omitempty"`
	RequiredMemory   []int64   `json:"requiredMemory,omitempty"`
	Seq              int64     `json:"seq"`
	Name             string    `json:"name"`
	Ownership        string    `json:"ownership"`
	ExternalID       string    `json:"externalId,omitempty"`
	Connection       string    `json:"connection,omitempty"`
	Author           string    `json:"author"`
	Revision         int       `json:"revision"`
	State            string    `json:"state"`
	Document         string    `json:"document"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	UpdatedBy        string    `json:"updatedBy"`
	DisabledAt       time.Time `json:"disabledAt,omitzero"`
	DisabledBy       string    `json:"disabledBy,omitempty"`
	RevokedAt        string    `json:"revokedAt,omitempty"`
	DeletedAt        string    `json:"deletedAt,omitempty"`
	PurgedAt         string    `json:"purgedAt,omitempty"`
	PausedReason     string    `json:"pausedReason,omitempty"`
}

// Active reports whether this routine may run right now. Disabled is
// checked here rather than inferred from state, because the runner
// checks it before every step and "exact and immediate" has to mean the
// same thing in both places.
func (r HouseholdRoutine) Active() bool {
	return r.DisabledAt.IsZero() && r.RevokedAt == "" && r.DeletedAt == "" && r.PausedReason == "" && (r.State == RoutineEnabled || r.State == RoutineTrial)
}

// PerformsActions reports whether this routine may actually do things.
// A trial routine runs its reads and its reasoning and performs
// nothing — which is what makes a preview a preview.
func (r HouseholdRoutine) PerformsActions() bool {
	return r.DisabledAt.IsZero() && r.RevokedAt == "" && r.DeletedAt == "" && r.PausedReason == "" && r.State == RoutineEnabled
}

// CreateHouseholdRoutine records a routine.
func (db *DB) CreateHouseholdRoutine(r HouseholdRoutine) (int64, error) {
	switch r.Ownership {
	case RoutineNative:
		if r.ExternalID == "" || r.Connection == "" {
			return 0, fmt.Errorf("runtimedb: a native routine needs the application's own id and the connection it lives on (ADR-W030 A6)")
		}
	case RoutineWildstone:
		if r.ExternalID != "" || r.Connection != "" {
			return 0, fmt.Errorf("runtimedb: a Wildstone routine has no external id: Wildstone runs it")
		}
	default:
		return 0, fmt.Errorf("runtimedb: routine ownership must be native|wildstone, got %q", r.Ownership)
	}
	if r.Name == "" || r.Author == "" || r.Document == "" {
		return 0, fmt.Errorf("runtimedb: a routine needs a name, an author, and a document")
	}
	if r.State == "" {
		// A new routine starts as a draft. Nothing a household has not
		// looked at should be running.
		r.State = RoutineDraft
	}
	switch r.State {
	case RoutineDraft, RoutineTrial, RoutineEnabled, RoutineDisabled:
	default:
		return 0, fmt.Errorf("runtimedb: routine state must be draft|trial|enabled|disabled, got %q", r.State)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: creating routine: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stamp := now()
	res, err := tx.Exec(
		`INSERT INTO household_routines (name, ownership, external_id, connection, author,
		     revision, state, document, created_at, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
		r.Name, r.Ownership, r.ExternalID, r.Connection, r.Author, r.State, r.Document,
		stamp, stamp, r.Author)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("runtimedb: a routine named %q already exists", r.Name)
		}
		return 0, fmt.Errorf("runtimedb: creating routine: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`INSERT INTO household_routine_revisions (routine, revision, document, author, created_at, note)
		 VALUES (?, 1, ?, ?, ?, 'created')`, seq, r.Document, r.Author, stamp); err != nil {
		return 0, fmt.Errorf("runtimedb: recording routine revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: creating routine: %w", err)
	}
	return seq, nil
}

// EditHouseholdRoutine records a new revision. The old document stays,
// so an approval pinned to a revision means something and "what did
// it do last week" is answerable.
func (db *DB) EditHouseholdRoutine(name, document, author, note string) (int, error) {
	if document == "" || author == "" {
		return 0, fmt.Errorf("runtimedb: an edit needs a document and an author")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: editing routine: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var seq int64
	var revision int
	err = tx.QueryRow(`SELECT seq, revision FROM household_routines WHERE name = ?`, name).
		Scan(&seq, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("runtimedb: editing routine: %w", err)
	}
	revision++
	stamp := now()
	if _, err := tx.Exec(
		`UPDATE household_routines SET document = ?, revision = ?, updated_at = ?, updated_by = ?
		 WHERE seq = ?`, document, revision, stamp, author, seq); err != nil {
		return 0, fmt.Errorf("runtimedb: editing routine: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO household_routine_revisions (routine, revision, document, author, created_at, note)
		 VALUES (?, ?, ?, ?, ?, ?)`, seq, revision, document, author, stamp, note); err != nil {
		return 0, fmt.Errorf("runtimedb: recording routine revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: editing routine: %w", err)
	}
	return revision, nil
}

// SetHouseholdRoutineState moves a routine between draft, trial and
// enabled.
func (db *DB) SetHouseholdRoutineState(name, state, by string) error {
	switch state {
	case RoutineDraft, RoutineTrial, RoutineEnabled:
	default:
		return fmt.Errorf("runtimedb: use DisableHouseholdRoutine to disable; state must be draft|trial|enabled, got %q", state)
	}
	res, err := db.sql.Exec(
		`UPDATE household_routines SET state = ?, disabled_at = '', disabled_by = '',
		     updated_at = ?, updated_by = ? WHERE name = ?`, state, now(), by, name)
	if err != nil {
		return fmt.Errorf("runtimedb: setting routine state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DisableHouseholdRoutine stops exactly this routine, now.
//
// It is a separate call from SetHouseholdRoutineState because disabling
// is the urgent one: "that thing keeps texting me at six in the
// morning, make it stop" needs one obvious path that takes effect at the
// next step rather than at the next reload.
func (db *DB) DisableHouseholdRoutine(name, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: disabling a routine needs who did it")
	}
	res, err := db.sql.Exec(
		`UPDATE household_routines SET state = ?, disabled_at = ?, disabled_by = ?,
		     updated_at = ?, updated_by = ? WHERE name = ?`,
		RoutineDisabled, now(), by, now(), by, name)
	if err != nil {
		return fmt.Errorf("runtimedb: disabling routine: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetHouseholdRoutine returns one routine.
func (db *DB) GetHouseholdRoutine(name string) (HouseholdRoutine, error) {
	return scanRoutine(db.sql.QueryRow(routineColumns+` WHERE name = ?`, name))
}

const routineColumns = `SELECT seq, name, ownership, external_id, connection, author, revision,
	 state, document, created_at, updated_at, updated_by, disabled_at, disabled_by, output_audience, approved_revision, resource_signature,revoked_at,deleted_at,paused_reason,purged_at
	 FROM household_routines`

func scanRoutine(row rowScanner) (HouseholdRoutine, error) {
	var r HouseholdRoutine
	var created, updated, disabled, signature string
	err := row.Scan(&r.Seq, &r.Name, &r.Ownership, &r.ExternalID, &r.Connection, &r.Author,
		&r.Revision, &r.State, &r.Document, &created, &updated, &r.UpdatedBy, &disabled, &r.DisabledBy, &r.OutputAudience, &r.ApprovedRevision, &signature, &r.RevokedAt, &r.DeletedAt, &r.PausedReason, &r.PurgedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return HouseholdRoutine{}, ErrNotFound
	}
	if err != nil {
		return HouseholdRoutine{}, fmt.Errorf("runtimedb: scanning routine: %w", err)
	}
	if signature != "" {
		var resources RoutineResources
		if err := json.Unmarshal([]byte(signature), &resources); err != nil {
			return HouseholdRoutine{}, err
		}
		r.RequiredSecrets, r.RequiredMemory = resources.SecretIDs, resources.MemoryIDs
	}
	for _, f := range []struct {
		text string
		into *time.Time
	}{{created, &r.CreatedAt}, {updated, &r.UpdatedAt}, {disabled, &r.DisabledAt}} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return HouseholdRoutine{}, fmt.Errorf("runtimedb: bad routine timestamp %q: %w", f.text, err)
		}
		*f.into = parsed
	}
	return r, nil
}

// ListHouseholdRoutines returns every routine, by name.
func (db *DB) ListHouseholdRoutines() ([]HouseholdRoutine, error) {
	rows, err := db.sql.Query(routineColumns + ` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing routines: %w", err)
	}
	defer rows.Close()
	var out []HouseholdRoutine
	for rows.Next() {
		r, err := scanRoutine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoutineRevision is one recorded edit to a routine.
//
// Document is omitted from a listing: every past document on the detail
// path is a lot of bytes for a card that shows three lines each. The
// field is here so a future single-revision fetch does not have to
// change the shape.
type RoutineRevision struct {
	Revision int       `json:"revision"`
	Author   string    `json:"author"`
	At       time.Time `json:"at"`
	Note     string    `json:"note"`
	Document string    `json:"document,omitempty"`
}

// ListHouseholdRoutineRevisions returns a routine's edit history, newest
// first.
//
// The table has been written since migration 0028 and read by nothing,
// so "what did it do last week, and who changed it" was answerable from
// the database and from no API. The migration's own comment states the
// intent; this is the half that was missing.
//
// Revisions key on the routine's seq rather than its name, so an unknown
// name is ErrNotFound rather than an empty history — the two are
// different answers and a caller cannot tell them apart from [].
func (db *DB) ListHouseholdRoutineRevisions(name string) ([]RoutineRevision, error) {
	var seq int64
	err := db.sql.QueryRow(`SELECT seq FROM household_routines WHERE name = ?`, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing routine revisions: %w", err)
	}
	rows, err := db.sql.Query(
		`SELECT revision, author, created_at, note FROM household_routine_revisions
		 WHERE routine = ? ORDER BY revision DESC`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing routine revisions: %w", err)
	}
	defer rows.Close()
	var out []RoutineRevision
	for rows.Next() {
		var rev RoutineRevision
		var created string
		if err := rows.Scan(&rev.Revision, &rev.Author, &created, &rev.Note); err != nil {
			return nil, fmt.Errorf("runtimedb: scanning routine revision: %w", err)
		}
		if created != "" {
			at, err := time.Parse(time.RFC3339Nano, created)
			if err != nil {
				return nil, fmt.Errorf("runtimedb: bad revision timestamp %q: %w", created, err)
			}
			rev.At = at
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

// HouseholdRoutineRevisionDocuments returns every recorded document for
// a routine, newest first.
//
// Separate from ListHouseholdRoutineRevisions because the documents are
// the expensive half and almost no caller wants them. The one that does
// needs them server-side only: the rule name an event was recorded
// against is the document's metadata.name at the time it fired, and an
// edit may change it, so finding a routine's whole history means
// knowing every name it has been known by.
func (db *DB) HouseholdRoutineRevisionDocuments(name string) ([]string, error) {
	var seq int64
	err := db.sql.QueryRow(`SELECT seq FROM household_routines WHERE name = ?`, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing routine documents: %w", err)
	}
	rows, err := db.sql.Query(
		`SELECT document FROM household_routine_revisions WHERE routine = ? ORDER BY revision DESC`, seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing routine documents: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("runtimedb: scanning routine document: %w", err)
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}
