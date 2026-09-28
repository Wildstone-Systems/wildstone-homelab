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
-- People recorded after 0093 ran and before this one did have no row
-- yet, so they are backfilled first, from the handle they were created
-- with (their earliest in person_handles): that is the folder their
-- files were written to. Oldest Person first, so where two would claim
-- one folder the earlier keeps it, as 0093's rule already does.
--
-- Forward-only (ADR-W007 A4).

WITH first_handle AS (
    SELECT b.seq, b.person_id,
           lower((SELECT h.handle FROM person_handles h
                   WHERE h.person_id = b.person_id
                   ORDER BY h.recorded_at, h.rowid LIMIT 1)) AS dir
      FROM butler_members b
     WHERE NOT EXISTS (SELECT 1 FROM person_file_dirs d WHERE d.person_id = b.person_id)
), claim AS (
    SELECT person_id, dir FROM first_handle f
     WHERE dir IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM person_file_dirs d WHERE d.dir = f.dir)
       AND f.seq = (SELECT MIN(f2.seq) FROM first_handle f2 WHERE f2.dir = f.dir)
)
INSERT INTO person_file_dirs (person_id, dir, created_at)
    SELECT person_id, dir, strftime('%Y-%m-%dT%H:%M:%fZ', 'now') FROM claim;

CREATE TRIGGER person_file_dirs_record_on_insert
AFTER INSERT ON butler_members
WHEN NOT EXISTS (SELECT 1 FROM person_file_dirs WHERE person_id = NEW.person_id)
 AND NOT EXISTS (SELECT 1 FROM person_file_dirs WHERE dir = lower(NEW.member))
BEGIN
    INSERT INTO person_file_dirs (person_id, dir, created_at)
        VALUES (NEW.person_id, lower(NEW.member), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
END;
