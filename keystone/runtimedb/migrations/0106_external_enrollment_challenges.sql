-- ADR-W049 / #598: short-lived Person-bound enrollment proofs. The opaque
-- secret is returned only at issuance; this table retains its SHA-256 digest.
CREATE UNIQUE INDEX external_client_registrations_id_person
    ON external_client_registrations(id, person_id);

CREATE TABLE external_enrollment_challenges (
    id                TEXT PRIMARY KEY,
    registration_id   TEXT NOT NULL REFERENCES external_client_registrations(id),
    person_id          TEXT NOT NULL REFERENCES butler_members(person_id),
    secret_hash        TEXT NOT NULL UNIQUE CHECK (length(secret_hash) = 64),
    issued_at          TEXT NOT NULL,
    expires_at         TEXT NOT NULL,
    issued_by          TEXT NOT NULL,
    consumed_at        TEXT NOT NULL DEFAULT '',
    consumed_by        TEXT NOT NULL DEFAULT '',
    invalidated_at     TEXT NOT NULL DEFAULT '',
    invalidated_by     TEXT NOT NULL DEFAULT '',
    invalidation_reason TEXT NOT NULL DEFAULT '',

    FOREIGN KEY (registration_id, person_id)
        REFERENCES external_client_registrations(id, person_id),

    CHECK (id <> ''),
    CHECK (person_id <> ''),
    CHECK (issued_by <> ''),
    CHECK (
        (consumed_at = '' AND consumed_by = '' AND invalidated_at = '' AND
         invalidated_by = '' AND invalidation_reason = '') OR
        (consumed_at <> '' AND consumed_by <> '' AND invalidated_at = '' AND
         invalidated_by = '' AND invalidation_reason = '') OR
        (consumed_at = '' AND consumed_by = '' AND invalidated_at <> '' AND
         invalidated_by <> '' AND invalidation_reason <> '')
    )
);

CREATE INDEX external_enrollment_challenges_registration
    ON external_enrollment_challenges(registration_id, issued_at);
CREATE INDEX external_enrollment_challenges_person
    ON external_enrollment_challenges(person_id, issued_at);

CREATE TRIGGER external_enrollment_challenge_identity_immutable
BEFORE UPDATE ON external_enrollment_challenges
WHEN NEW.id <> OLD.id OR NEW.registration_id <> OLD.registration_id OR
     NEW.person_id <> OLD.person_id OR NEW.secret_hash <> OLD.secret_hash OR
     NEW.issued_at <> OLD.issued_at OR NEW.expires_at <> OLD.expires_at OR
     NEW.issued_by <> OLD.issued_by
BEGIN
    SELECT RAISE(ABORT, 'external enrollment challenge identity is immutable');
END;

CREATE TRIGGER external_enrollment_challenge_terminal_state
BEFORE UPDATE OF consumed_at, consumed_by, invalidated_at, invalidated_by, invalidation_reason
ON external_enrollment_challenges
WHEN NOT (
    (NEW.consumed_at = OLD.consumed_at AND NEW.consumed_by = OLD.consumed_by AND
     NEW.invalidated_at = OLD.invalidated_at AND NEW.invalidated_by = OLD.invalidated_by AND
     NEW.invalidation_reason = OLD.invalidation_reason) OR
    (OLD.consumed_at = '' AND OLD.consumed_by = '' AND OLD.invalidated_at = '' AND
     OLD.invalidated_by = '' AND OLD.invalidation_reason = '' AND
     ((NEW.consumed_at <> '' AND NEW.consumed_by <> '' AND NEW.invalidated_at = '' AND
       NEW.invalidated_by = '' AND NEW.invalidation_reason = '') OR
      (NEW.consumed_at = '' AND NEW.consumed_by = '' AND NEW.invalidated_at <> '' AND
       NEW.invalidated_by <> '' AND NEW.invalidation_reason <> '')))
)
BEGIN
    SELECT RAISE(ABORT, 'external enrollment challenge terminal state is immutable');
END;
