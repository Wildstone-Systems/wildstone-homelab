-- 0051: the household's one principal namespace — groups of People
-- (ADR-W033 design §4, §18.1; work slice 6 of the agent-system epic).
--
-- Groups exist today only in LDAP, which means Wildstone has no notion
-- of `Adults` or `Kids` at all. Three things are about to need one:
-- service entitlements (slice 8), memory audiences (slice 11) and
-- secret grants (slice 12). Left to themselves each would invent its
-- own, and §4 says exactly what that costs:
--
--     The alternative — a separate audience vocabulary beside the
--     entitlement one — produces two group systems that drift, and a
--     household that cannot say why a person is in one and not the
--     other.
--
-- So the namespace is built once, here, before the first consumer.
--
-- IT GRANTS NOTHING, and that is the property to defend. §4 again:
--
--     One namespace is not one permission. Membership of `Adults` is an
--     *identity primitive*; what it authorizes is decided by each
--     resource separately.
--
-- There is deliberately no grants table in this migration, and adding
-- one here would be the mistake: the moment membership carries an
-- entitlement of its own, "who is an adult" and "what adults may do"
-- stop being separable, and every later slice inherits a permission
-- model nobody designed.
--
-- KEYED ON person_id, not on the handle. A handle is a display name
-- that can change; slice 2 moved every other reference off it for that
-- reason, and a group whose membership survived a rename in eleven
-- tables but not this one would be a strange exception to find later.

-- The groups themselves.
--
-- group_id is opaque and permanent. The display name is not part of it,
-- and that separation is the whole point of the column: a household
-- that renames `Adults` to `Grown-ups` must not thereby re-point every
-- rule that referenced it, and it does not, because no rule ever stored
-- the word "Adults".
--
-- seed_key names the ROLE a seeded group plays, and is the one thing
-- code may look a group up by. It is not the display name and does not
-- follow it: rename `Adults` and its seed_key is still 'adults',
-- because what changed was the household's word for the group, not
-- which group it is. NULL for every group a household makes itself —
-- those are referenced by id alone, since nothing in Wildstone should
-- have an opinion about a group it did not define.
CREATE TABLE person_groups (
    group_id   TEXT PRIMARY KEY,
    -- NOCASE because `Adults` and `adults` are one group to a person
    -- typing them, and two to a UNIQUE index that says otherwise. The
    -- case-sensitivity of handles is already an open defect
    -- (ADR-W032 A4); this does not add a second one.
    name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
    seed_key   TEXT UNIQUE CHECK (seed_key IS NULL OR seed_key IN ('family', 'adults', 'kids')),
    created_at TEXT NOT NULL,
    CHECK (group_id <> ''),
    CHECK (name <> '')
);

-- Membership. A Person may be in any number of groups, and resolution
-- returns the set rather than picking a winner: §4 lists `Adults` and
-- `Media` as examples of the same namespace, and somebody is plainly
-- allowed to be in both.
--
-- ON DELETE CASCADE on the group, because a group that no longer exists
-- has no members; NOT on the person, because a Person is never deleted
-- — they are offboarded, which is a state and not a removal, and a
-- cascade there would quietly erase the record of what they had been in.
CREATE TABLE person_group_members (
    group_id  TEXT NOT NULL REFERENCES person_groups(group_id) ON DELETE CASCADE,
    person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    added_at  TEXT NOT NULL,
    -- The Person who put them here, by handle, for the audit trail. Not
    -- a foreign key: the actor may be the installer or the host CLI
    -- rather than a Person, exactly as admitted_by already is.
    added_by  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (group_id, person_id)
);

-- Membership is read far more often than it is written — every
-- authorization evaluation in three future slices — and always from one
-- side or the other.
CREATE INDEX person_group_members_by_person ON person_group_members(person_id);

-- The three seeds §4 names. `Family` is the default household group
-- ("New Members inherit a default household access group such as
-- `Family`"); `Adults` and `Kids` are the two it uses by name when it
-- explains what one namespace means.
--
-- Seeded empty. Nobody is put in `Adults` by this migration, and nobody
-- is put in `Kids` by having the child preset — the preset and the
-- group are separate mechanisms and neither creates the other (§18.1).
-- Inferring membership from the preset here would be the drift this
-- slice exists to prevent, arriving in the migration that builds the
-- thing meant to prevent it.
INSERT INTO person_groups (group_id, name, seed_key, created_at) VALUES
    ('grp_0000000000000001', 'Family', 'family', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ('grp_0000000000000002', 'Adults', 'adults', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ('grp_0000000000000003', 'Kids',   'kids',   strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
