-- 0099: scheduled Agent subscriptions with durable external-source
-- checkpoints (epic #326 S03, #456; ADR-W039 §3.6 amendment).
--
-- 0086 gave an Agent an Owner-approved standing watch over the household
-- event stream. A mailbox is not on that stream: nothing emits "mail
-- arrived", so the monitor has to ask on a cadence. This adds a coarse
-- scheduled trigger beside the event trigger — no host cron, the same
-- coordinator-held Keystone loop — plus the three things polling an
-- external source needs that matching an internal event did not:
--
--   * one run at a time, fenced, with a lease that expires rather than
--     wedging the subscription forever when the runner dies;
--   * a schedule row that separates "not due", "held" and "blocked", and
--     keeps the health timestamps a Person reads to learn why nothing
--     happened;
--   * an opaque checkpoint per subscription and source, committed by
--     compare-and-swap only after the run that read past it reports its
--     results durable.

-- trigger_kind: 'event' is every subscription 0086 knew. 'schedule' is
-- due every cadence_seconds, never matched against the event stream.
ALTER TABLE agent_subscriptions ADD COLUMN trigger_kind TEXT NOT NULL DEFAULT 'event'
    CHECK (trigger_kind IN ('event', 'schedule'));
-- cadence_seconds is only meaningful for 'schedule' (5m..24h, enforced in
-- runtimedb where the bounds are named).
ALTER TABLE agent_subscriptions ADD COLUMN cadence_seconds INTEGER NOT NULL DEFAULT 0;
-- source_connection names the integration connection a scheduled run
-- reads (integration_connections.name), or '' for none. A Personal
-- connection binds the subscription to its owning Person below.
ALTER TABLE agent_subscriptions ADD COLUMN source_connection TEXT NOT NULL DEFAULT '';
-- person_id is the canonical Person a Personal-source subscription runs
-- for and is authored by: the connection owner at proposal time. NULL
-- for a subscription with no Personal source, which keeps 0086's rule
-- (unattended household work runs as the household's admitted Owner).
-- It is NOT an Owner default: a Personal mailbox is never read "as" an
-- Owner who does not own it (ADR-W033 D2).
ALTER TABLE agent_subscriptions ADD COLUMN person_id TEXT REFERENCES butler_members(person_id);
-- version counts edits. 0086 approved a row exactly once; an edit now
-- bumps version and dispatch requires approved_version = version, so an
-- edited subscription is inert until an Owner approves that exact
-- version (the approval path checks the number it was shown).
ALTER TABLE agent_subscriptions ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

CREATE TABLE agent_subscription_schedules (
    subscription         INTEGER PRIMARY KEY REFERENCES agent_subscriptions(id) ON DELETE CASCADE,
    -- next_due_at: '' means due at the next tick (a freshly approved
    -- subscription). A claim sets it to claim-time + cadence, so however
    -- long Keystone was down, the missed intervals collapse into at most
    -- ONE catch-up run rather than a backlog replay.
    next_due_at          TEXT NOT NULL DEFAULT '',
    -- hold is a stop a person (or a permanent failure) put on the
    -- schedule: '' | paused | auth_failed | config_failed. It survives
    -- restarts and is lifted only by an explicit resume.
    hold                 TEXT NOT NULL DEFAULT ''
        CHECK (hold IN ('', 'paused', 'auth_failed', 'config_failed')),
    hold_reason          TEXT NOT NULL DEFAULT '',
    held_at              TEXT NOT NULL DEFAULT '',
    held_by              TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    -- Health, for a Person reading why nothing happened.
    last_run_at          TEXT NOT NULL DEFAULT '',
    last_success_at      TEXT NOT NULL DEFAULT '',
    last_failure_at      TEXT NOT NULL DEFAULT '',
    last_outcome         TEXT NOT NULL DEFAULT '',
    last_error           TEXT NOT NULL DEFAULT '',
    -- blocked_* is the dispatch gate's most recent refusal, cleared by
    -- the next successful claim. A block is not a failure: it does not
    -- count toward backoff and it does not move next_due_at.
    blocked_at           TEXT NOT NULL DEFAULT '',
    blocked_reason       TEXT NOT NULL DEFAULT '',
    updated_at           TEXT NOT NULL
);

CREATE TABLE agent_subscription_runs (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    subscription            INTEGER NOT NULL REFERENCES agent_subscriptions(id) ON DELETE CASCADE,
    -- The exact approved version, source and fences this run was
    -- dispatched under. Completion re-checks every one of them live.
    subscription_version    INTEGER NOT NULL,
    source_connection       TEXT NOT NULL DEFAULT '',
    connection_epoch        INTEGER NOT NULL DEFAULT 0,
    person_id               TEXT,
    coordinator_epoch       INTEGER NOT NULL DEFAULT 0,
    -- The checkpoint version the run was handed. Its commit is a
    -- compare-and-swap against this number.
    base_checkpoint_version INTEGER NOT NULL DEFAULT 0,
    state                   TEXT NOT NULL
        CHECK (state IN ('running', 'succeeded', 'failed', 'abandoned', 'fenced')),
    outcome                 TEXT NOT NULL DEFAULT '',
    task_id                 TEXT NOT NULL DEFAULT '',
    error                   TEXT NOT NULL DEFAULT '',
    due_at                  TEXT NOT NULL DEFAULT '',
    started_at              TEXT NOT NULL,
    lease_expires_at        TEXT NOT NULL,
    finished_at             TEXT NOT NULL DEFAULT ''
);

-- No overlap, enforced by the schema rather than by the loop's care: at
-- most one running run per subscription, whichever process asks.
CREATE UNIQUE INDEX agent_subscription_runs_one_running
    ON agent_subscription_runs (subscription) WHERE state = 'running';
CREATE INDEX agent_subscription_runs_by_subscription
    ON agent_subscription_runs (subscription, id);

CREATE TABLE agent_subscription_checkpoints (
    subscription    INTEGER NOT NULL REFERENCES agent_subscriptions(id) ON DELETE CASCADE,
    -- source is the source_connection the cursor is FOR ('' for none).
    -- A changed source starts from no checkpoint rather than inheriting
    -- another mailbox's position.
    source          TEXT NOT NULL,
    -- version increments on every commit; a run commits only if it
    -- still equals the version it was handed.
    version         INTEGER NOT NULL,
    -- cursor_json is opaque here (an IMAP cursor is UIDVALIDITY + UID
    -- per folder; nothing in runtimedb parses it).
    cursor_json     TEXT NOT NULL,
    committed_at    TEXT NOT NULL,
    committed_by_run INTEGER NOT NULL,
    PRIMARY KEY (subscription, source)
);
