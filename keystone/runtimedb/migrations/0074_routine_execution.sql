-- One household-owned occurrence per approved Plan version and trigger. The
-- older butler_tasks(kind='routine') shape requires a human author and owner;
-- neither proposer nor approver may be borrowed by unattended work.
CREATE TABLE routine_execution_state (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 cursor INTEGER NOT NULL DEFAULT 0 CHECK(cursor>=0),
 initialized INTEGER NOT NULL DEFAULT 0 CHECK(initialized IN (0,1))
);
INSERT INTO routine_execution_state(singleton) VALUES(1);

CREATE TABLE routine_runs (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 plan INTEGER NOT NULL REFERENCES routine_plan_versions(plan),
 trigger_event INTEGER NOT NULL REFERENCES household_events(seq),
 state TEXT NOT NULL CHECK(state IN ('pending','running','blocked','completed','failed','uncertain','stopped')),
 reason TEXT NOT NULL DEFAULT '',
 effect_state TEXT NOT NULL DEFAULT 'none' CHECK(effect_state IN ('none','claimed','pending','confirmed','failed','uncertain')),
 effect_summary TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(plan,trigger_event)
);
CREATE INDEX routine_runs_by_state ON routine_runs(state,seq);
CREATE TABLE routine_effect_receipts (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 run INTEGER NOT NULL REFERENCES routine_runs(seq),
 recorded_at TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('pending','confirmed','failed','uncertain')),
 summary TEXT NOT NULL CHECK(summary<>''),
 UNIQUE(run)
);
CREATE TRIGGER routine_effect_receipts_immutable BEFORE UPDATE ON routine_effect_receipts
BEGIN SELECT RAISE(ABORT,'routine effect receipts are append-only'); END;
CREATE TABLE routine_run_transitions (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 run INTEGER NOT NULL REFERENCES routine_runs(seq),
 at TEXT NOT NULL,
 from_state TEXT NOT NULL,
 to_state TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT ''
);
CREATE TRIGGER routine_run_transitions_immutable BEFORE UPDATE ON routine_run_transitions
BEGIN SELECT RAISE(ABORT,'routine run transitions are append-only'); END;
