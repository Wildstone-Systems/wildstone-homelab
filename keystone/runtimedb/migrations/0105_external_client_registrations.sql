-- ADR-W049 / #586: inert external-client configuration. These rows carry no
-- authentication or Agent authority; later migrations may add activation
-- state only with the accepted consent and profile-fencing contract.
CREATE TABLE external_client_registrations (
    id                TEXT PRIMARY KEY,
    person_id         TEXT NOT NULL REFERENCES butler_members(person_id),
    butler             INTEGER NOT NULL REFERENCES butlers(seq),
    connection         INTEGER NOT NULL REFERENCES butler_addresses(seq),
    issuer              TEXT NOT NULL,
    client_id           TEXT NOT NULL,
    runtime_principal   TEXT NOT NULL REFERENCES runtime_principals(principal),
    state               TEXT NOT NULL DEFAULT 'pending_consent'
                        CHECK (state IN ('pending_consent', 'revoked')),
    configured_at       TEXT NOT NULL,
    configured_by       TEXT NOT NULL,
    revoked_at          TEXT NOT NULL DEFAULT '',
    revoked_by          TEXT NOT NULL DEFAULT '',

    CHECK (id <> ''),
    CHECK (person_id <> ''),
    CHECK (issuer <> ''),
    CHECK (client_id <> ''),
    CHECK (runtime_principal <> ''),
    CHECK (configured_by <> ''),
    CHECK ((state = 'pending_consent' AND revoked_at = '' AND revoked_by = '') OR
           (state = 'revoked' AND revoked_at <> '' AND revoked_by <> ''))
);

CREATE INDEX external_client_registrations_person
    ON external_client_registrations(person_id, state);
CREATE INDEX external_client_registrations_butler
    ON external_client_registrations(butler, state);
CREATE INDEX external_client_registrations_client
    ON external_client_registrations(issuer, client_id, state);

CREATE TRIGGER external_client_registration_identity_immutable
BEFORE UPDATE ON external_client_registrations
WHEN NEW.id <> OLD.id OR NEW.person_id <> OLD.person_id OR NEW.butler <> OLD.butler OR
     NEW.connection <> OLD.connection OR NEW.issuer <> OLD.issuer OR
     NEW.client_id <> OLD.client_id OR NEW.runtime_principal <> OLD.runtime_principal OR
     NEW.configured_at <> OLD.configured_at OR NEW.configured_by <> OLD.configured_by
BEGIN
    SELECT RAISE(ABORT, 'external registration identity is immutable; revoke it and create a successor');
END;

CREATE TRIGGER external_client_registration_terminal_state
BEFORE UPDATE OF state, revoked_at, revoked_by ON external_client_registrations
WHEN NOT (
    (NEW.state = OLD.state AND NEW.revoked_at = OLD.revoked_at AND NEW.revoked_by = OLD.revoked_by) OR
    (OLD.state = 'pending_consent' AND OLD.revoked_at = '' AND OLD.revoked_by = '' AND
     NEW.state = 'revoked' AND NEW.revoked_at <> '' AND NEW.revoked_by <> '')
)
BEGIN
    SELECT RAISE(ABORT, 'external registration revocation is terminal');
END;
