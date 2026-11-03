-- Crash-safe recovery-journal replay (#284 review correction).
--
-- A replay can itself fire deletion-journal triggers.  Those rows are not
-- part of the signed source head, so they carry the evidence id whose replay
-- produced them.  That durable provenance lets an interrupted reconciliation
-- resume without accepting an unrelated local extension.
ALTER TABLE deletion_journal ADD COLUMN recovery_evidence_id TEXT NOT NULL DEFAULT '';

-- A snapshot digest identifies the artifact; a generation identifies this
-- particular restore of it.  Audit rows copied into a later backup therefore
-- cannot become the verification projection for a new restore.
ALTER TABLE restore_state ADD COLUMN restore_generation TEXT NOT NULL DEFAULT '';
ALTER TABLE recovery_journal_reconciliations ADD COLUMN restore_generation TEXT NOT NULL DEFAULT '';
