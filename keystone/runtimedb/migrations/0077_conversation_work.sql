-- A turn keeps its own evidence ledger. The conversation holds the aggregate
-- budget and unresolved uncertainty across those fresh turn ledgers.
CREATE TABLE conversation_work (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 session INTEGER UNIQUE REFERENCES butler_sessions(seq),
 owner_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 tool_calls INTEGER NOT NULL DEFAULT 0 CHECK(tool_calls>=0),
 tool_limit INTEGER NOT NULL DEFAULT 64 CHECK(tool_limit>0),
 uncertain_outcome INTEGER NOT NULL DEFAULT 0 CHECK(uncertain_outcome IN (0,1)),
 uncertain_task INTEGER REFERENCES butler_tasks(seq),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
ALTER TABLE butler_tasks ADD COLUMN conversation_work INTEGER REFERENCES conversation_work(seq);
CREATE TABLE conversation_work_reviews (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 work INTEGER NOT NULL REFERENCES conversation_work(seq),
 actor_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 action TEXT NOT NULL CHECK(action IN ('resolve-outcome','extend-budget')),
 note TEXT NOT NULL CHECK(note<>''),
 at TEXT NOT NULL
);
CREATE TRIGGER conversation_work_reviews_immutable BEFORE UPDATE ON conversation_work_reviews
BEGIN SELECT RAISE(ABORT,'conversation work reviews are append-only'); END;
