package runtimedb

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/internal/confinedpath"
)

// WorkspaceReadFiles is a confined metadata query for read-tool coordinators.
// It discloses no paths until every selected revision passes current privacy
// and source authority, and persists provenance before returning metadata.
func (db *DB) WorkspaceReadFiles(ctx context.Context, a *WorkspaceAccess, workspace, directory, after string, limit int) ([]WorkspaceArtifact, string, error) {
	if _, err := confinedpath.ParseRelativePath(directory); err != nil {
		return nil, "", ErrWorkspaceDenied
	}
	prefix := directory
	if prefix != "" {
		prefix += "/"
	}
	p, err := db.workspaceTurn(ctx, a)
	if err != nil {
		return nil, "", err
	}
	defer p.mu.Unlock()
	if limit < 1 || limit > 21 {
		return nil, "", ErrWorkspaceDenied
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	if err = checkWorkspaceTask(tx, a.person, a.agent, a.task, a.connection, workspace); err != nil {
		return nil, "", err
	}
	if err = workspaceInputs(tx, p); err != nil {
		return nil, "", err
	}
	if _, err = db.workspacePolicy(tx, a, p, fmt.Sprintf("t:%d", p.task)); err != nil {
		return nil, "", err
	}
	// Stream metadata into a digest: no file bodies and no unbounded slice.
	// The cursor store retains this digest, never publishes it as authority.
	// Apply the exact directory boundary before policy checks or provenance.
	// substr avoids treating literal '%'/'_' path characters as LIKE wildcards.
	rows, err := tx.Query(`SELECT id,path,state,COALESCE(current_revision,'') FROM workspace_artifacts WHERE workspace_id=? AND (?='' OR substr(path,1,length(?))=?) ORDER BY path,id`, workspace, prefix, prefix, prefix)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%d:%s", len(prefix), prefix)
	files := []WorkspaceArtifact{}
	for rows.Next() {
		var file WorkspaceArtifact
		if err = rows.Scan(&file.ID, &file.Path, &file.State, &file.CurrentRevision); err != nil {
			rows.Close()
			return nil, "", err
		}
		if file.State != "committed" {
			continue
		}
		fmt.Fprintf(sum, "%d:%s%d:%s%d:%s%d:%s", len(file.ID), file.ID, len(file.Path), file.Path, len(file.State), file.State, len(file.CurrentRevision), file.CurrentRevision)
		if file.State == "committed" && file.Path > after && len(files) < limit {
			file.WorkspaceID = workspace
			files = append(files, file)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	for _, file := range files {
		if _, err = db.workspacePolicy(tx, a, p, "wr:"+file.CurrentRevision); err != nil {
			return nil, "", err
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO workspace_task_sources(task,revision) VALUES(?,?)`, p.task, file.CurrentRevision); err != nil {
			return nil, "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	return files, fmt.Sprintf("%x", sum.Sum(nil)), nil
}
