-- Journal-gated restore quarantine release (#285).
--
-- The application revalidates the signed evidence under the currently
-- installed trust root.  These triggers are the storage backstop: no writer
-- may clear quarantine unless the current restore projection has a complete,
-- generation-bound immutable reconciliation record and replay checkpoint.

DROP TRIGGER restore_state_release_needs_replay;

CREATE TRIGGER restore_state_release_needs_replay
BEFORE UPDATE OF released_at ON restore_state
WHEN NEW.released_at <> '' AND (
    NEW.quarantined <> 0 OR
    NEW.restore_generation = '' OR
    NEW.snapshot_sha256 = '' OR
    NEW.replayed_at = '' OR
    NEW.journal_current = 0 OR
    NEW.journal_evidence_id = '' OR
    NEW.journal_verified_at = '' OR
    NOT EXISTS (
        SELECT 1
          FROM recovery_journal_reconciliations r
         WHERE r.evidence_id = NEW.journal_evidence_id
           AND r.restore_generation = NEW.restore_generation
           AND r.snapshot_sha256 = NEW.snapshot_sha256
           AND r.snapshot_at = NEW.snapshot_at
           AND r.journal_source = NEW.journal_source
           AND r.head_seq = NEW.journal_head_seq
           AND r.head_sha256 = NEW.journal_head_sha256
           AND r.coverage_through = NEW.journal_coverage_through
           AND r.verified_at = NEW.journal_verified_at
           AND r.verified_by = NEW.journal_verified_by
           AND r.applied_count = NEW.replayed_count
           AND r.source_fenced_at <> ''
           AND r.signer_fingerprint <> ''
    )
)
BEGIN
    SELECT RAISE(ABORT, 'quarantine release requires current generation-bound journal evidence and replay');
END;

CREATE TRIGGER restore_state_quarantine_clear_needs_release
BEFORE UPDATE OF quarantined ON restore_state
WHEN OLD.quarantined = 1 AND NEW.quarantined = 0 AND (
    NEW.released_at = '' OR
    NEW.released_by = '' OR
    NEW.restore_generation = '' OR
    NEW.snapshot_sha256 = '' OR
    NEW.replayed_at = '' OR
    NEW.journal_current = 0 OR
    NEW.journal_evidence_id = '' OR
    NEW.journal_verified_at = '' OR
    NOT EXISTS (
        SELECT 1
          FROM recovery_journal_reconciliations r
         WHERE r.evidence_id = NEW.journal_evidence_id
           AND r.restore_generation = NEW.restore_generation
           AND r.snapshot_sha256 = NEW.snapshot_sha256
           AND r.snapshot_at = NEW.snapshot_at
           AND r.journal_source = NEW.journal_source
           AND r.head_seq = NEW.journal_head_seq
           AND r.head_sha256 = NEW.journal_head_sha256
           AND r.coverage_through = NEW.journal_coverage_through
           AND r.verified_at = NEW.journal_verified_at
           AND r.verified_by = NEW.journal_verified_by
           AND r.applied_count = NEW.replayed_count
           AND r.source_fenced_at <> ''
           AND r.signer_fingerprint <> ''
    )
)
BEGIN
    SELECT RAISE(ABORT, 'restore quarantine cannot be cleared without an audited journal-gated release');
END;
