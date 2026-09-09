-- 0025: named connections and the integration-work queue (ADR-W030 A5/
-- A5.1, work package P10).
--
-- An integration declares what it reports, what it answers, and what it
-- performs. The application-specific half lives in an ADAPTER, outside
-- Keystone; Keystone holds the contract, the queue, and the leases, and
-- stays application-neutral. There is nothing in these tables that
-- names Home Assistant or CalDAV — those arrive in P12 as adapters
-- against this contract.
--
-- The transport reuses the pattern the chat outbound queue already
-- proved: a durable queue, a long-poll claim, an explicit result. It
-- gets its OWN tables rather than overloading chat_outbound, because a
-- query with a five-second deadline and a message waiting for a phone to
-- come online are different things that would fight over one schema.
--
-- Two fences, and they do different jobs:
--
--   lease_epoch on a connection permits only the current holder to claim
--   work. When placement moves (P11), the epoch moves, and a result from
--   the old container is refused rather than applied.
--
--   The claim itself is connection-bound by Keystone, not by the
--   adapter's request. An adapter principal names no connection when it
--   claims; it gets only the work assigned to the connections its
--   credential is bound to, so a compromised adapter cannot claim
--   another connection's work by asking for it.

CREATE TABLE integration_connections (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    -- adapter names the adapter implementation this connection uses.
    -- Application-specific behaviour lives there; this row is a
    -- neutral description of one instance of it.
    adapter     TEXT NOT NULL,
    endpoint    TEXT NOT NULL DEFAULT '',
    -- Ownership decides the defaults (A5). A personal connection starts
    -- private, read-only, live-query-only and local-processing-only
    -- until its owner explicitly widens it; a household one starts
    -- household-shared and read-only.
    ownership   TEXT NOT NULL CHECK (ownership IN ('personal', 'household')),
    owner       TEXT NOT NULL,
    audience    TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    -- Discovered actions stay disabled until separately enabled and
    -- granted (A5). This is that switch, and it defaults off.
    actions_enabled INTEGER NOT NULL DEFAULT 0,
    indexing_enabled INTEGER NOT NULL DEFAULT 0,
    cloud_allowed   INTEGER NOT NULL DEFAULT 0,

    state       TEXT NOT NULL CHECK (state IN ('configured', 'ready', 'degraded', 'unavailable')),
    -- lease_epoch fences adapters. Only the holder of the current epoch
    -- may claim; results from an older one are refused.
    lease_epoch INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    -- degraded_reason is why a connection is not fully working. A
    -- connection that is degraded for a reason nobody can read is a
    -- connection nobody fixes.
    degraded_reason TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (adapter <> ''),
    CHECK (owner <> ''),
    CHECK (created_by <> '')
);

-- integration_principals binds an adapter credential to the connections
-- it may serve. This is what makes "an adapter cannot claim another
-- connection's work" structural: the claim query joins through here, and
-- an adapter that names a connection it is not bound to gets nothing
-- rather than an error it could probe with.
CREATE TABLE integration_principals (
    principal  TEXT NOT NULL,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    bound_at   TEXT NOT NULL,
    bound_by   TEXT NOT NULL,
    PRIMARY KEY (principal, connection)
);

-- integration_capabilities is what a connection was VERIFIED to support,
-- against the app's version and the access actually granted. Discovery
-- does not authorize newly available permissions (A5): a capability
-- appearing here means "this works", never "this is allowed".
CREATE TABLE integration_capabilities (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    kind       TEXT NOT NULL CHECK (kind IN ('event', 'query', 'action')),
    name       TEXT NOT NULL,
    version    TEXT NOT NULL DEFAULT '',
    -- available is false when the app is down or the granted access no
    -- longer covers it. Dependent work then waits rather than failing.
    available  INTEGER NOT NULL DEFAULT 1,
    verified_at TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    CHECK (name <> '')
);

CREATE UNIQUE INDEX integration_capabilities_key ON integration_capabilities (connection, kind, name);

-- integration_work is the durable queue. One row per request, claimed by
-- long poll, answered exactly once.
CREATE TABLE integration_work (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    connection  INTEGER NOT NULL REFERENCES integration_connections(seq),
    kind        TEXT NOT NULL CHECK (kind IN ('query', 'action', 'subscribe')),
    capability  TEXT NOT NULL,
    -- correlation_id ties the request to the task or routine that asked,
    -- so a result can be matched without the adapter knowing what a task
    -- is.
    correlation_id TEXT NOT NULL DEFAULT '',
    -- request_json is a versioned envelope. The adapter validates it
    -- against its own contract; Keystone carries it.
    request_json TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,

    state       TEXT NOT NULL CHECK (state IN ('queued', 'claimed', 'done', 'cancelled', 'expired')),
    -- epoch is the connection's lease at enqueue time. A result carrying
    -- an older epoch is refused.
    epoch       INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    -- deadline_at is when this stops being worth answering. An
    -- interactive query defaults to five seconds end to end including
    -- queue delay (A5.1), which is why the deadline is absolute rather
    -- than a duration the adapter starts counting on receipt.
    deadline_at TEXT NOT NULL,
    claimed_at  TEXT NOT NULL DEFAULT '',
    claimed_by  TEXT NOT NULL DEFAULT '',
    finished_at TEXT NOT NULL DEFAULT '',

    -- The four result types are the contract's whole point. A valid
    -- EMPTY answer ("the list has nothing on it") is not the same as
    -- UNAVAILABLE ("the app is down") or STALE ("this is what it said an
    -- hour ago"), and a caller that cannot tell them apart will report
    -- an outage as an empty shopping list.
    result_type TEXT NOT NULL DEFAULT '' CHECK (result_type IN ('', 'ok', 'empty', 'unavailable', 'stale')),
    result_json TEXT NOT NULL DEFAULT '{}',
    result_at   TEXT NOT NULL DEFAULT '',
    observed_at TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',

    CHECK (capability <> '')
);

CREATE INDEX integration_work_queue ON integration_work (connection, state, seq);
CREATE INDEX integration_work_correlation ON integration_work (correlation_id);

-- A finished item is finished. Reopening one would let a duplicate
-- delivery overwrite an answer the caller has already acted on.
CREATE TRIGGER integration_work_no_reopen
BEFORE UPDATE OF state ON integration_work
WHEN OLD.state IN ('done', 'cancelled', 'expired') AND NEW.state != OLD.state
BEGIN
    SELECT RAISE(ABORT, 'integration work that has finished cannot be reopened (ADR-W030 A5.1)');
END;

CREATE TRIGGER integration_connections_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON integration_connections
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'a connection lease epoch cannot move backwards (ADR-W030 A5)');
END;
