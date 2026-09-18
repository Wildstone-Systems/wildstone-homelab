-- Meter readings: what a number the household keeps was observed to be,
-- and when (ADR-W034 A3).
--
-- NOT DESIRED STATE, DELIBERATELY. The Meter itself is a Statefile
-- resource — what the number is, how stale it may get — but a reading is
-- an observation of the world at a moment, and the Statefile describes
-- what the household wants rather than what it saw. Requiring a Plan to
-- write down a mileage would mean nobody ever writes down a mileage,
-- which is the failure the whole design exists to prevent (A16).
--
-- WHO SAID SO, AND WHAT KIND OF WHO. `recorded_by` is the principal, and
-- `source` is the only thing about the door that identity can actually
-- prove: a person's credential, or a capability's publisher principal.
--
-- An earlier draft of this column recorded which of the four doors a
-- reading came through — console, cli, chat, publisher. Three of those
-- are the same credential wearing different clothes, so the intake could
-- only have learned them by being told, and a column documented as
-- stamped-from-identity would have been holding a client's claim. The
-- distinction identity can prove is the one worth storing; which screen
-- a person typed into is not a fact the control plane has.
--
-- NOTHING READS IT WHEN EVALUATING, either way. A4 says a capability
-- extends the tracker and never enables it, and the sharp form of that
-- promise is that the due-ness function cannot tell a typed number from
-- a published one. The column is audit, not input.
--
-- `at` is when the meter was READ; `recorded_at` is when Keystone heard
-- about it. They differ whenever somebody writes down a number they took
-- this morning, and the difference matters: staleness and replay are both
-- measured against `at`, never against when the row landed.
CREATE TABLE meter_readings (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    meter       TEXT    NOT NULL,             -- metadata.name of the Meter resource
    value       REAL    NOT NULL,
    at          TEXT    NOT NULL,             -- RFC3339Nano UTC, when it was read
    recorded_at TEXT    NOT NULL,             -- RFC3339Nano UTC, stamped here
    -- Who said so: a member name, or the publishing principal.
    recorded_by TEXT    NOT NULL,
    -- What kind of caller recorded it. Audit only; see above.
    source      TEXT    NOT NULL CHECK (source IN ('person', 'publisher')),
    -- A rebase marks the point where the meter's coordinate space
    -- restarts but the obligation does not: a replaced odometer, a
    -- swapped hour counter. `prior` is what the old meter read just
    -- before it went, so the thresholds can be carried across; without
    -- it the reading before this one is used, and with neither the
    -- commitments measured here report themselves unresolved rather
    -- than inventing a number.
    rebase      INTEGER NOT NULL DEFAULT 0 CHECK (rebase IN (0, 1)),
    prior       REAL    NOT NULL DEFAULT 0,
    -- Why, for a rebase or a correction. Free text, shown to a person.
    reason      TEXT    NOT NULL DEFAULT '',
    -- The idempotency key for a publisher that retries a delivery. Empty
    -- for a person typing a number in, who is not retrying anything and
    -- may legitimately record the same value twice.
    external_id TEXT    NOT NULL DEFAULT ''
);

-- The one query that matters: a meter's readings in time order, which is
-- what the due-ness function takes and what a history screen shows.
CREATE INDEX meter_readings_by_meter ON meter_readings (meter, at);

-- A retried publisher delivery is a no-op rather than a second reading.
-- Partial, so the empty external_id a person's reading carries does not
-- collide with every other person's reading.
CREATE UNIQUE INDEX meter_readings_external ON meter_readings (meter, recorded_by, external_id)
    WHERE external_id <> '';
