-- 0030: proactive communication policy and delivery state (ADR-W030
-- A10/A10.1, work package P15).
--
-- P4 sent one deduplicated stall notice to the owner's direct surface
-- and held it through the household's quiet hours. This is the rest of
-- it, and most of the schema exists to keep two pairs of things apart.
--
-- Delivered is not acknowledged. A message the adapter handed to
-- Telegram is delivered; a message a person actually saw is
-- acknowledged; and a message a person REPLIED to is a third thing
-- again. Collapsing them produces a Butler that treats "the notification
-- was sent" as "they know", which is how a household ends up blamed for
-- ignoring something they never saw.
--
-- Expired is not answered. A completed task whose notice expires stays
-- completed: the notice is marked undelivered and shown as such, and it
-- does not quietly become an unanswered approval that the task is
-- waiting on. Silence is never approval, and neither is a timeout.

-- member_notice_preferences is one person's override of the household
-- quiet hours. It is per person because households are not uniform: the
-- person on nights does not want the same window as the person who gets
-- up at six.
CREATE TABLE member_notice_preferences (
    member          TEXT PRIMARY KEY,
    -- Minutes from midnight in the member's own timezone. Equal values
    -- mean this person has no quiet hours of their own and follows the
    -- household's.
    quiet_start_min INTEGER NOT NULL DEFAULT 0,
    quiet_end_min   INTEGER NOT NULL DEFAULT 0,
    timezone        TEXT NOT NULL DEFAULT '',
    -- urgent_override lets a person say "wake me for anything marked
    -- urgent". Off by default: an urgent exception that every notice can
    -- claim is not an exception.
    urgent_override INTEGER NOT NULL DEFAULT 1,
    -- min_relevance drops notices below a threshold entirely. A person
    -- who only wants to hear about problems sets it high.
    min_relevance   INTEGER NOT NULL DEFAULT 0 CHECK (min_relevance BETWEEN 0 AND 100),
    updated_at      TEXT NOT NULL,

    CHECK (quiet_start_min BETWEEN 0 AND 1439),
    CHECK (quiet_end_min BETWEEN 0 AND 1439)
);

-- notice_kinds declares which kinds of notice may claim the urgent
-- exception. It is a closed, administrator-set list rather than a flag
-- on each message, because a sender that can mark its own message urgent
-- will, and then quiet hours mean nothing.
CREATE TABLE notice_kinds (
    kind        TEXT PRIMARY KEY,
    urgent      INTEGER NOT NULL DEFAULT 0,
    -- relevance is this kind's baseline importance, 0 to 100, checked
    -- against a person's threshold.
    relevance   INTEGER NOT NULL DEFAULT 50 CHECK (relevance BETWEEN 0 AND 100),
    description TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL,
    updated_by  TEXT NOT NULL DEFAULT ''
);

-- notices is the delivery record: one row per proactive message, with
-- the three states kept apart.
--
-- It sits beside chat_outbound rather than replacing it. The queue's job
-- is getting bytes to an adapter; this is the household-facing question
-- of whether somebody was told, and it outlives the queue row's TTL.
CREATE TABLE notices (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,
    -- recipient is the member. An unassigned notice routes to the
    -- configured administrator (A10) — and does so WITHOUT granting them
    -- access to whatever private thing prompted it, which is why the
    -- body is written for the recipient it ends up with.
    recipient  TEXT NOT NULL,
    routed_to_admin INTEGER NOT NULL DEFAULT 0,
    -- correlation_id ties the notice to the task or plan it is about.
    correlation_id TEXT NOT NULL DEFAULT '',
    -- outbound is the chat_outbound row that carried it, when one did.
    outbound   INTEGER,
    dedupe_key TEXT NOT NULL,
    urgent     INTEGER NOT NULL DEFAULT 0,
    relevance  INTEGER NOT NULL DEFAULT 50,
    created_at TEXT NOT NULL,
    hold_until TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,

    -- The three states, deliberately three columns.
    delivered_at    TEXT NOT NULL DEFAULT '',
    acknowledged_at TEXT NOT NULL DEFAULT '',
    -- undelivered_at is set when the notice expired without reaching
    -- anyone. It is visible in Console: a notice nobody can see was
    -- missed is a notice that was silently dropped.
    undelivered_at  TEXT NOT NULL DEFAULT '',

    -- asks marks a notice that poses a question. A notice that does not
    -- ask cannot be answered, so a reply to one authorizes nothing.
    asks       INTEGER NOT NULL DEFAULT 0,
    -- answered_at and answer record a human's reply, which is a
    -- different fact from acknowledgement.
    answered_at TEXT NOT NULL DEFAULT '',
    answer      TEXT NOT NULL DEFAULT '',
    answered_by TEXT NOT NULL DEFAULT '',

    CHECK (kind <> ''),
    CHECK (recipient <> ''),
    CHECK (dedupe_key <> ''),
    -- A notice that answers a question it never asked would be an
    -- authorization from nowhere.
    CHECK (answered_at = '' OR asks = 1)
);

CREATE UNIQUE INDEX notices_dedupe ON notices (dedupe_key);
CREATE INDEX notices_by_recipient ON notices (recipient, seq);
CREATE INDEX notices_pending ON notices (delivered_at, expires_at);

-- An expired notice stays expired. Re-arming one would let a stale
-- question be answered long after the thing it asked about was settled.
CREATE TRIGGER notices_expired_stays_expired
BEFORE UPDATE OF answered_at ON notices
WHEN OLD.undelivered_at <> '' AND NEW.answered_at <> ''
BEGIN
    SELECT RAISE(ABORT, 'an expired notice cannot be answered (ADR-W030 A10.1)');
END;

-- And an answered one is answered once. A second reply to the same
-- question is a new conversation, not a revised authorization.
CREATE TRIGGER notices_answered_once
BEFORE UPDATE OF answered_at ON notices
WHEN OLD.answered_at <> '' AND NEW.answered_at <> OLD.answered_at
BEGIN
    SELECT RAISE(ABORT, 'a notice is answered once (ADR-W030 A10)');
END;
