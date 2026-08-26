-- 0003: butler session store (ADR-W017 A1). Every Butler conversation
-- is a row bound to the authenticated caller (principal "butler"
-- acting_for <caller>) — no singleton admin session, no anonymous path
-- (ADR-W001). transcript_json holds the full session audit; only
-- post-Dispatch (already redacted/enveloped/bounded) content ever
-- reaches it.
CREATE TABLE butler_sessions (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at      TEXT NOT NULL,       -- RFC3339Nano UTC
    principal       TEXT NOT NULL,       -- always "butler"
    acting_for      TEXT NOT NULL,       -- the authenticated caller
    provider        TEXT NOT NULL,       -- provider name, or "" if none ran
    mode            TEXT NOT NULL,       -- 'full' | 'qa-only'
    final_text      TEXT NOT NULL,
    capped          INTEGER NOT NULL DEFAULT 0,
    transcript_json TEXT NOT NULL
);

CREATE INDEX butler_sessions_by_caller ON butler_sessions (acting_for, seq);
