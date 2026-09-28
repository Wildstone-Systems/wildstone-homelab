-- 0094: a Person's Files folder is recorded the moment they exist (#426
-- review of 0093).
--
-- 0093 recorded folders for everybody on record then, and afterwards only
-- when a Person first opened Files. That left a gap: somebody who used
-- Files as a plain directory account, became a Person and was renamed
-- before opening Files again had no record, so the folder their old name
-- gives stayed unclaimed and could be reached by a new account of that
-- name. Recording at creation closes it. A folder another Person already
-- holds is not taken; that Person keeps it, and the newcomer is given one
-- when they first open Files, or none.
--
-- Forward-only (ADR-W007 A4).

CREATE TRIGGER person_file_dirs_record_on_insert
AFTER INSERT ON butler_members
WHEN NOT EXISTS (SELECT 1 FROM person_file_dirs WHERE person_id = NEW.person_id)
 AND NOT EXISTS (SELECT 1 FROM person_file_dirs WHERE dir = lower(NEW.member))
BEGIN
    INSERT INTO person_file_dirs (person_id, dir, created_at)
        VALUES (NEW.person_id, lower(NEW.member), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
END;
