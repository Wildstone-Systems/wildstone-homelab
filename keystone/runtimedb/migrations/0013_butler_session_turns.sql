-- 0013: a Butler session becomes a conversation (ADR-W027).
--
-- Until now a session was one exchange: the caller's question was never
-- stored at all, and the answer was a single final_text column. That is
-- enough for an audit of what the Butler SAID, and not enough for two
-- things the product needs — asking a follow-up, and reading back what
-- was actually asked.
--
-- Turns are rows rather than a JSON blob on the session because they are
-- appended one at a time and read in order, which is what a table is
-- for. The tool trace stays per-turn: "which reads produced THIS answer"
-- is the question an audit asks, and one merged trace per session
-- cannot answer it once a session has three answers in it.
--
-- Only post-Dispatch content reaches text or tool_json, exactly as
-- ADR-W017 A1 requires of transcript_json — the same redact pass runs
-- over both.
CREATE TABLE butler_session_turns (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    session    INTEGER NOT NULL REFERENCES butler_sessions(seq),
    ordinal    INTEGER NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    text       TEXT NOT NULL,
    created_at TEXT NOT NULL,           -- RFC3339Nano UTC
    capped     INTEGER NOT NULL DEFAULT 0,
    tool_json  TEXT NOT NULL DEFAULT '{}',
    plan_ids   TEXT NOT NULL DEFAULT '' -- comma-separated plan record ids
);

CREATE UNIQUE INDEX butler_session_turns_order ON butler_session_turns (session, ordinal);

-- Backfill what can honestly be backfilled. Every existing session gets
-- its answer as turn 0; there is no turn for the question because the
-- question was never recorded. A reader sees a conversation that opens
-- with an answer, which is true, rather than an invented question.
INSERT INTO butler_session_turns (session, ordinal, role, text, created_at, capped, tool_json, plan_ids)
    SELECT seq, 0, 'assistant', final_text, created_at, capped, transcript_json, ''
    FROM butler_sessions;
