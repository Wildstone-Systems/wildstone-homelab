-- 0015: a chat conversation continues rather than restarting (ADR-W027).
--
-- ADR-W027 A11 made a Butler session multi-turn and wired the Console
-- to it. The chat path was threaded for it and never used it: every
-- inbound message opened a new session, so asking "why?" after an
-- answer reached a Butler that had never heard the first question.
--
-- This records which session a given chat conversation is currently
-- talking to.
--
-- The key is (transport, conversation, surface, identity), and every
-- part of it earns its place:
--
--   surface   — a Telegram direct chat is identified by the PERSON's
--               id, the same value for the household bot and the
--               operator bot. Without this, a household question could
--               continue an operator conversation.
--   identity  — a group room has several bound members speaking into
--               one conversation. A session is principal-bound
--               (ADR-W001), so Alice cannot continue Bob's; with one
--               row per room the pointer would be rewritten every time
--               the speaker changed, and BOTH of them would lose their
--               thread on every alternate message.
--
-- A row is a pointer, not history. The conversation itself lives in
-- butler_session_turns; dropping a row here only means the next message
-- starts a new session.
CREATE TABLE chat_threads (
    transport    TEXT NOT NULL,
    conversation TEXT NOT NULL,
    surface      TEXT NOT NULL,
    identity     TEXT NOT NULL,
    session_id   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,   -- RFC3339Nano UTC
    PRIMARY KEY (transport, conversation, surface, identity)
);
