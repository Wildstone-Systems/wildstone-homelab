-- 0018: the single active coordinator lease (ADR-W030 A11, work
-- package P3).
--
-- V1 runs one active Keystone/Butler coordinator and one authoritative
-- runtime database. No active-active writers, no leader election, no
-- concurrent task schedulers. Recovery may prepare a standby copy, but
-- promotion is EXPLICIT and fenced, so the prior coordinator cannot go
-- on dispatching into state the new one now owns.
--
-- One row, id = 1, is the whole design. A second process that starts
-- against the same database reads this row, sees another holder, and
-- refuses to dispatch — it does not wait, retry, or take over, because
-- "took over automatically" is exactly the failure mode a household
-- with one Keystone cannot debug.
--
-- epoch is the fence. Promotion increments it; every task dispatch
-- carries the epoch it was claimed under, so a coordinator whose epoch
-- has been superseded discovers it at the next checkpoint and stops
-- rather than completing work the new coordinator has already redone.
CREATE TABLE coordinator_lease (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    holder      TEXT NOT NULL,
    epoch       INTEGER NOT NULL,
    acquired_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    -- promoted_from records who was fenced, so a recovery has a written
    -- answer to "what happened to the old one".
    promoted_from TEXT NOT NULL DEFAULT ''
);

-- The epoch only moves forward, for the same reason a task's lease
-- epoch does: rewinding it un-fences a coordinator that was deliberately
-- stopped.
CREATE TRIGGER coordinator_lease_epoch_monotonic
BEFORE UPDATE OF epoch ON coordinator_lease
WHEN NEW.epoch < OLD.epoch
BEGIN
    SELECT RAISE(ABORT, 'coordinator lease epoch cannot move backwards (ADR-W030 A11)');
END;
