-- 0031: offboarding, restore quarantine, and the deletion journal
-- (ADR-W030 A2.1/A4/A10/A11, work package P16).
--
-- Two things a restore must not do, and one thing an offboarding must
-- do everywhere at once.
--
-- A restore must not revive what was deleted. An old backup does not
-- contain later decisions, and "we restored last week's database" must
-- not mean "the thing you asked us to forget is back". So deletions and
-- revocations are journalled SEPARATELY from the data they concern, and
-- reconciliation replays the journal over restored state. A journal
-- restored from the same old snapshot is itself incomplete — which is
-- exactly why quarantine holds until an administrator says the journal
-- is current, and why administrative acknowledgement alone is not
-- treated as evidence that the missing decisions were recovered.
--
-- A restore must not resume uncertain work. An action whose outcome was
-- unknown when the snapshot was taken is still unknown, and a restored
-- coordinator that retries it is doing the one thing A4 forbids.
--
-- And an offboarding has to land in every store in one act. Access
-- denied in six places at six different times is six windows.

CREATE TABLE deletion_journal (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    -- subject_kind names WHAT was removed, so reconciliation can replay
    -- it against the right store without the journal knowing their
    -- schemas.
    subject_kind TEXT NOT NULL CHECK (subject_kind IN (
                     'memory', 'member', 'link', 'connection', 'routine',
                     'grant', 'binding', 'task', 'notice', 'secret', 'index-source')),
    subject_id TEXT NOT NULL,
    action     TEXT NOT NULL CHECK (action IN ('deleted', 'revoked', 'offboarded', 'forgotten')),
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',

    CHECK (subject_id <> ''),
    CHECK (actor <> '')
);

CREATE INDEX deletion_journal_subject ON deletion_journal (subject_kind, subject_id);

-- restore_state is one row: is this database in quarantine, and what
-- has been reconciled.
--
-- Quarantine is the DEFAULT after a restore, not something an operator
-- remembers to turn on. Nothing Butler-facing runs while it holds: no
-- retrieval, no dispatch, no ingestion, no proactive delivery.
CREATE TABLE restore_state (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    quarantined    INTEGER NOT NULL DEFAULT 0,
    entered_at     TEXT NOT NULL DEFAULT '',
    reason         TEXT NOT NULL DEFAULT '',
    -- snapshot_at is when the restored data was taken. Everything
    -- decided after it is a decision this database does not have, which
    -- is the whole reason for the journal.
    snapshot_at    TEXT NOT NULL DEFAULT '',
    -- journal_current records an administrator's assertion that the
    -- deletion journal has been brought up to date from a source newer
    -- than the snapshot. It is a precondition for release and NOT
    -- evidence on its own — A10 is explicit that acknowledgement alone
    -- does not recover missing deletions — which is why released_at also
    -- requires the replay below to have run.
    journal_current INTEGER NOT NULL DEFAULT 0,
    journal_asserted_by TEXT NOT NULL DEFAULT '',
    replayed_at    TEXT NOT NULL DEFAULT '',
    replayed_count INTEGER NOT NULL DEFAULT 0,
    released_at    TEXT NOT NULL DEFAULT '',
    released_by    TEXT NOT NULL DEFAULT ''
);

-- Releasing quarantine requires the replay to have happened. An
-- administrator's word is a precondition, not the evidence.
CREATE TRIGGER restore_state_release_needs_replay
BEFORE UPDATE OF released_at ON restore_state
WHEN NEW.released_at <> '' AND (NEW.replayed_at = '' OR NEW.journal_current = 0)
BEGIN
    SELECT RAISE(ABORT, 'quarantine is released only after the deletion journal is asserted current AND replayed (ADR-W030 A10)');
END;

-- offboarding_records is what an offboarding actually did, per store.
-- It exists because "we offboarded them" is a claim, and a claim about
-- nine stores needs nine answers.
CREATE TABLE offboarding_records (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    member     TEXT NOT NULL,
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    store      TEXT NOT NULL,
    affected   INTEGER NOT NULL DEFAULT 0,
    detail     TEXT NOT NULL DEFAULT '',
    CHECK (member <> ''),
    CHECK (actor <> ''),
    CHECK (store <> '')
);

CREATE INDEX offboarding_records_member ON offboarding_records (member, seq);
