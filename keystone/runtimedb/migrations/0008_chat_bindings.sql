-- 0008: chat bindings and claim codes (ADR-W026 A3/A4), plus the
-- concierge role.
--
-- A chat account is authenticated by a vendor Wildstone does not
-- control, so an adapter never asserts who somebody is: it names the
-- chat account that spoke, and Keystone resolves that to a binding or
-- refuses. A binding is created by redeeming a single-use claim code an
-- operator issued — the ADR-W020 mechanism for getting a non-technical
-- person onto their own account, reused because it is the same problem.
--
-- The concierge role holds one verb, `converse`, and nothing else: it
-- can open a family session for a bound member and deliver the answer.
-- It cannot read state, propose, approve, or emit. SQLite cannot widen a
-- CHECK constraint in place, so principals (and tokens, which references
-- it) are rebuilt exactly as migration 0005 did.
CREATE TABLE principals_v8 (
    name       TEXT PRIMARY KEY,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'butler', 'publisher', 'concierge')),
    acting_for TEXT NOT NULL DEFAULT '',
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
INSERT INTO principals_v8 (name, role, acting_for, disabled, created_at)
    SELECT name, role, acting_for, disabled, created_at FROM principals;

CREATE TABLE tokens_v8 (
    hash       TEXT PRIMARY KEY,
    principal  TEXT NOT NULL REFERENCES principals_v8(name),
    acting_for TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    last_used  TEXT NOT NULL DEFAULT '',
    revoked_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO tokens_v8 (hash, principal, acting_for, expires_at, created_at, last_used, revoked_at)
    SELECT hash, principal, acting_for, expires_at, created_at, last_used, revoked_at FROM tokens;

DROP TABLE tokens;
DROP TABLE principals;
ALTER TABLE principals_v8 RENAME TO principals;
ALTER TABLE tokens_v8 RENAME TO tokens;

-- One binding is one chat account, on one transport, on ONE surface.
-- The uniqueness is partial on purpose: revoking leaves the row as
-- history and frees the account to be bound again.
CREATE TABLE chat_bindings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    transport    TEXT NOT NULL,          -- 'telegram', 'discord', ...
    account      TEXT NOT NULL,          -- the transport's own user id
    surface      TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    identity     TEXT NOT NULL,          -- principal (admin) or household member (family)
    display_name TEXT NOT NULL DEFAULT '', -- what the transport calls them, for the operator's list
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL,          -- who issued the code this redeemed
    last_used_at TEXT NOT NULL DEFAULT '',
    revoked_at   TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX chat_bindings_live
    ON chat_bindings (transport, account, surface) WHERE revoked_at = '';
CREATE INDEX chat_bindings_by_identity ON chat_bindings (identity, id);

-- A claim code is a bearer secret with a very small blast radius: it
-- binds ONE named identity to ONE surface, once, before it expires. Only
-- the sha256 is stored, exactly as tokens are (ADR-W007 A3), so a
-- database read cannot replay one.
CREATE TABLE chat_claim_codes (
    hash        TEXT PRIMARY KEY,
    surface     TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    identity    TEXT NOT NULL,
    transport   TEXT NOT NULL DEFAULT '',  -- '' means any transport
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT NOT NULL DEFAULT '',
    binding_id  INTEGER NOT NULL DEFAULT 0
);
