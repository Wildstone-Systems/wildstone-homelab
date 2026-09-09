-- 0033: what each Butler can see and use (ADR-W031 A4/A5/A3, work
-- package B2).
--
-- A4 is what this migration is for: one Butler's material does not
-- reach another Butler's model. The health Butler runs on a local model
-- and the nutrition Butler on a cloud one, and the isolation between
-- them has to be structural rather than a filter somebody remembers to
-- apply — because the failure mode is silent and the material has
-- already left by the time anyone notices.
--
-- Three scopes, and they compose differently:
--
--   MEMORY narrows by ADDING a dimension. A record already carries the
--   person it belongs to; it now also carries the Butler holding it.
--   Retrieval requires BOTH — the reader may see it AND this Butler
--   holds it. Replacing the person-filter with a Butler-filter would
--   have made a Butler a way to read someone else's private material.
--
--   TOOLS narrow by SUBTRACTION only. A Butler-level control can
--   disable a tool its profile allows; it can never enable one the
--   profile disabled. A3 says a Butler narrows a surface and never
--   widens it, and a control that could widen would make that
--   advisory.
--
--   CONNECTIONS fail closed. A Butler reads a connection only when it
--   has been granted one. A new Butler seeing every integration by
--   default is exactly the crossover A4 forbids, arriving on the day
--   somebody creates a Butler and forgets to scope it.
--
-- The empty butler '' is the SHARED POOL of A5: material any Butler may
-- read, like "we are vegetarian". Existing rows default into it, which
-- is right for a pre-release deployment with one Butler and is also the
-- honest description of what that material already was.

-- Memory and its index gain the Butler holding the material.
ALTER TABLE butler_memory ADD COLUMN butler TEXT NOT NULL DEFAULT '';
ALTER TABLE butler_index ADD COLUMN butler TEXT NOT NULL DEFAULT '';

-- Permission-first retrieval is the existing guarantee (ADR-W030 A3):
-- filtering happens in SQL before any content loads. These indexes keep
-- that true now that the filter has a second dimension.
CREATE INDEX butler_memory_scope ON butler_memory (butler, audience, owner, seq);
CREATE INDEX butler_index_scope ON butler_index (butler, audience, owner, seq);

-- butler_tool_controls_scoped is the per-Butler tool switch.
--
-- It only ever DISABLES. There is no 'enabled' state here and the CHECK
-- refuses one, so a Butler-level row cannot turn on something the
-- profile turned off — which is A3 enforced by the schema rather than
-- by the resolver remembering.
CREATE TABLE butler_tool_scope (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    tool       TEXT NOT NULL,
    -- The only value. Written out rather than implied so a reader of
    -- this table sees the rule without going to find the resolver.
    state      TEXT NOT NULL DEFAULT 'disabled' CHECK (state = 'disabled'),
    reason     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL,

    CHECK (tool <> ''),
    CHECK (updated_by <> '')
);

CREATE UNIQUE INDEX butler_tool_scope_key ON butler_tool_scope (butler, tool);

-- butler_connection_scope is which integrations a Butler may read.
--
-- Absent means NO, which is the whole point. The accounting Butler
-- cannot see the gym data because it was never granted it, not because
-- something checked and refused.
CREATE TABLE butler_connection_scope (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    granted_at TEXT NOT NULL,
    granted_by TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    revoked_by TEXT NOT NULL DEFAULT '',

    CHECK (granted_by <> '')
);

CREATE UNIQUE INDEX butler_connection_scope_live
    ON butler_connection_scope (butler, connection) WHERE revoked_at = '';

-- A Butler cannot be granted a connection whose audience is narrower
-- than the Butler's own surface can see.
--
-- A personal connection belongs to one person (ADR-W030 A5). Handing it
-- to a Butler that any admitted adult may talk to would make that
-- Butler a way to read it — the crossover A4 forbids, arriving through
-- a grant rather than through memory.
CREATE TRIGGER butler_connection_scope_audience
BEFORE INSERT ON butler_connection_scope
WHEN (SELECT audience FROM integration_connections WHERE seq = NEW.connection) = 'private'
 AND (SELECT surface FROM butlers WHERE seq = NEW.butler) = 'family'
BEGIN
    SELECT RAISE(ABORT, 'that connection is private to one person, so a household-surface butler cannot be given it (ADR-W031 A4)');
END;
