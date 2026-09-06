-- 0005: household events and the publisher role (ADR-W022 A2/A3).
--
-- The operational `events` table stays as it is: it is shaped for
-- infrastructure (node, kind, name). Household facts — "Mika left
-- home", "breakfast logged" — get their own append-only stream with the
-- canonical envelope, and their own cursor.
--
-- `source` is stamped by Keystone from the publishing principal's token,
-- never taken from the payload, so a publisher can only ever speak for
-- itself. Uniqueness is (source, event_id): a retried delivery from the
-- same publisher is a no-op, and two publishers that both number their
-- events from 1 cannot collide.
CREATE TABLE household_events (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        TEXT NOT NULL,             -- publisher-supplied
    occurred_at     TEXT NOT NULL,             -- RFC3339Nano UTC, per the publisher
    recorded_at     TEXT NOT NULL,             -- RFC3339Nano UTC, stamped here
    subject         TEXT NOT NULL DEFAULT '',  -- member / device / household id
    source          TEXT NOT NULL,             -- the publishing principal
    type            TEXT NOT NULL,             -- e.g. person.left_home
    entity          TEXT NOT NULL DEFAULT '',  -- optional affected thing
    payload         TEXT NOT NULL DEFAULT '{}',-- small JSON object
    correlation_id  TEXT NOT NULL DEFAULT '',
    schema_version  INTEGER NOT NULL DEFAULT 1,
    UNIQUE (source, event_id)
);

CREATE INDEX household_events_by_type ON household_events (type, seq);
CREATE INDEX household_events_by_subject ON household_events (subject, seq);

-- The publisher role: a principal that may emit household events and do
-- nothing else. SQLite cannot widen a CHECK constraint in place, so the
-- principals table is rebuilt — and because tokens references it and
-- migrations run with foreign keys enforced inside one transaction,
-- tokens is rebuilt alongside it: the new tokens table points at the new
-- principals table, the old pair is dropped child-first (so no implicit
-- delete ever violates a constraint), and the renames restore the
-- original names. RENAME rewrites the foreign-key reference in the new
-- tokens table to follow the renamed parent.
CREATE TABLE principals_v5 (
    name       TEXT PRIMARY KEY,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'butler', 'publisher')),
    acting_for TEXT NOT NULL DEFAULT '',
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
INSERT INTO principals_v5 (name, role, acting_for, disabled, created_at)
    SELECT name, role, acting_for, disabled, created_at FROM principals;

CREATE TABLE tokens_v5 (
    hash       TEXT PRIMARY KEY,
    principal  TEXT NOT NULL REFERENCES principals_v5(name),
    acting_for TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    last_used  TEXT NOT NULL DEFAULT '',
    revoked_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO tokens_v5 (hash, principal, acting_for, expires_at, created_at, last_used, revoked_at)
    SELECT hash, principal, acting_for, expires_at, created_at, last_used, revoked_at FROM tokens;

DROP TABLE tokens;
DROP TABLE principals;
ALTER TABLE principals_v5 RENAME TO principals;
ALTER TABLE tokens_v5 RENAME TO tokens;
