ALTER TABLE butler_tasks ADD COLUMN identity_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE butler_sessions ADD COLUMN identity_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE chat_outbound ADD COLUMN identity_generation INTEGER NOT NULL DEFAULT 0;

-- Existing work was admitted under the generation current at migration.
-- This preserves valid queued work while making every later generation bump
-- fence it. New live work supplies the exact ingress-observed generation.
UPDATE butler_tasks
SET identity_generation = (
    SELECT generation FROM butler_members WHERE person_id = butler_tasks.owner_person_id
)
WHERE identity_generation = 0;

UPDATE butler_sessions
SET identity_generation = (
    SELECT generation FROM butler_members WHERE person_id = butler_sessions.person_id
)
WHERE identity_generation = 0 AND person_id IS NOT NULL;

-- A chat binding is already a human-established Person binding. Project live
-- legacy rows into A2.1's canonical link table without inferring from names.
INSERT INTO butler_identity_links
    (link_type, external_id, person_id, generation, created_at, created_by)
SELECT 'chat-binding', CAST(cb.id AS TEXT), cb.person_id, m.generation,
       cb.created_at, cb.created_by
FROM chat_bindings cb
JOIN butler_members m ON m.person_id = cb.person_id
WHERE cb.revoked_at = ''
  AND NOT EXISTS (
      SELECT 1 FROM butler_identity_links l
      WHERE l.link_type = 'chat-binding'
        AND l.external_id = CAST(cb.id AS TEXT)
        AND l.revoked_at = ''
  );

UPDATE chat_outbound
SET identity_generation = (
    SELECT m.generation
    FROM chat_bindings b JOIN butler_members m ON m.person_id = b.person_id
    WHERE b.id = chat_outbound.binding_id
)
WHERE binding_id <> 0 AND identity_generation = 0;
