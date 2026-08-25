-- 0002: backup run ledger (ADR-W011 A4). One row per backup or restore-
-- drill execution the backup engine performed — the durable record the
-- scheduler reads "when did this plan last run successfully" from, and
-- part of the run's provenance alongside its plan record and events.
CREATE TABLE backup_runs (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_name   TEXT NOT NULL,             -- BackupPlan metadata.name
    run_kind    TEXT NOT NULL,             -- 'backup' | 'drill'
    plan_id     TEXT NOT NULL DEFAULT '',  -- the plans-table record for this run
    started_at  TEXT NOT NULL,             -- RFC3339Nano UTC
    finished_at TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,             -- 'running' | 'succeeded' | 'failed'
    detail      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX backup_runs_by_plan ON backup_runs (plan_name, run_kind, seq);
