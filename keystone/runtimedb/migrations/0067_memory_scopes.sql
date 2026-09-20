-- Session context remains in butler_sessions/turns (0066), never implicitly
-- promoted to durable memory. The other three design §11 scopes live here.
ALTER TABLE butler_memory ADD COLUMN scope TEXT NOT NULL DEFAULT 'household'
    CHECK (scope IN ('agent_local', 'personal', 'household'));

-- Retroactive defaults approved for slice 13: ownership defines durable scope,
-- not the old blanket Butler stamp. Explicit Agent inferences/procedures stay
-- local; stated Person-owned facts become Personal; household-owned facts
-- become Household. The old Butler stamp remains creation provenance.
UPDATE butler_memory SET scope = CASE
    WHEN butler <> '' AND kind IN ('inference', 'procedure') THEN 'agent_local'
    WHEN owner_person_id IS NOT NULL AND audience <> 'aud_household' THEN 'personal'
    ELSE 'household' END;

-- Reclassification cannot free a summary from an Agent-local ancestor.
-- Earlier derivation rules already preserved the holding Butler stamp.
WITH RECURSIVE local_descendants(seq) AS (
    SELECT seq FROM butler_memory WHERE scope='agent_local'
    UNION SELECT d.derivative FROM butler_memory_derivations d
      JOIN local_descendants a ON a.seq=d.source
)
UPDATE butler_memory SET scope='agent_local'
WHERE seq IN (SELECT seq FROM local_descendants) AND butler<>'';

CREATE TRIGGER memory_scope_valid_insert BEFORE INSERT ON butler_memory
WHEN (NEW.scope = 'personal' AND NEW.owner_person_id IS NULL)
  OR (NEW.scope = 'agent_local' AND NEW.butler = '')
BEGIN SELECT RAISE(ABORT, 'memory scope needs its owner or Agent'); END;

CREATE TRIGGER memory_scope_immutable BEFORE UPDATE OF scope, owner_person_id ON butler_memory
WHEN NEW.scope <> OLD.scope OR NEW.owner_person_id IS NOT OLD.owner_person_id
BEGIN SELECT RAISE(ABORT, 'memory scope and ownership require a new record'); END;

-- Store all inherited audience predicates, not just whichever audience is
-- narrowest today. Group memberships can change tomorrow.
CREATE TABLE memory_constraints (
    memory INTEGER NOT NULL REFERENCES butler_memory(seq),
    audience TEXT NOT NULL REFERENCES audiences(audience_id),
    owner_person_id TEXT REFERENCES butler_members(person_id)
);
CREATE INDEX memory_constraints_record ON memory_constraints(memory);
INSERT INTO memory_constraints(memory, audience, owner_person_id)
WITH RECURSIVE ancestors(memory, source) AS (
    SELECT derivative, source FROM butler_memory_derivations
    UNION SELECT a.memory, d.source FROM ancestors a
      JOIN butler_memory_derivations d ON d.derivative = a.source
)
SELECT DISTINCT a.memory, m.audience, m.owner_person_id
FROM ancestors a JOIN butler_memory m ON m.seq = a.source
UNION SELECT DISTINCT a.memory, 'aud_owner', m.owner_person_id
FROM ancestors a JOIN butler_memory m ON m.seq=a.source WHERE m.scope='personal';

-- Explicit restrictions only subtract from Person/audience visibility.
CREATE TABLE memory_agent_restrictions (
    agent INTEGER NOT NULL REFERENCES butlers(seq),
    scope TEXT NOT NULL CHECK(scope IN ('personal', 'household')),
    PRIMARY KEY(agent, scope)
);

CREATE TABLE memory_scope_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    memory INTEGER NOT NULL REFERENCES butler_memory(seq),
    kind TEXT NOT NULL CHECK(kind IN ('promotion', 'transfer')),
    source_memory INTEGER REFERENCES butler_memory(seq),
    source_agent TEXT NOT NULL DEFAULT '',
    destination_agent TEXT NOT NULL DEFAULT '',
    actor_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    recorded_at TEXT NOT NULL
);

CREATE INDEX memory_scope_events_source ON memory_scope_events(source_memory, kind);
