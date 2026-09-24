-- 0084: agent_deliveries, the Events family's own outbound record
-- (ADR-W039 §3.2 Events & subscriptions, §3.6; Phase 3, #339 — the
-- first writer; Phase 4, #340/#341 adds the long-poll consumer).
--
-- A Delivery is what Wildstone hands a runtime by push or on a pull:
-- a turn, a subscription match, an escalation, or — the one Phase 3
-- writes — how a proposal it filed was decided. Kept per-Agent (not
-- per-task, not per-principal) because a runtime's own credential can
-- rotate and a task closes, but "what has this Agent been told" is a
-- durable, per-subscription-cursor stream that outlives both (§3.6:
-- "per-subscription cursor, ack, replay window and TTL are
-- Wildstone-side state").
--
-- No reader exists yet. Phase 4's long-poll (GET
-- /api/v1/agent/deliveries?wait=, POST .../deliveries/{id}/ack) is what
-- consumes cursor/acked_at; this migration only gives Phase 3 somewhere
-- durable to write a proposal_outcome the moment one is decided, so
-- that outcome is not lost between now and Phase 4 shipping the
-- consumer.
CREATE TABLE agent_deliveries (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The Agent this delivery is FOR, never a runtime principal
    -- directly: a principal rotates: the Agent it is bound to does not
    -- (ADR-W039 §3.7 "runtime change ... replaces the runtime field").
    butler     INTEGER NOT NULL REFERENCES butlers(seq) ON DELETE CASCADE,
    -- kind mirrors agentapi.DeliveryKind's closed set.
    kind       TEXT NOT NULL DEFAULT 'proposal_outcome'
               CHECK (kind IN ('turn', 'subscription', 'escalation', 'proposal_outcome')),
    -- cursor is the per-subscription position this delivery advances
    -- to (§3.6); empty for a kind with no subscription behind it, which
    -- proposal_outcome — filed synchronously when a Plan is decided,
    -- not matched off the household stream — always is until a future
    -- kind needs it.
    cursor     TEXT NOT NULL DEFAULT '',
    -- payload is the enveloped event, turn or outcome, exactly as
    -- agentapi.Delivery.Payload carries it to a runtime: opaque JSON
    -- this table does not interpret.
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    -- expires_at is when an unacknowledged delivery is recorded as
    -- expired rather than redelivered forever (§3.6); empty means no
    -- expiry has been set (Phase 4's consumer decides the window).
    expires_at TEXT NOT NULL DEFAULT '',
    -- acked_at is set once a runtime acknowledges the delivery by ID
    -- (Phase 4); empty means still pending.
    acked_at   TEXT NOT NULL DEFAULT ''
);

-- The one access pattern Phase 4's long-poll and Phase 3's own "did
-- this Agent already see it" both need: an Agent's deliveries, oldest
-- first, cheaply excluding the acknowledged ones.
CREATE INDEX agent_deliveries_by_butler ON agent_deliveries (butler, acked_at, created_at);
