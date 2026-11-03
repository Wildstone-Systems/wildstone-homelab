-- Authenticated post-restore coordinator promotion (#286).
--
-- Release makes restored data available but does not grant execution
-- authority.  One immutable, generation-bound Owner decision authorizes the
-- existing coordinator lease to advance exactly once.  The running process
-- still has no in-memory lease; acquisition remains a separate restart step.

ALTER TABLE restore_state ADD COLUMN coordinator_promoted_at TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN coordinator_promoted_by TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN coordinator_promoted_holder TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN coordinator_promoted_epoch INTEGER NOT NULL DEFAULT 0;

CREATE TABLE recovery_coordinator_promotions (
    restore_generation  TEXT PRIMARY KEY,
    journal_evidence_id TEXT NOT NULL,
    previous_holder     TEXT NOT NULL,
    previous_epoch      INTEGER NOT NULL,
    promoted_holder     TEXT NOT NULL,
    promoted_epoch      INTEGER NOT NULL,
    promoted_at         TEXT NOT NULL,
    promoted_by         TEXT NOT NULL,

    CHECK (restore_generation <> ''),
    CHECK (journal_evidence_id <> ''),
    CHECK (previous_holder <> ''),
    CHECK (previous_epoch > 0),
    CHECK (promoted_holder <> ''),
    CHECK (promoted_holder <> previous_holder),
    CHECK (promoted_epoch = previous_epoch + 1),
    CHECK (promoted_at <> ''),
    CHECK (promoted_by <> '')
);

CREATE TRIGGER recovery_coordinator_promotions_require_release
BEFORE INSERT ON recovery_coordinator_promotions
WHEN NOT EXISTS (
    SELECT 1
      FROM restore_state s
      JOIN recovery_journal_reconciliations r
        ON r.evidence_id = s.journal_evidence_id
       AND r.restore_generation = s.restore_generation
       AND r.snapshot_sha256 = s.snapshot_sha256
       AND r.snapshot_at = s.snapshot_at
       AND r.journal_source = s.journal_source
       AND r.head_seq = s.journal_head_seq
       AND r.head_sha256 = s.journal_head_sha256
       AND r.coverage_through = s.journal_coverage_through
       AND r.verified_at = s.journal_verified_at
       AND r.verified_by = s.journal_verified_by
       AND r.applied_count = s.replayed_count
      JOIN coordinator_lease l ON l.id = 1
     WHERE s.id = 1
       AND s.restore_generation = NEW.restore_generation
       AND s.journal_evidence_id = NEW.journal_evidence_id
       AND s.quarantined = 0
       AND s.released_at <> ''
       AND s.released_by <> ''
       AND s.journal_current = 1
       AND s.replayed_at <> ''
       AND r.source_fenced_at <> ''
       AND r.signer_fingerprint <> ''
       AND l.holder = NEW.previous_holder
       AND l.epoch = NEW.previous_epoch
)
BEGIN
    SELECT RAISE(ABORT, 'restored coordinator promotion requires current released recovery evidence and lease');
END;

CREATE TRIGGER recovery_coordinator_promotions_immutable
BEFORE UPDATE ON recovery_coordinator_promotions
BEGIN
    SELECT RAISE(ABORT, 'restored coordinator promotion is immutable');
END;

CREATE TRIGGER recovery_coordinator_promotions_not_deleted
BEFORE DELETE ON recovery_coordinator_promotions
BEGIN
    SELECT RAISE(ABORT, 'restored coordinator promotion is immutable');
END;

-- A restored database cannot use the ordinary lease promotion path.  The
-- restore-specific transaction inserts the immutable decision first, then
-- performs the exact matching epoch update before committing both together.
CREATE TRIGGER coordinator_lease_restored_promotion_authorized
BEFORE UPDATE OF holder, epoch ON coordinator_lease
WHEN EXISTS (SELECT 1 FROM restore_state WHERE id = 1 AND restore_generation <> '')
 AND NOT EXISTS (
    SELECT 1
      FROM restore_state s
      JOIN recovery_coordinator_promotions p
        ON p.restore_generation = s.restore_generation
       AND p.journal_evidence_id = s.journal_evidence_id
     WHERE s.id = 1
       AND s.quarantined = 0
       AND s.released_at <> ''
       AND s.released_by <> ''
       AND p.previous_holder = OLD.holder
       AND p.previous_epoch = OLD.epoch
       AND p.promoted_holder = NEW.holder
       AND p.promoted_epoch = NEW.epoch
)
BEGIN
    SELECT RAISE(ABORT, 'restored coordinator lease requires an audited Owner promotion');
END;

CREATE TRIGGER restore_state_coordinator_promotion_requires_audit
BEFORE UPDATE OF coordinator_promoted_at ON restore_state
WHEN NEW.coordinator_promoted_at <> '' AND NOT EXISTS (
    SELECT 1
      FROM recovery_coordinator_promotions p
      JOIN coordinator_lease l ON l.id = 1
     WHERE p.restore_generation = NEW.restore_generation
       AND p.journal_evidence_id = NEW.journal_evidence_id
       AND p.promoted_at = NEW.coordinator_promoted_at
       AND p.promoted_by = NEW.coordinator_promoted_by
       AND p.promoted_holder = NEW.coordinator_promoted_holder
       AND p.promoted_epoch = NEW.coordinator_promoted_epoch
       AND l.holder = p.promoted_holder
       AND l.epoch = p.promoted_epoch
)
BEGIN
    SELECT RAISE(ABORT, 'restore promotion projection requires matching immutable audit and lease');
END;
