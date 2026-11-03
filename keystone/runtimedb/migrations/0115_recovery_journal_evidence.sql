-- Recovery journal reconciliation (#284).  The deletion journal remains the
-- product authority ledger; this migration records independently retained,
-- signed evidence that the restored snapshot's journal prefix is complete
-- through a terminal source cutoff.  An Owner assertion is deliberately not
-- sufficient to populate these fields.

ALTER TABLE restore_state ADD COLUMN snapshot_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN journal_evidence_id TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN journal_source TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN journal_head_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE restore_state ADD COLUMN journal_head_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN journal_coverage_through TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN journal_verified_at TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN journal_verified_by TEXT NOT NULL DEFAULT '';

CREATE TABLE recovery_journal_reconciliations (
    evidence_id       TEXT PRIMARY KEY,
    snapshot_sha256   TEXT NOT NULL,
    snapshot_at       TEXT NOT NULL,
    journal_source    TEXT NOT NULL,
    custody           TEXT NOT NULL,
    base_seq          INTEGER NOT NULL,
    head_seq          INTEGER NOT NULL,
    head_sha256       TEXT NOT NULL,
    coverage_through  TEXT NOT NULL,
    source_fenced_at  TEXT NOT NULL,
    signer_fingerprint TEXT NOT NULL,
    verified_at       TEXT NOT NULL,
    verified_by       TEXT NOT NULL,
    applied_count     INTEGER NOT NULL,
    evidence_json     TEXT NOT NULL,

    CHECK (evidence_id <> ''),
    CHECK (snapshot_sha256 <> ''),
    CHECK (journal_source <> ''),
    CHECK (custody <> ''),
    CHECK (base_seq >= 0),
    CHECK (head_seq >= base_seq),
    CHECK (head_sha256 <> ''),
    CHECK (coverage_through <> ''),
    CHECK (source_fenced_at <> ''),
    CHECK (signer_fingerprint <> ''),
    CHECK (verified_at <> ''),
    CHECK (verified_by <> ''),
    CHECK (applied_count >= 0)
);

CREATE TRIGGER recovery_journal_reconciliations_immutable
BEFORE UPDATE ON recovery_journal_reconciliations
BEGIN
    SELECT RAISE(ABORT, 'recovery journal reconciliation evidence is immutable');
END;

CREATE TRIGGER recovery_journal_reconciliations_not_deleted
BEFORE DELETE ON recovery_journal_reconciliations
BEGIN
    SELECT RAISE(ABORT, 'recovery journal reconciliation evidence is immutable');
END;

DROP TRIGGER restore_state_release_needs_replay;
CREATE TRIGGER restore_state_release_needs_replay
BEFORE UPDATE OF released_at ON restore_state
WHEN NEW.released_at <> '' AND (
    NEW.replayed_at = '' OR
    NEW.journal_current = 0 OR
    NEW.journal_evidence_id = '' OR
    NEW.journal_verified_at = ''
)
BEGIN
    SELECT RAISE(ABORT, 'quarantine release requires verified independent journal evidence and replay');
END;
