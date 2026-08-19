-- ADR-W007 A4: E2 runtime-DB schema. Forward-only; fix mistakes with a new
-- migration, never by editing this file after it has shipped.

CREATE TABLE principals (
    name       TEXT PRIMARY KEY,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'butler')),
    acting_for TEXT NOT NULL DEFAULT '',
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE TABLE tokens (
    hash       TEXT PRIMARY KEY,          -- hex sha256 of the bearer secret
    principal  TEXT NOT NULL REFERENCES principals(name),
    acting_for TEXT NOT NULL DEFAULT '',  -- override; '' = principal's own
    expires_at TEXT NOT NULL DEFAULT '',  -- RFC3339; '' = no expiry
    created_at TEXT NOT NULL,
    last_used  TEXT NOT NULL DEFAULT '',
    revoked_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE events (
    seq      INTEGER PRIMARY KEY AUTOINCREMENT,
    time     TEXT NOT NULL,
    type     TEXT NOT NULL,
    node     TEXT NOT NULL DEFAULT '',
    kind     TEXT NOT NULL DEFAULT '',
    name     TEXT NOT NULL DEFAULT '',
    message  TEXT NOT NULL DEFAULT '',
    attrs    TEXT NOT NULL DEFAULT '{}'  -- JSON object
);

CREATE TABLE plans (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    principal  TEXT NOT NULL,
    acting_for TEXT NOT NULL DEFAULT '',
    summary    TEXT NOT NULL,
    risk       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN
        ('proposed', 'approved', 'applying', 'applied',
         'failed', 'rejected', 'superseded')),
    plan_json   TEXT NOT NULL,
    resolved_by TEXT NOT NULL DEFAULT '',  -- principal that approved/rejected
    resolved_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE audit (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    time       TEXT NOT NULL,
    principal  TEXT NOT NULL,
    acting_for TEXT NOT NULL DEFAULT '',
    method     TEXT NOT NULL,
    path       TEXT NOT NULL,
    status     INTEGER NOT NULL,
    remote     TEXT NOT NULL DEFAULT ''
);

-- Single row (id enforced = 1): the last Statefile commit Keystone itself
-- wrote or accepted (ADR-W007 A6).
CREATE TABLE statefile_head (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    head TEXT NOT NULL
);

-- Latest observed status per resource; written by the reconciler (E3),
-- served by the public API (E2 owns the schema).
CREATE TABLE observed (
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    node       TEXT NOT NULL DEFAULT '',
    healthy    INTEGER NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (kind, name)
);
