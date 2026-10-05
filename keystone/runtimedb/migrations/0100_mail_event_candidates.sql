-- 0100: private mail event candidates (epic #326 S05, #458; ADR-W030
-- A5.6).
--
-- A mailbox monitor turns one Person's new mail into possible calendar
-- events. What it keeps is deliberately small, because the message it
-- read is not Wildstone's to copy (epic #326: mail stays at the
-- provider; no full body in the runtime DB, memory, index, trace, API,
-- log or backup):
--
--   * per-connection processing state keyed by the adapter's keyed
--     fingerprint, so at-least-once delivery (a replayed batch, a
--     UIDVALIDITY reset) never asks twice;
--   * versioned candidates holding only minimal event fields, their
--     confidence and explicit missing/ambiguous fields, the opaque
--     provider reference, and the target calendar;
--   * a short excerpt, AES-GCM sealed under a Keystone key that is NOT
--     part of any backup or recovery set, kept only while the candidate
--     is unresolved and never readable after a restore;
--   * a minimal audit of lifecycle changes, with no content at all.
--
-- Every row names the canonical Person who owns the mailbox. Reads are
-- scoped to that Person in runtimedb; no Owner, Agent or other Person
-- path exists.

CREATE TABLE mail_monitor_messages (
    connection    INTEGER NOT NULL REFERENCES integration_connections(seq) ON DELETE CASCADE,
    -- fingerprint is the adapter's keyed, per-connection identity
    -- ("mfp1_…"): stable across reconnects, duplicate deliveries and
    -- UIDVALIDITY resets, meaningless to any other connection.
    fingerprint   TEXT NOT NULL CHECK (fingerprint LIKE 'mfp1\_%' ESCAPE '\' AND length(fingerprint) <= 128),
    person_id     TEXT NOT NULL REFERENCES butler_members(person_id),
    -- state: candidate (produced or updated a candidate), duplicate_event
    -- (an event already on record), cancelled (an iTIP CANCEL applied),
    -- no_event, skipped (with reason), extraction_failed, or retrying (a
    -- transient model failure; attempts bounds it).
    state         TEXT NOT NULL CHECK (state IN ('candidate', 'duplicate_event', 'cancelled', 'no_event',
                      'skipped', 'extraction_failed', 'retrying')),
    -- reason is a fixed category (no_local_model, oversized, past_event,
    -- ics_unusable, …), never message text.
    reason        TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 64),
    -- source is how the message was read: ics, model or '' (neither).
    source        TEXT NOT NULL DEFAULT '' CHECK (source IN ('', 'ics', 'model')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    run           INTEGER NOT NULL DEFAULT 0,
    first_seen_at TEXT NOT NULL,
    processed_at  TEXT NOT NULL,
    PRIMARY KEY (connection, fingerprint)
);

CREATE INDEX mail_monitor_messages_by_age ON mail_monitor_messages (processed_at);

CREATE TABLE mail_candidates (
    id                TEXT PRIMARY KEY CHECK (id LIKE 'mc\_%' ESCAPE '\' AND length(id) <= 40),
    person_id         TEXT NOT NULL REFERENCES butler_members(person_id),
    connection        INTEGER NOT NULL REFERENCES integration_connections(seq) ON DELETE CASCADE,
    -- The source message: its opaque, connection-bound provider
    -- reference, its fingerprint and folder. Nothing else about it.
    source_ref        TEXT NOT NULL CHECK (length(source_ref) <= 512),
    fingerprint       TEXT NOT NULL CHECK (length(fingerprint) <= 128),
    source            TEXT NOT NULL CHECK (source IN ('ics', 'model')),
    -- Correlation keys, each a sha256 hex digest scoped to the
    -- connection: the invitation UID, the message's own Message-ID, its
    -- sender address and its normalized subject. They let a correction
    -- or an iTIP CANCEL find the candidate it corrects without the
    -- database holding the header values themselves.
    uid_key           TEXT NOT NULL DEFAULT '',
    ics_sequence      INTEGER NOT NULL DEFAULT 0,
    message_key       TEXT NOT NULL DEFAULT '',
    sender_key        TEXT NOT NULL DEFAULT '',
    subject_key       TEXT NOT NULL DEFAULT '',
    -- event_key digests the event itself (title, times, all-day) so a
    -- reminder or resend of an event already asked about — including a
    -- dismissed one, whose fields are blanked — is not asked again.
    event_key         TEXT NOT NULL DEFAULT '',
    target_calendar   TEXT NOT NULL CHECK (length(target_calendar) <= 255),
    -- revision is the current field revision; each Edit or correction
    -- is a new row in mail_candidate_revisions. A review is bound to
    -- (id, revision).
    revision          INTEGER NOT NULL DEFAULT 1,
    state             TEXT NOT NULL CHECK (state IN ('proposed', 'needs_details', 'accepted', 'created',
                          'dismissed', 'superseded', 'expired', 'cancelled')),
    state_reason      TEXT NOT NULL DEFAULT '' CHECK (length(state_reason) <= 200),
    -- supersedes / superseded_by link a correction to what it corrects.
    supersedes        TEXT NOT NULL DEFAULT '',
    superseded_by     TEXT NOT NULL DEFAULT '',
    -- update_pending: '' | update | cancel. Set on a candidate that
    -- already created (or was accepted to create) an event when a later
    -- correction or cancellation arrives; S06 proposes the exact
    -- calendar change. Nothing edits the event silently.
    update_pending    TEXT NOT NULL DEFAULT '' CHECK (update_pending IN ('', 'update', 'cancel')),
    -- announced_at: when the candidate-ready hook accepted it. '' means
    -- the private review has not been handed off yet.
    announced_at      TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    -- expires_at: an unresolved candidate expires at its start or after
    -- the unresolved window, whichever is earlier.
    expires_at        TEXT NOT NULL,
    resolved_at       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX mail_candidates_by_person ON mail_candidates (person_id, state, created_at);
CREATE INDEX mail_candidates_by_connection ON mail_candidates (connection, state);
CREATE INDEX mail_candidates_by_uid ON mail_candidates (connection, uid_key) WHERE uid_key <> '';
CREATE INDEX mail_candidates_by_event ON mail_candidates (connection, event_key) WHERE event_key <> '';

CREATE TABLE mail_candidate_revisions (
    candidate       TEXT NOT NULL REFERENCES mail_candidates(id) ON DELETE CASCADE,
    revision        INTEGER NOT NULL,
    -- The minimal event: bounded, sanitized, calendar-safe.
    title           TEXT NOT NULL CHECK (length(title) <= 200),
    -- start_at / end_at are RFC3339 instants, '' when unknown.
    -- duration_minutes is set when only a duration is known.
    start_at        TEXT NOT NULL DEFAULT '',
    end_at          TEXT NOT NULL DEFAULT '',
    duration_minutes INTEGER NOT NULL DEFAULT 0 CHECK (duration_minutes BETWEEN 0 AND 20160),
    all_day         INTEGER NOT NULL DEFAULT 0 CHECK (all_day IN (0, 1)),
    tz              TEXT NOT NULL DEFAULT '' CHECK (length(tz) <= 64),
    location        TEXT NOT NULL DEFAULT '' CHECK (length(location) <= 200),
    notes           TEXT NOT NULL DEFAULT '' CHECK (length(notes) <= 500),
    confidence      REAL NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
    -- JSON arrays of a closed field vocabulary.
    missing_json    TEXT NOT NULL DEFAULT '[]',
    ambiguous_json  TEXT NOT NULL DEFAULT '[]',
    -- author: 'monitor' for extraction, otherwise the Person's handle.
    author          TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    PRIMARY KEY (candidate, revision)
);

-- The temporary excerpt. One per candidate, sealed (nonce ‖ AES-GCM
-- ciphertext, AAD binds candidate and Person) under the excerpt key,
-- which lives beside the database but is never in a backup or recovery
-- set. Deleted on resolution, expiry, revocation and offboarding, and
-- wholesale on restore.
CREATE TABLE mail_candidate_excerpts (
    candidate   TEXT PRIMARY KEY REFERENCES mail_candidates(id) ON DELETE CASCADE,
    sealed      BLOB NOT NULL CHECK (length(sealed) <= 2048),
    expires_at  TEXT NOT NULL
);

-- Minimal audit: who moved which candidate revision from what to what.
-- No field values, no excerpt.
CREATE TABLE mail_candidate_audit (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    candidate   TEXT NOT NULL,
    revision    INTEGER NOT NULL,
    action      TEXT NOT NULL,
    from_state  TEXT NOT NULL DEFAULT '',
    to_state    TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 200),
    at          TEXT NOT NULL
);

CREATE INDEX mail_candidate_audit_by_candidate ON mail_candidate_audit (candidate, seq);
