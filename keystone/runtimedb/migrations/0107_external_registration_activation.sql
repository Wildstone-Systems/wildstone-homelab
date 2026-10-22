-- +wildstone: foreign_keys=off
-- ADR-W049 / #603: atomically activate one Person-bound external profile.
-- The parent table is rebuilt because 0105's state CHECK intentionally knew
-- only pending_consent and revoked. No existing row is inferred active.

-- Keep child REFERENCES clauses naming the final table while the parent is
-- temporarily renamed. Foreign-key enforcement is already off on the
-- migration runner's dedicated rebuild handle.
PRAGMA legacy_alter_table=ON;

DROP TRIGGER external_client_registration_identity_immutable;
DROP TRIGGER external_client_registration_terminal_state;
DROP INDEX external_client_registrations_person;
DROP INDEX external_client_registrations_butler;
DROP INDEX external_client_registrations_client;
DROP INDEX external_client_registrations_id_person;

ALTER TABLE external_client_registrations RENAME TO external_client_registrations_0106;

CREATE TABLE external_client_registrations (
    id                TEXT PRIMARY KEY,
    person_id         TEXT NOT NULL REFERENCES butler_members(person_id),
    butler             INTEGER NOT NULL REFERENCES butlers(seq),
    connection         INTEGER NOT NULL REFERENCES butler_addresses(seq),
    issuer              TEXT NOT NULL,
    client_id           TEXT NOT NULL,
    runtime_principal   TEXT NOT NULL REFERENCES runtime_principals(principal),
    subject             TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL DEFAULT 'pending_consent'
                        CHECK (state IN ('pending_consent', 'active', 'revoked')),
    configured_at       TEXT NOT NULL,
    configured_by       TEXT NOT NULL,
    activated_at        TEXT NOT NULL DEFAULT '',
    activated_by        TEXT NOT NULL DEFAULT '',
    credential_epoch    INTEGER NOT NULL DEFAULT 0 CHECK (credential_epoch >= 0),
    revoked_at          TEXT NOT NULL DEFAULT '',
    revoked_by          TEXT NOT NULL DEFAULT '',

    CHECK (id <> ''),
    CHECK (person_id <> ''),
    CHECK (issuer <> ''),
    CHECK (client_id <> ''),
    CHECK (runtime_principal <> ''),
    CHECK (configured_by <> ''),
    CHECK (
        (state = 'pending_consent' AND subject = '' AND activated_at = '' AND
         activated_by = '' AND credential_epoch = 0 AND revoked_at = '' AND revoked_by = '') OR
        (state = 'active' AND subject <> '' AND activated_at <> '' AND
         activated_by <> '' AND credential_epoch > 0 AND revoked_at = '' AND revoked_by = '') OR
        (state = 'revoked' AND revoked_at <> '' AND revoked_by <> '')
    )
);

INSERT INTO external_client_registrations
    (id, person_id, butler, connection, issuer, client_id, runtime_principal,
     state, configured_at, configured_by, revoked_at, revoked_by)
SELECT id, person_id, butler, connection, issuer, client_id, runtime_principal,
       state, configured_at, configured_by, revoked_at, revoked_by
  FROM external_client_registrations_0106;

DROP TABLE external_client_registrations_0106;

PRAGMA legacy_alter_table=OFF;

CREATE INDEX external_client_registrations_person
    ON external_client_registrations(person_id, state);
CREATE INDEX external_client_registrations_butler
    ON external_client_registrations(butler, state);
CREATE INDEX external_client_registrations_client
    ON external_client_registrations(issuer, client_id, state);
CREATE UNIQUE INDEX external_client_registrations_id_person
    ON external_client_registrations(id, person_id);
CREATE UNIQUE INDEX external_client_registrations_active_person
    ON external_client_registrations(person_id) WHERE state = 'active';
CREATE UNIQUE INDEX external_client_registrations_active_butler
    ON external_client_registrations(butler) WHERE state = 'active';
CREATE UNIQUE INDEX external_client_registrations_active_subject
    ON external_client_registrations(issuer, subject, client_id) WHERE state = 'active';

CREATE TRIGGER external_client_registration_identity_immutable
BEFORE UPDATE ON external_client_registrations
WHEN NEW.id <> OLD.id OR NEW.person_id <> OLD.person_id OR NEW.butler <> OLD.butler OR
     NEW.connection <> OLD.connection OR NEW.issuer <> OLD.issuer OR
     NEW.client_id <> OLD.client_id OR NEW.runtime_principal <> OLD.runtime_principal OR
     NEW.configured_at <> OLD.configured_at OR NEW.configured_by <> OLD.configured_by OR
     (OLD.subject <> '' AND NEW.subject <> OLD.subject)
BEGIN
    SELECT RAISE(ABORT, 'external registration identity is immutable; revoke it and create a successor');
END;

CREATE TRIGGER external_client_registration_terminal_state
BEFORE UPDATE OF subject, state, activated_at, activated_by, credential_epoch, revoked_at, revoked_by
ON external_client_registrations
WHEN NOT (
    (NEW.subject = OLD.subject AND NEW.state = OLD.state AND
     NEW.activated_at = OLD.activated_at AND NEW.activated_by = OLD.activated_by AND
     NEW.credential_epoch = OLD.credential_epoch AND
     NEW.revoked_at = OLD.revoked_at AND NEW.revoked_by = OLD.revoked_by) OR
    (OLD.state = 'pending_consent' AND OLD.subject = '' AND
     NEW.state = 'active' AND NEW.subject <> '' AND NEW.activated_at <> '' AND
     NEW.activated_by <> '' AND NEW.credential_epoch = OLD.credential_epoch + 1 AND
     NEW.revoked_at = '' AND NEW.revoked_by = '') OR
    ((OLD.state = 'pending_consent' OR OLD.state = 'active') AND
     NEW.state = 'revoked' AND NEW.subject = OLD.subject AND
     NEW.activated_at = OLD.activated_at AND NEW.activated_by = OLD.activated_by AND
     NEW.credential_epoch = OLD.credential_epoch AND
     NEW.revoked_at <> '' AND NEW.revoked_by <> '')
)
BEGIN
    SELECT RAISE(ABORT, 'external registration state transition is terminal');
END;

-- A live external profile is dedicated to its bound Person. These guards live
-- in SQLite so every current or future caller, and concurrent activation,
-- shares one serialization boundary.
CREATE TRIGGER active_external_registration_blocks_access_insert
BEFORE INSERT ON butler_access
WHEN EXISTS (
    SELECT 1 FROM external_client_registrations r
     WHERE r.state = 'active' AND r.butler = NEW.butler AND r.person_id <> NEW.person_id
)
BEGIN
    SELECT RAISE(ABORT, 'active external registration admits only its bound Person');
END;

CREATE TRIGGER active_external_registration_blocks_access_restore
BEFORE UPDATE OF revoked_at ON butler_access
WHEN NEW.revoked_at = '' AND OLD.revoked_at <> '' AND EXISTS (
    SELECT 1 FROM external_client_registrations r
     WHERE r.state = 'active' AND r.butler = NEW.butler AND r.person_id <> NEW.person_id
)
BEGIN
    SELECT RAISE(ABORT, 'active external registration admits only its bound Person');
END;

CREATE TRIGGER active_external_registration_blocks_person_admission
BEFORE UPDATE OF admitted, disabled_at, offboarded_at, role ON butler_members
WHEN NEW.admitted = 1 AND NEW.disabled_at = '' AND NEW.offboarded_at = '' AND EXISTS (
    SELECT 1 FROM external_client_registrations r
     WHERE r.state = 'active' AND r.person_id <> NEW.person_id AND (
       EXISTS (SELECT 1 FROM butler_access ba
                WHERE ba.butler = r.butler AND ba.person_id = NEW.person_id AND ba.revoked_at = '') OR
       EXISTS (SELECT 1 FROM chat_bindings cb
                 JOIN butler_addresses a ON a.seq = cb.connection
                WHERE a.butler = r.butler AND a.revoked_at = '' AND
                      cb.person_id = NEW.person_id AND cb.revoked_at = '')
     )
)
BEGIN
    SELECT RAISE(ABORT, 'active external registration admits only its bound Person');
END;

CREATE TRIGGER active_external_registration_blocks_chat_binding_insert
BEFORE INSERT ON chat_bindings
WHEN EXISTS (
    SELECT 1 FROM external_client_registrations r
      JOIN butler_addresses a ON a.seq = NEW.connection
     WHERE r.state = 'active' AND r.butler = a.butler AND r.person_id <> NEW.person_id
)
BEGIN
    SELECT RAISE(ABORT, 'active external registration admits only its bound Person');
END;

CREATE TRIGGER active_external_registration_blocks_chat_binding_restore
BEFORE UPDATE OF revoked_at ON chat_bindings
WHEN NEW.revoked_at = '' AND OLD.revoked_at <> '' AND EXISTS (
    SELECT 1 FROM external_client_registrations r
      JOIN butler_addresses a ON a.seq = NEW.connection
     WHERE r.state = 'active' AND r.butler = a.butler AND r.person_id <> NEW.person_id
)
BEGIN
    SELECT RAISE(ABORT, 'active external registration admits only its bound Person');
END;

CREATE TRIGGER active_external_registration_blocks_admission_widening
BEFORE UPDATE ON connection_admission
WHEN EXISTS (
    SELECT 1 FROM external_client_registrations r
      JOIN butler_addresses a ON a.seq = NEW.connection AND a.butler = r.butler
      JOIN chat_bindings cb ON cb.connection = a.seq AND cb.revoked_at = '' AND cb.person_id <> r.person_id
      JOIN butler_members m ON m.person_id = cb.person_id AND m.admitted = 1 AND
                               m.disabled_at = '' AND m.offboarded_at = ''
     WHERE r.state = 'active'
)
BEGIN
    SELECT RAISE(ABORT, 'active external registration admits only its bound Person');
END;

CREATE TRIGGER active_external_registration_blocks_disclosure_widening
BEFORE UPDATE ON connection_disclosure
WHEN EXISTS (
    SELECT 1 FROM external_client_registrations r
      JOIN butler_addresses a ON a.seq = NEW.connection AND a.butler = r.butler
      JOIN chat_bindings cb ON cb.connection = a.seq AND cb.revoked_at = '' AND cb.person_id <> r.person_id
      JOIN butler_members m ON m.person_id = cb.person_id AND m.admitted = 1 AND
                               m.disabled_at = '' AND m.offboarded_at = ''
     WHERE r.state = 'active'
)
BEGIN
    SELECT RAISE(ABORT, 'active external registration admits only its bound Person');
END;
