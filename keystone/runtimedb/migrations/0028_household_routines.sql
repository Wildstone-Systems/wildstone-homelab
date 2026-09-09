-- 0028: household routine authoring and review (ADR-W030 A6/A8/A8.1,
-- work package P13).
--
-- Two kinds of routine exist in a house, and keeping them distinct is
-- the first thing this schema does. A NATIVE routine runs inside an
-- application — a Home Assistant automation — and Wildstone records that
-- it exists, by the application's own external id, so a person can see
-- the whole picture without Wildstone pretending to own it. A WILDSTONE
-- routine is a cross-app document Wildstone runs itself. Blurring them
-- produces a Console that offers to edit something it cannot, or two
-- systems both firing on one departure.
--
-- The second thing is review. A pure household routine is exactly the
-- kind of change an admitted adult should be able to approve — it
-- changes what the house does, not what the house IS — and doing so
-- must not require Console credentials. So a review token is issued
-- against ONE proposal, bound to one identity, one revision and one
-- expiry, and usable once.
--
-- Everything about that token is a narrowing. It is not a session, it
-- does not authenticate, and it cannot approve anything but the
-- proposal it names: the household review page sits behind Authelia,
-- which establishes who the person is, and this row establishes what
-- that person may do with it.

CREATE TABLE household_routines (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    -- ownership is the distinction above and it is immutable: a native
    -- routine cannot become a Wildstone one by editing a column,
    -- because that would silently move responsibility for running it.
    ownership   TEXT NOT NULL CHECK (ownership IN ('native', 'wildstone')),
    -- external_id is the application's own identifier for a native
    -- routine, and empty for a Wildstone one. It is how "this is the
    -- same automation you can see in Home Assistant" is expressible.
    external_id TEXT NOT NULL DEFAULT '',
    connection  TEXT NOT NULL DEFAULT '',
    author      TEXT NOT NULL,
    -- revision increments on every accepted edit. A review token names
    -- one, so approving a proposal that has since been edited is
    -- refused rather than approving something nobody read.
    revision    INTEGER NOT NULL DEFAULT 1,
    state       TEXT NOT NULL CHECK (state IN ('draft', 'trial', 'enabled', 'disabled')),
    -- document is the routine itself: the YAML a household wrote.
    document    TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    updated_by  TEXT NOT NULL,
    -- disabled_at is set the moment someone disables it, and the
    -- runner checks it before every step. "Immediate and exact" means
    -- exactly this routine stops, now, without waiting for a reload.
    disabled_at TEXT NOT NULL DEFAULT '',
    disabled_by TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (author <> ''),
    CHECK (document <> ''),
    -- A native routine has an application and an id; a Wildstone one has
    -- neither, and mixing them is the confusion this table exists to
    -- prevent.
    CHECK ((ownership = 'native' AND external_id <> '' AND connection <> '')
        OR (ownership = 'wildstone' AND external_id = '' AND connection = ''))
);

-- Edits are versioned: the old document stays, so "what did it do last
-- week" is answerable and a review token pinned to a revision means
-- something.
CREATE TABLE household_routine_revisions (
    routine    INTEGER NOT NULL REFERENCES household_routines(seq),
    revision   INTEGER NOT NULL,
    document   TEXT NOT NULL,
    author     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (routine, revision)
);

-- household_review_tokens is a single-use, proposal-bound approval
-- capability.
--
-- The hash is stored, never the token, exactly as chat claim codes work:
-- this table describes a token, it does not contain one.
CREATE TABLE household_review_tokens (
    hash       TEXT PRIMARY KEY,
    -- plan_id is the ONE proposal this token can approve.
    plan_id    TEXT NOT NULL,
    routine    INTEGER REFERENCES household_routines(seq),
    -- revision pins what was proposed. If the routine has been edited
    -- since, the token approves nothing.
    revision   INTEGER NOT NULL DEFAULT 0,
    -- member is who it was issued to. A token that works for whoever
    -- holds it is a password, and this is not one.
    member     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT NOT NULL DEFAULT '',
    used_by    TEXT NOT NULL DEFAULT '',

    CHECK (plan_id <> ''),
    CHECK (member <> ''),
    CHECK (expires_at <> '')
);

CREATE INDEX household_review_tokens_plan ON household_review_tokens (plan_id);

-- Ownership never changes. A native routine that became a Wildstone one
-- would move responsibility for running it without anyone deciding to.
CREATE TRIGGER household_routines_ownership_immutable
BEFORE UPDATE OF ownership ON household_routines
WHEN NEW.ownership <> OLD.ownership
BEGIN
    SELECT RAISE(ABORT, 'a routine cannot change between native and Wildstone ownership (ADR-W030 A6)');
END;

-- A revision only moves forward, so a token pinned to one cannot be
-- made valid again by rewinding the routine.
CREATE TRIGGER household_routines_revision_monotonic
BEFORE UPDATE OF revision ON household_routines
WHEN NEW.revision < OLD.revision
BEGIN
    SELECT RAISE(ABORT, 'a routine revision cannot move backwards (ADR-W030 A8)');
END;

-- A used token stays used. Re-arming one would make it a password.
CREATE TRIGGER household_review_tokens_single_use
BEFORE UPDATE OF used_at ON household_review_tokens
WHEN OLD.used_at <> '' AND NEW.used_at <> OLD.used_at
BEGIN
    SELECT RAISE(ABORT, 'a household review token is single-use (ADR-W030 A8.1)');
END;
