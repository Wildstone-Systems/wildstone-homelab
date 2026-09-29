-- Verification history for ADR-W045 A6. Integrity checks and off-site
-- drills use backup_runs; source-size samples are kept separately so a
-- successful run cannot hide an abrupt disappearance of protected data.
CREATE TABLE backup_source_samples (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_name    TEXT NOT NULL,
    measured_at  TEXT NOT NULL,
    source_bytes INTEGER NOT NULL CHECK (source_bytes >= 0)
);

CREATE INDEX backup_source_samples_plan_time
    ON backup_source_samples(plan_name, seq DESC);
