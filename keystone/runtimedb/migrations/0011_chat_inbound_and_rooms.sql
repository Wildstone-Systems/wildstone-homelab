-- 0011: at-most-once inbound handling, and where to reach a member
-- (ADR-W026 A8).
--
-- chat_inbound is the claim an inbound message takes BEFORE a session
-- runs. Deduplicating the outbound reply was not enough: a transport
-- that redelivers a message — an adapter that lost the HTTP response,
-- a reconnect — would run the whole session again first, spending a
-- second provider call, writing a second session record, and (on the
-- family surface) possibly performing a domestic action twice. The
-- reply collapsed to one row; everything before it did not.
--
-- This is the same claim-before-dispatch shape the automation engine
-- uses (ADR-W024 H4): at-most-once, chosen deliberately over
-- at-least-once, because a duplicated action is worse than a missed
-- one. A session that FAILS releases its claim, so a retry after a
-- provider error still works.
CREATE TABLE chat_inbound (
    dedupe_key TEXT PRIMARY KEY,
    surface    TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    claimed_at TEXT NOT NULL,
    queued_seq INTEGER NOT NULL DEFAULT 0
);

-- Where to reach this account when nobody asked — a rule's question,
-- rather than a reply to something.
--
-- A binding's `account` is the transport's id for the PERSON, and on
-- Telegram a private chat's id happens to equal it, so replies worked
-- by coincidence. On Matrix it does not: the account is an MXID
-- (@sam:example.org) and a send needs a room (!abc:example.org), so a
-- rule-driven push addressed to the account would go nowhere. The
-- direct conversation is recorded here, from the redemption that
-- created the binding and refreshed by every direct message after it.
ALTER TABLE chat_bindings ADD COLUMN conversation TEXT NOT NULL DEFAULT '';
