ALTER TABLE household_routines ADD COLUMN revoked_at TEXT NOT NULL DEFAULT '';
ALTER TABLE household_routines ADD COLUMN revoked_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE household_routines ADD COLUMN deleted_at TEXT NOT NULL DEFAULT '';
ALTER TABLE household_routines ADD COLUMN paused_reason TEXT NOT NULL DEFAULT '';
CREATE TRIGGER routine_revocation_fences_activation BEFORE UPDATE OF state ON household_routines
WHEN NEW.ownership='wildstone' AND NEW.state IN ('enabled','trial')
 AND (NEW.revoked_at<>'' OR NEW.deleted_at<>'' OR NEW.paused_reason<>'')
BEGIN SELECT RAISE(ABORT,'routine requires Owner resolution or a new approved version'); END;
