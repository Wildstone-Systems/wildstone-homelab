-- 0032: Butlers are user-created specialists (ADR-W031 A1/A3/A8/A10,
-- work package B1).
--
-- There is not one Butler. There are as many as the household creates,
-- each with its own model, directive, tools, connections and memory. A
-- routine is assigned to one. The health Butler runs on a local model
-- and its material does not reach the nutrition Butler's cloud one.
--
-- The reason a Butler HAS a model rather than choosing one per call is
-- A2: anything that decides which model should handle a message must
-- read the message in order to decide, so the routing step is itself
-- the disclosure it was meant to prevent. An assignment has no such
-- step, and it can answer "which model saw this" by name.
--
-- Three things this schema refuses, because each is a way the guarantee
-- could erode without anyone breaking a rule:
--
--   A Butler cannot change surface. The surface is the structural
--   ceiling it was created against (A3), and moving it would hand a
--   family Butler an operator gateway.
--
--   A Butler cannot point at a disabled or deleted route. The model is
--   not an optional field to be filled in later — a Butler with
--   nowhere to run is a Butler whose next message goes somewhere
--   unpredictable.
--
--   A person who is not admitted cannot be granted access. Access to a
--   Butler is downstream of admission, never a way around it.

CREATE TABLE butlers (
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
    -- surface is the structural ceiling. A family Butler cannot be
    -- given an operator tool because the family gateway does not
    -- contain one — configuration selects from what the ceiling allows
    -- and can never add to it (A3).
    surface    TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    -- profile is the ceiling template within that surface, matching
    -- butler_members. 'child' is storable and, as there, not yet
    -- admissible.
    profile    TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
    -- route is the assigned model. NOT NULL and a real reference: the
    -- whole design rests on this being answerable.
    route      INTEGER NOT NULL REFERENCES model_routes(seq),
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
    CHECK (created_by <> '')
);

CREATE INDEX butlers_live ON butlers (enabled, retired_at, surface);

-- butler_access is the Console holder's answer to "who may talk to
-- Alfred" (A10).
--
-- It is a separate table rather than a column because it is the thing
-- that stops a person reaching a Butler AT ALL, rather than reaching it
-- and being refused tool by tool. A person with no row here does not
-- see the Butler.
CREATE TABLE butler_access (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    member     TEXT NOT NULL,
    granted_at TEXT NOT NULL,
    granted_by TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    revoked_by TEXT NOT NULL DEFAULT '',

    CHECK (member <> ''),
    CHECK (granted_by <> '')
);

CREATE UNIQUE INDEX butler_access_live ON butler_access (butler, member)
    WHERE revoked_at = '';
CREATE INDEX butler_access_member ON butler_access (member, revoked_at);

-- A Butler's surface is the ceiling it was created against. Changing it
-- would move that ceiling under everything already assigned to the
-- Butler — its tools, its routines, its conversations (ADR-W031 A3).
CREATE TRIGGER butlers_surface_immutable
BEFORE UPDATE OF surface ON butlers
WHEN NEW.surface <> OLD.surface
BEGIN
    SELECT RAISE(ABORT, 'a butler cannot change surface: the ceiling is what it was created against (ADR-W031 A3)');
END;

-- The same for the profile, for the same reason: it is the ceiling
-- template, not a preference.
CREATE TRIGGER butlers_profile_immutable
BEFORE UPDATE OF profile ON butlers
WHEN NEW.profile <> OLD.profile
BEGIN
    SELECT RAISE(ABORT, 'a butler cannot change profile: the ceiling is what it was created against (ADR-W031 A3)');
END;

-- A Butler must point at a route that is enabled and not unavailable.
--
-- The reference alone is not enough. A Butler assigned to a route
-- somebody has since disabled has nowhere to run, and the failure that
-- produces is exactly the one A6 says must be a WAIT rather than a
-- substitution — which requires the assignment to still be readable and
-- to still name the model that is missing. Refusing the assignment at
-- write time is how it stays that way.
CREATE TRIGGER butlers_route_usable_insert
BEFORE INSERT ON butlers
WHEN (SELECT enabled FROM model_routes WHERE seq = NEW.route) = 0
BEGIN
    SELECT RAISE(ABORT, 'that model route is disabled, so a butler assigned to it would have nowhere to run (ADR-W031 A6)');
END;

CREATE TRIGGER butlers_route_usable_update
BEFORE UPDATE OF route ON butlers
WHEN (SELECT enabled FROM model_routes WHERE seq = NEW.route) = 0
BEGIN
    SELECT RAISE(ABORT, 'that model route is disabled, so a butler assigned to it would have nowhere to run (ADR-W031 A6)');
END;

-- Access to a Butler is downstream of Butler admission, never a way
-- around it. Granting a person who was never admitted — or who was
-- offboarded — would make this table a second admission path with none
-- of the first one's checks (ADR-W030 A2.1).
CREATE TRIGGER butler_access_requires_admission
BEFORE INSERT ON butler_access
WHEN (SELECT COUNT(*) FROM butler_members
      WHERE member = NEW.member AND admitted = 1 AND offboarded_at = '') = 0
BEGIN
    SELECT RAISE(ABORT, 'that person is not an admitted butler member, so they cannot be given access to a butler (ADR-W030 A2.1)');
END;
