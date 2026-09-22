-- A soft-deleted routine remains recoverable until an Owner explicitly
-- purges it. Purge leaves stable identifiers and effect receipts, but
-- removes every copy of the executable document and reviewed Plan body.
ALTER TABLE household_routines ADD COLUMN purged_at TEXT NOT NULL DEFAULT '';

DROP TRIGGER routine_payload_requires_version;
CREATE TRIGGER routine_payload_requires_version BEFORE UPDATE OF document,output_audience,butler ON household_routines
WHEN (NEW.document<>OLD.document OR NEW.output_audience<>OLD.output_audience OR NEW.butler<>OLD.butler)
 AND NEW.revision<=OLD.revision AND NEW.purged_at=''
BEGIN SELECT RAISE(ABORT,'changed routine behavior requires a new version'); END;

DROP TRIGGER routine_review_snapshot_immutable;
CREATE TRIGGER routine_review_snapshot_immutable BEFORE UPDATE OF routine,revision,document,output_audience,delivery,assigned_butler ON routine_plan_versions
WHEN (SELECT purged_at FROM household_routines WHERE seq=OLD.routine)=''
BEGIN SELECT RAISE(ABORT,'a reviewed version is immutable; submit a new Plan'); END;

DROP TRIGGER reviewed_plan_immutable;
CREATE TRIGGER reviewed_plan_immutable BEFORE UPDATE OF plan_json ON plans
WHEN NEW.plan_json<>OLD.plan_json AND NOT EXISTS (
 SELECT 1 FROM routine_plan_versions v JOIN household_routines r ON r.seq=v.routine
 WHERE v.plan=OLD.seq AND r.purged_at<>'')
BEGIN SELECT RAISE(ABORT,'editing a Plan requires a new submitted version'); END;

CREATE TRIGGER routine_purged_cannot_restore BEFORE UPDATE OF deleted_at ON household_routines
WHEN OLD.purged_at<>'' AND NEW.deleted_at=''
BEGIN SELECT RAISE(ABORT,'purged routine cannot be restored'); END;
CREATE TRIGGER routine_purged_payload_sealed BEFORE UPDATE OF document,revision ON household_routines
WHEN OLD.purged_at<>'' AND (NEW.document<>'[purged]' OR NEW.revision<>OLD.revision)
BEGIN SELECT RAISE(ABORT,'purged routine cannot be edited'); END;
CREATE TRIGGER routine_review_purged_payload_sealed BEFORE UPDATE OF document ON routine_plan_versions
WHEN (SELECT purged_at FROM household_routines WHERE seq=OLD.routine)<>'' AND NEW.document<>'[purged]'
BEGIN SELECT RAISE(ABORT,'purged reviewed version cannot be edited'); END;
CREATE TRIGGER routine_plan_purged_payload_sealed BEFORE UPDATE OF plan_json ON plans
WHEN EXISTS (SELECT 1 FROM routine_plan_versions v JOIN household_routines r ON r.seq=v.routine
 WHERE v.plan=OLD.seq AND r.purged_at<>'') AND NEW.plan_json<>'{}'
BEGIN SELECT RAISE(ABORT,'purged Plan cannot be edited'); END;

DROP TRIGGER routine_resources_require_version;
CREATE TRIGGER routine_resources_require_version BEFORE UPDATE OF resource_signature ON household_routines
WHEN NEW.resource_signature<>OLD.resource_signature AND NEW.revision<=OLD.revision AND NEW.purged_at=''
BEGIN SELECT RAISE(ABORT,'changed routine grants require a new version'); END;

DROP TRIGGER routine_resource_snapshot_immutable;
CREATE TRIGGER routine_resource_snapshot_immutable BEFORE UPDATE OF secret_ids,memory_ids ON routine_plan_versions
WHEN (SELECT purged_at FROM household_routines WHERE seq=OLD.routine)=''
BEGIN SELECT RAISE(ABORT,'reviewed resource requests are immutable; submit a new Plan'); END;
