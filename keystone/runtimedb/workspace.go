package runtimedb

// ADR-W047 metadata persistence. These methods are trusted Keystone storage
// primitives, not authorization or filesystem publication. Callers must resolve
// current grants, privacy and execution fences before invoking them. IDs are
// opaque server-generated values, never display names or model ownership fields.
import (
	"database/sql"
	"fmt"
)

type Workspace struct {
	ID, PersonID string
	Visibility   string
	CreatedAt    string
	AgentID      int64
}
type WorkspaceArtifact struct{ ID, WorkspaceID, Path, State, CurrentRevision, TrashedAt, CreatedAt string }
type WorkspaceRevision struct {
	ID, ArtifactID     string
	TaskID             int64
	Hash               string
	SizeBytes          int64
	MediaType, Privacy string
	CreatedAt          string
	TrustedOnly        bool
}
type WorkspaceReservation struct {
	ID, WorkspaceID   string
	TaskID, SizeBytes int64
	Purpose, State    string
	CreatedAt         string
}
type WorkspacePublication struct {
	ID, WorkspaceID, ArtifactID, RevisionID, BaseRevision string
	TaskID                                                int64
	ReservationID, EffectKey, State                       string
	Operation, CreatedAt, UpdatedAt                       string
}
type WorkspaceProvenance struct {
	ID, RevisionID, SourceRevision, SourceID, SourceVersion, Privacy, Audience string
	TrustedOnly                                                                bool
}

func (db *DB) CreateWorkspace(w Workspace) error {
	if w.Visibility == "" {
		w.Visibility = "private"
	}
	_, err := db.sql.Exec(`INSERT INTO agent_workspaces(id,person_id,agent_id,visibility,created_at) VALUES(?,?,?,?,?)`, w.ID, w.PersonID, w.AgentID, w.Visibility, now())
	return err
}
func (db *DB) Workspace(personID string, agentID int64) (Workspace, error) {
	var w Workspace
	err := db.sql.QueryRow(`SELECT id,person_id,agent_id,visibility,created_at FROM agent_workspaces WHERE person_id=? AND agent_id=? AND visibility='private'`, personID, agentID).Scan(&w.ID, &w.PersonID, &w.AgentID, &w.Visibility, &w.CreatedAt)
	return w, err
}
func (db *DB) CreateWorkspaceArtifact(a WorkspaceArtifact) error {
	_, err := db.sql.Exec(`INSERT INTO workspace_artifacts(id,workspace_id,path,created_at) VALUES(?,?,?,?)`, a.ID, a.WorkspaceID, a.Path, now())
	return err
}

// WorkspaceArtifact returns only a visible current revision. Recovery inspects
// pending intent via WorkspacePublication instead of exposing partial files.
func (db *DB) WorkspaceArtifact(workspaceID, id string) (WorkspaceArtifact, error) {
	var a WorkspaceArtifact
	err := db.sql.QueryRow(`SELECT id,workspace_id,path,state,current_revision,trashed_at,created_at FROM workspace_artifacts WHERE workspace_id=? AND id=? AND state='committed'`, workspaceID, id).Scan(&a.ID, &a.WorkspaceID, &a.Path, &a.State, &a.CurrentRevision, &a.TrashedAt, &a.CreatedAt)
	return a, err
}
func (db *DB) CreateWorkspaceRevision(r WorkspaceRevision) error {
	_, err := db.sql.Exec(`INSERT INTO workspace_revisions(id,artifact_id,task_id,hash,size_bytes,media_type,privacy,trusted_only,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, r.ID, r.ArtifactID, r.TaskID, r.Hash, r.SizeBytes, r.MediaType, r.Privacy, r.TrustedOnly, now())
	return err
}

// WorkspaceRevision reads retained committed history, including trashed files.
// This is a trusted metadata read; the caller decides whether trash is admitted.
func (db *DB) WorkspaceRevision(workspaceID, id string) (WorkspaceRevision, error) {
	var r WorkspaceRevision
	err := db.sql.QueryRow(`SELECT r.id,r.artifact_id,r.task_id,r.hash,r.size_bytes,r.media_type,r.privacy,r.trusted_only,r.created_at FROM workspace_revisions r JOIN workspace_artifacts a ON a.id=r.artifact_id JOIN workspace_publications p ON p.revision_id=r.id WHERE a.workspace_id=? AND r.id=? AND p.state='committed'`, workspaceID, id).Scan(&r.ID, &r.ArtifactID, &r.TaskID, &r.Hash, &r.SizeBytes, &r.MediaType, &r.Privacy, &r.TrustedOnly, &r.CreatedAt)
	return r, err
}
func (db *DB) ReserveWorkspaceBytes(r WorkspaceReservation) error {
	_, err := db.sql.Exec(`INSERT INTO workspace_reservations(id,workspace_id,task_id,size_bytes,purpose,created_at) VALUES(?,?,?,?,?,?)`, r.ID, r.WorkspaceID, r.TaskID, r.SizeBytes, r.Purpose, now())
	return workspaceQuotaError(err)
}
func (db *DB) ReleaseWorkspaceReservation(workspaceID, id string) error {
	result, err := db.sql.Exec(`UPDATE workspace_reservations SET state='released' WHERE workspace_id=? AND id=? AND state='held'`, workspaceID, id)
	return workspaceChanged(result, err)
}
func (db *DB) CreateWorkspacePublication(p WorkspacePublication) error {
	if p.Operation == "" {
		p.Operation = "workspace_write_commit"
	}
	_, err := db.sql.Exec(`INSERT INTO workspace_publications(id,workspace_id,artifact_id,revision_id,base_revision,task_id,reservation_id,effect_key,operation,created_at,updated_at) VALUES(?,?,?,?,NULLIF(?,''),?,?,?,?,?,?)`, p.ID, p.WorkspaceID, p.ArtifactID, p.RevisionID, p.BaseRevision, p.TaskID, p.ReservationID, p.EffectKey, p.Operation, now(), now())
	return err
}
func (db *DB) WorkspacePublication(workspaceID, id string) (WorkspacePublication, error) {
	var p WorkspacePublication
	err := db.sql.QueryRow(`SELECT id,workspace_id,artifact_id,revision_id,COALESCE(base_revision,''),task_id,reservation_id,effect_key,state,operation,created_at,updated_at FROM workspace_publications WHERE workspace_id=? AND id=?`, workspaceID, id).Scan(&p.ID, &p.WorkspaceID, &p.ArtifactID, &p.RevisionID, &p.BaseRevision, &p.TaskID, &p.ReservationID, &p.EffectKey, &p.State, &p.Operation, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// PrepareWorkspacePublication seals the immutable hash, size and provenance.
func (db *DB) PrepareWorkspacePublication(workspaceID, id string) error {
	return db.advanceWorkspacePublication(workspaceID, id, "staging", "prepared")
}

// MarkWorkspaceBytesPublished records the coordinator's verified durable byte
// publication. This method does not write or verify filesystem objects itself.
func (db *DB) MarkWorkspaceBytesPublished(workspaceID, id string) error {
	return db.advanceWorkspacePublication(workspaceID, id, "prepared", "bytes_published")
}
func (db *DB) advanceWorkspacePublication(workspaceID, id, from, to string) error {
	result, err := db.sql.Exec(`UPDATE workspace_publications SET state=?,updated_at=? WHERE workspace_id=? AND id=? AND state=?`, to, now(), workspaceID, id, from)
	return workspaceChanged(result, err)
}

// CommitWorkspacePublication atomically compares the base, makes the new
// revision current, converts reserved bytes to retained usage and records a
// receipt. It never destroys the old revision. An exact settled retry succeeds.
func (db *DB) CommitWorkspacePublication(workspaceID, id, receiptID string) error {
	var session int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM workspace_write_sessions WHERE workspace_id=? AND id=?`, workspaceID, id).Scan(&session); err != nil {
		return err
	}
	if session != 0 {
		if receiptID != id {
			return ErrWorkspaceConflict
		}
		return db.CommitWorkspaceWrite(workspaceID, id)
	}
	return db.commitWorkspacePublication(workspaceID, id, receiptID, false)
}

// CommitWorkspaceWrite settles the S05 session and original receipt in the
// same metadata transaction. The coordinator verifies durable bytes first.
func (db *DB) CommitWorkspaceWrite(workspaceID, id string) error {
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	return db.commitWorkspacePublication(workspaceID, id, id, true)
}
func (db *DB) commitWorkspacePublication(workspaceID, id, receiptID string, fenced bool) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if fenced {
		if err := db.checkWorkspaceExecutionTx(tx); err != nil {
			return err
		}
	}
	var existing string
	err = tx.QueryRow(`SELECT r.id FROM workspace_receipts r JOIN workspace_publications p ON p.id=r.publication_id WHERE p.workspace_id=? AND p.id=?`, workspaceID, id).Scan(&existing)
	if err == nil {
		if existing == receiptID {
			return nil
		}
		return fmt.Errorf("workspace receipt conflict")
	}
	if err != sql.ErrNoRows {
		return err
	}
	result, err := tx.Exec(`UPDATE workspace_publications SET state='committed',updated_at=? WHERE workspace_id=? AND id=? AND state='bytes_published'`, now(), workspaceID, id)
	if err = workspaceChanged(result, err); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE workspace_artifacts SET current_revision=(SELECT revision_id FROM workspace_publications WHERE id=?),state='committed' WHERE id=(SELECT artifact_id FROM workspace_publications WHERE id=?)`, id, id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE workspace_reservations SET state='consumed' WHERE id=(SELECT reservation_id FROM workspace_publications WHERE id=?)`, id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO workspace_receipts(id,publication_id,recorded_at) VALUES(?,?,?)`, receiptID, id, now())
	if err != nil {
		return err
	}
	if fenced {
		result, err = tx.Exec(`UPDATE workspace_write_sessions SET state='committed' WHERE workspace_id=? AND id=? AND state='sealed'`, workspaceID, id)
		if err = workspaceChanged(result, err); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AbortWorkspacePublication is a recovery metadata operation. The coordinator
// must resolve uncertain filesystem effects before calling it; no bytes vanish.
// The aborted journal and pending artifact/path remain reserved for recovery.
// A subsequent attempt reuses that artifact with a new revision and intent;
// it cannot create a fresh artifact at the same path until lifecycle coordination.
func (db *DB) AbortWorkspacePublication(workspaceID, id string) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE workspace_publications SET state='aborted',updated_at=? WHERE workspace_id=? AND id=? AND state IN ('staging','prepared','bytes_published')`, now(), workspaceID, id)
	if err = workspaceChanged(result, err); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE workspace_reservations SET state='released' WHERE id=(SELECT reservation_id FROM workspace_publications WHERE id=?)`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) RecordWorkspaceProvenance(p WorkspaceProvenance) error {
	_, err := db.sql.Exec(`INSERT INTO workspace_provenance(id,revision_id,source_revision,source_id,source_version,privacy,trusted_only,audience,created_at) VALUES(?,?,NULLIF(?,''),?,?,?,?,?,?)`, p.ID, p.RevisionID, p.SourceRevision, p.SourceID, p.SourceVersion, p.Privacy, p.TrustedOnly, p.Audience, now())
	return err
}
func (db *DB) WorkspaceProvenance(revisionID string) ([]WorkspaceProvenance, error) {
	rows, err := db.sql.Query(`SELECT id,revision_id,COALESCE(source_revision,''),source_id,source_version,privacy,trusted_only,audience FROM workspace_provenance WHERE revision_id=? ORDER BY id`, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceProvenance
	for rows.Next() {
		var p WorkspaceProvenance
		if err = rows.Scan(&p.ID, &p.RevisionID, &p.SourceRevision, &p.SourceID, &p.SourceVersion, &p.Privacy, &p.TrustedOnly, &p.Audience); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (db *DB) TrashWorkspaceArtifact(workspaceID, id, expectedRevision string) error {
	result, err := db.sql.Exec(`UPDATE workspace_artifacts SET state='trashed',trashed_at=? WHERE workspace_id=? AND id=? AND current_revision=? AND state='committed' AND NOT EXISTS(SELECT 1 FROM workspace_publications WHERE artifact_id=? AND state NOT IN ('committed','aborted'))`, now(), workspaceID, id, expectedRevision, id)
	return workspaceChanged(result, err)
}
func (db *DB) RestoreWorkspaceArtifact(workspaceID, id, expectedRevision, path string) error {
	result, err := db.sql.Exec(`UPDATE workspace_artifacts SET state='committed',trashed_at='',path=? WHERE workspace_id=? AND id=? AND current_revision=? AND state='trashed'`, path, workspaceID, id, expectedRevision)
	return workspaceChanged(result, err)
}
func workspaceChanged(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("workspace state conflict or missing resource")
	}
	return nil
}

// WorkspaceReceipt recovers the durable receipt identity for an exact retry.
func (db *DB) WorkspaceReceipt(workspaceID, publicationID string) (string, error) {
	var id string
	err := db.sql.QueryRow(`SELECT r.id FROM workspace_receipts r JOIN workspace_publications p ON p.id=r.publication_id WHERE p.workspace_id=? AND p.id=? AND p.state='committed'`, workspaceID, publicationID).Scan(&id)
	return id, err
}

// WorkspaceReservation returns accounting state for recovery; a released intent
// remains durable so a retry cannot silently create another charge.
func (db *DB) WorkspaceReservation(workspaceID, id string) (WorkspaceReservation, error) {
	var r WorkspaceReservation
	err := db.sql.QueryRow(`SELECT id,workspace_id,task_id,size_bytes,purpose,state,created_at FROM workspace_reservations WHERE workspace_id=? AND id=?`, workspaceID, id).Scan(&r.ID, &r.WorkspaceID, &r.TaskID, &r.SizeBytes, &r.Purpose, &r.State, &r.CreatedAt)
	return r, err
}
