-- 0017: durable work and trace schema (ADR-W030 A4, A11, work package P1).
--
-- Until now a Butler run lived in memory: keystone/butler held a Session,
-- looped, and wrote the answer to butler_sessions when it was over. A
-- restart in the middle lost the work, and there was nowhere to record
-- WHY a run stopped, what it had already verified, or what evidence
-- justified calling it done.
--
-- These tables are that ledger. Three properties are the point, and each
-- is enforced here rather than left to the caller:
--
--   1. Attribution is not rewritten. Tasks POINT AT butler_sessions and
--      its turns; nothing in 0003/0014 is altered. An existing
--      conversation keeps reading exactly as it did, with no task rows
--      at all (ADR-W030 P1). Deterministic W024 timeline replay likewise
--      still works against an empty task table (A4).
--   2. A task cannot appear completed from a partial write. Completion
--      requires evidence, and the trigger below refuses the state change
--      when there is none — so a process killed between "write the
--      evidence" and "mark it done" leaves a running task, never a
--      completed one that never happened (A4: "completion requires
--      evidence appropriate to the objective, not merely a model saying
--      done").
--   3. Trace and transition history are append-only. Rows may be DELETED
--      — retention and offboarding in P16 need that — but never UPDATED.
--      A record that can be edited after the fact is not an audit trail.
--
-- Serialized payloads carry an explicit envelope_version so a later
-- package can change a payload shape without guessing what an old row
-- meant (A5.1 "versioned request/result envelopes").
--
-- Permission metadata is NOT NULL and non-empty everywhere content can
-- appear. A trace event or evidence row whose audience is unknown cannot
-- be permission-filtered later, so it is refused at write time rather
-- than becoming a row nobody can safely show (A11).

-- butler_tasks is one unit of durable work: a conversational task or a
-- Wildstone-owned routine run. Both use this machinery (A4); the routine
-- DEFINITION lives elsewhere and each occurrence is a row here.
CREATE TABLE butler_tasks (
    seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at          TEXT NOT NULL,     -- RFC3339Nano UTC
    updated_at          TEXT NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('conversation', 'routine')),
    state               TEXT NOT NULL CHECK (state IN (
                            'pending', 'running', 'waiting',
                            'completed', 'failed', 'cancelled',
                            'paused-offboarded')),

    -- author is the member who asked for the work and never changes;
    -- owner is who operates it now. Offboarding moves a shared task's
    -- owner to the homelab administrator and "preserves original
    -- authorship" (A4) — two columns is what that sentence costs.
    author              TEXT NOT NULL,
    owner               TEXT NOT NULL,

    -- surface and profile are the authority the task was admitted under.
    -- They are snapshotted because authority is rechecked on resume
    -- against what the task CLAIMED, not against whatever the caller
    -- would get today (A4, A9).
    surface             TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    profile             TEXT NOT NULL,
    audience            TEXT NOT NULL CHECK (audience IN ('private', 'shared')),

    -- Conversation linkage. Both nullable: a routine run has no
    -- conversation. origin_turn is the butler_session_turns ordinal that
    -- started the task, so a Console timeline can point back at the
    -- exact message without a join through text.
    session             INTEGER REFERENCES butler_sessions(seq),
    origin_turn         INTEGER,

    -- correlation_id ties a routine run to the W024 household timeline's
    -- occurrence/action ids. The timeline stays authoritative for
    -- deterministic outcomes; anything copied here is a projection (A4).
    correlation_id      TEXT NOT NULL DEFAULT '',

    objective           TEXT NOT NULL,
    completion_criteria TEXT NOT NULL,

    -- Budgets and grants are persisted so they survive a restart and do
    -- NOT reset on retry (A4). Waiting does not consume active-work
    -- budget; the absolute deadline still expires.
    grants_json         TEXT NOT NULL DEFAULT '{}',
    budget_json         TEXT NOT NULL DEFAULT '{}',
    deadline_at         TEXT NOT NULL DEFAULT '',
    next_wakeup_at      TEXT NOT NULL DEFAULT '',

    -- lease_epoch fences attempts and, in P3, the single active
    -- coordinator (A11). It only ever increases.
    lease_epoch         INTEGER NOT NULL DEFAULT 0,

    -- uncertainty records that an action's outcome is unknown, so a
    -- resume pauses instead of blindly retrying it (A4).
    uncertainty         TEXT NOT NULL DEFAULT '',
    result_summary      TEXT NOT NULL DEFAULT '',

    -- retention follows the conversation's configured policy; the
    -- default is manual deletion (A10).
    retention           TEXT NOT NULL CHECK (retention IN ('manual', 'conversation', 'ephemeral')),
    envelope_version    INTEGER NOT NULL
);

CREATE INDEX butler_tasks_by_owner ON butler_tasks (owner, seq);
CREATE INDEX butler_tasks_by_session ON butler_tasks (session, seq);
CREATE INDEX butler_tasks_by_state ON butler_tasks (state, seq);
-- The scheduler's only hot query: what is due to wake up.
CREATE INDEX butler_tasks_by_wakeup ON butler_tasks (next_wakeup_at) WHERE next_wakeup_at <> '';

-- butler_task_steps is the validated step ledger a resume replays from.
-- A resumed task reconstructs context from THESE rows with current
-- permissions, rather than replaying a saved model transcript (A4).
CREATE TABLE butler_task_steps (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    ordinal          INTEGER NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('model', 'tool', 'wait', 'delivery')),
    name             TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('proposed', 'running', 'verified', 'failed', 'skipped')),
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    params_json      TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL
);

CREATE UNIQUE INDEX butler_task_steps_order ON butler_task_steps (task, ordinal);

-- butler_task_attempts is the checkpoint pair around every dispatch: a
-- row goes in BEFORE the tool or model call and is transitioned when its
-- result arrives (A4). epoch is copied from the task's lease at dispatch,
-- which is what lets a late result from a superseded attempt be rejected
-- instead of overwriting good state.
CREATE TABLE butler_task_attempts (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    step             INTEGER NOT NULL REFERENCES butler_task_steps(seq),
    attempt          INTEGER NOT NULL,
    epoch            INTEGER NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('dispatched', 'succeeded', 'failed', 'stalled', 'fenced', 'cancelled')),
    started_at       TEXT NOT NULL,
    ended_at         TEXT NOT NULL DEFAULT '',
    -- Post-redaction result only, exactly as butler_sessions requires of
    -- its transcript. Nothing pre-Dispatch reaches this column.
    result_json      TEXT NOT NULL DEFAULT '{}',
    error_text       TEXT NOT NULL DEFAULT '',
    envelope_version INTEGER NOT NULL
);

CREATE UNIQUE INDEX butler_task_attempts_order ON butler_task_attempts (step, attempt);
CREATE INDEX butler_task_attempts_by_task ON butler_task_attempts (task, seq);

-- butler_task_transitions is the append-only history of state changes.
-- The task row carries the CURRENT state; this carries how it got there,
-- including who decided.
CREATE TABLE butler_task_transitions (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    task       INTEGER NOT NULL REFERENCES butler_tasks(seq),
    at         TEXT NOT NULL,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    actor      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX butler_task_transitions_by_task ON butler_task_transitions (task, seq);

-- butler_task_waits records WHY a task is waiting. The set is closed
-- because "waiting" with a free-text reason is how a stalled task
-- becomes indistinguishable from a task waiting for a human — and
-- Console has to tell those apart (A4).
CREATE TABLE butler_task_waits (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    task        INTEGER NOT NULL REFERENCES butler_tasks(seq),
    created_at  TEXT NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN (
                    'reply', 'approval', 'schedule', 'model-unavailable',
                    'budget-exhausted', 'clarification', 'source-unavailable',
                    'uncertain-outcome')),
    detail      TEXT NOT NULL DEFAULT '',
    resume_at   TEXT NOT NULL DEFAULT '',
    resolved_at TEXT NOT NULL DEFAULT '',
    resolved_by TEXT NOT NULL DEFAULT ''
);

CREATE INDEX butler_task_waits_by_task ON butler_task_waits (task, seq);
-- Only one wait may be open per task: a task waits for one thing at a
-- time, and two open waits means nobody knows what resumes it.
CREATE UNIQUE INDEX butler_task_waits_open ON butler_task_waits (task) WHERE resolved_at = '';

-- butler_task_evidence is what makes completion honest. Every row names
-- what was observed, where, and when — and under which permission label,
-- so an evidence list can be filtered for a reader who may not see every
-- source (A11).
CREATE TABLE butler_task_evidence (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    recorded_at      TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('observation', 'tool-result', 'approval', 'delivery', 'external-verification')),
    description      TEXT NOT NULL,
    source_id        TEXT NOT NULL DEFAULT '',
    observed_at      TEXT NOT NULL DEFAULT '',
    permission_label TEXT NOT NULL,
    audience         TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    payload_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (permission_label <> ''),
    CHECK (description <> '')
);

CREATE INDEX butler_task_evidence_by_task ON butler_task_evidence (task, seq);

-- butler_work_trace is the structured, append-only work trace of A11:
-- what the system can actually observe about a run. Structural metadata
-- lives here; captured CONTENT inherits its source's retention, which is
-- why permission_label and audience are mandatory on every row and why
-- payload_json holds redacted, bounded material only.
--
-- Private chain-of-thought is never requested or stored. A rationale
-- that was shown to a person may be recorded as model-authored
-- explanation — that is a 'proposal' or 'answer' event, not reasoning.
CREATE TABLE butler_work_trace (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    at               TEXT NOT NULL,
    type             TEXT NOT NULL,
    step             INTEGER REFERENCES butler_task_steps(seq),
    attempt          INTEGER REFERENCES butler_task_attempts(seq),
    epoch            INTEGER NOT NULL DEFAULT 0,
    actor            TEXT NOT NULL DEFAULT '',
    -- Versions of the things that decide behaviour, so a before/after
    -- comparison in P5 knows what actually changed (A11).
    model_endpoint   TEXT NOT NULL DEFAULT '',
    model_verdict    TEXT NOT NULL DEFAULT '',
    prompt_version   TEXT NOT NULL DEFAULT '',
    policy_version   TEXT NOT NULL DEFAULT '',
    manifest_version TEXT NOT NULL DEFAULT '',
    source_ids       TEXT NOT NULL DEFAULT '',  -- comma-separated context source ids
    permission_label TEXT NOT NULL,
    audience         TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    redacted         INTEGER NOT NULL DEFAULT 1,
    payload_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (permission_label <> ''),
    CHECK (type <> '')
);

CREATE INDEX butler_work_trace_by_task ON butler_work_trace (task, seq);

-- Completion needs evidence — enforced by the schema, not by hoping the
-- caller remembered. Both triggers are needed: one for a task updated
-- into completion, one for the (illegal) shortcut of inserting a task
-- that is already done.
CREATE TRIGGER butler_tasks_complete_needs_evidence_update
BEFORE UPDATE OF state ON butler_tasks
WHEN NEW.state = 'completed'
 AND NOT EXISTS (SELECT 1 FROM butler_task_evidence WHERE task = NEW.seq)
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot complete without evidence (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_complete_needs_evidence_insert
BEFORE INSERT ON butler_tasks
WHEN NEW.state = 'completed'
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot be created completed (ADR-W030 A4)');
END;

-- The lease epoch only moves forward. A rewound epoch would un-fence
-- attempts that were already superseded (A5, A11).
CREATE TRIGGER butler_tasks_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON butler_tasks
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'butler task lease epoch cannot move backwards (ADR-W030 A11)');
END;

-- Authorship is immutable; ownership transfers on offboarding.
CREATE TRIGGER butler_tasks_author_immutable
BEFORE UPDATE OF author ON butler_tasks
WHEN NEW.author <> OLD.author
BEGIN
    SELECT RAISE(ABORT, 'butler task authorship is immutable (ADR-W030 A4)');
END;

-- Append-only: history and trace rows may be deleted under retention,
-- never edited.
CREATE TRIGGER butler_task_transitions_immutable
BEFORE UPDATE ON butler_task_transitions
BEGIN
    SELECT RAISE(ABORT, 'butler task transitions are append-only (ADR-W030 A11)');
END;

CREATE TRIGGER butler_work_trace_immutable
BEFORE UPDATE ON butler_work_trace
BEGIN
    SELECT RAISE(ABORT, 'butler work trace is append-only (ADR-W030 A11)');
END;

CREATE TRIGGER butler_task_evidence_immutable
BEFORE UPDATE ON butler_task_evidence
BEGIN
    SELECT RAISE(ABORT, 'butler task evidence is append-only (ADR-W030 A4)');
END;
