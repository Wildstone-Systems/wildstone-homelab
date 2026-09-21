-- A reviewed Plan is the exact unattended principal. Resource requests are
-- part of its immutable snapshot; approval materializes only that version's
-- grants. Existing routines acquire no unattended resource grants.
ALTER TABLE household_routines ADD COLUMN resource_signature TEXT NOT NULL DEFAULT '';
ALTER TABLE routine_plan_versions ADD COLUMN secret_ids TEXT NOT NULL DEFAULT '[]';
ALTER TABLE routine_plan_versions ADD COLUMN memory_ids TEXT NOT NULL DEFAULT '[]';

CREATE TRIGGER routine_resources_require_version BEFORE UPDATE OF resource_signature ON household_routines
WHEN NEW.resource_signature<>OLD.resource_signature AND NEW.revision<=OLD.revision BEGIN
 SELECT RAISE(ABORT,'changed routine grants require a new version');
END;
CREATE TRIGGER routine_resource_snapshot_immutable BEFORE UPDATE OF secret_ids,memory_ids ON routine_plan_versions BEGIN
 SELECT RAISE(ABORT,'reviewed resource requests are immutable; submit a new Plan');
END;

CREATE TABLE household_secret_routine_grants (
 plan INTEGER NOT NULL REFERENCES routine_plan_versions(plan),
 secret_id TEXT NOT NULL REFERENCES household_secrets(id),
 verb TEXT NOT NULL DEFAULT 'use' CHECK(verb='use'),
 PRIMARY KEY(plan,secret_id,verb)
);
CREATE TABLE routine_memory_grants (
 plan INTEGER NOT NULL REFERENCES routine_plan_versions(plan),
 memory INTEGER NOT NULL REFERENCES butler_memory(seq),
 PRIMARY KEY(plan,memory)
);
CREATE TABLE household_secret_routine_audit (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 secret_id TEXT NOT NULL REFERENCES household_secrets(id),
 plan INTEGER NOT NULL REFERENCES routine_plan_versions(plan),
 action TEXT NOT NULL CHECK(action='use'),
 at TEXT NOT NULL
);
CREATE TRIGGER routine_secret_grant_matches_review BEFORE INSERT ON household_secret_routine_grants
WHEN NOT EXISTS (
 SELECT 1 FROM routine_plan_versions v JOIN plans p ON p.seq=v.plan
 JOIN json_each(v.secret_ids) requested ON requested.value=NEW.secret_id
 WHERE v.plan=NEW.plan AND v.approved_by IS NOT NULL AND p.state='proposed'
) BEGIN SELECT RAISE(ABORT,'routine secret grant was not in the reviewed Plan'); END;
CREATE TRIGGER routine_memory_grant_matches_review BEFORE INSERT ON routine_memory_grants
WHEN NOT EXISTS (
 SELECT 1 FROM routine_plan_versions v JOIN plans p ON p.seq=v.plan
 JOIN json_each(v.memory_ids) requested ON requested.value=NEW.memory
 WHERE v.plan=NEW.plan AND v.approved_by IS NOT NULL AND p.state='proposed'
) BEGIN SELECT RAISE(ABORT,'routine memory grant was not in the reviewed Plan'); END;
