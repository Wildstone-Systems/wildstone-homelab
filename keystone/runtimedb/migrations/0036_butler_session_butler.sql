-- Which Butler answered (ADR-W031 A1/A2).
--
-- A session already records its SURFACE, and for a deployment with one
-- Butler per surface that was enough. It stopped being enough the moment
-- a household could have seven: a reader filtering by surface to show
-- "this Butler's conversations" shows every household Butler's, under
-- one Butler's name. That is not a missing feature, it is a screen
-- stating something false about who saw what — the one thing the whole
-- named-Butler design exists to keep answerable.
--
-- Empty is a real and permanent value, not a backfill waiting to happen.
-- Sessions recorded before this column, and sessions with no Butler at
-- all (a household escalation the engine phrases for itself), genuinely
-- did not run as one. Guessing a name for them from the surface would
-- put words in a Butler's mouth that it never said.
ALTER TABLE butler_sessions ADD COLUMN butler TEXT NOT NULL DEFAULT '';

CREATE INDEX butler_sessions_butler ON butler_sessions (butler, seq);
