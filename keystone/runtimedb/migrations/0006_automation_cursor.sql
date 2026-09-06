-- 0006: the automation engine's position in the household stream
-- (ADR-W022 H3). One row: the last household_events.seq the engine has
-- evaluated. A restart resumes from here rather than re-reading history;
-- idempotency does not depend on it (a decision is recorded under a
-- deterministic event id, so re-evaluating a trigger is a no-op), but
-- re-reading a long stream on every start is work with no purpose.
CREATE TABLE automation_cursor (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    seq  INTEGER NOT NULL
);
