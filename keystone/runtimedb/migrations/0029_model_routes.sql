-- 0029: named local and cloud model routes (ADR-W030 A9/A9.1, work
-- package P14).
--
-- Which model answers is a household decision with a boundary attached:
-- some material must not leave the house, and no amount of "the local
-- one is busy" changes that. So a route is a named thing with a KIND,
-- and the data policy is checked against the kind before anything else
-- is considered.
--
-- The ordering matters and is the whole design:
--
--   1. Locality. Local-only material can reach a local route and no
--      other. This is checked first, so no fallback, escalation or
--      concurrency pressure can route around it.
--   2. Readiness. A route that has not passed its probe, or passed it
--      only as QA-only, cannot carry action authority — it may answer
--      questions and may not be trusted to use tools that do things.
--   3. Capacity. Concurrency is per route, because "the local model is
--      busy" is a real condition and queueing behind it is usually
--      better than escalating past it.
--
-- Escalation is bounded and never widens authority. A9 is explicit that
-- fallback must not widen tools, so the selection carries a verdict and
-- the caller recomputes the manifest from it — a fallback to a QA-only
-- route produces a QA-only session, not a full one that happens to be
-- answered by a weaker model.

CREATE TABLE model_routes (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    -- kind is the boundary. A route is local — running on hardware the
    -- household owns — or cloud, meaning material sent to it leaves the
    -- house. There is no third answer, and a route cannot change kind:
    -- that would silently move the boundary.
    kind       TEXT NOT NULL CHECK (kind IN ('local', 'cloud')),
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    endpoint   TEXT NOT NULL DEFAULT '',
    -- readiness is the probe verdict for THIS route (ADR-W017 A4/A9),
    -- recorded per route rather than per deployment, because a
    -- deployment with a good cloud model and a weak local one has two
    -- different answers.
    readiness  TEXT NOT NULL CHECK (readiness IN ('unprobed', 'ready', 'qa-only', 'unavailable')),
    readiness_detail TEXT NOT NULL DEFAULT '',
    probed_at  TEXT NOT NULL DEFAULT '',
    -- max_concurrency bounds in-flight calls. A local model on a small
    -- box answers one question well and four badly.
    max_concurrency INTEGER NOT NULL DEFAULT 1 CHECK (max_concurrency BETWEEN 1 AND 64),
    -- priority orders selection, lower first. Local routes sit ahead of
    -- cloud ones by default because A9's policy is local-first.
    priority   INTEGER NOT NULL DEFAULT 100,
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (provider <> '')
);

CREATE INDEX model_routes_selection ON model_routes (enabled, kind, priority, seq);

-- model_route_leases counts what is in flight per route. A row per
-- in-flight call rather than a counter column, so a crashed caller
-- leaves a row with a timestamp that can be swept rather than a counter
-- that is permanently wrong.
CREATE TABLE model_route_leases (
    seq       INTEGER PRIMARY KEY AUTOINCREMENT,
    route     INTEGER NOT NULL REFERENCES model_routes(seq),
    holder    TEXT NOT NULL,
    -- job separates interactive work from background processing, which
    -- runs local-first at lower priority (A9.1).
    job       TEXT NOT NULL CHECK (job IN ('interactive', 'background')),
    taken_at  TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    released_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX model_route_leases_live ON model_route_leases (route, released_at, expires_at);

-- A route's kind is the data boundary. Changing it would move that
-- boundary for every policy that references the route by name.
CREATE TRIGGER model_routes_kind_immutable
BEFORE UPDATE OF kind ON model_routes
WHEN NEW.kind <> OLD.kind
BEGIN
    SELECT RAISE(ABORT, 'a model route cannot change between local and cloud (ADR-W030 A9)');
END;
