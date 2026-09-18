-- 0042: a Connection belongs to exactly one Agent (ADR-W033 B7; design
-- §8.1; work slice 2b of the agent-system epic).
--
-- 0041 made a Connection admit a Person. This one gives it an Agent.
--
-- What was wrong, in the household's own terms: a binding named a
-- SURFACE. `admin` is not an Agent — it is a class of them — so
-- admitting an account to the admin surface admitted it to every Owner
-- Agent at once, and to every Owner Agent created afterwards. An Owner
-- with Alfred and Robert could not give somebody Alfred and not Robert,
-- because the schema had nowhere to write the difference.
--
-- B7 says the opposite in one line: "A Connection belongs to exactly one
-- Agent. An Agent may have many Connections, including multiple
-- Connections of the same transport type."
--
-- The Agent half of that already exists. butler_addresses is where a
-- Butler's reach is recorded — one live address per (transport,
-- external_id), so "the bot a message arrives at IS the address"
-- (ADR-W031 A7) — and an adapter's credential names the capability that
-- address carries. So this migration does not invent the Connection. It
-- points the admission, the codes, the threads and the outbound queue at
-- the Connection that was already there, instead of at a surface.
--
-- Forward-only (ADR-W007 A4). No foreign_keys=off: nothing REFERENCES
-- these four tables, so each rebuild runs under ordinary enforcement,
-- which checks the new connection columns as they are written.

-- ---------------------------------------------------------------------
-- The gate.
--
-- Every row must land on a Connection, and the mapping is exact rather
-- than a guess: a chat capability is named chat-<transport>-<surface>,
-- an address carries that name as its external_id, and the address
-- names the Butler. So the Connection for a binding is the live address
-- whose external_id is 'chat-' || transport || '-' || surface. There is
-- at most one, because butler_addresses_live says so.
--
-- Where that address does not exist, the deployment has chat bound to a
-- surface and no Agent answering on it. That is not a row this
-- migration can place: choosing an Agent on the household's behalf is a
-- decision it could never explain afterwards, and leaving the binding
-- attached to nothing is the defect it is here to remove. So it
-- refuses, and says what to create.
--
-- This will refuse every deployment upgrading with chat already bound,
-- because none of them has an address for the capability yet. That is
-- the intended cost: an Owner decides which Agent each of their chat
-- Connections belongs to, once, and the alternative is the system
-- deciding it for them silently and permanently.
CREATE TEMP TABLE connection_unassigned (problem TEXT NOT NULL);

CREATE TEMP TRIGGER connection_unassigned_refuse
BEFORE INSERT ON connection_unassigned
BEGIN
    SELECT RAISE(ABORT, 'migration 0042: ' || NEW.problem ||
        ' -- a connection belongs to exactly one agent (ADR-W033 B7). Create or choose the butler that should answer there and give it that address (`wild butler address`), then run again');
END;

INSERT INTO connection_unassigned (problem)
SELECT 'the ' || surface || '-surface ' || transport || ' connection has bindings (' ||
       COUNT(*) || ', including account ' || quote(MIN(account)) ||
       ') but no butler answers on it: nothing holds the address ' ||
       quote('chat-' || transport || '-' || surface)
FROM chat_bindings b
WHERE b.revoked_at = ''
  AND NOT EXISTS (
      SELECT 1 FROM butler_addresses a
      WHERE a.revoked_at = '' AND a.transport = b.transport
        AND a.external_id = 'chat-' || b.transport || '-' || b.surface)
GROUP BY surface, transport
LIMIT 1;

INSERT INTO connection_unassigned (problem)
SELECT 'an unredeemed ' || surface || '-surface ' || transport ||
       ' claim code names a connection no butler answers on: nothing holds the address ' ||
       quote('chat-' || transport || '-' || surface)
FROM chat_claim_codes c
WHERE c.redeemed_at = '' AND c.transport <> ''
  AND NOT EXISTS (
      SELECT 1 FROM butler_addresses a
      WHERE a.revoked_at = '' AND a.transport = c.transport
        AND a.external_id = 'chat-' || c.transport || '-' || c.surface)
LIMIT 1;

-- ---------------------------------------------------------------------
-- chat_bindings.
--
-- transport goes with surface. The Connection fixes both — an address
-- has one transport and one Agent — and a copy of a fact the referenced
-- row already holds is a copy that can disagree with it.
CREATE TABLE chat_bindings_v42 (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The Connection this admission is under (B7, B9). One Agent, one
    -- transport, one bot.
    connection   INTEGER NOT NULL REFERENCES butler_addresses(seq),
    account      TEXT NOT NULL,          -- the transport's own user id
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    display_name TEXT NOT NULL DEFAULT '', -- what the transport calls them, for the operator's list
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL,          -- who issued the code this redeemed
    last_used_at TEXT NOT NULL DEFAULT '',
    revoked_at   TEXT NOT NULL DEFAULT '',
    conversation TEXT NOT NULL DEFAULT ''
);

INSERT INTO chat_bindings_v42 (id, connection, account, person_id, display_name,
    created_at, created_by, last_used_at, revoked_at, conversation)
SELECT b.id,
       (SELECT a.seq FROM butler_addresses a
        WHERE a.revoked_at = '' AND a.transport = b.transport
          AND a.external_id = 'chat-' || b.transport || '-' || b.surface),
       b.account, b.person_id, b.display_name,
       b.created_at, b.created_by, b.last_used_at, b.revoked_at, b.conversation
FROM chat_bindings b
-- A revoked binding whose surface never had an Agent is history about a
-- model that no longer exists, and it has nowhere to land. It is
-- dropped rather than blocking the upgrade: the gate above deliberately
-- asks only about LIVE bindings, because those are the ones somebody is
-- still talking through.
WHERE EXISTS (
    SELECT 1 FROM butler_addresses a
    WHERE a.revoked_at = '' AND a.transport = b.transport
      AND a.external_id = 'chat-' || b.transport || '-' || b.surface);

DROP TABLE chat_bindings;
ALTER TABLE chat_bindings_v42 RENAME TO chat_bindings;

-- One live admission per account per Connection. The same account may
-- hold Connections to several Agents — that is B7's "an Agent may have
-- many Connections" seen from the other side — and could not be
-- expressed while the key was a surface.
CREATE UNIQUE INDEX chat_bindings_live
    ON chat_bindings (connection, account) WHERE revoked_at = '';
CREATE INDEX chat_bindings_by_person ON chat_bindings (person_id, id);
CREATE INDEX chat_bindings_by_connection ON chat_bindings (connection, id);

-- ---------------------------------------------------------------------
-- chat_claim_codes.
--
-- A code admits one Person to one Connection. transport is gone for the
-- same reason it is gone above, and so is the '' that meant "any
-- transport": a code that could be redeemed anywhere cannot name the
-- Agent it admits somebody to.
CREATE TABLE chat_claim_codes_v42 (
    hash        TEXT PRIMARY KEY,
    connection  INTEGER NOT NULL REFERENCES butler_addresses(seq),
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT NOT NULL DEFAULT '',
    binding_id  INTEGER NOT NULL DEFAULT 0
);

INSERT INTO chat_claim_codes_v42 (hash, connection, person_id, created_at,
    created_by, expires_at, redeemed_at, binding_id)
SELECT c.hash,
       (SELECT a.seq FROM butler_addresses a
        WHERE a.revoked_at = '' AND a.transport = c.transport
          AND a.external_id = 'chat-' || c.transport || '-' || c.surface),
       c.person_id, c.created_at, c.created_by, c.expires_at, c.redeemed_at, c.binding_id
FROM chat_claim_codes c
WHERE EXISTS (
    SELECT 1 FROM butler_addresses a
    WHERE a.revoked_at = '' AND a.transport = c.transport
      AND a.external_id = 'chat-' || c.transport || '-' || c.surface);

DROP TABLE chat_claim_codes;
ALTER TABLE chat_claim_codes_v42 RENAME TO chat_claim_codes;

-- ---------------------------------------------------------------------
-- chat_threads.
--
-- Closer to B10's Person + Agent + session_id: the Connection carries
-- the Agent, and the Person is already here. The conversation stays in
-- the key because one Connection carries many — a direct chat and a
-- group are different conversations on one bot.
CREATE TABLE chat_threads_v42 (
    connection   INTEGER NOT NULL REFERENCES butler_addresses(seq),
    conversation TEXT NOT NULL,
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    session_id   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,   -- RFC3339Nano UTC
    PRIMARY KEY (connection, conversation, person_id)
);

INSERT INTO chat_threads_v42 (connection, conversation, person_id, session_id, updated_at)
SELECT
       (SELECT a.seq FROM butler_addresses a
        WHERE a.revoked_at = '' AND a.transport = t.transport
          AND a.external_id = 'chat-' || t.transport || '-' || t.surface),
       t.conversation, t.person_id, t.session_id, t.updated_at
FROM chat_threads t
WHERE EXISTS (
    SELECT 1 FROM butler_addresses a
    WHERE a.revoked_at = '' AND a.transport = t.transport
      AND a.external_id = 'chat-' || t.transport || '-' || t.surface);

DROP TABLE chat_threads;
ALTER TABLE chat_threads_v42 RENAME TO chat_threads;

-- ---------------------------------------------------------------------
-- chat_outbound.
--
-- The delivery queue is per Connection, which is what an adapter polls:
-- its own bot's messages, and no other Agent's. Polling by surface gave
-- one bot every Owner Agent's replies.
CREATE TABLE chat_outbound_v42 (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    connection     INTEGER NOT NULL REFERENCES butler_addresses(seq),
    target         TEXT NOT NULL,          -- conversation id: a direct chat or a group
    body           TEXT NOT NULL,
    kind           TEXT NOT NULL DEFAULT 'reply',  -- 'reply' | 'push'
    binding_id     INTEGER NOT NULL DEFAULT 0,
    correlation_id TEXT NOT NULL DEFAULT '',
    dedupe_key     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    delivered_at   TEXT NOT NULL DEFAULT '',
    -- Added by 0019: quiet hours hold a message rather than dropping it.
    hold_until     TEXT NOT NULL DEFAULT ''
);

INSERT INTO chat_outbound_v42 (seq, connection, target, body, kind, binding_id,
    correlation_id, dedupe_key, created_at, expires_at, delivered_at, hold_until)
SELECT o.seq,
       (SELECT a.seq FROM butler_addresses a
        WHERE a.revoked_at = '' AND a.transport = o.transport
          AND a.external_id = 'chat-' || o.transport || '-' || o.surface),
       o.target, o.body, o.kind, o.binding_id,
       o.correlation_id, o.dedupe_key, o.created_at, o.expires_at, o.delivered_at, o.hold_until
FROM chat_outbound o
WHERE EXISTS (
    SELECT 1 FROM butler_addresses a
    WHERE a.revoked_at = '' AND a.transport = o.transport
      AND a.external_id = 'chat-' || o.transport || '-' || o.surface);

DROP TABLE chat_outbound;
ALTER TABLE chat_outbound_v42 RENAME TO chat_outbound;

CREATE UNIQUE INDEX chat_outbound_dedupe ON chat_outbound (dedupe_key);
CREATE INDEX chat_outbound_pending ON chat_outbound (connection, seq);

DROP TRIGGER connection_unassigned_refuse;
DROP TABLE connection_unassigned;
