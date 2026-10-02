package runtimedb

import (
	"errors"
	"fmt"
	"strings"
)

const (
	defaultWorkspaceQuota int64 = 1 << 30
	maximumWorkspaceQuota int64 = 1 << 40
)

var ErrWorkspaceQuota = errors.New("quota_exceeded: workspace quota exceeded for Agent")

// SetWorkspaceQuotaPolicy installs the currently approved per-Agent setting.
// Every workspace of the Agent shares this limit; another Person's shared
// workspace does not acquire an additional allowance. A lower limit may be
// installed while already over capacity, but no further bytes may be reserved.
func (db *DB) SetWorkspaceQuotaPolicy(workspaceID string, limit int64, revision string) error {
	if limit < 1 || limit > maximumWorkspaceQuota || revision == "" {
		return fmt.Errorf("invalid workspace quota policy")
	}
	if err := db.CheckHouseholdExecution(); err != nil {
		return err
	}
	result, err := db.sql.Exec(`INSERT INTO workspace_quota_policy(agent_id,limit_bytes,config_revision,updated_at)
		SELECT agent_id,?,?,? FROM agent_workspaces WHERE id=?
		ON CONFLICT(agent_id) DO UPDATE SET limit_bytes=excluded.limit_bytes,config_revision=excluded.config_revision,updated_at=excluded.updated_at
		WHERE workspace_quota_policy.limit_bytes<>excluded.limit_bytes OR workspace_quota_policy.config_revision<>excluded.config_revision`, limit, revision, now(), workspaceID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM agent_workspaces WHERE id=?`, workspaceID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("workspace quota scope missing")
		}
	}
	return nil
}

// WorkspaceQuotaStats returns Agent-wide accounting through a specific owning
// workspace. Callers must authorize that workspace before exposing these
// aggregate numbers; paths, filenames and content are never returned.
type WorkspaceQuotaStats struct {
	LimitBytes, HeldBytes, CommittedBytes, PrivateBytes, SharedBytes int64
}

func (db *DB) WorkspaceQuotaStats(workspaceID string) (WorkspaceQuotaStats, error) {
	var stats WorkspaceQuotaStats
	tx, err := db.sql.Begin()
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()
	var agentID int64
	err = tx.QueryRow(`SELECT agent_id FROM agent_workspaces WHERE id=?`, workspaceID).Scan(&agentID)
	if err != nil {
		return stats, err
	}
	err = tx.QueryRow(`SELECT COALESCE((SELECT limit_bytes FROM workspace_quota_policy WHERE agent_id=?),?)`, agentID, defaultWorkspaceQuota).Scan(&stats.LimitBytes)
	if err != nil {
		return stats, err
	}
	rows, err := tx.Query(`SELECT w.visibility, COALESCE(SUM(charged),0) FROM (
		SELECT workspace_id,size_bytes AS charged FROM workspace_reservations WHERE state='held'
		UNION ALL
		SELECT a.workspace_id,r.size_bytes AS charged FROM workspace_revisions r
		JOIN workspace_publications p ON p.revision_id=r.id
		JOIN workspace_artifacts a ON a.id=r.artifact_id WHERE p.state='committed'
	) usage JOIN agent_workspaces w ON w.id=usage.workspace_id WHERE w.agent_id=? GROUP BY w.visibility`, agentID)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var size int64
		if err := rows.Scan(&scope, &size); err != nil {
			return stats, err
		}
		if scope == "shared" {
			stats.SharedBytes = size
		} else {
			stats.PrivateBytes = size
		}
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}
	err = tx.QueryRow(`SELECT COALESCE(SUM(r.size_bytes),0) FROM workspace_reservations r JOIN agent_workspaces w ON w.id=r.workspace_id WHERE w.agent_id=? AND r.state='held'`, agentID).Scan(&stats.HeldBytes)
	if err != nil {
		return stats, err
	}
	stats.CommittedBytes = stats.PrivateBytes + stats.SharedBytes - stats.HeldBytes
	return stats, tx.Commit()
}

func workspaceQuotaError(err error) error {
	if err != nil && strings.Contains(err.Error(), "quota_exceeded:") {
		return ErrWorkspaceQuota
	}
	return err
}
