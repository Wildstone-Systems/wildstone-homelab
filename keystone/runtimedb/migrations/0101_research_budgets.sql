-- S24 / ADR-W047. Merge only after 0099 (#475) and 0100 (#478).
-- Research is a child of the existing work ledger, not another task engine.
CREATE TABLE research_budgets (
    task INTEGER PRIMARY KEY REFERENCES butler_tasks(seq),
    searches_used INTEGER NOT NULL DEFAULT 0 CHECK(searches_used >= 0),
    fetches_used INTEGER NOT NULL DEFAULT 0 CHECK(fetches_used >= 0),
    searches_allowed INTEGER NOT NULL CHECK(searches_allowed BETWEEN 1 AND 100),
    fetches_allowed INTEGER NOT NULL CHECK(fetches_allowed BETWEEN 1 AND 200),
    config_searches INTEGER NOT NULL CHECK(config_searches BETWEEN 1 AND 100),
    config_fetches INTEGER NOT NULL CHECK(config_fetches BETWEEN 1 AND 200),
    revision INTEGER NOT NULL DEFAULT 1,
    wait_seq INTEGER REFERENCES butler_task_waits(seq)
);
CREATE TABLE research_attempts (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    task INTEGER NOT NULL REFERENCES research_budgets(task),
    ledger_attempt INTEGER NOT NULL REFERENCES butler_task_attempts(seq),
    kind TEXT NOT NULL CHECK(kind IN ('search','fetch')),
    state TEXT NOT NULL DEFAULT 'reserved' CHECK(state IN ('reserved','succeeded','failed','cancelled')),
    created_at TEXT NOT NULL,
    ended_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX research_attempts_task ON research_attempts(task,seq);
CREATE TABLE research_checkpoints (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    task INTEGER NOT NULL REFERENCES butler_tasks(seq),
    created_at TEXT NOT NULL,
    steps_json TEXT NOT NULL,
    next_action TEXT NOT NULL CHECK(next_action IN ('search','fetch','read','report'))
);
CREATE INDEX research_checkpoints_task ON research_checkpoints(task,seq);
CREATE TABLE research_continuations (
    task INTEGER NOT NULL REFERENCES research_budgets(task),
    revision INTEGER NOT NULL,
    actor_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    created_at TEXT NOT NULL,
    searches_allowed INTEGER NOT NULL,
    fetches_allowed INTEGER NOT NULL,
    PRIMARY KEY(task,revision)
);
