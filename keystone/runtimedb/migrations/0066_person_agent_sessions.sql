-- 0066: a conversation follows Person + Agent, not a transport.
--
-- acting_for and butler are audit labels.  They are useful to a human,
-- but neither is a durable identity key: names are case-insensitive and a
-- Butler has an immutable numeric row.  Keep the labels for old readers and
-- add the two structural legs slice 12 (#213) needs for continuation.
ALTER TABLE butler_sessions ADD COLUMN person_id TEXT REFERENCES butler_members(person_id);
ALTER TABLE butler_sessions ADD COLUMN agent_id INTEGER REFERENCES butlers(seq);

UPDATE butler_sessions
   SET person_id = COALESCE((SELECT m.person_id FROM butler_members m
                             WHERE m.member = butler_sessions.acting_for COLLATE NOCASE), person_id)
 WHERE person_id IS NULL;

UPDATE butler_sessions
   SET agent_id = COALESCE((SELECT b.seq FROM butlers b
                            WHERE b.name = butler_sessions.butler), agent_id)
 WHERE agent_id IS NULL AND butler <> '';

CREATE INDEX butler_sessions_person_agent
    ON butler_sessions (person_id, agent_id, seq DESC);

-- Message provenance is history, not a mutable "current connection" field.
-- connection_ref is zero for a built-in Connection (Console or Homepage) and
-- the immutable butler_addresses.seq for an external one.  The readable name
-- remains beside it so an audit can explain a revoked external Connection.
CREATE TABLE butler_session_connections (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    session         INTEGER NOT NULL REFERENCES butler_sessions(seq),
    direction       TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    connection_ref  INTEGER NOT NULL DEFAULT 0,
    connection_name TEXT NOT NULL,
    recorded_at     TEXT NOT NULL,
    CHECK (connection_name <> '')
);

CREATE INDEX butler_session_connections_latest
    ON butler_session_connections (session, direction, seq DESC);

-- /new is a boundary, not merely deletion of a transport pointer.  Without
-- this marker candidate discovery would find the conversation that /new just
-- left and immediately resume it again.
CREATE TABLE chat_thread_breaks (
    connection   INTEGER NOT NULL REFERENCES butler_addresses(seq),
    conversation TEXT NOT NULL,
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    broken_at    TEXT NOT NULL,
    PRIMARY KEY (connection, conversation, person_id)
);

-- Resume choices are opaque capabilities scoped to the Person, Agent and
-- Connection that received the filtered offer.  The raw token is shown once;
-- only its SHA-256 digest is retained.
CREATE TABLE session_resume_offers (
    token_hash      TEXT PRIMARY KEY,
    person_id       TEXT NOT NULL REFERENCES butler_members(person_id),
    agent_id        INTEGER NOT NULL REFERENCES butlers(seq),
    connection      INTEGER NOT NULL REFERENCES butler_addresses(seq),
    session         INTEGER NOT NULL REFERENCES butler_sessions(seq),
    created_at      TEXT NOT NULL,
    expires_at      TEXT NOT NULL,
    consumed_at     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX session_resume_offers_expiry
    ON session_resume_offers (expires_at, consumed_at);

-- One household setting governs every Connection. Thirty minutes preserves
-- the behaviour that predated Person+Agent continuation.
ALTER TABLE butler_runtime_settings
    ADD COLUMN session_recency_seconds INTEGER NOT NULL DEFAULT 1800
    CHECK (session_recency_seconds BETWEEN 60 AND 604800);
