-- 0085: `runtime-offline` joins butler_task_waits' closed reason
-- vocabulary (ADR-W039 §3.7, Phase 4, #340).
--
-- A task whose Agent's runtime is not connected — offline, revoked, or
-- never registered — waits exactly like a task waiting on a reply or an
-- approval: Console has to be able to tell the three apart, which is
-- the same reason 0017 closed this set in the first place. SQLite has
-- no ALTER-the-CHECK, so the table is rebuilt with the widened set and
-- both indexes recreated verbatim.
CREATE TABLE butler_task_waits_new (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    task        INTEGER NOT NULL REFERENCES butler_tasks(seq),
    created_at  TEXT NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN (
                    'reply', 'approval', 'schedule', 'model-unavailable',
                    'budget-exhausted', 'clarification', 'source-unavailable',
                    'uncertain-outcome', 'runtime-offline')),
    detail      TEXT NOT NULL DEFAULT '',
    resume_at   TEXT NOT NULL DEFAULT '',
    resolved_at TEXT NOT NULL DEFAULT '',
    resolved_by TEXT NOT NULL DEFAULT ''
);

INSERT INTO butler_task_waits_new (seq, task, created_at, reason, detail, resume_at, resolved_at, resolved_by)
SELECT seq, task, created_at, reason, detail, resume_at, resolved_at, resolved_by
  FROM butler_task_waits;

DROP INDEX butler_task_waits_by_task;
DROP INDEX butler_task_waits_open;
DROP TABLE butler_task_waits;
ALTER TABLE butler_task_waits_new RENAME TO butler_task_waits;

CREATE INDEX butler_task_waits_by_task ON butler_task_waits (task, seq);
CREATE UNIQUE INDEX butler_task_waits_open ON butler_task_waits (task) WHERE resolved_at = '';
