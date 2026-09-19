-- 0059: an Agent's tools are assigned, not merely un-narrowed
-- (ADR-W033 design §6.2, §6.3, §6.4; slice 9 PR 2).
--
-- WHY THIS IS A NEW MODEL AND NOT A COLUMN. Until now an Agent could
-- reach everything its gateway registers, minus whatever 0033's
-- butler_tool_scope denied — and that table only ever DENIES, by a CHECK
-- that refuses any other state. Under those rules §6.3's dynamic bundle
-- grants nothing: "all Member tools for Home Assistant" describes tools
-- the Agent could already reach. A bundle only means something when the
-- default is deny.
--
-- SO THE DEFAULT CHANGES, FOR EVERY AGENT. An Agent listed in
-- butler_tool_governance is default-deny and reaches exactly what is
-- assigned to it, and every Agent ends up listed: new ones when they are
-- created, existing ones on the first start after this migration, by
-- api.BackfillAgentTools.
--
-- THE BACKFILL IS NOT IN THIS FILE, and cannot be. The default set is
-- "all currently available tools" (§6.4), and what is available is a
-- fact about the gateways this deployment wired — which SQL cannot see.
-- A migration could only have guessed, and guessing in the widening
-- direction is how an Agent ends up holding something nobody chose.
--
-- The window between this migration and that call is the one moment the
-- old model is still in force, and it fails in the safe direction: an
-- Agent nobody has enrolled yet reaches what it always did, rather than
-- nothing.

-- butler_tool_governance is the marker, and it exists SEPARATELY from
-- the assignments on purpose.
--
-- The obvious shortcut is to say an Agent is governed when it has at
-- least one assignment. That inverts catastrophically: an Owner
-- removing an Agent's last tool would take it out of the governed set
-- and hand it everything the gateway registers. Revoking all access
-- would grant all access, at exactly the moment somebody was trying to
-- lock an Agent down.
--
-- So enrollment is its own fact, written once and never inferred.
CREATE TABLE butler_tool_governance (
    butler     INTEGER PRIMARY KEY REFERENCES butlers(seq),
    -- since is when this Agent started being default-deny. Kept because
    -- "why can this Agent suddenly not do X" is answered by the date it
    -- changed model, not by the assignments it has now.
    since      TEXT NOT NULL,
    enrolled_by TEXT NOT NULL,

    CHECK (enrolled_by <> '')
);

-- butler_tool_assignments is an explicit tool, named one at a time.
--
-- §6.3: "Explicit tool lists remain static." A tool named here is this
-- Agent's until somebody removes it, and a new tool appearing in the
-- catalog never joins the list by itself.
CREATE TABLE butler_tool_assignments (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    butler      INTEGER NOT NULL REFERENCES butlers(seq),
    tool        TEXT NOT NULL,
    assigned_at TEXT NOT NULL,
    assigned_by TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',

    CHECK (tool <> ''),
    CHECK (assigned_by <> '')
);

CREATE UNIQUE INDEX butler_tool_assignments_live
    ON butler_tool_assignments (butler, tool) WHERE revoked_at = '';

-- butler_tool_bundles is the dynamic half: a service and a class, not a
-- list of tools.
--
-- §6.3: "A dynamic bundle automatically includes future tools only when
-- they match the selected service/class and are not opt-in." That is
-- why it stores the QUESTION rather than the answer — a stored list of
-- tool names would be a snapshot, and the whole point is that it is not
-- one.
--
-- agent_class here is the class of the TOOLS the bundle covers, not the
-- Agent's. "All Member tools for Home Assistant" is a member-class
-- bundle, and an Owner Agent may hold it (§6.1's ordering).
CREATE TABLE butler_tool_bundles (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    butler      INTEGER NOT NULL REFERENCES butlers(seq),
    -- service is the capability whose tools this covers, e.g. 'photos'.
    -- Keystone's own tools are 'system'.
    service     TEXT NOT NULL,
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    granted_at  TEXT NOT NULL,
    granted_by  TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',

    CHECK (service <> ''),
    CHECK (granted_by <> '')
);

CREATE UNIQUE INDEX butler_tool_bundles_live
    ON butler_tool_bundles (butler, service, agent_class) WHERE revoked_at = '';
