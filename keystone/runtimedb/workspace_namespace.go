package runtimedb

// ADR-W047 S09 part 1. Trusted logical namespace primitives only: the caller
// must authorize the current Person/Agent/scope/tool and verify filesystem
// durability before CommitWorkspaceNamespace. No method here exposes a tool,
// creates a host directory, or claims a filesystem operation completed.
import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/internal/confinedpath"
)

type WorkspaceDirectory struct{ ID, WorkspaceID, Path, Revision, State, CreatedAt string }
type WorkspaceNamespaceRequest struct {
	WorkspaceID, Operation, Key, Path, Destination, ArtifactID, ExpectedRevision, ExpectedDirectoryRevision string
	TaskID                                                                                                  int64
}
type WorkspaceNamespaceIntent struct {
	ID, WorkspaceID, Operation, Key, RequestHash, Path, Destination, ArtifactID, ExpectedRevision, DirectoryID, DirectoryRevision, State, CreatedAt string
	TaskID                                                                                                                                          int64
}

const namespaceColumns = `id,workspace_id,task_id,operation,effect_key,request_hash,path,destination,COALESCE(artifact_id,''),expected_revision,directory_id,directory_revision,state,created_at`

func scanNamespace(row interface{ Scan(...any) error }) (n WorkspaceNamespaceIntent, err error) {
	err = row.Scan(&n.ID, &n.WorkspaceID, &n.TaskID, &n.Operation, &n.Key, &n.RequestHash, &n.Path, &n.Destination, &n.ArtifactID, &n.ExpectedRevision, &n.DirectoryID, &n.DirectoryRevision, &n.State, &n.CreatedAt)
	return
}
func (db *DB) WorkspaceNamespaceIntent(workspaceID, id string) (WorkspaceNamespaceIntent, error) {
	return scanNamespace(db.sql.QueryRow(`SELECT `+namespaceColumns+` FROM workspace_namespace_intents WHERE workspace_id=? AND id=?`, workspaceID, id))
}

// PendingWorkspaceNamespaceIntents is a trusted startup-recovery inventory,
// never a user-facing list. Reservations remain pinned until settled explicitly.
func (db *DB) PendingWorkspaceNamespaceIntents() ([]WorkspaceNamespaceIntent, error) {
	rows, err := db.sql.Query(`SELECT ` + namespaceColumns + ` FROM workspace_namespace_intents WHERE state='prepared' ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceNamespaceIntent
	for rows.Next() {
		n, err := scanNamespace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func namespaceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (db *DB) WorkspaceDirectory(workspaceID, p string) (WorkspaceDirectory, error) {
	return scanDirectory(db.sql.QueryRow(`SELECT id,workspace_id,path,revision,state,created_at FROM workspace_directories WHERE workspace_id=? AND path=? AND state='committed'`, workspaceID, p))
}
func scanDirectory(row interface{ Scan(...any) error }) (d WorkspaceDirectory, err error) {
	err = row.Scan(&d.ID, &d.WorkspaceID, &d.Path, &d.Revision, &d.State, &d.CreatedAt)
	return
}

// WorkspaceDirectories returns durable logical identities, not physical readiness.
func (db *DB) WorkspaceDirectories(workspaceID string) ([]WorkspaceDirectory, error) {
	rows, err := db.sql.Query(`SELECT id,workspace_id,path,revision,state,created_at FROM workspace_directories WHERE workspace_id=? AND state='committed' ORDER BY path`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceDirectory
	for rows.Next() {
		d, err := scanDirectory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func namespaceInput(r WorkspaceNamespaceRequest) error {
	if r.WorkspaceID == "" || r.TaskID <= 0 || r.Key == "" || len(r.Key) > 128 || !utf8.ValidString(r.Key) || r.Path == "" {
		return ErrWorkspaceConflict
	}
	if _, err := confinedpath.ParseRelativePath(r.Path); err != nil {
		return ErrWorkspaceConflict
	}
	switch r.Operation {
	case "workspace_mkdir":
		if r.Destination != "" || r.ArtifactID != "" || r.ExpectedRevision != "" || r.ExpectedDirectoryRevision != "" {
			return ErrWorkspaceConflict
		}
	case "workspace_rmdir":
		if r.Destination != "" || r.ArtifactID != "" || r.ExpectedRevision != "" || r.ExpectedDirectoryRevision == "" {
			return ErrWorkspaceConflict
		}
	case "workspace_move":
		if r.ArtifactID == "" || r.ExpectedRevision == "" || r.ExpectedDirectoryRevision != "" || r.Destination == "" || r.Destination == r.Path {
			return ErrWorkspaceConflict
		}
		if _, err := confinedpath.ParseRelativePath(r.Destination); err != nil {
			return ErrWorkspaceConflict
		}
		switch path.Ext(r.Destination) {
		case ".md", ".txt", ".json", ".csv":
		default:
			return ErrWorkspaceConflict
		}
	default:
		return ErrWorkspaceConflict
	}
	return nil
}
func namespaceTask(tx *sql.Tx, n WorkspaceNamespaceIntent) error {
	var ok bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_workspaces w JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=? WHERE w.id=? AND w.visibility='private' AND t.owner_person_id=w.person_id AND t.butler=b.name)`, n.TaskID, n.WorkspaceID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrWorkspaceDenied
	}
	return nil
}
func namespaceError(err error) error {
	if err != nil && strings.Contains(err.Error(), "workspace namespace conflict") {
		return ErrWorkspaceConflict
	}
	return err
}

// PrepareWorkspaceNamespace reserves both endpoints before filesystem I/O.
// Exact retries return the original immutable intent, including settled receipt.
func (db *DB) PrepareWorkspaceNamespace(r WorkspaceNamespaceRequest) (WorkspaceNamespaceIntent, error) {
	var n WorkspaceNamespaceIntent
	if err := namespaceInput(r); err != nil {
		return n, err
	}
	if err := db.CheckHouseholdExecution(); err != nil {
		return n, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return n, err
	}
	defer tx.Rollback()
	if err = db.checkWorkspaceExecutionTx(tx); err != nil {
		return n, err
	}
	n = WorkspaceNamespaceIntent{WorkspaceID: r.WorkspaceID, TaskID: r.TaskID, Operation: r.Operation, Key: r.Key, Path: r.Path, Destination: r.Destination, ArtifactID: r.ArtifactID, ExpectedRevision: r.ExpectedRevision}
	if err = namespaceTask(tx, n); err != nil {
		return n, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return n, err
	}
	sum := sha256.Sum256(data)
	n.RequestHash = hex.EncodeToString(sum[:])
	old, err := scanNamespace(tx.QueryRow(`SELECT `+namespaceColumns+` FROM workspace_namespace_intents WHERE task_id=? AND operation=? AND effect_key=?`, n.TaskID, n.Operation, n.Key))
	if err == nil {
		if old.RequestHash != n.RequestHash || old.WorkspaceID != n.WorkspaceID || old.State == "aborted" {
			return n, ErrWorkspaceConflict
		}
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return n, err
	}
	if err = namespaceAvailable(tx, n); err != nil {
		return n, err
	}
	if n.Operation != "workspace_move" {
		d, e := scanDirectory(tx.QueryRow(`SELECT id,workspace_id,path,revision,state,created_at FROM workspace_directories WHERE workspace_id=? AND path=? AND state='committed'`, n.WorkspaceID, n.Path))
		if e == nil {
			n.DirectoryID = d.ID
			n.DirectoryRevision = d.Revision
			if n.Operation == "workspace_rmdir" && d.Revision != r.ExpectedDirectoryRevision {
				return n, ErrWorkspaceConflict
			}
		} else if errors.Is(e, sql.ErrNoRows) && n.Operation == "workspace_mkdir" {
			if n.DirectoryID, err = namespaceID(); err != nil {
				return n, err
			}
			if n.DirectoryRevision, err = namespaceID(); err != nil {
				return n, err
			}
		} else {
			if errors.Is(e, sql.ErrNoRows) {
				return n, ErrWorkspaceConflict
			}
			return n, e
		}
	}
	if err = namespaceBase(tx, n); err != nil {
		return n, err
	}
	if n.ID, err = namespaceID(); err != nil {
		return n, err
	}
	_, err = tx.Exec(`INSERT INTO workspace_namespace_intents(id,workspace_id,task_id,operation,effect_key,request_hash,path,destination,artifact_id,expected_revision,directory_id,directory_revision,created_at) VALUES(?,?,?,?,?,?,?,?,NULLIF(?,''),?,?,?,?)`, n.ID, n.WorkspaceID, n.TaskID, n.Operation, n.Key, n.RequestHash, n.Path, n.Destination, n.ArtifactID, n.ExpectedRevision, n.DirectoryID, n.DirectoryRevision, now())
	if err != nil {
		return n, namespaceError(err)
	}
	if err = tx.Commit(); err != nil {
		return n, err
	}
	return db.WorkspaceNamespaceIntent(n.WorkspaceID, n.ID)
}
func namespaceAvailable(tx *sql.Tx, n WorkspaceNamespaceIntent) error {
	// Prefix overlap also prevents simultaneous remove/create of descendants.
	for _, p := range []string{n.Path, n.Destination} {
		if p == "" {
			continue
		}
		var count int
		err := tx.QueryRow(`SELECT COUNT(*) FROM workspace_namespace_intents WHERE workspace_id=? AND state='prepared' AND id<>? AND (path=? OR destination=? OR substr(path,1,length(?)+1)=?||'/' OR substr(?,1,length(path)+1)=path||'/' OR (destination<>'' AND (substr(destination,1,length(?)+1)=?||'/' OR substr(?,1,length(destination)+1)=destination||'/')))`, n.WorkspaceID, n.ID, p, p, p, p, p, p, p, p).Scan(&count)
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrWorkspaceConflict
		}
	}
	return nil
}
func namespaceBase(tx *sql.Tx, n WorkspaceNamespaceIntent) error {
	if n.Operation == "workspace_mkdir" {
		var changed int
		err := tx.QueryRow(`SELECT COUNT(*) FROM workspace_directories WHERE workspace_id=? AND ((path=? AND state='committed' AND (id<>? OR revision<>?)) OR (id=? AND state<>'committed'))`, n.WorkspaceID, n.Path, n.DirectoryID, n.DirectoryRevision, n.DirectoryID).Scan(&changed)
		if err != nil {
			return err
		}
		if changed != 0 {
			return ErrWorkspaceConflict
		}
	}
	if n.Operation == "workspace_rmdir" {
		var ok bool
		err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workspace_directories WHERE workspace_id=? AND id=? AND path=? AND revision=? AND state='committed')`, n.WorkspaceID, n.DirectoryID, n.Path, n.DirectoryRevision).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok {
			return ErrWorkspaceConflict
		}
		var count int
		err = tx.QueryRow(`SELECT (SELECT COUNT(*) FROM workspace_artifacts WHERE workspace_id=? AND state<>'trashed' AND substr(path,1,length(?)+1)=?||'/')+(SELECT COUNT(*) FROM workspace_directories WHERE workspace_id=? AND state='committed' AND substr(path,1,length(?)+1)=?||'/')+(SELECT COUNT(*) FROM workspace_write_sessions WHERE workspace_id=? AND state IN ('open','sealed') AND substr(path,1,length(?)+1)=?||'/')`, n.WorkspaceID, n.Path, n.Path, n.WorkspaceID, n.Path, n.Path, n.WorkspaceID, n.Path, n.Path).Scan(&count)
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrWorkspaceConflict
		}
		return nil
	}
	p := n.Path
	if n.Operation == "workspace_move" {
		var ok bool
		err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workspace_artifacts a WHERE a.workspace_id=? AND a.id=? AND a.path=? AND a.current_revision=? AND a.state='committed' AND NOT EXISTS(SELECT 1 FROM workspace_publications WHERE artifact_id=a.id AND state NOT IN ('committed','aborted')) AND NOT EXISTS(SELECT 1 FROM workspace_write_sessions WHERE artifact_id=a.id AND state IN ('open','sealed')))`, n.WorkspaceID, n.ArtifactID, n.Path, n.ExpectedRevision).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok {
			return ErrWorkspaceConflict
		}
		p = n.Destination
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM workspace_artifacts WHERE workspace_id=? AND state<>'trashed' AND (path=? OR substr(?,1,length(path)+1)=path||'/' OR (?='workspace_move' AND substr(path,1,length(?)+1)=?||'/'))`, n.WorkspaceID, p, p, n.Operation, p, p).Scan(&count)
	if err != nil {
		return err
	}
	if count != 0 {
		return ErrWorkspaceConflict
	}
	err = tx.QueryRow(`SELECT COUNT(*) FROM workspace_write_sessions WHERE workspace_id=? AND state IN ('open','sealed') AND (path=? OR substr(?,1,length(path)+1)=path||'/' OR (?='workspace_move' AND substr(path,1,length(?)+1)=?||'/'))`, n.WorkspaceID, p, p, n.Operation, p, p).Scan(&count)
	if err != nil {
		return err
	}
	if count != 0 {
		return ErrWorkspaceConflict
	}
	if n.Operation == "workspace_move" {
		err = tx.QueryRow(`SELECT COUNT(*) FROM workspace_directories WHERE workspace_id=? AND state='committed' AND (path=? OR substr(path,1,length(?)+1)=?||'/')`, n.WorkspaceID, p, p, p).Scan(&count)
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrWorkspaceConflict
		}
	}
	return nil
}

// CommitWorkspaceNamespace is the metadata half of publication. Call only
// after current authority and confined physical durability were verified. Its
// committed intent is the durable effect receipt; retries never repeat mutation.
func (db *DB) CommitWorkspaceNamespace(workspaceID, id string) (WorkspaceNamespaceIntent, error) {
	var n WorkspaceNamespaceIntent
	if err := db.CheckHouseholdExecution(); err != nil {
		return n, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return n, err
	}
	defer tx.Rollback()
	if err = db.checkWorkspaceExecutionTx(tx); err != nil {
		return n, err
	}
	n, err = scanNamespace(tx.QueryRow(`SELECT `+namespaceColumns+` FROM workspace_namespace_intents WHERE workspace_id=? AND id=?`, workspaceID, id))
	if err != nil {
		return n, err
	}
	if err = namespaceTask(tx, n); err != nil {
		return n, err
	}
	if n.State == "committed" {
		return n, nil
	}
	if n.State != "prepared" {
		return n, ErrWorkspaceConflict
	}
	if err = namespaceAvailable(tx, n); err != nil {
		return n, err
	}
	if err = namespaceBase(tx, n); err != nil {
		return n, err
	}
	// Clear this reservation within the same transaction before changing paths.
	if _, err = tx.Exec(`UPDATE workspace_namespace_intents SET state='committed' WHERE id=?`, id); err != nil {
		return n, err
	}
	switch n.Operation {
	case "workspace_mkdir":
		_, err = tx.Exec(`INSERT INTO workspace_directories(id,workspace_id,path,revision,created_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, n.DirectoryID, n.WorkspaceID, n.Path, n.DirectoryRevision, now())
	case "workspace_rmdir":
		var result sql.Result
		result, err = tx.Exec(`UPDATE workspace_directories SET state='removed' WHERE workspace_id=? AND id=? AND revision=? AND state='committed'`, workspaceID, n.DirectoryID, n.DirectoryRevision)
		err = workspaceChanged(result, err)
	case "workspace_move":
		var result sql.Result
		result, err = tx.Exec(`UPDATE workspace_artifacts SET path=? WHERE workspace_id=? AND id=? AND path=? AND current_revision=? AND state='committed'`, n.Destination, workspaceID, n.ArtifactID, n.Path, n.ExpectedRevision)
		err = workspaceChanged(result, err)
	}
	if err != nil {
		return n, namespaceError(err)
	}
	if err = tx.Commit(); err != nil {
		return n, err
	}
	return db.WorkspaceNamespaceIntent(workspaceID, id)
}

// AbortWorkspaceNamespace only releases metadata reservation. Recovery must
// first resolve uncertain filesystem effects; this method cannot undo them.
func (db *DB) AbortWorkspaceNamespace(workspaceID, id string) error {
	return db.abortWorkspaceNamespace(workspaceID, id, db.sql.Begin)
}

func (db *DB) abortWorkspaceNamespace(workspaceID, id string, begin func() (*sql.Tx, error)) error {
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	tx, err := begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = db.checkWorkspaceExecutionTx(tx); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE workspace_namespace_intents SET state='aborted' WHERE workspace_id=? AND id=? AND state='prepared'`, workspaceID, id)
	if err = workspaceChanged(result, err); err != nil {
		return err
	}
	return tx.Commit()
}
