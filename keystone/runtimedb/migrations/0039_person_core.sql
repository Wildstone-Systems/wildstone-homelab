-- +wildstone: foreign_keys=off
-- 0039: the Person core — a stable person_id, role and profile preset
-- split apart, and the Last Owner invariant (ADR-W033 design §2.2,
-- §2.3, §2.5, §18.1; work slice 1 of the agent-system epic).
--
-- 0021 gave butler_members one column doing two jobs:
--
--     profile TEXT NOT NULL CHECK (profile IN ('household','operator','child'))
--
-- The design separates those jobs, and the reason is not tidiness. It
-- is that "demote an Owner" and "restrict a child" currently look like
-- the same operation — one UPDATE of one column — when they are not
-- remotely the same act. One moves a person across the RBAC boundary
-- that decides who administers this deployment. The other narrows what
-- a Member may do inside it.
--
--   role    — 'owner' or 'member'. There are exactly two, and §2.3 says
--             so in those words. Owners administer the deployment,
--             Members consume it.
--   profile — a preset that constrains a Member. 'child' is the only
--             one so far, and it is deliberately NOT a third role: a
--             child is a Member with the child preset, so every check
--             that asks "is this person a Member" answers yes for them
--             without knowing the preset exists.
--
-- The legacy mapping follows from that split:
--
--   operator  -> role 'owner',  profile NULL
--   household -> role 'member', profile NULL
--   child     -> role 'member', profile 'child'
--
-- Note that 'operator' here is the HUMAN profile from 0021 and has
-- nothing to do with principals.role = 'operator', which is a machine
-- and API role on a different axis, or with a Butler's 'operator'
-- capability profile. Those keep their meanings and their columns.
--
-- person_id is the other half of the slice. Until now a person WAS the
-- string in butler_members.member: every reference to a human anywhere
-- in the schema is that free text, so a handle could never be renamed
-- without either breaking references or rewriting history. person_id is
-- the identity; the handle becomes a label on it. Repointing the other
-- tables onto person_id is the NEXT slice, deliberately not this one,
-- so this migration adds the identity and leaves every existing
-- reference exactly where it is.
--
-- Forward-only (ADR-W007 A4): a bad migration is fixed by a new one.

-- Why the rebuild, and why foreign keys are off for it.
--
-- profile has to go from NOT NULL with one CHECK to nullable with a
-- different one. SQLite cannot alter or drop a CHECK constraint in
-- place, and it cannot drop the column either, because profile is named
-- by two table-level CHECKs. So the table is recreated.
--
-- butler_identity_links and butler_conversations hold foreign keys into
-- butler_members(member), which makes DROP TABLE butler_members fail
-- while foreign keys are enforced. The earlier rebuilds here (0005,
-- 0008, 0012, 0026) answered that by rebuilding the children too, but
-- butler_conversations is itself referenced by
-- butler_conversation_sessions and butler_handoffs, so that approach
-- would rebuild five tables to change one — and would touch precisely
-- the human references the next slice is going to repoint.
--
-- Instead this runs under SQLite's documented procedure for a schema
-- change of this kind: foreign keys off for the rebuild, then
-- foreign_key_check before commit. The runner enforces that gate; see
-- rebuildDirective in runtimedb.go. The children's REFERENCES clauses
-- still name butler_members and still resolve, because the replacement
-- table takes the same name and keeps member unique.

CREATE TABLE butler_members_v39 (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,

    -- person_id is the identity. It is opaque and carries no meaning:
    -- not the handle, not the seq, not a directory login, nothing that
    -- could change or that anyone could be tempted to parse. A Person
    -- keeps it across a rename, a re-admission and a directory move,
    -- which is the entire point of having it.
    person_id    TEXT NOT NULL UNIQUE,

    -- member is now a display and login HANDLE, not the identity.
    --
    -- It stays strictly unique in this slice. §2.2 wants it unique
    -- among ACTIVE people, which would let a departed person's handle
    -- be reused — but "active" here is a predicate over three columns
    -- (admitted, disabled_at, offboarded_at), and a partial unique
    -- index over it would let a staged, not-yet-admitted row claim an
    -- active person's handle. That is a new lifecycle rule, not a
    -- constraint change, and inventing one to enable a reuse nobody has
    -- asked for yet is how a schema acquires behaviour no design
    -- document describes. Handle reuse waits for the slice that
    -- actually specifies it.
    member       TEXT NOT NULL UNIQUE,

    -- role is the RBAC axis. Two values, closed (§2.3).
    role         TEXT NOT NULL CHECK (role IN ('owner', 'member')),

    -- profile is the preset axis, and NULL is the ordinary case: most
    -- Members are just Members. The CHECK is closed at one value so a
    -- future preset is a deliberate migration rather than whatever
    -- string a caller happened to pass.
    profile      TEXT CHECK (profile IS NULL OR profile IN ('child')),

    admitted     INTEGER NOT NULL DEFAULT 0,
    -- generation increments on every admission, disable, unlink and
    -- offboarding. It is what makes revocation immediate.
    generation   INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    admitted_at  TEXT NOT NULL DEFAULT '',
    admitted_by  TEXT NOT NULL DEFAULT '',
    disabled_at  TEXT NOT NULL DEFAULT '',
    disabled_by  TEXT NOT NULL DEFAULT '',
    -- Offboarding is one audited administrator action that immediately
    -- denies access. Desired-state cleanup follows through Plans; failed
    -- cleanup never restores access (A2.1).
    offboarded_at TEXT NOT NULL DEFAULT '',
    offboarded_by TEXT NOT NULL DEFAULT '',
    -- guardian is required for a child record and meaningless otherwise.
    guardian     TEXT NOT NULL DEFAULT '',

    CHECK (member <> ''),
    CHECK (person_id <> ''),
    -- A child is never admitted. Carried over from 0021 unchanged: the
    -- schema refuses the row, so no code path, no migration and no
    -- console form can produce an admitted child before that surface
    -- has its own evidence (ADR-W030 Part C).
    CHECK (NOT (profile = 'child' AND admitted = 1)),
    CHECK (profile <> 'child' OR guardian <> ''),
    -- A child is a constrained MEMBER (§2.3). Without this the preset
    -- and the role could drift apart in storage, and "is a child an
    -- Owner?" would become a question the data could answer either way.
    CHECK (profile <> 'child' OR role = 'member')
);

INSERT INTO butler_members_v39 (seq, person_id, member, role, profile, admitted, generation,
    created_at, admitted_at, admitted_by, disabled_at, disabled_by,
    offboarded_at, offboarded_by, guardian)
SELECT
    seq,
    -- randomblob per row: opaque, distinct, and not derived from
    -- anything that can change.
    'psn_' || lower(hex(randomblob(8))),
    member,
    CASE profile WHEN 'operator' THEN 'owner' ELSE 'member' END,
    CASE profile WHEN 'child' THEN 'child' END,
    admitted, generation, created_at, admitted_at, admitted_by,
    disabled_at, disabled_by, offboarded_at, offboarded_by, guardian
FROM butler_members;

-- 0032's admission guard on butler_access reads butler_members in its
-- body. SQLite validates every trigger body when a table is dropped, so
-- it has to stand aside for the swap and be restored afterwards,
-- unchanged. It is a trigger on a DIFFERENT table, so dropping the
-- rebuilt table would not have taken it with it.
DROP TRIGGER butler_access_requires_admission;

DROP TABLE butler_members;
ALTER TABLE butler_members_v39 RENAME TO butler_members;

-- Restored verbatim from 0032.
CREATE TRIGGER butler_access_requires_admission
BEFORE INSERT ON butler_access
WHEN (SELECT COUNT(*) FROM butler_members
      WHERE member = NEW.member AND admitted = 1 AND offboarded_at = '') = 0
BEGIN
    SELECT RAISE(ABORT, 'that person is not an admitted butler member, so they cannot be given access to a butler (ADR-W030 A2.1)');
END;

CREATE INDEX butler_members_admitted ON butler_members (admitted, member);
-- Every Last-Owner check counts active Owners, so give that count an
-- index rather than a scan.
CREATE INDEX butler_members_active_owners
    ON butler_members (role, admitted, disabled_at, offboarded_at);

-- Triggers live on the table, so the rebuild dropped 0021's with it.
-- These two are restored verbatim in meaning.

-- The member generation only moves forward, for the same reason every
-- other epoch here does: rewinding it revives access that was
-- deliberately withdrawn.
CREATE TRIGGER butler_members_generation_monotonic
BEFORE UPDATE OF generation ON butler_members
WHEN NEW.generation < OLD.generation
BEGIN
    SELECT RAISE(ABORT, 'butler member generation cannot move backwards (ADR-W030 A2.1)');
END;

-- person_id never changes. "Stable" is the column's entire purpose, and
-- a purpose stated only in a comment is a convention, not an identity.
--
-- What a rewrite costs is concrete rather than theoretical: role history
-- in butler_role_changes is keyed on person_id, so changing it strands
-- the trail under an id nothing resolves to any more — the audit still
-- exists and no longer belongs to anybody. Every table slice 2 repoints
-- onto person_id would detach the same way.
--
-- Renaming someone is the supported operation and it moves the handle,
-- not the identity. An UPDATE that leaves person_id alone passes here
-- untouched.
CREATE TRIGGER butler_members_person_id_immutable
BEFORE UPDATE OF person_id ON butler_members
WHEN NEW.person_id <> OLD.person_id
BEGIN
    SELECT RAISE(ABORT, 'person_id is the stable identity and cannot be changed (design §2.2)');
END;

-- An offboarded member cannot be re-admitted by flipping a column. It
-- takes an explicit new admission, which clears the offboarding and
-- bumps the generation — so re-admission is visibly a new decision
-- rather than an undo.
CREATE TRIGGER butler_members_offboarded_stays_denied
BEFORE UPDATE OF admitted ON butler_members
WHEN NEW.admitted = 1 AND OLD.offboarded_at <> '' AND NEW.offboarded_at <> ''
BEGIN
    SELECT RAISE(ABORT, 'an offboarded member cannot be re-admitted without a new admission (ADR-W030 A2.1)');
END;

-- The Last Owner invariant (§2.5, §18.1).
--
-- "At least one active Owner must always exist." The Go write path
-- checks this too and gives the readable error; this is the backstop,
-- and it is the authoritative one. A caller written next year that
-- updates butler_members directly — a console form, a repair script, a
-- future slice that has not read §2.5 — gets refused by the database
-- rather than quietly leaving a household with nobody who can
-- administer it.
--
-- Active means all three: admitted, not disabled, not offboarded. The
-- trigger fires on the transition OUT of active-Owner, whichever column
-- causes it, which is why it is not three separate rules for demote,
-- disable and offboard. Any UPDATE that leaves this person no longer an
-- active Owner is the same event, including un-admitting them, and a
-- rule written per-operation would have missed whichever operation
-- someone added next.
CREATE TRIGGER butler_members_last_owner_stays
BEFORE UPDATE ON butler_members
WHEN OLD.role = 'owner'
 AND OLD.admitted = 1 AND OLD.disabled_at = '' AND OLD.offboarded_at = ''
 AND NOT (NEW.role = 'owner' AND NEW.admitted = 1
          AND NEW.disabled_at = '' AND NEW.offboarded_at = '')
 AND (SELECT COUNT(*) FROM butler_members
      WHERE role = 'owner' AND admitted = 1
        AND disabled_at = '' AND offboarded_at = ''
        AND seq <> OLD.seq) = 0
BEGIN
    SELECT RAISE(ABORT, 'the last active owner cannot be demoted, disabled, or offboarded while no other active owner exists (design §2.5)');
END;

-- Soft deletion is what the design specifies (§17.4), but a hard DELETE
-- would strip the last Owner just as completely, so it is refused on
-- the same terms.
CREATE TRIGGER butler_members_last_owner_not_deleted
BEFORE DELETE ON butler_members
WHEN OLD.role = 'owner'
 AND OLD.admitted = 1 AND OLD.disabled_at = '' AND OLD.offboarded_at = ''
 AND (SELECT COUNT(*) FROM butler_members
      WHERE role = 'owner' AND admitted = 1
        AND disabled_at = '' AND offboarded_at = ''
        AND seq <> OLD.seq) = 0
BEGIN
    SELECT RAISE(ABORT, 'the last active owner cannot be deleted while no other active owner exists (design §2.5)');
END;

-- Role changes are audited: who changed it, when, from what to what
-- (§18.1).
--
-- This is deliberately a narrow table and not a new audit subsystem.
-- The existing `audit` table answers a different question — it is one
-- row per HTTP request that passed authn, shaped as principal/method/
-- path/status — and a role change is not a request shape. Crossing the
-- Owner boundary is the single most consequential thing that happens to
-- a Person, so it gets a record that names the subject by person_id,
-- which survives the handle being renamed afterwards.
CREATE TABLE butler_role_changes (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,
    -- The subject, by identity and by the handle they had at the time.
    -- Both, because person_id is what still resolves later and the
    -- handle is what the person reading the audit will recognise.
    person_id  TEXT NOT NULL,
    member     TEXT NOT NULL,
    -- actor is the administrator who made the change.
    actor      TEXT NOT NULL,
    old_role   TEXT NOT NULL CHECK (old_role IN ('owner', 'member')),
    new_role   TEXT NOT NULL CHECK (new_role IN ('owner', 'member')),

    CHECK (person_id <> ''),
    CHECK (actor <> ''),
    -- A row that records no change is noise in an audit trail.
    CHECK (old_role <> new_role)
);

CREATE INDEX butler_role_changes_by_person ON butler_role_changes (person_id, seq);

-- Attribution recorded in history is never reinterpreted (A2.1). The
-- audit trail is append-only in the same sense the trace and the
-- handoffs are.
--
-- Append-only takes both triggers, for the same reason the Last Owner
-- invariant above refuses a hard DELETE as well as a demotion: erasing
-- the row and rewriting it put the trail in the same state, so a rule
-- that only catches the rewrite is not the rule it claims to be. An
-- audit nobody can edit but anyone can empty answers no question worth
-- asking.
CREATE TRIGGER butler_role_changes_immutable
BEFORE UPDATE ON butler_role_changes
BEGIN
    SELECT RAISE(ABORT, 'role change records are append-only (ADR-W030 A2.1)');
END;

CREATE TRIGGER butler_role_changes_not_deleted
BEFORE DELETE ON butler_role_changes
BEGIN
    SELECT RAISE(ABORT, 'role change records are append-only and cannot be deleted (ADR-W030 A2.1)');
END;
