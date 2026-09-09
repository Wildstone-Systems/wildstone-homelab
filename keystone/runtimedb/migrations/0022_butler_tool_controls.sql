-- 0022: tool controls, grants, and capability requests (ADR-W030 A7/
-- A7.1/A8, work package P7).
--
-- Three separate questions that were previously one:
--
--   May this profile use this tool at all? An administrator's switch,
--   which wins over everything — a cached manifest, a saved task, a
--   grant issued five minutes ago. That is what "disable takes effect
--   immediately" has to mean.
--
--   May this particular piece of work use it right now? A temporary
--   grant, bound to one task or one routine, that expires when the work
--   ends whether or not anyone remembers to withdraw it.
--
--   May the Butler ASK for something it does not have? A durable
--   request record. A7: Butler can request a missing capability and
--   cannot self-escalate, so a request is a row a human acts on, never
--   a thing that grants itself.
--
-- What none of these can do is widen a profile's STRUCTURAL ceiling. The
-- household gateway's tool map does not contain an operator tool, so
-- there is nothing for a control or a grant to switch on: the resolver
-- refuses before it looks at either. A grant that could widen the
-- ceiling would make the structural boundary advisory, and the whole
-- point of building it structurally was that it is not.

-- butler_tool_controls is the administrator's switch, per profile and
-- tool. Absent means "follow the tool's own default", which is why this
-- table is sparse rather than a row per tool per profile.
CREATE TABLE butler_tool_controls (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    profile    TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
    tool       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('enabled', 'disabled')),
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    CHECK (tool <> ''),
    CHECK (updated_by <> '')
);

CREATE UNIQUE INDEX butler_tool_controls_key ON butler_tool_controls (profile, tool);

-- butler_tool_grants is a temporary permission bound to a piece of work.
--
-- scope is what it dies with. A task grant expires when the task ends or
-- is cancelled; a routine grant expires with its run. Both also carry an
-- absolute deadline, because "the task ended" depends on the task ending
-- and a deadline does not.
CREATE TABLE butler_tool_grants (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    scope      TEXT NOT NULL CHECK (scope IN ('task', 'routine')),
    -- scope_id is the task id ("bt-<n>") or the routine name. Text so
    -- one column serves both.
    scope_id   TEXT NOT NULL,
    profile    TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
    tool       TEXT NOT NULL,
    granted_at TEXT NOT NULL,
    granted_by TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    revoked_by TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    -- cleanup_plan links the Plan that must run when this grant ends —
    -- removing a credential, closing an opened path. Recorded here so a
    -- grant that needs cleanup cannot quietly expire without one.
    cleanup_plan TEXT NOT NULL DEFAULT '',
    CHECK (scope_id <> ''),
    CHECK (tool <> ''),
    CHECK (granted_by <> ''),
    CHECK (expires_at <> '')
);

CREATE INDEX butler_tool_grants_live ON butler_tool_grants (scope, scope_id, tool);

-- butler_capability_requests is how the Butler asks for something it
-- does not have. A request grants nothing: its terminal states are
-- decided by a person, and 'approved' still does not enable the tool —
-- enabling is a separate control row, deliberately, so approving a
-- request and turning something on cannot be one click by accident.
CREATE TABLE butler_capability_requests (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL CHECK (kind IN ('supported-tool', 'development')),
    -- requested_by is the MEMBER the asking session was acting for. The
    -- Butler asks on someone's behalf; it does not have wishes of its
    -- own, and an unattributed request is one nobody can weigh.
    requested_by TEXT NOT NULL,
    profile     TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
    -- tool is the name for a supported-tool request, and empty for a
    -- development request, which describes something that does not exist
    -- yet.
    tool        TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('open', 'approved', 'declined', 'implemented')),
    created_at  TEXT NOT NULL,
    decided_at  TEXT NOT NULL DEFAULT '',
    decided_by  TEXT NOT NULL DEFAULT '',
    decision_note TEXT NOT NULL DEFAULT '',
    CHECK (requested_by <> ''),
    CHECK (description <> ''),
    CHECK (kind <> 'supported-tool' OR tool <> '')
);

CREATE INDEX butler_capability_requests_state ON butler_capability_requests (state, seq);
