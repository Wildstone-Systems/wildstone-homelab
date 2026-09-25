-- 0086: agent_subscriptions, the Events family's Owner-approved standing
-- watch (ADR-W039 §3.2 Events & subscriptions, §3.6; Phase 4, #341).
--
-- A subscription is an Agent's standing request to be told about a
-- household event again and again, without a person re-approving each
-- occurrence. Because it grants exactly that — the automation evaluator
-- (keystone/automation/subscriptions.go) opens a bounded Task on a match
-- with no person present at all — it is never enabled by the act of
-- proposing it: approved_version, approved_by and approved_at are set
-- ONLY by the same Plan-approval path every other proposal goes through
-- (keystone/proposals.FileSubscription + the approvePlan Subscription
-- branch), and enabled starts and stays 0 until that happens.
CREATE TABLE agent_subscriptions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The Agent this subscription is FOR. A bounded Task it spawns runs
    -- as this Agent, exactly as a routine occurrence runs as its
    -- assigned Butler (routine_plan_versions.assigned_butler).
    butler         INTEGER NOT NULL REFERENCES butlers(seq) ON DELETE CASCADE,
    -- match_kind names the matcher keystone/automation/subscriptions.go
    -- runs; match_json is that matcher's own parameters, opaque here.
    match_kind     TEXT NOT NULL,
    match_json     TEXT NOT NULL DEFAULT '{}',
    -- audience is who a matched Task's output may be disclosed to.
    audience       TEXT NOT NULL DEFAULT '',
    -- capability_set is a JSON array of tool names a matched Task may
    -- use. It can only ever NARROW what the Agent already holds — never
    -- widen it, however it is proposed (§3.6) — and it is re-read live
    -- from this row at every use, not cached on the Task, so narrowing
    -- or disabling a subscription after it has already matched still
    -- takes effect on the Task it spawned.
    capability_set TEXT NOT NULL DEFAULT '[]',
    budget_json    TEXT NOT NULL DEFAULT '{}',
    -- approved_version, approved_by and approved_at are the Owner
    -- approval this subscription cannot run without. version is a plain
    -- counter (1 the first time this row is approved); a later change
    -- to an approved subscription is a new proposal against a new row,
    -- the same shape a routine's own revisions take.
    approved_version INTEGER NOT NULL DEFAULT 0,
    approved_by    TEXT NOT NULL DEFAULT '',
    approved_at    TEXT NOT NULL DEFAULT '',
    -- enabled is the one bit the evaluator actually reads before it
    -- ever matches anything. 0 until approval; an Owner can also turn
    -- an approved subscription back off without deleting the row.
    enabled        INTEGER NOT NULL DEFAULT 0,
    -- cursor is this subscription's own position in the household event
    -- stream (§3.6 "per-subscription cursor"), the same cursor-based
    -- pattern automation.Engine already keeps for its own tick.
    cursor         INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE INDEX agent_subscriptions_by_butler ON agent_subscriptions (butler, enabled);

-- The evaluator's own read: every enabled subscription, regardless of
-- which Agent, is one indexed scan rather than a table scan per tick.
CREATE INDEX agent_subscriptions_enabled ON agent_subscriptions (enabled) WHERE enabled = 1;
