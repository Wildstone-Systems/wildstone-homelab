-- 0019: validated Butler runtime settings and quiet-hours holds
-- (ADR-W030 A4/A10, work package P4).
--
-- Iteration limits, the provider-stall deadline, the retry ceiling and
-- the household's quiet hours were all constants in code. Every one of
-- them is a household preference — how long the Butler may grind on a
-- question before checking in, and what hour of the night it may not
-- send a message about it — so they follow the ADR-W017 A9 pattern the
-- provider choice already uses: runtime state, editable without a
-- restart, validated on write rather than trusted on read.
--
-- ONE ROW, like butler_config, for the same reason: this is the
-- deployment's policy, not a collection of them.
--
-- The two surfaces get separate round limits because they are answering
-- different questions. A household "what's for dinner" that takes 24
-- model rounds has gone wrong; an operator diagnosis that walks a
-- dependency chain legitimately needs them. A4's starting points are 16
-- and 24, subject to evaluation, which is exactly why they are here and
-- not in a const block.
CREATE TABLE butler_runtime_settings (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    household_rounds  INTEGER NOT NULL,
    operator_rounds   INTEGER NOT NULL,
    -- Aggregate tool calls per task, across every slice and restart.
    max_tool_calls    INTEGER NOT NULL,
    -- How long one model call may hang before the attempt is fenced and
    -- retried. A stall is not a failure and not a hang: it is a deadline.
    stall_seconds     INTEGER NOT NULL,
    -- How many times one stalled or failed model call is retried inside
    -- the remaining budget before a person is told.
    retry_limit       INTEGER NOT NULL,
    -- How long a recorded observation may be reused on a resume before
    -- it is re-read instead. "Do not repeat already-verified effects"
    -- and "refresh stale observations" are both A4; this number is where
    -- they meet.
    observation_ttl_seconds INTEGER NOT NULL,
    -- Quiet hours in the household timezone, as minutes from midnight.
    -- Equal values mean no quiet hours. A window that wraps midnight is
    -- normal and is what a household actually configures.
    quiet_start_min   INTEGER NOT NULL,
    quiet_end_min     INTEGER NOT NULL,
    timezone          TEXT NOT NULL DEFAULT 'UTC',
    updated_at        TEXT NOT NULL,
    updated_by        TEXT NOT NULL DEFAULT '',

    CHECK (household_rounds BETWEEN 1 AND 64),
    CHECK (operator_rounds BETWEEN 1 AND 64),
    CHECK (max_tool_calls BETWEEN 1 AND 512),
    CHECK (stall_seconds BETWEEN 5 AND 900),
    CHECK (retry_limit BETWEEN 0 AND 10),
    CHECK (observation_ttl_seconds BETWEEN 0 AND 86400),
    CHECK (quiet_start_min BETWEEN 0 AND 1439),
    CHECK (quiet_end_min BETWEEN 0 AND 1439)
);

-- hold_until defers a message without dropping it (ADR-W030 A10:
-- non-urgent notices WAIT through quiet hours). A held message is still
-- queued, still deduplicated, and still expires on its own TTL — the
-- adapter simply does not see it yet.
--
-- Existing rows default to '', which means "deliverable now", so nothing
-- already queued changes behaviour.
ALTER TABLE chat_outbound ADD COLUMN hold_until TEXT NOT NULL DEFAULT '';
