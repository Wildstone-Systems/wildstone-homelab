-- 0095: record a Files folder for every Person who has none (#426 review).
--
-- 0093 recorded folders for everybody on record when it ran, and 0094
-- records one when a Person is created. A Person created between those
-- two has neither, and if they are renamed before opening Files again the
-- folder their files are in is unclaimed. Each is given the folder named
-- by the handle they were created with (their earliest in person_handles),
-- which is where their files were written. No folder is given to two
-- People: one already recorded stays its owner's, and where two would
-- claim the same one the earlier Person gets it.
--
-- A separate migration, not an edit to 0094: a database that already
-- applied 0094 would never run a change made to it (ADR-W007 A4).
--
-- Forward-only.

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
