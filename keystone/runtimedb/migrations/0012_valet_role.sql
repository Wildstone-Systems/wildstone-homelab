-- 0012: the admin-surface chat adapter gets its own role (ADR-W026 A3).
--
-- It was `butler`, which was wrong in a way that only shows on upgrade.
-- ADR-W024 A5 says an external agent — Hermes, Codex, anything else —
-- is a principal with at most the butler role, so granting `converse`
-- to that role handed every existing external-agent token the ability
-- to poll the OPERATOR's private chat queue and to post messages as
-- bound accounts. A new capability must not appear on credentials that
-- were minted before it existed.
--
-- `valet` holds `converse` and nothing else, exactly as `concierge`
-- does for the family surface. Neither adapter needs read or propose:
-- a session's proposals are submitted under an internally constructed
-- butler identity acting-for the bound person, never under the
-- adapter's own credential.
--
-- SQLite cannot widen a CHECK constraint in place, so principals (and
-- tokens, which references it) are rebuilt — the same shape migrations
-- 0005 and 0008 used.
CREATE TABLE principals_v12 (
    name       TEXT PRIMARY KEY,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'butler', 'publisher', 'concierge', 'valet')),
    acting_for TEXT NOT NULL DEFAULT '',
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
INSERT INTO principals_v12 (name, role, acting_for, disabled, created_at)
    SELECT name, role, acting_for, disabled, created_at FROM principals;

CREATE TABLE tokens_v12 (
    hash       TEXT PRIMARY KEY,
    principal  TEXT NOT NULL REFERENCES principals_v12(name),
    acting_for TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    last_used  TEXT NOT NULL DEFAULT '',
    revoked_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO tokens_v12 (hash, principal, acting_for, expires_at, created_at, last_used, revoked_at)
    SELECT hash, principal, acting_for, expires_at, created_at, last_used, revoked_at FROM tokens;

DROP TABLE tokens;
DROP TABLE principals;
ALTER TABLE principals_v12 RENAME TO principals;
ALTER TABLE tokens_v12 RENAME TO tokens;
