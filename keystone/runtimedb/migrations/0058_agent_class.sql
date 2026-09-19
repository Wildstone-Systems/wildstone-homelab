-- +wildstone: foreign_keys=off
-- 0058: an Agent's class is not a Person's profile (ADR-W033 design
-- §5.2, §18.3; slice 9 of the agent-system epic).
--
-- Four tables describe an AGENT and spell its ceiling in the vocabulary
-- of a PERSON:
--
--   butlers.profile                     (0032)
--   butler_tool_controls.profile        (0022)
--   butler_tool_grants.profile          (0022)
--   butler_capability_requests.profile  (0022)
--
-- 'household', 'operator' and 'child' are words about people. §5.2 says
-- an Agent is an `owner` Agent or a `member` Agent, and that is a
-- property of the Agent — the structural ceiling it runs under — not of
-- anybody using it. One Agent serves several people at once (§5.3), so a
-- column that reads like a person's role is a column that will
-- eventually be read as one.
--
-- THREE AXES, NOT ONE. After this migration:
--
--   agent_class          owner | member     what an AGENT may reach
--   butler_members.role  owner | member     what a PERSON is
--   principals.role      admin | operator…  what a MACHINE credential is
--
-- The third is legacy (epic #201 decision 6) and is deliberately left
-- alone; it is named here only so nobody reads `operator` in two places
-- and assumes one meaning.
--
-- 'child' IS NOT AN AGENT CLASS, and never was. It reached this column
-- because butlers.profile borrowed butler_members' vocabulary wholesale.
-- A child is a constrained Member PERSON (§2.3); it says nothing about
-- what an Agent may do. See the resolution below, which is the one part
-- of this migration that changes behaviour rather than spelling.
--
-- THE CLASS CAN NOW MOVE, AND THE SURFACE MOVES WITH IT. 0032 froze both
-- `surface` and `profile` with triggers, on ADR-W031 A3: the ceiling is
-- what the Butler was created against. §18.3 supersedes that — it
-- requires downgrading an Owner Agent to Member, and stripping its tool
-- grants when that happens — so the class must be mutable.
--
-- That is why the two columns move together rather than one being frozen
-- while the other is not. They carry the same fact: CreateButler has
-- always enforced operator ⟺ admin, so once 'child' is resolved away,
-- `agent_class` and `surface` are one bit written twice. Freezing one
-- and freeing the other would let them disagree, and a disagreement here
-- means the class on the record is not the ceiling the gateway enforces.
--
-- A3's actual worry was WIDENING — "moving it would hand a family Butler
-- an operator gateway". That worry is answered by §18.3's asymmetry
-- rather than by immutability: a downgrade narrows, which only ever
-- takes tools away, and an upgrade widens the gateway while auto-adding
-- no grants to it. So the trigger below no longer refuses the move; it
-- refuses the two columns moving APART, which is the failure that was
-- actually worth preventing.

-- ---------------------------------------------------------------- --
-- butlers
-- ---------------------------------------------------------------- --

CREATE TABLE butlers_new (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    -- name is the Butler's own, given by the person who made it
    -- (A11): Alfred, Robert. "Butler" is the role; this is the
    -- instance, and it is what a person says and what a routine is
    -- assigned to.
    name       TEXT NOT NULL UNIQUE,
    -- directive is what this Butler is for, in the owner's words. It
    -- shapes how the Butler talks and grants nothing (A9): tools,
    -- connections and memory scope are separate and explicit, or
    -- "you may read whatever you need" becomes an authority grant
    -- written in prose.
    directive  TEXT NOT NULL DEFAULT '',
    -- surface is what ENFORCES the ceiling: the family gateway does not
    -- register an operator tool, so there is nothing for a control or a
    -- grant to switch on (A3).
    surface    TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    -- agent_class is what NAMES that ceiling, in §5.2's words. The pair
    -- below is what keeps the name and the enforcement the same fact.
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    -- model is the assigned model. NOT NULL and a real reference: the
    -- whole design rests on this being answerable.
    model      INTEGER NOT NULL REFERENCES "models"(seq),
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    -- retired_at makes a Butler stop without deleting what it did. Its
    -- conversations, memory and traces are still evidence.
    retired_at TEXT NOT NULL DEFAULT '',
    retired_by TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (created_by <> ''),
    -- The paired CHECK. An Owner Agent lives on the admin surface and a
    -- Member Agent does not, in both directions, so neither column can
    -- be written without the other agreeing.
    CHECK ((agent_class = 'owner') = (surface = 'admin'))
);

-- operator -> owner, household -> member.
--
-- 'child' also becomes 'member', because the row has to land in a closed
-- two-value vocabulary and 'member' is the honest reading: a child's
-- constraint belongs to the Person, and an Agent made under that profile
-- was never an Owner Agent.
--
-- But it lands DISABLED, and that is deliberate. A child-profile Butler
-- can dispatch nothing today: keystone/api/handlers_butlers.go has no
-- arm for it and returns "runs on the child profile, which has no tool
-- surface". Mapping it to 'member' and leaving it enabled would hand it
-- the entire household tool set — a Butler that could do nothing
-- yesterday doing everything a Member Agent can today, as a side effect
-- of a migration that was supposed to be about spelling. An Owner turns
-- it back on, having looked at it.
INSERT INTO butlers_new (seq, name, directive, surface, agent_class, model, enabled,
                         created_at, created_by, updated_at, updated_by,
                         retired_at, retired_by)
SELECT seq, name, directive, surface,
       CASE profile
           WHEN 'operator' THEN 'owner'
           ELSE 'member'
       END,
       model,
       CASE WHEN profile = 'child' THEN 0 ELSE enabled END,
       created_at, created_by, updated_at, updated_by, retired_at, retired_by
  FROM butlers;

-- What was resolved, kept as evidence rather than inferred later.
--
-- An Owner looking at a disabled Butler deserves to find out why from
-- the database rather than from a migration file, and "it was a child
-- profile and a child profile is not an Agent class" is not a fact any
-- surviving column still carries.
CREATE TABLE agent_class_resolutions (
    butler       INTEGER NOT NULL REFERENCES butlers(seq),
    former_profile TEXT NOT NULL,
    agent_class  TEXT NOT NULL,
    disabled     INTEGER NOT NULL,
    note         TEXT NOT NULL,
    resolved_at  TEXT NOT NULL
);

INSERT INTO agent_class_resolutions (butler, former_profile, agent_class, disabled, note, resolved_at)
SELECT seq, 'child', 'member', 1,
       'a child profile is a constraint on a Person, not an Agent class (design §2.3, §5.2). '
       || 'This Agent had no tool surface before the migration, so it was resolved to a Member '
       || 'Agent and left disabled rather than silently gaining one.',
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
  FROM butlers WHERE profile = 'child';

DROP TRIGGER butlers_profile_immutable;
DROP TRIGGER butlers_surface_immutable;
DROP TRIGGER butlers_model_usable_insert;
DROP TRIGGER butlers_model_usable_update;

-- Two triggers on OTHER tables read `butlers`, and SQLite resolves a
-- trigger body when it fires rather than when it is created — so
-- dropping the table underneath them leaves triggers that abort with
-- "no such table" the next time somebody adds a Butler address. They
-- are dropped here and recreated verbatim below.
DROP TRIGGER butler_addresses_live_butler;
DROP TRIGGER butler_connection_scope_audience;

DROP INDEX butlers_live;
DROP TABLE butlers;
ALTER TABLE butlers_new RENAME TO butlers;

CREATE INDEX butlers_live ON butlers (enabled, retired_at, surface);

-- The pair moves together or not at all.
--
-- The CHECK above already refuses a row where they disagree. This says
-- the same thing at the moment of the UPDATE so the error names what
-- the caller actually did wrong — "you moved one of them" is a very
-- different message to debug than a constraint failure on a row.
CREATE TRIGGER butlers_class_moves_with_surface
BEFORE UPDATE OF agent_class, surface ON butlers
WHEN (NEW.agent_class <> OLD.agent_class) <> (NEW.surface <> OLD.surface)
BEGIN
    SELECT RAISE(ABORT, 'an agent class and its surface move together or not at all: the class names the ceiling and the surface enforces it (ADR-W033 design §5.2)');
END;

-- Unchanged from 0037, recreated because the table was rebuilt.
CREATE TRIGGER butlers_model_usable_insert
BEFORE INSERT ON butlers
WHEN (SELECT enabled FROM models WHERE seq = NEW.model) = 0
BEGIN
    SELECT RAISE(ABORT, 'that model is disabled, so a butler assigned to it would have nowhere to run (ADR-W031 A6)');
END;

CREATE TRIGGER butlers_model_usable_update
BEFORE UPDATE OF model ON butlers
WHEN (SELECT enabled FROM models WHERE seq = NEW.model) = 0
BEGIN
    SELECT RAISE(ABORT, 'that model is disabled, so a butler assigned to it would have nowhere to run (ADR-W031 A6)');
END;

-- The two borrowed triggers, back verbatim. Unchanged from 0034 and
-- 0052 — they read `retired_at` and `surface`, neither of which this
-- migration touches, so restating them is bookkeeping rather than a
-- decision.
CREATE TRIGGER butler_addresses_live_butler
BEFORE INSERT ON butler_addresses
WHEN (SELECT retired_at FROM butlers WHERE seq = NEW.butler) <> ''
BEGIN
    SELECT RAISE(ABORT, 'that butler has been retired, so it cannot be given an address (ADR-W031 A7)');
END;

CREATE TRIGGER butler_connection_scope_audience
BEFORE INSERT ON butler_connection_scope
WHEN (SELECT a.kind
        FROM integration_connections c
        JOIN audiences a ON a.audience_id = c.audience
       WHERE c.seq = NEW.connection) <> 'household'
 AND (SELECT surface FROM butlers WHERE seq = NEW.butler) = 'family'
BEGIN
    SELECT RAISE(ABORT, 'that connection is not shared with the whole household, so a household-surface butler cannot be given it (ADR-W031 A4)');
END;

-- ---------------------------------------------------------------- --
-- butler_tool_controls
-- ---------------------------------------------------------------- --

CREATE TABLE butler_tool_controls_new (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    tool       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('enabled', 'disabled')),
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    CHECK (tool <> ''),
    CHECK (updated_by <> '')
);

-- A control is the administrator's switch for a whole class, and stays
-- one. Two rows can collapse onto the same key here — a 'household' and
-- a 'child' control for the same tool both become ('member', tool) — and
-- when they disagree the DISABLED one wins. An administrator who turned
-- something off for one of the two profiles meant it to be off; keeping
-- the 'enabled' row would turn a switch back on during a migration
-- nobody was watching.
--
-- Done as two guarded inserts rather than one GROUP BY. A grouped query
-- would need bare `updated_by` and `reason` alongside two aggregates,
-- and SQLite only promises which row those come from when there is
-- exactly one MIN or MAX — so the winning state could have arrived with
-- the losing row's author and explanation attached.
CREATE UNIQUE INDEX butler_tool_controls_key_new
    ON butler_tool_controls_new (agent_class, tool);

INSERT OR IGNORE INTO butler_tool_controls_new
       (agent_class, tool, state, updated_at, updated_by, reason)
SELECT CASE profile WHEN 'operator' THEN 'owner' ELSE 'member' END,
       tool, state, updated_at, updated_by, reason
  FROM butler_tool_controls
 WHERE state = 'disabled'
 ORDER BY updated_at DESC, seq DESC;

INSERT OR IGNORE INTO butler_tool_controls_new
       (agent_class, tool, state, updated_at, updated_by, reason)
SELECT CASE profile WHEN 'operator' THEN 'owner' ELSE 'member' END,
       tool, state, updated_at, updated_by, reason
  FROM butler_tool_controls
 WHERE state <> 'disabled'
 ORDER BY updated_at DESC, seq DESC;

DROP INDEX butler_tool_controls_key;
DROP INDEX butler_tool_controls_key_new;
DROP TABLE butler_tool_controls;
ALTER TABLE butler_tool_controls_new RENAME TO butler_tool_controls;

CREATE UNIQUE INDEX butler_tool_controls_key ON butler_tool_controls (agent_class, tool);

-- ---------------------------------------------------------------- --
-- butler_tool_grants
-- ---------------------------------------------------------------- --

CREATE TABLE butler_tool_grants_new (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    scope      TEXT NOT NULL CHECK (scope IN ('task', 'routine')),
    -- scope_id is the task id ("bt-<n>") or the routine name. Text so
    -- one column serves both.
    scope_id   TEXT NOT NULL,
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    -- butler is WHOSE grant this is.
    --
    -- Without it a grant belongs to a class, so every Agent of that
    -- class holds it, and §18.3's "downgrading an Owner Agent strips ITS
    -- tool grants" cannot be said: stripping would take the grant from
    -- every other Owner Agent too.
    --
    -- NULL is the pre-0058 shape, and is kept rather than backfilled.
    -- A grant issued before this migration was genuinely class-level —
    -- guessing an Agent for it would invent an attribution the household
    -- never made. A deployment with no Butlers at all still resolves
    -- through this path, which is why it stays supported rather than
    -- being migrated away.
    butler     INTEGER REFERENCES butlers(seq),
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

INSERT INTO butler_tool_grants_new (seq, scope, scope_id, agent_class, butler, tool,
                                    granted_at, granted_by, expires_at,
                                    revoked_at, revoked_by, reason, cleanup_plan)
SELECT seq, scope, scope_id,
       CASE profile WHEN 'operator' THEN 'owner' ELSE 'member' END,
       NULL,
       tool, granted_at, granted_by, expires_at,
       revoked_at, revoked_by, reason, cleanup_plan
  FROM butler_tool_grants;

DROP INDEX butler_tool_grants_live;
DROP TABLE butler_tool_grants;
ALTER TABLE butler_tool_grants_new RENAME TO butler_tool_grants;

CREATE INDEX butler_tool_grants_live ON butler_tool_grants (scope, scope_id, tool);

-- ---------------------------------------------------------------- --
-- butler_tasks
-- ---------------------------------------------------------------- --
--
-- A FIFTH TABLE. #210 lists four, and butler_tasks.profile is the same
-- axis under the same name: 0017 calls it "the authority the task was
-- admitted under", and butlerlive writes the Butler's own class into it
-- at admission. Renaming four of five would leave the old vocabulary
-- alive in the one nobody counted — the drift arriving inside the change
-- meant to end it, which is exactly what slice 7 found when its four
-- audience tables turned out to be six.
--
-- A cheap rename rather than a rebuild, because this column has no CHECK
-- and never had one. It is not left without one by oversight: the column
-- is HISTORICAL EVIDENCE of what a task ran under, and a closed
-- vocabulary on it would mean a future class change could no longer
-- record what yesterday's tasks were admitted as. Nothing resolves
-- against it — it is written once and displayed — so the constraint
-- would buy nothing the four tables above do not already guarantee.
ALTER TABLE butler_tasks RENAME COLUMN profile TO agent_class;

UPDATE butler_tasks
   SET agent_class = CASE agent_class WHEN 'operator' THEN 'owner' ELSE 'member' END;

-- ---------------------------------------------------------------- --
-- butler_capability_requests
-- ---------------------------------------------------------------- --

CREATE TABLE butler_capability_requests_new (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL CHECK (kind IN ('supported-tool', 'development')),
    -- requested_by is the MEMBER the asking session was acting for. The
    -- Butler asks on someone's behalf; it does not have wishes of its
    -- own, and an unattributed request is one nobody can weigh.
    requested_by TEXT NOT NULL,
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
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

INSERT INTO butler_capability_requests_new (seq, kind, requested_by, agent_class, tool,
                                            description, state, created_at,
                                            decided_at, decided_by, decision_note)
SELECT seq, kind, requested_by,
       CASE profile WHEN 'operator' THEN 'owner' ELSE 'member' END,
       tool, description, state, created_at, decided_at, decided_by, decision_note
  FROM butler_capability_requests;

DROP INDEX butler_capability_requests_state;
DROP TABLE butler_capability_requests;
ALTER TABLE butler_capability_requests_new RENAME TO butler_capability_requests;

CREATE INDEX butler_capability_requests_state ON butler_capability_requests (state, seq);
