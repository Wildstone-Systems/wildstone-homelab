-- ADR-W047 / #479. Keep the original workspace migration immutable.
-- A missing policy retains the original 1 GiB per-Agent ceiling. Keystone
-- installs the current Owner-approved workspace setting before writes.
CREATE TABLE workspace_quota_policy (
 agent_id INTEGER PRIMARY KEY REFERENCES butlers(seq),
 limit_bytes INTEGER NOT NULL CHECK(limit_bytes BETWEEN 1 AND 1099511627776),
 config_revision TEXT NOT NULL CHECK(config_revision<>''),
 updated_at TEXT NOT NULL
);

DROP TRIGGER workspace_reservations_quota;
CREATE TRIGGER workspace_reservations_quota BEFORE INSERT ON workspace_reservations
WHEN NEW.size_bytes +
 COALESCE((SELECT SUM(r.size_bytes) FROM workspace_reservations r JOIN agent_workspaces w ON w.id=r.workspace_id WHERE w.agent_id=(SELECT agent_id FROM agent_workspaces WHERE id=NEW.workspace_id) AND r.state='held'),0) +
 COALESCE((SELECT SUM(r.size_bytes) FROM workspace_revisions r JOIN workspace_publications p ON p.revision_id=r.id JOIN workspace_artifacts a ON a.id=r.artifact_id JOIN agent_workspaces w ON w.id=a.workspace_id WHERE p.state='committed' AND w.agent_id=(SELECT agent_id FROM agent_workspaces WHERE id=NEW.workspace_id)),0) >
 COALESCE((SELECT limit_bytes FROM workspace_quota_policy WHERE agent_id=(SELECT agent_id FROM agent_workspaces WHERE id=NEW.workspace_id)),1073741824)
BEGIN SELECT RAISE(ABORT,'quota_exceeded: workspace quota exceeded for Agent'); END;
