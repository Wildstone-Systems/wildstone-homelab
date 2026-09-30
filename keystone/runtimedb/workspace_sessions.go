package runtimedb

// Trusted persistence for ADR-W047 S05. This layer does not grant resource
// access. The coordinator must also recheck current resource/privacy authority.
import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrWorkspaceConflict = errors.New("conflict: workspace write state or arguments changed")

type WorkspaceWriteSession struct {
	ID, WorkspaceID, ArtifactID, Path, BaseRevision, BeginKey, RequestHash, ConfigRevision string
	TaskID, MaxBytes                                                                       int64
	Privacy                                                                                string
	TrustedOnly                                                                            bool
	Provenance                                                                             []WorkspaceProvenance
	State, CreatedAt                                                                       string
}
type WorkspaceWriteChunk struct {
	SessionID, ObjectID, Hash, ReservationID, State string
	Index                                           int
	SizeBytes                                       int64
}

const sessionColumns = `id,workspace_id,task_id,artifact_id,path,base_revision,begin_key,request_hash,config_revision,max_bytes,privacy,trusted_only,provenance,state,created_at`

func scanWriteSession(row interface{ Scan(...any) error }) (s WorkspaceWriteSession, err error) {
	var provenance string
	err = row.Scan(&s.ID, &s.WorkspaceID, &s.TaskID, &s.ArtifactID, &s.Path, &s.BaseRevision, &s.BeginKey, &s.RequestHash, &s.ConfigRevision, &s.MaxBytes, &s.Privacy, &s.TrustedOnly, &provenance, &s.State, &s.CreatedAt)
	if err == nil {
		err = json.Unmarshal([]byte(provenance), &s.Provenance)
	}
	return
}
func (db *DB) WorkspaceWriteSession(workspaceID, id string) (WorkspaceWriteSession, error) {
	return scanWriteSession(db.sql.QueryRow(`SELECT `+sessionColumns+` FROM workspace_write_sessions WHERE workspace_id=? AND id=?`, workspaceID, id))
}
func (db *DB) BeginWorkspaceWrite(s WorkspaceWriteSession) (WorkspaceWriteSession, error) {
	if err := db.CheckHouseholdExecution(); err != nil {
		return s, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return s, err
	}
	defer tx.Rollback()
	if err := db.checkWorkspaceExecutionTx(tx); err != nil {
		return s, err
	}
	existing, err := scanWriteSession(tx.QueryRow(`SELECT `+sessionColumns+` FROM workspace_write_sessions WHERE task_id=? AND begin_key=?`, s.TaskID, s.BeginKey))
	if err == nil {
		if existing.WorkspaceID != s.WorkspaceID || existing.RequestHash != s.RequestHash {
			return s, ErrWorkspaceConflict
		}
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return s, err
	}
	// A rolled-back create retains its immutable journal/history, but a fresh
	// authorized create may reuse the empty pending artifact at this path.
	if s.BaseRevision == "" {
		var pending string
		err := tx.QueryRow(`SELECT a.id FROM workspace_artifacts a WHERE a.workspace_id=? AND a.path=? AND a.state='pending' AND NOT EXISTS(SELECT 1 FROM workspace_publications p WHERE p.artifact_id=a.id AND p.state<>'aborted')`, s.WorkspaceID, s.Path).Scan(&pending)
		if err == nil {
			s.ArtifactID = pending
		} else if err != sql.ErrNoRows {
			return s, err
		}
	}
	if err := checkWriteBase(tx, s); err != nil {
		return s, err
	}
	edges, err := json.Marshal(s.Provenance)
	if err != nil {
		return s, err
	}
	_, err = tx.Exec(`INSERT INTO workspace_write_sessions(`+sessionColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'open',?)`, s.ID, s.WorkspaceID, s.TaskID, s.ArtifactID, s.Path, s.BaseRevision, s.BeginKey, s.RequestHash, s.ConfigRevision, s.MaxBytes, s.Privacy, s.TrustedOnly, string(edges), now())
	if err != nil {
		return s, err
	}
	if err = tx.Commit(); err != nil {
		return s, err
	}
	return db.WorkspaceWriteSession(s.WorkspaceID, s.ID)
}
func checkWriteBase(tx *sql.Tx, s WorkspaceWriteSession) error {
	var n int
	if s.BaseRevision == "" {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM workspace_artifacts WHERE workspace_id=? AND path=? AND state<>'trashed' AND NOT (id=? AND state='pending' AND NOT EXISTS(SELECT 1 FROM workspace_publications p WHERE p.artifact_id=workspace_artifacts.id AND p.state<>'aborted'))`, s.WorkspaceID, s.Path, s.ArtifactID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return ErrWorkspaceConflict
		}
	} else {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM workspace_artifacts WHERE workspace_id=? AND id=? AND path=? AND state='committed' AND current_revision=?`, s.WorkspaceID, s.ArtifactID, s.Path, s.BaseRevision).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrWorkspaceConflict
		}
	}
	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM workspace_publications WHERE artifact_id=? AND state NOT IN ('committed','aborted')`, s.ArtifactID).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return ErrWorkspaceConflict
	}
	return nil
}
func scanChunk(row interface{ Scan(...any) error }) (c WorkspaceWriteChunk, err error) {
	err = row.Scan(&c.SessionID, &c.Index, &c.ObjectID, &c.Hash, &c.SizeBytes, &c.ReservationID, &c.State)
	return
}

const chunkColumns = `session_id,chunk_index,object_id,hash,size_bytes,reservation_id,state`

func (db *DB) WorkspaceWriteChunks(id string) ([]WorkspaceWriteChunk, error) {
	rows, err := db.sql.Query(`SELECT `+chunkColumns+` FROM workspace_write_chunks WHERE session_id=? ORDER BY chunk_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceWriteChunk
	for rows.Next() {
		c, err := scanChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// JournalWorkspaceChunk reserves bytes and records their identity atomically,
// before any filesystem write. Exact retries reuse this original object.
func (db *DB) JournalWorkspaceChunk(s WorkspaceWriteSession, c WorkspaceWriteChunk) (WorkspaceWriteChunk, error) {
	if err := db.CheckHouseholdExecution(); err != nil {
		return c, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	if err := db.checkWorkspaceExecutionTx(tx); err != nil {
		return c, err
	}
	current, err := scanWriteSession(tx.QueryRow(`SELECT `+sessionColumns+` FROM workspace_write_sessions WHERE workspace_id=? AND id=?`, s.WorkspaceID, s.ID))
	if err != nil {
		return c, err
	}
	if current.State != "open" || current.TaskID != s.TaskID {
		return c, ErrWorkspaceConflict
	}
	existing, err := scanChunk(tx.QueryRow(`SELECT `+chunkColumns+` FROM workspace_write_chunks WHERE session_id=? AND chunk_index=?`, s.ID, c.Index))
	if err == nil {
		if existing.Hash != c.Hash || existing.SizeBytes != c.SizeBytes {
			return c, ErrWorkspaceConflict
		}
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return c, err
	}
	var count, pending int
	var size int64
	err = tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(state<>'durable'),0),COALESCE(SUM(size_bytes),0) FROM workspace_write_chunks WHERE session_id=?`, s.ID).Scan(&count, &pending, &size)
	if err != nil {
		return c, err
	}
	if count != c.Index || pending != 0 {
		return c, ErrWorkspaceConflict
	}
	if c.SizeBytes < 0 || c.SizeBytes > 1024 || size+c.SizeBytes > current.MaxBytes {
		return c, fmt.Errorf("quota_exceeded: workspace file ceiling reached")
	}
	_, err = tx.Exec(`INSERT INTO workspace_reservations(id,workspace_id,task_id,size_bytes,purpose,created_at) VALUES(?,?,?,?,'scratch',?)`, c.ReservationID, s.WorkspaceID, s.TaskID, c.SizeBytes, now())
	if err != nil {
		return c, err
	}
	_, err = tx.Exec(`INSERT INTO workspace_write_chunks(session_id,chunk_index,object_id,hash,size_bytes,reservation_id) VALUES(?,?,?,?,?,?)`, s.ID, c.Index, c.ObjectID, c.Hash, c.SizeBytes, c.ReservationID)
	if err != nil {
		return c, err
	}
	c.SessionID = s.ID
	c.State = "pending"
	return c, tx.Commit()
}
func (db *DB) MarkWorkspaceChunkDurable(id string, index int) error {
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = db.checkWorkspaceExecutionTx(tx); err != nil {
		return err
	}
	var state string
	if err = tx.QueryRow(`SELECT state FROM workspace_write_chunks WHERE session_id=? AND chunk_index=?`, id, index).Scan(&state); err != nil {
		return err
	}
	if state == "durable" {
		return nil
	}
	result, err := tx.Exec(`UPDATE workspace_write_chunks SET state='durable' WHERE session_id=? AND chunk_index=? AND state='pending' AND EXISTS(SELECT 1 FROM workspace_write_sessions WHERE id=? AND state='open')`, id, index, id)
	if err = workspaceChanged(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

// AdvanceWorkspaceWrite records a verified byte boundary under the acquired
// process epoch. Exact concurrent retries may have advanced farther already.
func (db *DB) AdvanceWorkspaceWrite(workspaceID, id, to string) error {
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	from := "staging"
	if to == "bytes_published" {
		from = "prepared"
	} else if to != "prepared" {
		return ErrWorkspaceConflict
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = db.checkWorkspaceExecutionTx(tx); err != nil {
		return err
	}
	var state string
	if err = tx.QueryRow(`SELECT p.state FROM workspace_publications p JOIN workspace_write_sessions s ON s.id=p.id WHERE p.workspace_id=? AND p.id=?`, workspaceID, id).Scan(&state); err != nil {
		return err
	}
	if state == to || state == "committed" || (to == "prepared" && state == "bytes_published") {
		return nil
	}
	if state != from {
		return ErrWorkspaceConflict
	}
	result, err := tx.Exec(`UPDATE workspace_publications SET state=?,updated_at=? WHERE workspace_id=? AND id=? AND state=?`, to, now(), workspaceID, id, from)
	if err = workspaceChanged(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

// SealWorkspaceWrite compares the complete chunk set and base under SQLite's
// writer lock. No lock spans model/network waits or filesystem publication.
func (db *DB) SealWorkspaceWrite(s WorkspaceWriteSession, hash, mediaType, key string, count int, size int64) (WorkspacePublication, error) {
	var p WorkspacePublication
	if err := db.CheckHouseholdExecution(); err != nil {
		return p, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	if err := db.checkWorkspaceExecutionTx(tx); err != nil {
		return p, err
	}
	current, err := scanWriteSession(tx.QueryRow(`SELECT `+sessionColumns+` FROM workspace_write_sessions WHERE workspace_id=? AND id=?`, s.WorkspaceID, s.ID))
	if err != nil {
		return p, err
	}
	if current.TaskID != s.TaskID || current.State == "aborted" {
		return p, ErrWorkspaceConflict
	}
	var oldID string
	err = tx.QueryRow(`SELECT session_id FROM workspace_write_commit_keys WHERE task_id=? AND effect_key=?`, s.TaskID, key).Scan(&oldID)
	if err == nil && oldID != s.ID {
		return p, ErrWorkspaceConflict
	}
	if err != nil && err != sql.ErrNoRows {
		return p, err
	}
	if _, err = tx.Exec(`INSERT INTO workspace_write_commit_keys(task_id,effect_key,session_id) VALUES(?,?,?) ON CONFLICT(task_id,effect_key) DO NOTHING`, s.TaskID, key, s.ID); err != nil {
		return p, err
	}
	if current.State != "open" {
		if err = tx.Commit(); err != nil {
			return p, err
		}
		return db.WorkspacePublication(s.WorkspaceID, s.ID)
	}
	if err = checkWriteBase(tx, current); err != nil {
		return p, err
	}
	var actualCount, pending int
	var actualSize int64
	err = tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(size_bytes),0),COALESCE(SUM(state<>'durable'),0) FROM workspace_write_chunks WHERE session_id=?`, s.ID).Scan(&actualCount, &actualSize, &pending)
	if err != nil {
		return p, err
	}
	if actualCount != count || actualSize != size || pending != 0 || size > current.MaxBytes {
		return p, ErrWorkspaceConflict
	}
	if current.BaseRevision == "" {
		_, err = tx.Exec(`INSERT INTO workspace_artifacts(id,workspace_id,path,created_at) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, current.ArtifactID, current.WorkspaceID, current.Path, now())
		if err != nil {
			return p, err
		}
	}
	_, err = tx.Exec(`INSERT INTO workspace_revisions(id,artifact_id,task_id,hash,size_bytes,media_type,privacy,trusted_only,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, s.ID, current.ArtifactID, current.TaskID, hash, size, mediaType, current.Privacy, current.TrustedOnly, now())
	if err != nil {
		return p, err
	}
	_, err = tx.Exec(`INSERT INTO workspace_reservations(id,workspace_id,task_id,size_bytes,purpose,created_at) VALUES(?,?,?,?,'publication',?)`, s.ID, s.WorkspaceID, s.TaskID, size, now())
	if err != nil {
		return p, err
	}
	_, err = tx.Exec(`INSERT INTO workspace_publications(id,workspace_id,artifact_id,revision_id,base_revision,task_id,reservation_id,effect_key,created_at,updated_at) VALUES(?,?,?,?,NULLIF(?,''),?,?,?,?,?)`, s.ID, s.WorkspaceID, current.ArtifactID, s.ID, current.BaseRevision, s.TaskID, s.ID, key, now(), now())
	if err != nil {
		return p, err
	}
	for i, e := range current.Provenance {
		_, err = tx.Exec(`INSERT INTO workspace_provenance(id,revision_id,source_revision,source_id,source_version,privacy,trusted_only,audience,created_at) VALUES(?,?,NULLIF(?,''),?,?,?,?,?,?)`, fmt.Sprintf("%s-%d", s.ID, i), s.ID, e.SourceRevision, e.SourceID, e.SourceVersion, e.Privacy, e.TrustedOnly, e.Audience, now())
		if err != nil {
			return p, err
		}
	}
	_, err = tx.Exec(`UPDATE workspace_write_sessions SET state='sealed' WHERE id=?`, s.ID)
	if err != nil {
		return p, err
	}
	if err = tx.Commit(); err != nil {
		return p, err
	}
	return db.WorkspacePublication(s.WorkspaceID, s.ID)
}

// WorkspaceWriteObject reads pending hash/size strictly through its write
// session. It is recovery metadata, not the committed artifact read surface.
func (db *DB) WorkspaceWriteObject(workspaceID, id string) (WorkspaceRevision, error) {
	var r WorkspaceRevision
	err := db.sql.QueryRow(`SELECT r.id,r.artifact_id,r.task_id,r.hash,r.size_bytes,r.media_type,r.privacy,r.trusted_only,r.created_at FROM workspace_revisions r JOIN workspace_write_sessions s ON s.id=r.id WHERE s.workspace_id=? AND s.id=?`, workspaceID, id).Scan(&r.ID, &r.ArtifactID, &r.TaskID, &r.Hash, &r.SizeBytes, &r.MediaType, &r.Privacy, &r.TrustedOnly, &r.CreatedAt)
	return r, err
}

// RecoverWorkspaceWrites rolls back only unsettled S05 metadata at startup.
// Without a current per-task authorization decision startup never completes a
// user effect. All bytes/reservations and old committed revisions are retained
// for explicit reconciliation and lifecycle cleanup; no invented receipts.
func (db *DB) RecoverWorkspaceWrites() (int64, error) {
	if err := db.CheckHouseholdExecution(); err != nil {
		return 0, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := db.checkWorkspaceExecutionTx(tx); err != nil {
		return 0, err
	}
	result, err := tx.Exec(`UPDATE workspace_publications SET state='aborted',updated_at=? WHERE state IN ('staging','prepared','bytes_published') AND id IN (SELECT id FROM workspace_write_sessions WHERE state='sealed')`, now())
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`UPDATE workspace_write_sessions SET state='aborted' WHERE state='sealed' AND id IN (SELECT id FROM workspace_publications WHERE state='aborted')`)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Compare the acquired process epoch under the same SQLite write lock as the
// transition. Promotion cannot slip between this check and metadata commit.
func (db *DB) checkWorkspaceExecutionTx(tx *sql.Tx) error {
	lease := db.executionLease.Load()
	if lease == nil {
		return ErrCoordinatorFenced
	}
	current, err := readCoordinator(tx)
	if err != nil {
		return err
	}
	if current.Holder != lease.Holder || current.Epoch != lease.Epoch {
		return ErrCoordinatorFenced
	}
	return nil
}
func (db *DB) WorkspaceByID(id string) (Workspace, error) {
	var w Workspace
	err := db.sql.QueryRow(`SELECT id,person_id,agent_id,visibility,created_at FROM agent_workspaces WHERE id=?`, id).Scan(&w.ID, &w.PersonID, &w.AgentID, &w.Visibility, &w.CreatedAt)
	return w, err
}
