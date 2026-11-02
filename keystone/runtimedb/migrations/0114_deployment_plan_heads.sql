-- One current approved projection Plan per installation-owned application slot.
-- This is an authority version pointer; approval/audit remains in plans.
CREATE TABLE deployment_plan_heads (
    instance_id TEXT PRIMARY KEY,
    plan_seq INTEGER NOT NULL REFERENCES plans(seq)
);

-- Safe, non-secret diagnostics for resumable local publication, not authority.
CREATE TABLE deployment_publication_attempts (
    plan_seq INTEGER PRIMARY KEY REFERENCES plans(seq),
    phase TEXT NOT NULL CHECK(phase IN ('promoting','blocked','failed','unknown','promoted')),
    detail_code TEXT NOT NULL CHECK(detail_code IN ('inputs_changed','authority_changed','ref_conflict','local_unavailable','ack_pending','observed')),
    observed_at TEXT NOT NULL
);
