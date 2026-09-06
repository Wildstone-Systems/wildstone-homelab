-- 0009: the outbound chat queue (ADR-W026 A8).
--
-- Keystone does not dial a chat vendor and holds no long-running
-- connection per transport. Replies and rule-driven pushes are written
-- here; the adapter long-polls with a `seq` cursor and acknowledges what
-- it delivered — the same store-and-cursor pattern the event streams
-- use, because SQLite is the store and the bus (ADR-W024 A2).
--
-- Delivery is at-least-once: an adapter that dies between sending and
-- acking re-reads the message. dedupe_key is what makes that safe for
-- the producer side — enqueuing the same logical message twice (a retried
-- request, a re-fired rule) collapses into one row.
CREATE TABLE chat_outbound (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    surface        TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    transport      TEXT NOT NULL,
    target         TEXT NOT NULL,          -- conversation id: a direct chat or a group
    body           TEXT NOT NULL,
    kind           TEXT NOT NULL DEFAULT 'reply',  -- 'reply' | 'push'
    binding_id     INTEGER NOT NULL DEFAULT 0,
    correlation_id TEXT NOT NULL DEFAULT '',
    dedupe_key     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    delivered_at   TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX chat_outbound_dedupe ON chat_outbound (dedupe_key);
CREATE INDEX chat_outbound_pending ON chat_outbound (surface, transport, seq);
