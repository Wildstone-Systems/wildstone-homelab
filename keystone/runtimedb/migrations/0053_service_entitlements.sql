-- 0053: which people may reach which service (ADR-W033 design §4,
-- §17.3, §18.2; work slice 8 of the agent-system epic).
--
-- There is no entitlement table today and no entitlement code. Access to
-- the packages in `packages/` is whatever the LDAP groups happen to say,
-- managed by hand in LLDAP's own UI. §4 makes Wildstone the source of
-- truth and requires it to project effective membership outward; this is
-- the half that records the truth. The projection follows.
--
-- IT REFERENCES SLICE 6'S GROUPS AND DEFINES NO OTHERS. §4 is explicit
-- that entitlements, memory audiences and secret grants all name the
-- same `Adults` — and equally explicit that naming the same group is not
-- granting the same thing:
--
--     One namespace is not one permission. Membership of `Adults` is an
--     *identity primitive*; what it authorizes is decided by each
--     resource separately.
--
-- So this table is where `Adults` means "may open the password manager",
-- and it says nothing whatever about what `Adults` may remember or which
-- secrets they hold. A row here grants no memory audience and no secret
-- (§11, §12), which is the property TestEntitlementDoesNotGrantMemoryOr-
-- Secrets exists to keep true.

-- An entitlement: a subject, a service, and which way it points.
--
-- KEYED ON THE CAPABILITY, NOT THE PACKAGE. §17.3 says reinstalling the
-- same *compatible* capability reactivates dormant bindings, and the
-- capability is what compatibility is about: a household that moves
-- photos from Immich to another profile has not changed who may see the
-- photos. Keying on `photos-immich` would quietly drop every assignment
-- on a profile swap, which is the failure §17.3 names in its own words.
--
-- RESTRICTION IS A ROW, NOT AN ABSENCE. §4 requires that "explicit
-- restriction overrides inherited allow", and a restriction expressed by
-- deleting an allow cannot do that — the group's allow is still there,
-- and deleting it would remove the whole group's access rather than one
-- person's. A restriction has to outrank an allow it does not erase, so
-- it is written down.
CREATE TABLE service_entitlements (
    seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The capability, e.g. 'photos'. Not a foreign key: a service may be
    -- uninstalled while its entitlements go dormant (below), so the row
    -- outlives anything that could be referenced.
    service TEXT NOT NULL,

    -- Who. A group_id from slice 6's namespace, or a person_id.
    --
    -- Both are opaque ids rather than names, for the reason slice 6
    -- gives: a household that renames `Adults` must not thereby
    -- re-point every rule that named it, and it does not, because no
    -- rule ever stored the word.
    subject_kind TEXT NOT NULL CHECK (subject_kind IN ('group', 'person')),
    subject      TEXT NOT NULL,

    -- Which way it points. 'restrict' is not "no allow": it is an
    -- explicit refusal that outranks an inherited allow.
    effect TEXT NOT NULL CHECK (effect IN ('allow', 'restrict')),

    -- Set when the service was removed (§17.3, §17.4). A dormant row
    -- grants nothing and is not deleted, so reinstalling the capability
    -- restores what the household had decided rather than asking them
    -- to decide it again.
    dormant_at TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    -- Who decided, by handle, for the audit trail. Not a foreign key:
    -- the actor may be the installer or the host CLI rather than a
    -- Person, exactly as admitted_by already is.
    created_by TEXT NOT NULL DEFAULT '',

    -- One row per subject per service. A subject cannot be both allowed
    -- and restricted from the same service: that is a contradiction to
    -- resolve when it is written, not a precedence puzzle to resolve on
    -- every read.
    UNIQUE (service, subject_kind, subject),
    CHECK (service <> ''),
    CHECK (subject <> '')
);

-- Resolution reads by service (every subject entitled to this one) and
-- by subject (every service this person reaches). Both are hot: the
-- first on each authorization evaluation, the second on the Homepage.
CREATE INDEX service_entitlements_by_service ON service_entitlements (service, dormant_at);
CREATE INDEX service_entitlements_by_subject ON service_entitlements (subject_kind, subject);

-- ---------------------------------------------------------------------
-- The default household group gets its members.
--
-- §18.2: "A new Member inherits the default access group." From here on
-- that happens at admission. This backfill is about the households that
-- already exist.
--
-- Slice 6 seeded `Family`, `Adults` and `Kids` EMPTY and defended that
-- deliberately — it refused to put anybody in `Kids` by reading the
-- child preset, because inferring a distinction nobody stated is how two
-- group systems start to drift. This is not that. `Family` is the group
-- every household member is in by definition; putting every active
-- Person in it infers no distinction and states nothing the household
-- did not already mean.
--
-- And the alternative is worse than untidy. `Family` is empty today
-- because nothing consumed it; entitlements are the first consumer, and
-- they default to deny. An upgrade that left it empty would mean every
-- existing household's members belong to no group, hold no inherited
-- entitlement, and lose access to everything the moment projection
-- lands — a silent lockout produced by a migration that looked like it
-- only added a table.
--
-- ACTIVE PEOPLE ONLY, the same standing rule slice 6's resolver applies:
-- admitted, not disabled, not offboarded. Someone who has left the
-- household does not rejoin it by way of a backfill.
INSERT INTO person_group_members (group_id, person_id, added_at, added_by)
SELECT g.group_id, b.person_id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'migration:0053'
  FROM person_groups g
  -- Every active Person against the one `family` group: deliberately a
  -- cross join, filtered below, because there is nothing to join ON —
  -- the whole point is that membership did not exist yet.
 CROSS JOIN butler_members b
 WHERE g.seed_key = 'family'
   AND b.admitted = 1
   AND b.disabled_at = ''
   AND b.offboarded_at = ''
ON CONFLICT (group_id, person_id) DO NOTHING;
