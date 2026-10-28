-- ADR-W049 S07: one exact OAuth registration/session/retry tuple admits one
-- ordinary task. The task handle is encrypted with the household secret key;
-- plaintext and OAuth bearers never enter SQLite.
CREATE TABLE external_task_admissions (
    registration_id   TEXT NOT NULL REFERENCES external_client_registrations(id),
    client_session_id TEXT NOT NULL,
    idempotency_key   TEXT NOT NULL,
    payload_digest    TEXT NOT NULL CHECK (length(payload_digest) = 64),
    task              INTEGER NOT NULL UNIQUE REFERENCES butler_tasks(seq),
    sealed_handle     BLOB NOT NULL CHECK (length(sealed_handle) > 0),
    created_at        TEXT NOT NULL,

    PRIMARY KEY (registration_id, client_session_id, idempotency_key),
    CHECK (client_session_id <> ''),
    CHECK (idempotency_key <> '')
);

CREATE INDEX external_task_admissions_registration
    ON external_task_admissions(registration_id, created_at);
