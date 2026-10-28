-- ADR-W049 S09: the OAuth MCP presentation receives one high-entropy,
-- expiry-bounded session per admitted task. Only its digest and encrypted
-- recoverable value are stored; the bearer and plaintext task handle never
-- enter SQLite.
ALTER TABLE external_task_admissions ADD COLUMN mcp_session_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE external_task_admissions ADD COLUMN sealed_mcp_session BLOB NOT NULL DEFAULT X'';
ALTER TABLE external_task_admissions ADD COLUMN mcp_session_expires_at TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX external_task_admissions_mcp_session
    ON external_task_admissions(mcp_session_hash)
    WHERE mcp_session_hash <> '';
