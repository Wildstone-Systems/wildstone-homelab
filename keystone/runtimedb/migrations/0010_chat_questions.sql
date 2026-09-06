-- 0010: the chat question ledger (ADR-W026 A8, work package G6).
--
-- When a household rule asks a question over chat, the answer is a free
-- text reply rather than a button — so something has to remember what
-- the question was about. This table is that memory: one row per pushed
-- question, holding the correlation id the automation engine gave it.
--
-- The next message from that conversation, within the window, is the
-- answer: it is recorded on the household timeline as
-- `household.answered` with the SAME correlation id, so a rule's
-- trigger, its question and the household's reply are one thread — the
-- same shape an ntfy reply button produces (ADR-W024 H4), reached by a
-- different road.
CREATE TABLE chat_questions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    correlation_id TEXT NOT NULL,
    surface        TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    transport      TEXT NOT NULL,
    target         TEXT NOT NULL,
    rule           TEXT NOT NULL DEFAULT '',
    subject        TEXT NOT NULL DEFAULT '',
    asked_at       TEXT NOT NULL,
    answered_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX chat_questions_open ON chat_questions (transport, target, answered_at, id);
