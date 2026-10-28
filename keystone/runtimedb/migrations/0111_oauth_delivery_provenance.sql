-- ADR-W049 S08: OAuth-visible deliveries carry immutable authority
-- provenance. Existing rows deliberately remain static-profile rows; no
-- identity is guessed from payloads, labels, Agent names, or access lists.
ALTER TABLE agent_deliveries ADD COLUMN registration_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_deliveries ADD COLUMN person_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_deliveries ADD COLUMN connection INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_deliveries ADD COLUMN runtime_principal TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_deliveries ADD COLUMN credential_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_deliveries ADD COLUMN task INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_deliveries ADD COLUMN audience TEXT NOT NULL DEFAULT '';

CREATE INDEX agent_deliveries_oauth_poll
    ON agent_deliveries(registration_id, id, acked_at, expired_at);

CREATE TRIGGER agent_delivery_provenance_complete
BEFORE INSERT ON agent_deliveries
WHEN NOT (
    (NEW.registration_id = '' AND NEW.person_id = '' AND NEW.connection = 0 AND
     NEW.runtime_principal = '' AND NEW.credential_epoch = 0 AND NEW.task = 0 AND NEW.audience = '') OR
    (NEW.registration_id <> '' AND NEW.person_id <> '' AND NEW.connection > 0 AND
     NEW.runtime_principal <> '' AND NEW.credential_epoch > 0 AND NEW.task > 0 AND NEW.audience <> '')
)
BEGIN
    SELECT RAISE(ABORT, 'OAuth delivery provenance must be complete or absent');
END;

CREATE TRIGGER agent_delivery_provenance_immutable
BEFORE UPDATE OF registration_id, person_id, connection, runtime_principal,
                 credential_epoch, task, audience ON agent_deliveries
WHEN NEW.registration_id <> OLD.registration_id OR NEW.person_id <> OLD.person_id OR
     NEW.connection <> OLD.connection OR NEW.runtime_principal <> OLD.runtime_principal OR
     NEW.credential_epoch <> OLD.credential_epoch OR NEW.task <> OLD.task OR NEW.audience <> OLD.audience
BEGIN
    SELECT RAISE(ABORT, 'OAuth delivery provenance is immutable');
END;

-- A filed Plan does not carry a task in its historical schema. Record the
-- exact originating task only when the gateway can prove it; outcome delivery
-- later consumes this link rather than parsing opaque Plan or payload data.
CREATE TABLE external_plan_origins (
    plan INTEGER PRIMARY KEY REFERENCES plans(seq),
    task INTEGER NOT NULL REFERENCES butler_tasks(seq),
    UNIQUE(plan, task)
);

