-- 0026: the integration-adapter role (ADR-W030 A5.1, work package P10).
--
-- An adapter needs two verbs — claim the work assigned to its
-- connections, and answer that work — and nothing else. It could not
-- reuse an existing role for the reason 0012 gives at length: a new
-- capability must not appear on credentials minted before it existed.
-- Putting these verbs on `publisher` would hand every event shim the
-- ability to claim integration work; putting them on `butler` would hand
-- them to every external agent.
--
-- Its authority is not merely narrow but CONNECTION-BOUND. The claim
-- query joins through integration_principals, so an adapter gets the
-- work for the connections a human bound it to and cannot name another
-- one. The two verbs carry no read of Keystone state, no query, no act,
-- and no manage, and Keystone deliberately gains no inbound
-- administration surface an adapter could reach.
--
-- SQLite cannot widen a CHECK constraint in place, so principals (and
-- tokens, which references it) are rebuilt — the same shape 0005, 0008
-- and 0012 used.
CREATE TABLE principals_v26 (
    name       TEXT PRIMARY KEY,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'butler', 'publisher', 'concierge', 'valet', 'integration-adapter')),
    acting_for TEXT NOT NULL DEFAULT '',
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
INSERT INTO principals_v26 (name, role, acting_for, disabled, created_at)
    SELECT name, role, acting_for, disabled, created_at FROM principals;

CREATE TABLE tokens_v26 (
    hash       TEXT PRIMARY KEY,
    principal  TEXT NOT NULL REFERENCES principals_v26(name),
    acting_for TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    last_used  TEXT NOT NULL DEFAULT '',
    revoked_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO tokens_v26 (hash, principal, acting_for, expires_at, created_at, last_used, revoked_at)
    SELECT hash, principal, acting_for, expires_at, created_at, last_used, revoked_at FROM tokens;

DROP TABLE tokens;
DROP TABLE principals;
ALTER TABLE principals_v26 RENAME TO principals;
ALTER TABLE tokens_v26 RENAME TO tokens;
