-- Existing unclassified work is effectful. Historical completed rows remain
-- history; new completions must meet the stronger evidence contract.
ALTER TABLE butler_tasks ADD COLUMN objective_kind TEXT NOT NULL DEFAULT 'effectful'
 CHECK(objective_kind IN ('informational','effectful'));
ALTER TABLE butler_tasks ADD COLUMN proposal_only INTEGER NOT NULL DEFAULT 0 CHECK(proposal_only IN (0,1));
CREATE TABLE butler_effect_receipts (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 recorded_at TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('tool','scheduler','executor')),
 source_id TEXT NOT NULL CHECK(source_id<>''),
 step INTEGER REFERENCES butler_task_steps(seq) ON DELETE CASCADE,
 proposal_only INTEGER NOT NULL DEFAULT 0 CHECK(proposal_only IN (0,1)),
 outcome TEXT NOT NULL CHECK(outcome IN ('succeeded','failed','uncertain')),
 permission_label TEXT NOT NULL CHECK(permission_label<>''),
 audience TEXT NOT NULL REFERENCES audiences(audience_id),
 CHECK(kind<>'tool' OR step IS NOT NULL),
 UNIQUE(task,kind,source_id)
);
ALTER TABLE butler_task_evidence ADD COLUMN receipt INTEGER REFERENCES butler_effect_receipts(seq);
CREATE TRIGGER effect_receipt_tool_requires_verified_step BEFORE INSERT ON butler_effect_receipts
WHEN NEW.kind='tool' AND NEW.outcome='succeeded' AND NOT EXISTS (
 SELECT 1 FROM butler_task_steps s JOIN butler_task_attempts a ON a.step=s.seq
 WHERE s.seq=NEW.step AND s.task=NEW.task AND s.kind='tool' AND s.state='verified'
 AND a.state='succeeded' AND json_valid(a.result_json) AND ((NEW.proposal_only=0 AND json_extract(a.result_json,'$.effectConfirmed')=1) OR (NEW.proposal_only=1 AND json_extract(a.result_json,'$.proposalConfirmed')=1)) AND COALESCE(json_extract(a.result_json,'$.rejected'),0)=0)
BEGIN SELECT RAISE(ABORT,'tool receipt needs a verified successful step'); END;
CREATE TRIGGER effect_receipt_immutable BEFORE UPDATE ON butler_effect_receipts
BEGIN SELECT RAISE(ABORT,'effect receipts are immutable'); END;
CREATE TRIGGER evidence_receipt_matches_task BEFORE INSERT ON butler_task_evidence
WHEN NEW.receipt IS NOT NULL AND (NEW.kind NOT IN ('tool-result','external-verification') OR NOT EXISTS (
 SELECT 1 FROM butler_effect_receipts r WHERE r.seq=NEW.receipt AND r.task=NEW.task
 AND r.permission_label=NEW.permission_label AND r.audience=NEW.audience))
BEGIN SELECT RAISE(ABORT,'evidence receipt must match task and disclosure metadata'); END;
CREATE TABLE butler_required_effects (
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL,
 description TEXT NOT NULL CHECK(description<>''),
 source_key TEXT NOT NULL DEFAULT '',
 proposal_only INTEGER NOT NULL DEFAULT 0 CHECK(proposal_only IN (0,1)),
 PRIMARY KEY(task,ordinal)
);
CREATE TRIGGER effectful_completion_requires_receipt BEFORE UPDATE OF state,objective_kind,proposal_only ON butler_tasks
WHEN NEW.state='completed' AND NEW.objective_kind<>'informational' AND (
 NOT EXISTS (SELECT 1 FROM butler_required_effects WHERE task=NEW.seq) OR
 EXISTS (SELECT 1 FROM butler_required_effects required WHERE required.task=NEW.seq AND NOT EXISTS (
 SELECT 1 FROM butler_task_evidence e JOIN butler_effect_receipts r ON r.seq=e.receipt
 WHERE e.task=NEW.seq AND r.task=NEW.seq AND r.outcome='succeeded' AND r.proposal_only=required.proposal_only
 AND required.source_key=CASE WHEN r.step IS NOT NULL THEN 'step:'||r.step ELSE r.kind||':'||r.source_id END
 AND e.kind IN ('tool-result','external-verification'))))
BEGIN SELECT RAISE(ABORT,'every required effect needs a matching successful receipt'); END;
CREATE TRIGGER required_effects_no_completed_insert BEFORE INSERT ON butler_required_effects
WHEN (SELECT state FROM butler_tasks WHERE seq=NEW.task)='completed'
BEGIN SELECT RAISE(ABORT,'completed objective requirements are immutable'); END;
CREATE TRIGGER required_effects_no_completed_update BEFORE UPDATE ON butler_required_effects
WHEN (SELECT state FROM butler_tasks WHERE seq=OLD.task)='completed' OR (SELECT state FROM butler_tasks WHERE seq=NEW.task)='completed'
BEGIN SELECT RAISE(ABORT,'completed objective requirements are immutable'); END;
CREATE TRIGGER required_effects_no_completed_delete BEFORE DELETE ON butler_required_effects
WHEN (SELECT state FROM butler_tasks WHERE seq=OLD.task)='completed'
BEGIN SELECT RAISE(ABORT,'completed objective requirements are immutable'); END;

CREATE TRIGGER effect_receipt_step_matches_task BEFORE INSERT ON butler_effect_receipts
WHEN NEW.step IS NOT NULL AND NOT EXISTS (SELECT 1 FROM butler_task_steps WHERE seq=NEW.step AND task=NEW.task)
BEGIN SELECT RAISE(ABORT,'receipt operation must belong to its task'); END;

-- Enqueue is pending; only the adapter acknowledgement certifies delivery.
CREATE TABLE chat_outbound_effects (
 outbound INTEGER PRIMARY KEY REFERENCES chat_outbound(seq) ON DELETE CASCADE,
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 step INTEGER NOT NULL REFERENCES butler_task_steps(seq) ON DELETE CASCADE,
 permission_label TEXT NOT NULL CHECK(permission_label<>''),
 audience TEXT NOT NULL REFERENCES audiences(audience_id),
 expected_body TEXT NOT NULL
);
CREATE TRIGGER chat_effect_requires_own_operation BEFORE INSERT ON chat_outbound_effects
WHEN NOT EXISTS(SELECT 1 FROM butler_task_steps WHERE seq=NEW.step AND task=NEW.task AND kind='tool')
BEGIN SELECT RAISE(ABORT,'chat effect must belong to its originating operation'); END;
CREATE TRIGGER chat_delivery_receipt AFTER UPDATE OF delivered_at ON chat_outbound
WHEN OLD.delivered_at='' AND NEW.delivered_at<>'' BEGIN
 INSERT OR IGNORE INTO butler_effect_receipts(task,recorded_at,kind,source_id,step,outcome,permission_label,audience,proposal_only)
 SELECT task,NEW.delivered_at,'executor','chat-outbound:'||NEW.seq,step,'succeeded',permission_label,audience,0
 FROM chat_outbound_effects WHERE outbound=NEW.seq AND expected_body=NEW.body;
END;
