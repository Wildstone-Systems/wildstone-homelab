-- +wildstone: foreign_keys=off
-- 0043: repoint every provably-human reference at person_id (design
-- §2.2, §19 area 1; work slice 2b of the agent-system epic).
--
-- 0039 gave a Person a stable identity and deliberately left every
-- reference to one alone. This migration moves them. A handle is a
-- label from here on: renaming somebody moves the label and touches
-- nothing else, because nothing else stores it.
--
-- WHICH columns move is not a judgement made here. Slice 2a committed
-- the classification to keystone/runtimedb/personref, one verdict per
-- column with the evidence attached, and the only columns this
-- migration touches are the ones it marks `human` or `scoped`. The
-- distinction that matters:
--
--   human   the value is a person and nothing else -> person_id,
--           NOT NULL.
--   scoped  a person in one ownership mode, the literal `household`
--           in another -> person_id, NULL meaning the household.
--   actor   whoever acted: a person on a Console call, a machine
--           principal (`root-admin`, `op`, a chat adapter) otherwise.
--           NOT touched. An audit row must stay readable after the
--           person it names is purged, and an id that resolves to
--           nothing is not evidence. These wait for the Person/
--           principal split in slice 9.
--
-- Two `human` columns are classified but not repointed, and both say so
-- in their own verdict: butler_members.member IS the handle the
-- identity belongs to, and butler_role_changes.member is the handle
-- recorded beside a person_id that row already carries.
--
-- Forward-only (ADR-W007 A4). There is no down-migration; recovery from
-- a bad run is restore-from-backup, so take one first.

-- ---------------------------------------------------------------------
-- The gate.
--
-- Every value in an in-scope column must name a Person on record. A
-- value that does not is refused here, with the table, the row and the
-- value in the message, and the whole migration rolls back: the
-- database stays at 0039 and nothing is half-moved.
--
-- Refusing is the only honest answer. The alternatives are worse in
-- ways that are permanent: inventing a Person for an unknown handle
-- creates somebody who never existed, and migrating the row with a null
-- owner silently discards who it belonged to. Both look like data
-- afterwards rather than like a mistake.
--
-- Matching is EXACT. `Sam` does not resolve to `sam` — 2a decided
-- handles are unique case-insensitively and that collisions are
-- reported and never merged, so a case variant lands here as
-- unresolvable, which is the intended outcome and not an oversight.
-- Because butler_members.member is UNIQUE, an exact match is never
-- ambiguous.
--
-- `keystone person-refs` reports EVERY offending row across every
-- column, and exits non-zero while any remain. This gate names the
-- first one it meets, because RAISE(ABORT) ends the statement: it is
-- here to make the migration impossible to run on a database that has
-- not been cleared, not to be the report. Run the report first.
CREATE TEMP TABLE person_refs_unresolved (problem TEXT NOT NULL);

CREATE TEMP TRIGGER person_refs_refuse
BEFORE INSERT ON person_refs_unresolved
BEGIN
    SELECT RAISE(ABORT, 'migration 0043: ' || NEW.problem ||
        ' -- run `keystone person-refs` for the full list and resolve it first');
END;

-- One probe per in-scope column. `human` columns must resolve outright.
INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_access.member at seq ' || seq || ' is ' || quote(member) ||
       ', which is not a handle on record'
FROM butler_access WHERE member NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_identity_links.member at seq ' || seq || ' is ' || quote(member) ||
       ', which is not a handle on record'
FROM butler_identity_links WHERE member NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_conversations.member at seq ' || seq || ' is ' || quote(member) ||
       ', which is not a handle on record'
FROM butler_conversations WHERE member NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_handoffs.member at seq ' || seq || ' is ' || quote(member) ||
       ', which is not a handle on record'
FROM butler_handoffs WHERE member NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_memory.author at seq ' || seq || ' is ' || quote(author) ||
       ', which is not a handle on record'
FROM butler_memory WHERE author NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_tasks.author at seq ' || seq || ' is ' || quote(author) ||
       ', which is not a handle on record'
FROM butler_tasks WHERE author NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_tasks.owner at seq ' || seq || ' is ' || quote(owner) ||
       ', which is not a handle on record'
FROM butler_tasks WHERE owner NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'household_review_tokens.member for plan ' || quote(plan_id) || ' is ' || quote(member) ||
       ', which is not a handle on record'
FROM household_review_tokens WHERE member NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'offboarding_records.member at seq ' || seq || ' is ' || quote(member) ||
       ', which is not a handle on record'
FROM offboarding_records WHERE member NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'notices.recipient at seq ' || seq || ' is ' || quote(recipient) ||
       ', which is not a handle on record'
FROM notices WHERE recipient NOT IN (SELECT member FROM butler_members) LIMIT 1;

-- butler_members.guardian is the one `human` column that is legitimately
-- absent most of the time: 0021 gave it NOT NULL DEFAULT '' and the
-- schema requires it only for a child record. Empty stays empty — as
-- NULL, now that the column holds an identity rather than a string —
-- and anything non-empty must resolve like any other person reference.
INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_members.guardian for ' || quote(member) || ' is ' || quote(guardian) ||
       ', which is not a handle on record'
FROM butler_members
WHERE guardian <> '' AND guardian NOT IN (SELECT member FROM butler_members) LIMIT 1;

-- The two `scoped` columns. `household` is the documented non-person
-- owner and is not a missing person; every other value must resolve.
-- The scope is recognised as a value in a column that takes one, never
-- as a name anywhere — a Person who manages to be called `household`
-- sitting in butler_tasks.author is still refused by the probe above.
INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_memory.owner at seq ' || seq || ' is ' || quote(owner) ||
       ', which is neither a handle on record nor the household'
FROM butler_memory
WHERE owner <> 'household' AND owner NOT IN (SELECT member FROM butler_members) LIMIT 1;

INSERT INTO person_refs_unresolved (problem)
SELECT 'butler_index.owner at seq ' || seq || ' is ' || quote(owner) ||
       ', which is neither a handle on record nor the household'
FROM butler_index
WHERE owner <> 'household' AND owner NOT IN (SELECT member FROM butler_members) LIMIT 1;

-- ---------------------------------------------------------------------
-- The rebuilds.
--
-- Every one of these tables states CHECK (<handle> <> ''), indexes the
-- handle, or both, and SQLite can neither drop a column an index or a
-- CHECK names nor alter a CHECK in place. So each table is recreated,
-- which is what the foreign_keys=off directive on line 1 is for: see
-- rebuildDirective in runtimedb.go for why that is safe and what
-- foreign_key_check then guarantees before this commits.
--
-- seq is carried across verbatim everywhere. Other tables reference
-- these rows by seq, the FTS index below is keyed on it, and an audit
-- trail whose row numbers move is a different audit trail.

-- ---- butler_access ----
--
-- Rebuilt FIRST, ahead of butler_members, and its admission trigger is
-- not recreated until afterwards. The trigger reads butler_members in
-- its body; SQLite validates every trigger body when a table is
-- dropped, so leaving it in place would make the butler_members rebuild
-- below fail. 0039 stood it aside with an explicit DROP TRIGGER for the
-- same reason — here the rebuild takes it down anyway, so the only care
-- needed is not to put it back too early.
CREATE TABLE butler_access_v43 (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    -- person_id replaces member. The grant follows the person, so a
    -- rename no longer detaches somebody from the Butlers they were
    -- admitted to.
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    granted_at TEXT NOT NULL,
    -- granted_by and revoked_by are actor columns and keep their
    -- recorded text: a Console caller's handle, or a machine
    -- principal's name. See the header.
    granted_by TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    revoked_by TEXT NOT NULL DEFAULT '',

    CHECK (person_id <> ''),
    CHECK (granted_by <> '')
);

INSERT INTO butler_access_v43 (seq, butler, person_id, granted_at, granted_by, revoked_at, revoked_by)
SELECT a.seq, a.butler, m.person_id, a.granted_at, a.granted_by, a.revoked_at, a.revoked_by
FROM butler_access a JOIN butler_members m ON m.member = a.member;

DROP TABLE butler_access;
ALTER TABLE butler_access_v43 RENAME TO butler_access;

CREATE UNIQUE INDEX butler_access_live ON butler_access (butler, person_id)
    WHERE revoked_at = '';
CREATE INDEX butler_access_person ON butler_access (person_id, revoked_at);

-- ---- butler_members ----
--
-- guardian is the only column moving here. Everything else is 0039's
-- table, carried across unchanged, because SQLite offers no way to
-- change one column's type and constraints without restating the rest.
--
-- guardian becomes nullable, which is a widening rather than a loss.
-- 0021 made it NOT NULL DEFAULT '' and the schema requires it only for
-- a child, so '' has always meant "none" — and '' is not an identity.
-- NULL says the same thing in a column that holds ids, and it is what
-- the CHECK below can test honestly.
CREATE TABLE butler_members_v43 (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id    TEXT NOT NULL UNIQUE,
    member       TEXT NOT NULL UNIQUE,
    role         TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    profile      TEXT CHECK (profile IS NULL OR profile IN ('child')),
    admitted     INTEGER NOT NULL DEFAULT 0,
    generation   INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    admitted_at  TEXT NOT NULL DEFAULT '',
    admitted_by  TEXT NOT NULL DEFAULT '',
    disabled_at  TEXT NOT NULL DEFAULT '',
    disabled_by  TEXT NOT NULL DEFAULT '',
    offboarded_at TEXT NOT NULL DEFAULT '',
    offboarded_by TEXT NOT NULL DEFAULT '',
    -- The responsible adult, by identity. Self-referencing: a guardian
    -- is a Person in this same table.
    guardian_person_id TEXT REFERENCES butler_members(person_id),

    CHECK (member <> ''),
    CHECK (person_id <> ''),
    CHECK (NOT (profile = 'child' AND admitted = 1)),
    CHECK (profile <> 'child' OR guardian_person_id IS NOT NULL),
    CHECK (profile <> 'child' OR role = 'member')
);

INSERT INTO butler_members_v43 (seq, person_id, member, role, profile, admitted, generation,
    created_at, admitted_at, admitted_by, disabled_at, disabled_by,
    offboarded_at, offboarded_by, guardian_person_id)
SELECT
    b.seq, b.person_id, b.member, b.role, b.profile, b.admitted, b.generation,
    b.created_at, b.admitted_at, b.admitted_by, b.disabled_at, b.disabled_by,
    b.offboarded_at, b.offboarded_by,
    (SELECT g.person_id FROM butler_members g WHERE g.member = b.guardian)
FROM butler_members b;

DROP TABLE butler_members;
ALTER TABLE butler_members_v43 RENAME TO butler_members;

CREATE INDEX butler_members_admitted ON butler_members (admitted, member);
CREATE INDEX butler_members_active_owners
    ON butler_members (role, admitted, disabled_at, offboarded_at);

-- Restored from 0039 in meaning, rewritten in person_id terms. Admission
-- is a fact about a Person, so the grant it guards names one.
CREATE TRIGGER butler_access_requires_admission
BEFORE INSERT ON butler_access
WHEN (SELECT COUNT(*) FROM butler_members
      WHERE person_id = NEW.person_id AND admitted = 1 AND offboarded_at = '') = 0
BEGIN
    SELECT RAISE(ABORT, 'that person is not an admitted butler member, so they cannot be given access to a butler (ADR-W030 A2.1)');
END;

-- The rest are 0039's, verbatim: the rebuild dropped them with the
-- table and none of them reads a column this migration moved.
CREATE TRIGGER butler_members_generation_monotonic
BEFORE UPDATE OF generation ON butler_members
WHEN NEW.generation < OLD.generation
BEGIN
    SELECT RAISE(ABORT, 'butler member generation cannot move backwards (ADR-W030 A2.1)');
END;

CREATE TRIGGER butler_members_person_id_immutable
BEFORE UPDATE OF person_id ON butler_members
WHEN NEW.person_id <> OLD.person_id
BEGIN
    SELECT RAISE(ABORT, 'person_id is the stable identity and cannot be changed (design §2.2)');
END;

CREATE TRIGGER butler_members_offboarded_stays_denied
BEFORE UPDATE OF admitted ON butler_members
WHEN NEW.admitted = 1 AND OLD.offboarded_at <> '' AND NEW.offboarded_at <> ''
BEGIN
    SELECT RAISE(ABORT, 'an offboarded member cannot be re-admitted without a new admission (ADR-W030 A2.1)');
END;

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

-- ---- butler_identity_links ----
--
-- One of only two columns in the whole schema that already carried a
-- foreign key into butler_members(member). It moves to person_id, which
-- is what that key should have been: a surface link that survives a
-- rename is the point of linking to an identity rather than a label.
CREATE TABLE butler_identity_links_v43 (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    link_type   TEXT NOT NULL CHECK (link_type IN ('api-principal', 'chat-binding', 'trusted-ingress')),
    -- external_id is the principal name, the chat binding id rendered as
    -- text, or the ingress subject. Stored as text so one column serves
    -- all three without a nullable pair.
    external_id TEXT NOT NULL,
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    -- generation is the member generation this link was created under.
    -- A link whose generation is behind the member's is stale and
    -- resolves to nothing.
    generation  INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    -- created_by is the administrator who approved it. Butler cannot
    -- grant itself manage authority (A2.1), and there is no code path
    -- that writes this row with the Butler as the creator.
    created_by  TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',

    CHECK (external_id <> ''),
    CHECK (created_by <> '')
);

INSERT INTO butler_identity_links_v43 (seq, link_type, external_id, person_id, generation,
    created_at, created_by, revoked_at, revoked_by)
SELECT l.seq, l.link_type, l.external_id, m.person_id, l.generation,
       l.created_at, l.created_by, l.revoked_at, l.revoked_by
FROM butler_identity_links l JOIN butler_members m ON m.member = l.member;

DROP TABLE butler_identity_links;
ALTER TABLE butler_identity_links_v43 RENAME TO butler_identity_links;

CREATE INDEX butler_identity_links_by_person ON butler_identity_links (person_id, seq);
CREATE UNIQUE INDEX butler_identity_links_live
    ON butler_identity_links (link_type, external_id) WHERE revoked_at = '';

-- ---- butler_conversations ----
--
-- The other pre-existing foreign key into butler_members(member).
-- Referenced in turn by butler_conversation_sessions and
-- butler_handoffs, both by seq, which is why seq is carried across.
CREATE TABLE butler_conversations_v43 (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    created_at TEXT NOT NULL,
    subject    TEXT NOT NULL DEFAULT ''
);

INSERT INTO butler_conversations_v43 (seq, person_id, created_at, subject)
SELECT c.seq, m.person_id, c.created_at, c.subject
FROM butler_conversations c JOIN butler_members m ON m.member = c.member;

DROP TABLE butler_conversations;
ALTER TABLE butler_conversations_v43 RENAME TO butler_conversations;

CREATE INDEX butler_conversations_by_person ON butler_conversations (person_id, seq);

-- ---- butler_handoffs ----
--
-- member here had no foreign key and no CHECK; it gains both, because
-- the column names the person whose conversation crossed surfaces and
-- there is no other kind of value it could hold.
CREATE TABLE butler_handoffs_v43 (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation INTEGER NOT NULL REFERENCES butler_conversations(seq),
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    from_surface TEXT NOT NULL CHECK (from_surface IN ('admin', 'family')),
    to_surface   TEXT NOT NULL CHECK (to_surface IN ('admin', 'family')),
    from_session INTEGER REFERENCES butler_sessions(seq),
    at           TEXT NOT NULL,
    -- carried is how many turns the destination was given after
    -- filtering. Recorded because "no privileged context crossed" is a
    -- claim, and a claim with a number behind it can be checked.
    carried      INTEGER NOT NULL DEFAULT 0,
    withheld     INTEGER NOT NULL DEFAULT 0,

    CHECK (from_surface <> to_surface)
);

INSERT INTO butler_handoffs_v43 (seq, conversation, person_id, from_surface, to_surface,
    from_session, at, carried, withheld)
SELECT h.seq, h.conversation, m.person_id, h.from_surface, h.to_surface,
       h.from_session, h.at, h.carried, h.withheld
FROM butler_handoffs h JOIN butler_members m ON m.member = h.member;

DROP TABLE butler_handoffs;
ALTER TABLE butler_handoffs_v43 RENAME TO butler_handoffs;

CREATE INDEX butler_handoffs_by_conversation ON butler_handoffs (conversation, seq);

CREATE TRIGGER butler_handoffs_immutable
BEFORE UPDATE ON butler_handoffs
BEGIN
    SELECT RAISE(ABORT, 'butler handoffs are append-only (ADR-W030 A2.1)');
END;

-- ---- butler_memory ----
--
-- Two columns move, and they do not move the same way.
--
-- author is `human`: the person who stated the record, and nothing
-- else. owner is `scoped` — the schema says so in full: "owner is the
-- person the record belongs to ... For household-shared records it is
-- the household". So owner_person_id is NULLABLE, and NULL means the
-- household owns it.
--
-- NULL rather than a reserved id, because a sentinel person_id would be
-- a row in butler_members that is not a person, and every query that
-- joins people would have to know to exclude it. NULL is already the
-- thing SQL has for "no person here", and the foreign key keeps working.
--
-- What NULL does NOT mean is "audience = shared". Nothing in the schema
-- or the write path ties the two together (#241), and a shared record
-- that genuinely belongs to one person resolves to that person here, as
-- it should. The value in the column decides, not the row's audience.
CREATE TABLE butler_memory_v43 (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL CHECK (kind IN ('fact', 'preference', 'inference', 'procedure')),
    subject    TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL,
    audience   TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    permission_label TEXT NOT NULL,
    -- The person the record belongs to: who may correct it, forget it,
    -- and export it. NULL is the household (A1).
    owner_person_id TEXT REFERENCES butler_members(person_id),

    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT '',

    confidence TEXT NOT NULL DEFAULT 'stated' CHECK (confidence IN ('stated', 'inferred', 'uncertain')),
    safety_relevant INTEGER NOT NULL DEFAULT 0,

    created_at TEXT NOT NULL,
    -- The person who stated it. Never the household and never a
    -- principal: a record nobody stated is not a record.
    author_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    superseded_by INTEGER REFERENCES butler_memory(seq),
    forgotten_at  TEXT NOT NULL DEFAULT '',
    -- forgotten_by is an actor column and keeps its recorded text.
    forgotten_by  TEXT NOT NULL DEFAULT '',
    envelope_version INTEGER NOT NULL,
    butler     TEXT NOT NULL DEFAULT '',

    CHECK (content <> ''),
    CHECK (permission_label <> ''),
    CHECK (author_person_id <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
);

INSERT INTO butler_memory_v43 (seq, kind, subject, content, audience, permission_label,
    owner_person_id, source_kind, source_id, observed_at, confidence, safety_relevant,
    created_at, author_person_id, superseded_by, forgotten_at, forgotten_by,
    envelope_version, butler)
SELECT r.seq, r.kind, r.subject, r.content, r.audience, r.permission_label,
       (SELECT o.person_id FROM butler_members o WHERE o.member = r.owner),
       r.source_kind, r.source_id, r.observed_at, r.confidence, r.safety_relevant,
       r.created_at,
       (SELECT a.person_id FROM butler_members a WHERE a.member = r.author),
       r.superseded_by, r.forgotten_at, r.forgotten_by, r.envelope_version, r.butler
FROM butler_memory r;

DROP TABLE butler_memory;
ALTER TABLE butler_memory_v43 RENAME TO butler_memory;

CREATE INDEX butler_memory_eligible ON butler_memory (subject, forgotten_at, seq);
CREATE INDEX butler_memory_by_source ON butler_memory (source_kind, source_id);
CREATE INDEX butler_memory_by_owner ON butler_memory (owner_person_id, seq);
CREATE INDEX butler_memory_scope ON butler_memory (butler, audience, owner_person_id, seq);

CREATE TRIGGER butler_memory_content_immutable
BEFORE UPDATE OF content, source_kind, source_id, author_person_id, kind ON butler_memory
BEGIN
    SELECT RAISE(ABORT, 'a memory record is corrected by superseding it, not by editing it (ADR-W030 A3)');
END;

CREATE TRIGGER butler_memory_audience_cannot_loosen
BEFORE UPDATE OF audience ON butler_memory
WHEN OLD.audience = 'private' AND NEW.audience = 'shared'
BEGIN
    SELECT RAISE(ABORT, 'private memory cannot be widened to shared by an update (ADR-W030 A2/A3)');
END;

-- ---- butler_index ----
--
-- owner mirrors butler_memory.owner and carries the same household
-- scope, so it is nullable for the same reason.
--
-- The FTS index is external-content (content='butler_index',
-- content_rowid='seq'): it stores its own inverted index and reads the
-- text back through seq. Carrying seq and content across unchanged
-- therefore leaves it valid, and the three triggers below are dropped
-- with the old table before the copy, so the copy does not reindex
-- anything. Restored verbatim afterwards.
CREATE TABLE butler_index_v43 (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    memory      INTEGER REFERENCES butler_memory(seq),
    -- NULL is the household, as in butler_memory.
    owner_person_id TEXT REFERENCES butler_members(person_id),
    audience    TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    permission_label TEXT NOT NULL,
    content     TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT '',
    indexed_at  TEXT NOT NULL,
    volatile    INTEGER NOT NULL DEFAULT 0,
    embedding   BLOB,
    tokens      INTEGER NOT NULL DEFAULT 0,
    butler      TEXT NOT NULL DEFAULT '',

    CHECK (content <> ''),
    CHECK (permission_label <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
);

INSERT INTO butler_index_v43 (seq, source_kind, source_id, memory, owner_person_id,
    audience, permission_label, content, observed_at, indexed_at, volatile,
    embedding, tokens, butler)
SELECT x.seq, x.source_kind, x.source_id, x.memory,
       (SELECT o.person_id FROM butler_members o WHERE o.member = x.owner),
       x.audience, x.permission_label, x.content, x.observed_at, x.indexed_at,
       x.volatile, x.embedding, x.tokens, x.butler
FROM butler_index x;

DROP TABLE butler_index;
ALTER TABLE butler_index_v43 RENAME TO butler_index;

CREATE INDEX butler_index_permission ON butler_index (audience, owner_person_id, seq);
CREATE INDEX butler_index_scope ON butler_index (butler, audience, owner_person_id, seq);

CREATE TRIGGER butler_index_ai AFTER INSERT ON butler_index BEGIN
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

CREATE TRIGGER butler_index_ad AFTER DELETE ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
END;

CREATE TRIGGER butler_index_au AFTER UPDATE OF content ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

-- ---- butler_tasks ----
--
-- Both author and owner are `human`: the schema says author is the
-- member who asked for the work and never changes, and that owner is
-- who operates it now, which offboarding moves. Neither takes the
-- household — a task with no person behind it has nobody to hand it to.
CREATE TABLE butler_tasks_v43 (
    seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at          TEXT NOT NULL,     -- RFC3339Nano UTC
    updated_at          TEXT NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('conversation', 'routine')),
    state               TEXT NOT NULL CHECK (state IN (
                            'pending', 'running', 'waiting',
                            'completed', 'failed', 'cancelled',
                            'paused-offboarded')),

    -- The person who asked for the work, and the person who operates it
    -- now. Offboarding moves the second and never the first.
    author_person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    owner_person_id     TEXT NOT NULL REFERENCES butler_members(person_id),

    surface             TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    profile             TEXT NOT NULL,
    audience            TEXT NOT NULL CHECK (audience IN ('private', 'shared')),

    session             INTEGER REFERENCES butler_sessions(seq),
    origin_turn         INTEGER,

    correlation_id      TEXT NOT NULL DEFAULT '',

    objective           TEXT NOT NULL,
    completion_criteria TEXT NOT NULL,

    grants_json         TEXT NOT NULL DEFAULT '{}',
    budget_json         TEXT NOT NULL DEFAULT '{}',
    deadline_at         TEXT NOT NULL DEFAULT '',
    next_wakeup_at      TEXT NOT NULL DEFAULT '',

    lease_epoch         INTEGER NOT NULL DEFAULT 0,

    uncertainty         TEXT NOT NULL DEFAULT '',
    result_summary      TEXT NOT NULL DEFAULT '',

    retention           TEXT NOT NULL CHECK (retention IN ('manual', 'conversation', 'ephemeral')),
    envelope_version    INTEGER NOT NULL,
    butler              TEXT NOT NULL DEFAULT ''
);

INSERT INTO butler_tasks_v43 (seq, created_at, updated_at, kind, state,
    author_person_id, owner_person_id, surface, profile, audience, session, origin_turn,
    correlation_id, objective, completion_criteria, grants_json, budget_json,
    deadline_at, next_wakeup_at, lease_epoch, uncertainty, result_summary,
    retention, envelope_version, butler)
SELECT t.seq, t.created_at, t.updated_at, t.kind, t.state,
       (SELECT a.person_id FROM butler_members a WHERE a.member = t.author),
       (SELECT o.person_id FROM butler_members o WHERE o.member = t.owner),
       t.surface, t.profile, t.audience, t.session, t.origin_turn,
       t.correlation_id, t.objective, t.completion_criteria, t.grants_json, t.budget_json,
       t.deadline_at, t.next_wakeup_at, t.lease_epoch, t.uncertainty, t.result_summary,
       t.retention, t.envelope_version, t.butler
FROM butler_tasks t;

DROP TABLE butler_tasks;
ALTER TABLE butler_tasks_v43 RENAME TO butler_tasks;

CREATE INDEX butler_tasks_butler ON butler_tasks (butler, state);
CREATE INDEX butler_tasks_by_owner ON butler_tasks (owner_person_id, seq);
CREATE INDEX butler_tasks_by_session ON butler_tasks (session, seq);
CREATE INDEX butler_tasks_by_state ON butler_tasks (state, seq);
CREATE INDEX butler_tasks_by_wakeup ON butler_tasks (next_wakeup_at) WHERE next_wakeup_at <> '';

CREATE TRIGGER butler_tasks_author_immutable
BEFORE UPDATE OF author_person_id ON butler_tasks
WHEN NEW.author_person_id <> OLD.author_person_id
BEGIN
    SELECT RAISE(ABORT, 'butler task authorship is immutable (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_complete_needs_evidence_insert
BEFORE INSERT ON butler_tasks
WHEN NEW.state = 'completed'
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot be created completed (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_complete_needs_evidence_update
BEFORE UPDATE OF state ON butler_tasks
WHEN NEW.state = 'completed'
 AND NOT EXISTS (SELECT 1 FROM butler_task_evidence WHERE task = NEW.seq)
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot complete without evidence (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON butler_tasks
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'butler task lease epoch cannot move backwards (ADR-W030 A11)');
END;

-- ---- household_review_tokens ----
--
-- "member is who it was issued to. A token that works for whoever holds
-- it is a password, and this is not one." A token that stops resolving
-- because its holder was renamed is not much better, which is what this
-- repoint fixes. created_by and used_by are actors and stay as text.
CREATE TABLE household_review_tokens_v43 (
    hash       TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL,
    routine    INTEGER REFERENCES household_routines(seq),
    revision   INTEGER NOT NULL DEFAULT 0,
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT NOT NULL DEFAULT '',
    used_by    TEXT NOT NULL DEFAULT '',

    CHECK (plan_id <> ''),
    CHECK (person_id <> ''),
    CHECK (expires_at <> '')
);

INSERT INTO household_review_tokens_v43 (hash, plan_id, routine, revision, person_id,
    created_at, created_by, expires_at, used_at, used_by)
SELECT k.hash, k.plan_id, k.routine, k.revision, m.person_id,
       k.created_at, k.created_by, k.expires_at, k.used_at, k.used_by
FROM household_review_tokens k JOIN butler_members m ON m.member = k.member;

DROP TABLE household_review_tokens;
ALTER TABLE household_review_tokens_v43 RENAME TO household_review_tokens;

CREATE INDEX household_review_tokens_plan ON household_review_tokens (plan_id);

CREATE TRIGGER household_review_tokens_single_use
BEFORE UPDATE OF used_at ON household_review_tokens
WHEN OLD.used_at <> '' AND NEW.used_at <> OLD.used_at
BEGIN
    SELECT RAISE(ABORT, 'a household review token is single-use (ADR-W030 A8.1)');
END;

-- ---- offboarding_records ----
--
-- member is the person being offboarded; actor is who did it, and stays
-- text. Offboarding is the one operation that most needs the reference
-- to survive: it is a record about a person at the moment their handle
-- stops being interesting.
CREATE TABLE offboarding_records_v43 (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    store      TEXT NOT NULL,
    affected   INTEGER NOT NULL DEFAULT 0,
    detail     TEXT NOT NULL DEFAULT '',

    CHECK (person_id <> ''),
    CHECK (actor <> ''),
    CHECK (store <> '')
);

INSERT INTO offboarding_records_v43 (seq, person_id, at, actor, store, affected, detail)
SELECT o.seq, m.person_id, o.at, o.actor, o.store, o.affected, o.detail
FROM offboarding_records o JOIN butler_members m ON m.member = o.member;

DROP TABLE offboarding_records;
ALTER TABLE offboarding_records_v43 RENAME TO offboarding_records;

CREATE INDEX offboarding_records_person ON offboarding_records (person_id, seq);

-- ---- notices ----
--
-- recipient is the member the notice is for: the runtime filters on it,
-- authorises replies by it, and offboarding sweeps by it. answered_by
-- is an actor column and keeps its text.
CREATE TABLE notices_v43 (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,
    recipient_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    routed_to_admin INTEGER NOT NULL DEFAULT 0,
    correlation_id TEXT NOT NULL DEFAULT '',
    outbound   INTEGER,
    dedupe_key TEXT NOT NULL,
    urgent     INTEGER NOT NULL DEFAULT 0,
    relevance  INTEGER NOT NULL DEFAULT 50,
    created_at TEXT NOT NULL,
    hold_until TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,

    delivered_at    TEXT NOT NULL DEFAULT '',
    acknowledged_at TEXT NOT NULL DEFAULT '',
    undelivered_at  TEXT NOT NULL DEFAULT '',

    asks       INTEGER NOT NULL DEFAULT 0,
    answered_at TEXT NOT NULL DEFAULT '',
    answer      TEXT NOT NULL DEFAULT '',
    answered_by TEXT NOT NULL DEFAULT '',
    -- Added by 0040: a notice that ASKS carries what it asked, because
    -- consent to an unnamed action is not consent. A rebuild that
    -- forgot it would put that back.
    question   TEXT NOT NULL DEFAULT '',

    CHECK (kind <> ''),
    CHECK (recipient_person_id <> ''),
    CHECK (dedupe_key <> ''),
    CHECK (answered_at = '' OR asks = 1)
);

INSERT INTO notices_v43 (seq, kind, recipient_person_id, routed_to_admin, correlation_id,
    outbound, dedupe_key, urgent, relevance, created_at, hold_until, expires_at,
    delivered_at, acknowledged_at, undelivered_at, asks, answered_at, answer, answered_by, question)
SELECT n.seq, n.kind, m.person_id, n.routed_to_admin, n.correlation_id,
       n.outbound, n.dedupe_key, n.urgent, n.relevance, n.created_at, n.hold_until, n.expires_at,
       n.delivered_at, n.acknowledged_at, n.undelivered_at, n.asks, n.answered_at, n.answer,
       n.answered_by, n.question
FROM notices n JOIN butler_members m ON m.member = n.recipient;

DROP TABLE notices;
ALTER TABLE notices_v43 RENAME TO notices;

CREATE INDEX notices_by_recipient ON notices (recipient_person_id, seq);
CREATE UNIQUE INDEX notices_dedupe ON notices (dedupe_key);

CREATE TRIGGER notices_answered_once
BEFORE UPDATE OF answered_at ON notices
WHEN OLD.answered_at <> '' AND NEW.answered_at <> OLD.answered_at
BEGIN
    SELECT RAISE(ABORT, 'a notice is answered once (ADR-W030 A10)');
END;

CREATE TRIGGER notices_expired_stays_expired
BEFORE UPDATE OF answered_at ON notices
WHEN OLD.undelivered_at <> '' AND NEW.answered_at <> ''
BEGIN
    SELECT RAISE(ABORT, 'an expired notice cannot be answered (ADR-W030 A10.1)');
END;

-- The gate's scaffolding is temp and goes with the connection, but
-- dropping it here keeps the migration's footprint explicit.
DROP TRIGGER person_refs_refuse;
DROP TABLE person_refs_unresolved;
