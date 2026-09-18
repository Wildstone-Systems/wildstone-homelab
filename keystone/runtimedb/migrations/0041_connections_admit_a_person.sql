-- 0041: a Connection admits a Person (ADR-W033 A2, B9, B10; design
-- §8.3, §9; work slice 2b of the agent-system epic).
--
-- 0040 repointed every column slice 2a classified as naming a person.
-- These three were not among them, and the reason is worth recording
-- rather than quietly fixing: 2a classified them from their write path,
-- and their write path is the defect.
--
-- chat_bindings.identity has carried this comment since 0008:
--
--     identity TEXT NOT NULL,  -- principal (admin) or household member (family)
--
-- and principal/chatclaim.go enforces it — an admin-surface claim is
-- REQUIRED to name a Keystone principal. Read from there the column is
-- polymorphic by construction, which is exactly what 2a concluded.
--
-- ADR-W033 supersedes it, and is authoritative where it conflicts with
-- an earlier decision (G):
--
--   A2   every external identity — directory login, web session,
--        Telegram, Discord — attaches to the same canonical Person.
--   B9   a Connection's admission subject is a Person or a Group, and
--        an Owner-Agent Connection admits Owners only. Configuration
--        AND runtime enforce it.
--   B10  a session is Person + Agent + session_id. The Connection is
--        never the session identity.
--
-- W033 G keeps ADR-W026 A3 only "where [it requires] independent Agent
-- credentials, generalized through the Connection abstraction". The
-- ADAPTER keeps its own credential — the concierge and valet principals
-- still authenticate the bot — and the admission subject does not
-- become that credential. Conflating the two is what let a chat account
-- reach an Owner Agent as `op`, with no person behind it at all.
--
-- Forward-only (ADR-W007 A4).
--
-- No foreign_keys=off directive, unlike 0039 and 0040. Nothing in the
-- schema REFERENCES these three tables, so dropping each one orphans
-- nothing and the rebuild runs under ordinary enforcement — which is
-- stricter, not looser: the new person_id columns are checked as they
-- are written rather than swept for afterwards. The directive turns
-- enforcement off for a whole migration, so a file that declares it
-- without needing it is a hole nobody notices.

-- ---------------------------------------------------------------------
-- The gate, as 0040's.
--
-- A binding, claim code or thread whose identity names no Person on
-- record refuses the migration. That is deliberately every
-- admin-surface binding on a deployment that used the superseded model,
-- and the message says what to do about it: those accounts re-claim
-- against the Owner they belong to.
--
-- Revoking them here instead was the alternative, and it is worse in
-- the way that matters. A migration that silently withdraws somebody's
-- reach is a migration whose effect nobody reads until a bot stops
-- answering, and the operator would have to reconstruct which bindings
-- it took. `keystone person-refs` already lists them by name — these
-- three columns are person columns in the classification now — so the
-- work is visible before the upgrade rather than discovered after it.
CREATE TEMP TABLE connection_subject_unresolved (problem TEXT NOT NULL);

CREATE TEMP TRIGGER connection_subject_refuse
BEFORE INSERT ON connection_subject_unresolved
BEGIN
    SELECT RAISE(ABORT, 'migration 0041: ' || NEW.problem ||
        ' -- a Connection admits a Person (ADR-W033 B9); run `keystone person-refs`, then have that account claim again against the Person it belongs to');
END;

INSERT INTO connection_subject_unresolved (problem)
SELECT 'chat_bindings.identity for ' || quote(transport) || ' account ' || quote(account) ||
       ' on the ' || surface || ' surface is ' || quote(identity) || ', which is not a person on record'
FROM chat_bindings WHERE identity NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO connection_subject_unresolved (problem)
SELECT 'chat_claim_codes.identity on an unredeemed ' || surface || ' code is ' || quote(identity) ||
       ', which is not a person on record'
FROM chat_claim_codes WHERE identity NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO connection_subject_unresolved (problem)
SELECT 'chat_threads.identity for conversation ' || quote(conversation) || ' is ' || quote(identity) ||
       ', which is not a person on record'
FROM chat_threads WHERE identity NOT IN (SELECT member FROM butler_members) LIMIT 1;

-- ---------------------------------------------------------------------
-- chat_bindings.
--
-- The surface column stays for now. It is the wrong axis — B7 says a
-- Connection belongs to exactly one AGENT, and a surface is not an
-- Agent — but replacing it is the reach half of this work and a
-- different change from the identity half. Doing both in one migration
-- would mean neither could be reviewed on its own.
CREATE TABLE chat_bindings_v41 (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    transport    TEXT NOT NULL,          -- 'telegram', 'discord', ...
    account      TEXT NOT NULL,          -- the transport's own user id
    surface      TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    -- The Person this Connection admits (A2, B9). Not a credential: the
    -- adapter's own principal authenticates the bot, and says nothing
    -- about who is speaking through it.
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    display_name TEXT NOT NULL DEFAULT '', -- what the transport calls them, for the operator's list
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL,          -- who issued the code this redeemed
    last_used_at TEXT NOT NULL DEFAULT '',
    revoked_at   TEXT NOT NULL DEFAULT '',
    conversation TEXT NOT NULL DEFAULT ''
);

INSERT INTO chat_bindings_v41 (id, transport, account, surface, person_id, display_name,
    created_at, created_by, last_used_at, revoked_at, conversation)
SELECT b.id, b.transport, b.account, b.surface, m.person_id, b.display_name,
       b.created_at, b.created_by, b.last_used_at, b.revoked_at, b.conversation
FROM chat_bindings b JOIN butler_members m ON m.member = b.identity;

DROP TABLE chat_bindings;
ALTER TABLE chat_bindings_v41 RENAME TO chat_bindings;

CREATE UNIQUE INDEX chat_bindings_live
    ON chat_bindings (transport, account, surface) WHERE revoked_at = '';
CREATE INDEX chat_bindings_by_person ON chat_bindings (person_id, id);

-- ---------------------------------------------------------------------
-- chat_claim_codes.
--
-- A code names the Person it will admit, so the binding it produces
-- cannot name anybody else. The hash stays the primary key: only the
-- sha256 is stored, exactly as tokens are (ADR-W007 A3).
CREATE TABLE chat_claim_codes_v41 (
    hash        TEXT PRIMARY KEY,
    surface     TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    transport   TEXT NOT NULL DEFAULT '',  -- '' means any transport
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT NOT NULL DEFAULT '',
    binding_id  INTEGER NOT NULL DEFAULT 0
);

INSERT INTO chat_claim_codes_v41 (hash, surface, person_id, transport, created_at,
    created_by, expires_at, redeemed_at, binding_id)
SELECT c.hash, c.surface, m.person_id, c.transport, c.created_at,
       c.created_by, c.expires_at, c.redeemed_at, c.binding_id
FROM chat_claim_codes c JOIN butler_members m ON m.member = c.identity;

DROP TABLE chat_claim_codes;
ALTER TABLE chat_claim_codes_v41 RENAME TO chat_claim_codes;

-- ---------------------------------------------------------------------
-- chat_threads.
--
-- B10: a session is Person + Agent + session_id, and the Connection is
-- never the session identity. This table is not that key yet — it is
-- still transport + conversation + surface — but the identity leg of it
-- is a Person now, which is the half this slice owns. Slice 12 (#213)
-- takes the rest.
CREATE TABLE chat_threads_v41 (
    transport    TEXT NOT NULL,
    conversation TEXT NOT NULL,
    surface      TEXT NOT NULL,
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    session_id   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,   -- RFC3339Nano UTC
    PRIMARY KEY (transport, conversation, surface, person_id)
);

INSERT INTO chat_threads_v41 (transport, conversation, surface, person_id, session_id, updated_at)
SELECT t.transport, t.conversation, t.surface, m.person_id, t.session_id, t.updated_at
FROM chat_threads t JOIN butler_members m ON m.member = t.identity;

DROP TABLE chat_threads;
ALTER TABLE chat_threads_v41 RENAME TO chat_threads;

DROP TRIGGER connection_subject_refuse;
DROP TABLE connection_subject_unresolved;
