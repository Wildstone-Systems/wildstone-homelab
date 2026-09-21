-- A Plan remains a unique immutable reviewed document. Every submission is
-- queued, regardless of which API or runtime path created it.
CREATE TABLE owner_review_queue (
 plan INTEGER PRIMARY KEY REFERENCES plans(seq),
 proposer_person_id TEXT REFERENCES butler_members(person_id),
 proposing_agent INTEGER REFERENCES butlers(seq),
 queued_at TEXT NOT NULL
);
INSERT INTO owner_review_queue(plan,proposer_person_id,queued_at)
SELECT p.seq,COALESCE((SELECT person_id FROM butler_members WHERE member=COALESCE(NULLIF(p.acting_for,''),p.principal) COLLATE BINARY), (SELECT CASE WHEN COUNT(*)=1 THEN MIN(person_id) END FROM butler_members WHERE member=COALESCE(NULLIF(p.acting_for,''),p.principal) COLLATE NOCASE)),p.created_at FROM plans p;
CREATE TRIGGER owner_queue_submission AFTER INSERT ON plans WHEN NEW.state='proposed' BEGIN
 INSERT INTO owner_review_queue(plan,proposer_person_id,queued_at)
 VALUES(NEW.seq,COALESCE((SELECT person_id FROM butler_members WHERE member=COALESCE(NULLIF(NEW.acting_for,''),NEW.principal) COLLATE BINARY), (SELECT CASE WHEN COUNT(*)=1 THEN MIN(person_id) END FROM butler_members WHERE member=COALESCE(NULLIF(NEW.acting_for,''),NEW.principal) COLLATE NOCASE)),NEW.created_at);
END;
CREATE TRIGGER reviewed_plan_immutable BEFORE UPDATE OF plan_json ON plans
WHEN NEW.plan_json<>OLD.plan_json BEGIN
 SELECT RAISE(ABORT,'editing a Plan requires a new submitted version');
END;
ALTER TABLE household_routines ADD COLUMN output_audience TEXT REFERENCES audiences(audience_id);
UPDATE household_routines SET output_audience='aud_household';
CREATE TRIGGER routine_default_output_audience AFTER INSERT ON household_routines WHEN NEW.output_audience IS NULL BEGIN
 UPDATE household_routines SET output_audience='aud_household' WHERE seq=NEW.seq;
END;
ALTER TABLE household_routines ADD COLUMN approved_revision INTEGER NOT NULL DEFAULT 0;
-- Preserve already-running household routines; future edits revoke this version.
UPDATE household_routines SET approved_revision=revision WHERE state IN ('enabled','trial');
CREATE TABLE routine_plan_versions (
 plan INTEGER PRIMARY KEY REFERENCES plans(seq),
 routine INTEGER NOT NULL REFERENCES household_routines(seq),
 revision INTEGER NOT NULL,
 document TEXT NOT NULL,
 output_audience TEXT NOT NULL REFERENCES audiences(audience_id),
 delivery TEXT NOT NULL,
 assigned_butler TEXT NOT NULL DEFAULT '',
 approved_by TEXT REFERENCES butler_members(person_id)
);
CREATE TRIGGER routine_edit_requires_review AFTER UPDATE OF revision ON household_routines
WHEN NEW.revision<>OLD.revision BEGIN
 UPDATE household_routines SET state='draft' WHERE seq=NEW.seq AND ownership='wildstone';
 UPDATE plans SET state='superseded' WHERE state='proposed' AND seq IN
 (SELECT plan FROM routine_plan_versions WHERE routine=NEW.seq AND revision<>NEW.revision);
END;
CREATE TRIGGER routine_activation_requires_review BEFORE UPDATE OF state ON household_routines
WHEN NEW.ownership='wildstone' AND NEW.state IN ('enabled','trial') AND NEW.approved_revision<>NEW.revision BEGIN
 SELECT RAISE(ABORT,'the exact routine version needs Owner approval');
END;
CREATE TRIGGER owner_review_revoke_pending AFTER UPDATE OF generation ON butler_members
WHEN NEW.generation<>OLD.generation BEGIN
 UPDATE plans SET state='superseded' WHERE state='proposed' AND seq IN
 (SELECT plan FROM owner_review_queue WHERE proposer_person_id=NEW.person_id);
 UPDATE household_review_tokens SET used_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),used_by='revoked'
 WHERE person_id=NEW.person_id AND used_at='';
END;

CREATE TRIGGER routine_creation_requires_review BEFORE INSERT ON household_routines
WHEN NEW.ownership='wildstone' AND NEW.state IN ('enabled','trial') BEGIN
 SELECT RAISE(ABORT,'create a draft and submit it for Owner review');
END;
CREATE TRIGGER routine_payload_requires_version BEFORE UPDATE OF document,output_audience,butler ON household_routines
WHEN (NEW.document<>OLD.document OR NEW.output_audience<>OLD.output_audience OR NEW.butler<>OLD.butler) AND NEW.revision<=OLD.revision BEGIN
 SELECT RAISE(ABORT,'changed routine behavior requires a new version');
END;
CREATE TRIGGER routine_review_snapshot_immutable BEFORE UPDATE OF routine,revision,document,output_audience,delivery,assigned_butler ON routine_plan_versions BEGIN
 SELECT RAISE(ABORT,'a reviewed version is immutable; submit a new Plan');
END;

CREATE TABLE owner_review_preference (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 binding_id INTEGER REFERENCES chat_bindings(id),
 console_url TEXT NOT NULL DEFAULT '',
 generation INTEGER NOT NULL DEFAULT 0
);
INSERT INTO owner_review_preference(singleton) VALUES(1);
INSERT INTO notice_kinds(kind,urgent,relevance,description,updated_at,updated_by)
 VALUES('owner.review',0,75,'A Plan is waiting for Owner review',strftime('%Y-%m-%dT%H:%M:%fZ','now'),'migration');

CREATE TRIGGER routine_plan_approval_requires_owner BEFORE UPDATE OF state ON plans
WHEN NEW.state='approved' AND EXISTS(SELECT 1 FROM routine_plan_versions WHERE plan=NEW.seq AND approved_by IS NULL) BEGIN
 SELECT RAISE(ABORT,'routine Plans require explicit Owner approval of the exact version');
END;

-- Resolution cancels held/offline notices; a stale queue snapshot cannot
-- republish a notice after the resolution transaction has committed.
CREATE TRIGGER owner_review_cancel_notice AFTER UPDATE OF state ON plans
WHEN NEW.state<>'proposed' BEGIN
 UPDATE chat_outbound SET expires_at='2000-01-01T00:00:00Z'
 WHERE delivered_at='' AND dedupe_key LIKE 'owner-review:plan-'||NEW.seq||':%';
 UPDATE notices SET undelivered_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE kind='owner.review' AND delivered_at='' AND dedupe_key LIKE 'owner-review:plan-'||NEW.seq||':%';
END;
CREATE TRIGGER owner_review_refuse_stale_notice AFTER INSERT ON chat_outbound
WHEN NEW.dedupe_key LIKE 'owner-review:%' AND NOT EXISTS (
 SELECT 1 FROM plans p,owner_review_preference pref WHERE pref.binding_id=NEW.binding_id AND NEW.correlation_id='owner-review-pref:'||pref.generation AND p.state='proposed' AND NEW.dedupe_key LIKE 'owner-review:plan-'||p.seq||':%'
) BEGIN
 UPDATE chat_outbound SET expires_at='2000-01-01T00:00:00Z' WHERE seq=NEW.seq;
END;

-- Cancel old destinations atomically, including workers holding stale preferences.
CREATE TRIGGER owner_review_preference_changed AFTER UPDATE OF generation ON owner_review_preference
WHEN NEW.generation<>OLD.generation BEGIN
 UPDATE chat_outbound SET expires_at='2000-01-01T00:00:00Z'
 WHERE delivered_at='' AND dedupe_key LIKE 'owner-review:%';
 UPDATE notices SET undelivered_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE kind='owner.review' AND delivered_at='';
END;

-- Revocation also retires the designated destination and any worker snapshot.
CREATE TRIGGER owner_review_recipient_revoked AFTER UPDATE OF generation,role ON butler_members
WHEN NEW.generation<>OLD.generation OR (NEW.role<>OLD.role AND NEW.role<>'owner') BEGIN
 UPDATE owner_review_preference SET binding_id=NULL,console_url='',generation=generation+1
 WHERE binding_id IN (SELECT id FROM chat_bindings WHERE person_id=NEW.person_id);
END;

CREATE TRIGGER owner_review_binding_revoked AFTER UPDATE OF revoked_at ON chat_bindings
WHEN NEW.revoked_at<>'' AND NEW.revoked_at<>OLD.revoked_at BEGIN
 UPDATE owner_review_preference SET binding_id=NULL,console_url='',generation=generation+1 WHERE binding_id=NEW.id;
END;
