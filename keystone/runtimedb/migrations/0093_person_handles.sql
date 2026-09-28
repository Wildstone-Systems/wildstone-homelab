-- 0093: every handle a Person has ever carried, so a reference written as
-- a handle keeps naming the same Person after a rename (#372).
--
-- 0043 moved every reference the RUNTIME DATABASE holds onto person_id.
-- Three kinds of reference could not move with it, because they are not
-- in this database at all: the shared calendar registry's
-- readMembers/writeMembers, a commitment's `household/<member>`, and the
-- Files capability's Members/<name> folder. The first two are desired
-- state an Owner typed, and the third is a directory on disk. Each names
-- a person by the handle they had when it was written, so renaming
-- somebody silently detached their calendar grants, their commitments
-- and their own files.
--
-- This table is how those references reach a Person. A handle is
-- recorded the moment a Person is created or renamed and is never
-- removed, so resolving `sam` finds the Person who was `sam` even after
-- they became `samuel`. The desired state does not have to be rewritten,
-- which it could not be here: a migration must not change what an Owner
-- approved (ADR-W033 E2).
--
-- A RETIRED HANDLE STAYS ITS PERSON'S. That is what makes the lookup
-- safe. If a new Person could take `sam` after the first one was
-- renamed, every grant written for the first would quietly pass to the
-- second, which is a worse failure than detaching. So the triggers below
-- refuse to give a handle to anyone other than the Person it belonged
-- to. The same Person may take an old handle back.
--
-- Exact match, like resolvePersonID: `Sam` and `sam` are two handles
-- until ADR-W032 A4 is decided, and this table does not decide it.
--
-- Forward-only (ADR-W007 A4).

CREATE TABLE person_handles (
    handle      TEXT PRIMARY KEY,
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    recorded_at TEXT NOT NULL,
    CHECK (handle <> '')
);

CREATE INDEX person_handles_by_person ON person_handles(person_id);

-- Every Person's current handle is the one they have always had, as far
-- as this table can know. Handles retired before this migration were
-- already detached and cannot be recovered here.
INSERT INTO person_handles (handle, person_id, recorded_at)
    SELECT member, person_id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now') FROM butler_members;

-- Refuse a handle that another Person used to have. The join on the
-- current handle lets RecordButlerMember's upsert through: it proposes a
-- fresh person_id for somebody who already holds the handle, and the
-- conflict clause then updates that same Person instead of inserting.
CREATE TRIGGER person_handles_refuse_reuse_on_insert
BEFORE INSERT ON butler_members
WHEN EXISTS (
    SELECT 1 FROM person_handles h
      JOIN butler_members b ON b.person_id = h.person_id
     WHERE h.handle = NEW.member
       AND h.person_id <> NEW.person_id
       AND b.member <> NEW.member)
BEGIN
    SELECT RAISE(ABORT, 'runtimedb: that handle belonged to another person and is kept so their references still reach them');
END;

CREATE TRIGGER person_handles_refuse_reuse_on_rename
BEFORE UPDATE OF member ON butler_members
WHEN EXISTS (
    SELECT 1 FROM person_handles h
     WHERE h.handle = NEW.member
       AND h.person_id <> NEW.person_id)
BEGIN
    SELECT RAISE(ABORT, 'runtimedb: that handle belonged to another person and is kept so their references still reach them');
END;

CREATE TRIGGER person_handles_record_on_insert
AFTER INSERT ON butler_members
BEGIN
    INSERT OR IGNORE INTO person_handles (handle, person_id, recorded_at)
        VALUES (NEW.member, NEW.person_id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
END;

CREATE TRIGGER person_handles_record_on_rename
AFTER UPDATE OF member ON butler_members
BEGIN
    INSERT OR IGNORE INTO person_handles (handle, person_id, recorded_at)
        VALUES (NEW.member, NEW.person_id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
END;

-- Calendar display preferences follow the Person (ADR-W038). A row whose
-- handle names nobody on record is a preference nobody can see, and it
-- is not carried: the calendar is then simply shown, which is the
-- default every new member gets.
CREATE TABLE calendar_visibility_v93 (
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    calendar_id TEXT NOT NULL,
    visible     INTEGER NOT NULL CHECK (visible IN (0, 1)),
    PRIMARY KEY (person_id, calendar_id)
);
INSERT INTO calendar_visibility_v93 (person_id, calendar_id, visible)
    SELECT h.person_id, v.calendar_id, v.visible
      FROM calendar_visibility v
      JOIN person_handles h ON h.handle = v.member;
DROP TABLE calendar_visibility;
ALTER TABLE calendar_visibility_v93 RENAME TO calendar_visibility;

-- A Person's own Files folder, by Person (ADR-W037). Until now it was
-- Members/<lower-cased handle>, recomputed on every request, so a rename
-- pointed the person at an empty folder. The folder each Person already
-- has is recorded here and kept for good. Two handles that differ only
-- in case already shared one folder, and still do (ADR-W032 A4).
CREATE TABLE person_file_dirs (
    person_id  TEXT PRIMARY KEY REFERENCES butler_members(person_id),
    dir        TEXT NOT NULL,
    created_at TEXT NOT NULL,
    CHECK (dir <> '')
);
CREATE INDEX person_file_dirs_by_dir ON person_file_dirs(dir);
INSERT INTO person_file_dirs (person_id, dir, created_at)
    SELECT person_id, lower(member), strftime('%Y-%m-%dT%H:%M:%fZ', 'now') FROM butler_members;
