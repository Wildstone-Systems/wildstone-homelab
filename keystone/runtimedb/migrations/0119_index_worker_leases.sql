-- Durable, coordinator-drained semantic-index work (#233).

ALTER TABLE butler_index_jobs ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE butler_index_jobs ADD COLUMN lease_owner TEXT NOT NULL DEFAULT '';
ALTER TABLE butler_index_jobs ADD COLUMN lease_until TEXT NOT NULL DEFAULT '';
ALTER TABLE butler_index_jobs ADD COLUMN available_at TEXT NOT NULL DEFAULT '';

ALTER TABLE butler_index ADD COLUMN embedding_model TEXT NOT NULL DEFAULT '';
ALTER TABLE butler_index ADD COLUMN embedding_version TEXT NOT NULL DEFAULT '';
ALTER TABLE butler_index ADD COLUMN embedding_dimension INTEGER NOT NULL DEFAULT 0;
ALTER TABLE butler_index ADD COLUMN source_revision TEXT NOT NULL DEFAULT '';

CREATE INDEX butler_index_jobs_lease
    ON butler_index_jobs (state, available_at, lease_until, priority, seq);

