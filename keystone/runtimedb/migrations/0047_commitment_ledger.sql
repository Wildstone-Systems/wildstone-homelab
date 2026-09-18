-- What the household has actually done about its commitments, and what
-- it has arranged to do (ADR-W034 A6, A8).
--
-- THREE TABLES, AND ONLY TWO OF THEM ARE TRUE. A completion and a booking
-- are facts: somebody did the thing, or somebody made an appointment.
-- Where a cycle STANDS — pending, due soon, scheduled, overdue — is not
-- stored as truth, because A17 makes it a pure function of the readings
-- and these rows. The engine re-derives it every pass, which is what
-- makes a restored backup recompute rather than believe a stale verdict.
--
-- commitment_state is therefore a cache and says so. Its only job is to
-- remember what the household was last TOLD, so a commitment that has
-- been due for three weeks is announced once rather than every morning.

-- A closed cycle. Evidence is not optional: the positions the meters
-- stood at are what the next cycle is measured from, so an oil change
-- recorded without a mileage is a note rather than a maintenance record.
CREATE TABLE commitment_completions (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    commitment  TEXT    NOT NULL,
    -- When the work was done, which is not when it was written down.
    at          TEXT    NOT NULL,
    recorded_at TEXT    NOT NULL,
    recorded_by TEXT    NOT NULL,
    -- A waive is a cycle deliberately skipped. It closes the cycle
    -- exactly as a completion does and is kept distinct so that "we
    -- decided not to" never reads later as "it was done".
    waived      INTEGER NOT NULL DEFAULT 0 CHECK (waived IN (0, 1)),
    reason      TEXT    NOT NULL DEFAULT '',
    -- meter name -> reading at completion, as a JSON object. Empty for a
    -- commitment measured only by the calendar, whose position is `at`.
    positions   TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX commitment_completions_by_commitment ON commitment_completions (commitment, at);

-- An appointment for the open cycle: the fact that separates "I know the
-- checkup is due" from "the checkup is going to happen". Only a
-- commitment that requires booking has any.
CREATE TABLE commitment_bookings (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    commitment   TEXT    NOT NULL,
    -- When the appointment is.
    at           TEXT    NOT NULL,
    -- When it was made, and by whom. `booked_at` is why a replay cannot
    -- see an appointment nobody had arranged yet.
    booked_at    TEXT    NOT NULL,
    booked_by    TEXT    NOT NULL,
    note         TEXT    NOT NULL DEFAULT '',
    -- Cancelling is recorded rather than deleted: "we had an appointment
    -- and it was cancelled" and "we never made one" are different
    -- stories, and only one of them is somebody's fault.
    cancelled_at TEXT    NOT NULL DEFAULT '',
    cancelled_by TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX commitment_bookings_by_commitment ON commitment_bookings (commitment, booked_at);

-- A cache of what the household was last told, so a state change is
-- announced once. Deliberately NOT the source of truth for state: drop
-- this table and the next pass recomputes every verdict correctly, and
-- merely re-announces some of them.
CREATE TABLE commitment_state (
    commitment TEXT PRIMARY KEY,
    state      TEXT NOT NULL,
    cycle      INTEGER NOT NULL,
    -- Which meter decided it, so a commitment that moves from due-on-miles
    -- to due-on-months is treated as news.
    trigger    TEXT NOT NULL DEFAULT '',
    changed_at TEXT NOT NULL
);
