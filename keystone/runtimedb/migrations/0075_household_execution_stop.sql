-- Incident stop is distinct from restore quarantine. Releasing this stop
-- does not assert or replay a deletion journal; restore quarantine keeps its
-- stricter release path.
CREATE TABLE household_execution_stop (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 active INTEGER NOT NULL DEFAULT 0 CHECK(active IN (0,1)),
 generation INTEGER NOT NULL DEFAULT 0,
 entered_at TEXT NOT NULL DEFAULT '',
 entered_by TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL DEFAULT '',
 released_at TEXT NOT NULL DEFAULT '',
 released_by TEXT NOT NULL DEFAULT ''
);
INSERT INTO household_execution_stop(singleton) VALUES(1);
CREATE TABLE household_execution_stop_events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 generation INTEGER NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('stop','resume')),
 at TEXT NOT NULL,
 actor TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT ''
);
CREATE TRIGGER household_execution_stop_events_immutable BEFORE UPDATE ON household_execution_stop_events
BEGIN SELECT RAISE(ABORT,'household execution stop history is append-only'); END;
