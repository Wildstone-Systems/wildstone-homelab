package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
)

// Desired-state publication journal (ADR-W007 A8, contract D3).
//
//	prepared -> accepted -> mirrored
//	       \-> abandoned
//
// prepared grants nothing: the candidate may not even exist yet.
// accepted is the publication point: one transaction compared the
// expected revision, selected the candidate and moved its Plan. mirrored
// says the human-facing branch has caught up, which is presentation and
// repairable, never a second acceptance.

// PublicationState is a journal row's state.
type PublicationState string

const (
	PublicationPrepared  PublicationState = "prepared"
	PublicationAccepted  PublicationState = "accepted"
	PublicationMirrored  PublicationState = "mirrored"
	PublicationAbandoned PublicationState = "abandoned"
)

// Publication sources.
const (
	SourceAPIPlan        = "api-plan"
	SourceHouseholdAudit = "household-audit"
	SourceOutOfBand      = "out-of-band"
)

// Publication is one journal row.
type Publication struct {
	ID                string
	Source            string
	PlanID            string
	ExpectedRevision  string
	CandidateRevision string
	TreeDigest        string
	State             PublicationState
	Detail            string
}

// ErrRevisionMoved is AcceptPublication's answer when the accepted
// revision is no longer the one the publication expected: a concurrent
// publication won, and this one must be reviewed again.
var ErrRevisionMoved = errors.New("runtimedb: the accepted revision moved since this publication was prepared")

// PreparePublication records a publication before any candidate exists.
func (db *DB) PreparePublication(p Publication) error {
	if p.ID == "" {
		return fmt.Errorf("runtimedb: publication id required")
	}
	t := now()
	_, err := db.sql.Exec(`INSERT INTO statefile_publications
		(publication_id, source, plan_id, expected_revision, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'prepared', ?, ?)`,
		p.ID, p.Source, p.PlanID, p.ExpectedRevision, t, t)
	if err != nil {
		return fmt.Errorf("runtimedb: preparing publication %s: %w", p.ID, err)
	}
	return nil
}

// SetPublicationCandidate records the candidate a prepared publication
// built, once its internal ref makes it reachable.
func (db *DB) SetPublicationCandidate(id, revision, treeDigest string) error {
	res, err := db.sql.Exec(`UPDATE statefile_publications
		SET candidate_revision = ?, tree_digest = ?, updated_at = ?
		WHERE publication_id = ? AND state = 'prepared'`, revision, treeDigest, now(), id)
	if err != nil {
		return fmt.Errorf("runtimedb: recording candidate for %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("runtimedb: publication %s is not prepared", id)
	}
	return nil
}

// AcceptPublication is the publication point. In one transaction it
// checks that the accepted revision is still the publication's expected
// one, selects the candidate, records the journal state and, when planID
// is set, moves that Plan to planTo. If the revision moved it changes
// nothing but the journal (abandoned) and returns ErrRevisionMoved.
func (db *DB) AcceptPublication(id string, planTo PlanState, resolvedBy string) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: accepting publication: %w", err)
	}
	defer tx.Rollback()

	var p Publication
	var state string
	err = tx.QueryRow(`SELECT plan_id, expected_revision, candidate_revision, tree_digest, state
		FROM statefile_publications WHERE publication_id = ?`, id).
		Scan(&p.PlanID, &p.ExpectedRevision, &p.CandidateRevision, &p.TreeDigest, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: accepting publication %s: %w", id, err)
	}
	if PublicationState(state) != PublicationPrepared {
		return fmt.Errorf("runtimedb: publication %s is %s, not prepared", id, state)
	}
	if p.CandidateRevision == "" {
		return fmt.Errorf("runtimedb: publication %s has no candidate", id)
	}

	var current string
	err = tx.QueryRow(`SELECT head FROM statefile_head WHERE id = 1`).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("runtimedb: reading accepted revision: %w", err)
	}
	if current != p.ExpectedRevision {
		if _, err := tx.Exec(`UPDATE statefile_publications SET state = 'abandoned', detail = ?, updated_at = ? WHERE publication_id = ?`,
			"superseded: accepted revision is now "+current, now(), id); err != nil {
			return fmt.Errorf("runtimedb: abandoning publication %s: %w", id, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("runtimedb: abandoning publication %s: %w", id, err)
		}
		return ErrRevisionMoved
	}

	if _, err := tx.Exec(`INSERT INTO statefile_head (id, head, tree_digest, publication_id) VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET head = excluded.head, tree_digest = excluded.tree_digest,
		publication_id = excluded.publication_id`, p.CandidateRevision, p.TreeDigest, id); err != nil {
		return fmt.Errorf("runtimedb: selecting candidate for %s: %w", id, err)
	}
	if _, err := tx.Exec(`UPDATE statefile_publications SET state = 'accepted', updated_at = ? WHERE publication_id = ?`, now(), id); err != nil {
		return fmt.Errorf("runtimedb: accepting publication %s: %w", id, err)
	}
	if p.PlanID != "" && planTo != "" {
		if err := transitionPlanTx(tx, p.PlanID, planTo, resolvedBy); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkPublication moves a publication to mirrored or abandoned.
func (db *DB) MarkPublication(id string, to PublicationState, detail string) error {
	from := "accepted"
	if to == PublicationAbandoned {
		from = "prepared"
	}
	res, err := db.sql.Exec(`UPDATE statefile_publications SET state = ?, detail = ?, updated_at = ?
		WHERE publication_id = ? AND state = ?`, string(to), detail, now(), id, from)
	if err != nil {
		return fmt.Errorf("runtimedb: marking publication %s %s: %w", id, to, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("runtimedb: publication %s is not %s", id, from)
	}
	return nil
}

// OpenPublications lists prepared and accepted-but-unmirrored
// publications, oldest first: what startup recovery must finish.
func (db *DB) OpenPublications() ([]Publication, error) {
	rows, err := db.sql.Query(`SELECT publication_id, source, plan_id, expected_revision, candidate_revision, tree_digest, state, detail
		FROM statefile_publications WHERE state IN ('prepared', 'accepted') ORDER BY created_at, publication_id`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing open publications: %w", err)
	}
	defer rows.Close()
	var out []Publication
	for rows.Next() {
		var p Publication
		var state string
		if err := rows.Scan(&p.ID, &p.Source, &p.PlanID, &p.ExpectedRevision, &p.CandidateRevision, &p.TreeDigest, &state, &p.Detail); err != nil {
			return nil, fmt.Errorf("runtimedb: listing open publications: %w", err)
		}
		p.State = PublicationState(state)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPublication reads one journal row.
func (db *DB) GetPublication(id string) (Publication, error) {
	var p Publication
	var state string
	err := db.sql.QueryRow(`SELECT publication_id, source, plan_id, expected_revision, candidate_revision, tree_digest, state, detail
		FROM statefile_publications WHERE publication_id = ?`, id).
		Scan(&p.ID, &p.Source, &p.PlanID, &p.ExpectedRevision, &p.CandidateRevision, &p.TreeDigest, &state, &p.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, fmt.Errorf("runtimedb: reading publication %s: %w", id, err)
	}
	p.State = PublicationState(state)
	return p, nil
}

// AcceptedRecord returns the accepted revision with the tree digest and
// publication that selected it ("" for rows written before they were
// recorded).
func (db *DB) AcceptedRecord() (revision, treeDigest, publicationID string, err error) {
	err = db.sql.QueryRow(`SELECT head, tree_digest, publication_id FROM statefile_head WHERE id = 1`).
		Scan(&revision, &treeDigest, &publicationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("runtimedb: reading accepted revision: %w", err)
	}
	return revision, treeDigest, publicationID, nil
}
