-- 0020: work inspection and the improvement loop (ADR-W030 A11, work
-- package P5).
--
-- P1 made the trace exist. This makes it usable by a person: labels for
-- what went wrong, evaluations built from a real failure, and a bounded
-- way to capture more detail when the structural trace is not enough.
--
-- The rule that shapes all three tables: Butler may recommend an
-- improvement or file a capability request, and cannot rewrite its own
-- prompts, tools, permissions, policies or evaluations. So every row
-- here records a HUMAN's judgement, with the human named, and nothing
-- writes to them on the model's behalf.

-- butler_task_feedback is one person's label on a task, optionally
-- pinned to the trace event that prompted it. The vocabulary is the A11
-- list and it is closed: free-text labels cannot be counted, and
-- counting them ("this happens twice a week") is the point.
CREATE TABLE butler_task_feedback (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    task       INTEGER NOT NULL REFERENCES butler_tasks(seq),
    trace      INTEGER REFERENCES butler_work_trace(seq),
    created_at TEXT NOT NULL,
    -- author is the person, never the Butler. There is no code path
    -- that writes this row on a model's behalf.
    author     TEXT NOT NULL,
    label      TEXT NOT NULL CHECK (label IN (
                   'wrong-answer', 'stale-read', 'repeated-work',
                   'inappropriate-tool', 'permission-refusal',
                   'excessive-interruption', 'useful-completion')),
    note       TEXT NOT NULL DEFAULT '',
    CHECK (author <> '')
);

CREATE INDEX butler_task_feedback_by_task ON butler_task_feedback (task, seq);
CREATE INDEX butler_task_feedback_by_label ON butler_task_feedback (label, seq);

-- butler_evaluations is a reviewed failure turned into something that
-- can be run again.
--
-- It is version-pinned on purpose (A11: "prompt, model routing, tool
-- metadata, policy, and evaluator versions are recorded so before/after
-- results can be compared"). An evaluation that does not say which
-- prompt and manifest produced the original behaviour cannot tell a fix
-- from a coincidence.
--
-- fixture_json holds SANITIZED material only: the request, the tool
-- calls and their shapes, the expected behaviour. It is written by an
-- export path that strips content, not by copying the trace.
CREATE TABLE butler_evaluations (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at       TEXT NOT NULL,
    author           TEXT NOT NULL,
    name             TEXT NOT NULL,
    -- source_task is where it came from. Nullable because an evaluation
    -- outlives the task's retention: deleting the conversation must not
    -- delete the lesson learned from it.
    source_task      INTEGER,
    summary          TEXT NOT NULL,
    expected         TEXT NOT NULL,
    -- The versions in force when the original behaviour happened.
    prompt_version   TEXT NOT NULL DEFAULT '',
    policy_version   TEXT NOT NULL DEFAULT '',
    manifest_version TEXT NOT NULL DEFAULT '',
    model_endpoint   TEXT NOT NULL DEFAULT '',
    evaluator_version TEXT NOT NULL DEFAULT '',
    fixture_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (name <> ''),
    CHECK (expected <> ''),
    CHECK (author <> '')
);

CREATE UNIQUE INDEX butler_evaluations_name ON butler_evaluations (name);

-- butler_debug_capture is temporary extra detail: explicit, scoped,
-- visibly enabled, and automatically expired (A11).
--
-- expires_at is NOT NULL and the reader refuses a window that has
-- passed, so "we turned on debug capture last March" cannot be a live
-- state nobody remembers. There is no "until I turn it off" option,
-- deliberately: that is the state this table exists to prevent.
CREATE TABLE butler_debug_capture (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    enabled_by TEXT NOT NULL,
    -- scope is one task id, so capture is never deployment-wide.
    task       INTEGER NOT NULL REFERENCES butler_tasks(seq),
    reason     TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    CHECK (enabled_by <> ''),
    CHECK (reason <> '')
);

CREATE INDEX butler_debug_capture_by_task ON butler_debug_capture (task, seq);

-- Feedback and evaluations are the record of a human judgement, so they
-- are append-only in the same sense the trace is: deletable under
-- retention, never editable after the fact.
CREATE TRIGGER butler_task_feedback_immutable
BEFORE UPDATE ON butler_task_feedback
BEGIN
    SELECT RAISE(ABORT, 'butler feedback is append-only (ADR-W030 A11)');
END;
