-- 0015: a chat conversation continues rather than restarting (ADR-W027).
--
-- ADR-W027 A11 made a Butler session multi-turn and wired the Console
-- to it. The chat path was threaded for it and never used it: every
-- inbound message opened a new session, so asking "why?" after an
-- answer reached a Butler that had never heard the first question.
--
-- This records which session a given chat conversation is currently
-- talking to. It is keyed by surface as well as transport and
-- conversation, because a Telegram direct chat is identified by the
-- PERSON's id — the same value for the household bot and the operator
-- bot — and threading those together would let a household question
-- continue an operator conversation.
--
-- A row is a pointer, not history. The conversation itself lives in
-- butler_session_turns; dropping a row here only means the next message
-- starts a new session.
CREATE TABLE chat_threads (
    transport    TEXT NOT NULL,
    conversation TEXT NOT NULL,
    surface      TEXT NOT NULL,
    session_id   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,   -- RFC3339Nano UTC
    PRIMARY KEY (transport, conversation, surface)
);
