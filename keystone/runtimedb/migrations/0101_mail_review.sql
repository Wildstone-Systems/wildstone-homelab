-- 0101: exact private review and at-most-once calendar effects for mail
-- event candidates (epic #326 S06, #459; ADR-W030 A5.7).
--
-- A candidate (0100) is only a question. This migration keeps what it
-- takes to ask it exactly once and to act on the answer at most once:
--
--   * mail_reviews: one offer of one candidate revision to its Person —
--     bound to that Person, the candidate id and revision, the target
--     calendar, a digest of the exact event fields, the live
--     authorization and subscription version it was issued under, the
--     route it was delivered on, and an expiry (72 hours or the event's
--     start, whichever is earlier). At most one is open per candidate;
--     using, revoking or expiring it is final.
--   * mail_review_tokens: the review's typed action tokens, stored only
--     as sha256 digests. The token values are derived from a key kept
--     outside the database and every backup (the mail excerpt key), so
--     a database copy holds nothing anyone can present.
--   * mail_candidate_effects: the calendar effect a used review
--     authorized, recorded BEFORE the write is sent and settled after,
--     so a timeout, a lost response or a crash is reconciled against
--     the calendar (by the deterministic event UID) rather than retried
--     blind.

CREATE TABLE mail_reviews (
    id                    TEXT PRIMARY KEY CHECK (id LIKE 'mr\_%' ESCAPE '\' AND length(id) <= 40),
    candidate             TEXT NOT NULL REFERENCES mail_candidates(id) ON DELETE CASCADE,
    person_id             TEXT NOT NULL REFERENCES butler_members(person_id),
    revision              INTEGER NOT NULL,
    -- kind: create (Add a new event), update (apply a correction to the
    -- event an earlier candidate created) or cancel (remove it).
    kind                  TEXT NOT NULL CHECK (kind IN ('create', 'update', 'cancel')),
    target_calendar       TEXT NOT NULL CHECK (length(target_calendar) <= 255),
    -- The existing event an update or cancel acts on: its deterministic
    -- UID. '' for create, whose UID is derived from the candidate.
    event_uid             TEXT NOT NULL DEFAULT '' CHECK (length(event_uid) <= 128),
    fields_digest         TEXT NOT NULL CHECK (length(fields_digest) = 64),
    authorization_digest  TEXT NOT NULL,
    subscription_version  INTEGER NOT NULL,
    -- route: how the review reached the Person — 'chat:<binding id>' for
    -- their one-to-one Connection, or 'notice' for the in-app notice
    -- alone. A chat reply must arrive on the same binding.
    route                 TEXT NOT NULL CHECK (length(route) <= 64),
    -- availability at issue: free, busy (conflicts named below),
    -- unavailable (the calendar did not answer) or not_checked.
    availability          TEXT NOT NULL CHECK (availability IN ('free', 'busy', 'unavailable', 'not_checked')),
    -- The conflicts this Person may see: calendar id, title and times,
    -- read from calendars they can read now. Bounded; no other content.
    conflicts_json        TEXT NOT NULL DEFAULT '[]' CHECK (length(conflicts_json) <= 8192),
    checked_calendars     TEXT NOT NULL DEFAULT '[]' CHECK (length(checked_calendars) <= 4096),
    notice                INTEGER NOT NULL DEFAULT 0,
    issued_at             TEXT NOT NULL,
    expires_at            TEXT NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('open', 'used', 'revoked', 'expired')),
    state_reason          TEXT NOT NULL DEFAULT '' CHECK (length(state_reason) <= 200),
    used_action           TEXT NOT NULL DEFAULT '',
    used_by               TEXT NOT NULL DEFAULT '',
    used_via              TEXT NOT NULL DEFAULT '',
    closed_at             TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX mail_reviews_one_open ON mail_reviews (candidate) WHERE state = 'open';
CREATE INDEX mail_reviews_by_person ON mail_reviews (person_id, state);

-- A closed review stays closed: nothing reopens a used, revoked or
-- expired offer, so a replay can never find it open again.
CREATE TRIGGER mail_reviews_closed_final
BEFORE UPDATE OF state ON mail_reviews
WHEN OLD.state <> 'open'
BEGIN
    SELECT RAISE(ABORT, 'a closed mail review cannot be reopened');
END;

CREATE TABLE mail_review_tokens (
    -- sha256 hex of the token. The token itself is never stored.
    token_hash  TEXT PRIMARY KEY CHECK (length(token_hash) = 64),
    review      TEXT NOT NULL REFERENCES mail_reviews(id) ON DELETE CASCADE,
    action      TEXT NOT NULL CHECK (action IN ('add', 'edit', 'dismiss', 'apply')),
    UNIQUE (review, action)
);

CREATE TABLE mail_candidate_effects (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    candidate     TEXT NOT NULL REFERENCES mail_candidates(id) ON DELETE CASCADE,
    revision      INTEGER NOT NULL,
    review        TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('create', 'update', 'cancel')),
    -- dispatching: recorded, the write may or may not have reached the
    -- calendar. confirmed: the calendar answered. reconciled: an
    -- indeterminate write was found (or found absent, for a cancel) by
    -- looking the UID up. returned: found absent and handed back to
    -- review. failed: refused by the calendar; nothing changed.
    state         TEXT NOT NULL CHECK (state IN ('dispatching', 'confirmed', 'reconciled', 'returned', 'failed')),
    calendar      TEXT NOT NULL,
    uid           TEXT NOT NULL CHECK (length(uid) <= 128),
    href          TEXT NOT NULL DEFAULT '' CHECK (length(href) <= 1024),
    etag          TEXT NOT NULL DEFAULT '' CHECK (length(etag) <= 256),
    fields_digest TEXT NOT NULL,
    actor         TEXT NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,
    started_at    TEXT NOT NULL,
    finished_at   TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '' CHECK (length(detail) <= 200)
);

-- One effect in flight per candidate: a duplicate approval that raced
-- past the review claim still cannot start a second write.
CREATE UNIQUE INDEX mail_candidate_effects_one_inflight ON mail_candidate_effects (candidate) WHERE state = 'dispatching';
CREATE INDEX mail_candidate_effects_by_candidate ON mail_candidate_effects (candidate, seq);
