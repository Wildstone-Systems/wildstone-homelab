package runtimedb

import (
	"context"
	"fmt"
)

// CheckWorkspaceWriteSession rechecks the current originating task, tool grant,
// QA ceiling and complete source policy before each staged mutation/fence.
func (db *DB) CheckWorkspaceWriteSession(ctx context.Context, a *WorkspaceAccess, s WorkspaceWriteSession, tool string) (PrivacyPolicy, error) {
	p, err := db.workspaceTurn(ctx, a)
	if err != nil {
		return PrivacyPolicy{}, err
	}
	defer p.mu.Unlock()
	if s.TaskID != p.task {
		return PrivacyPolicy{}, ErrWorkspaceContext
	}
	switch tool {
	case "workspace_write_begin", "workspace_write_chunk", "workspace_write_commit", "workspace_patch":
	default:
		return PrivacyPolicy{}, ErrWorkspaceDenied
	}
	agent, err := db.GetButler(a.agent)
	if err != nil {
		return PrivacyPolicy{}, ErrWorkspaceDenied
	}
	// Canonical Owner and Member registries each supply their own tool class.
	// The task/resource check below also requires that original class/surface
	// still match this current Agent; a later class change cannot widen a task.
	decision, err := db.ResolveButlerTool(a.agent, ToolQuery{AgentClass: agent.AgentClass, Tool: tool, Structural: true, Service: ServiceSystem, ToolClass: agent.AgentClass, OptIn: true, TaskID: a.task})
	if err != nil {
		return PrivacyPolicy{}, err
	}
	if !decision.Allowed {
		return PrivacyPolicy{}, ErrWorkspaceDenied
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return PrivacyPolicy{}, err
	}
	defer tx.Rollback()
	if err = checkWorkspaceTask(tx, a.person, a.agent, a.task, a.connection, s.WorkspaceID); err != nil {
		return PrivacyPolicy{}, err
	}
	var writable bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM butler_tasks task JOIN butlers agent ON agent.name=task.butler JOIN models model ON model.seq=agent.model LEFT JOIN butler_sessions session ON session.seq=task.session WHERE task.seq=? AND model.enabled=1 AND model.readiness='ready' AND (task.session IS NULL OR session.mode='full'))`, p.task).Scan(&writable)
	if err != nil {
		return PrivacyPolicy{}, err
	}
	if !writable {
		return PrivacyPolicy{}, ErrWorkspaceDenied
	}
	if err = workspaceInputs(tx, p); err != nil {
		return PrivacyPolicy{}, err
	}
	policy, err := db.workspacePolicy(tx, a, p, fmt.Sprintf("t:%d", p.task))
	if err != nil {
		return policy, err
	}
	if s.BaseRevision != "" {
		base, err := db.workspacePolicy(tx, a, p, "wr:"+s.BaseRevision)
		if err != nil {
			return policy, err
		}
		policy.LocalOnly = policy.LocalOnly || base.LocalOnly
		policy.TrustedOnly = policy.TrustedOnly || base.TrustedOnly
		if _, err = tx.Exec(`INSERT OR IGNORE INTO workspace_task_sources(task,revision) VALUES(?,?)`, p.task, s.BaseRevision); err != nil {
			return policy, err
		}
	}
	policy.LocalOnly = policy.LocalOnly || s.Privacy == "local-only"
	policy.TrustedOnly = policy.TrustedOnly || s.TrustedOnly
	if err = policy.Check(p.model, p.destination); err != nil {
		return policy, err
	}
	return policy, tx.Commit()
}
