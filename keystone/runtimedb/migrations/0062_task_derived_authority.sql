-- 0062: what a Task was admitted under, so it can be re-evaluated.
--
-- Design §10.1: "Task authority is derived, not granted. A Task
-- inherits the bounded authority of the request that created it, is
-- re-evaluated at every authorization boundary, and can never widen
-- beyond that originating authority. If the Person, session, Connection
-- or grant behind it later loses authority, the Task loses it too at
-- its next boundary."
--
-- NOTHING HERE IS A GRANT. There is no new capability row, no new
-- principal, and nothing to issue, hold or revoke. Both columns are
-- POINTERS BACK at the request that already existed — the same thing
-- 0017 did with `surface` and `profile`, whose own comment says they
-- are snapshotted "because authority is rechecked on resume against
-- what the task CLAIMED". This adds the two legs §10.1 names that 0017
-- had no way to record.
--
-- WHY A COLUMN AND NOT A LOOKUP. The Connection a task was admitted
-- under is not recoverable later: a conversation can move between
-- Connections (§8, B10 keys a conversation to Person + Agent, not to
-- transport), so asking "which Connection is this session on now"
-- answers a different question from "which one admitted this work".
-- Deriving it would silently re-admit a task over a Connection its
-- originating request never came through.
--
-- ROUTINES HAVE NEITHER, AND THAT IS CORRECT. An approved routine runs
-- unattended and is its own principal bounded to its exact approved
-- version (§10.2) — it does not borrow a Person's session or
-- Connection. Both columns stay '' for a routine run, and the empty
-- value means "this leg does not apply", never "this leg passed".

ALTER TABLE butler_tasks ADD COLUMN origin_connection TEXT NOT NULL DEFAULT '';

-- And the Connection ROW it was, not just the address it wore.
--
-- transport:external_id is an ADDRESS, and an address is reusable: a
-- butler_addresses row can be revoked and another inserted with the
-- same pair, even for a different Agent (butler_addresses_live is
-- unique only over the LIVE rows, which is what makes that legal).
-- Checking the address alone would let a task whose Connection was
-- withdrawn become authorized again the moment somebody re-added the
-- bot — silently, and possibly onto another Agent's Connection.
--
-- The seq is immutable and a new row always gets a new one, so a
-- revoked Connection stays revoked for the work that derived from it.
-- The address stays alongside it because that is what a refusal cites;
-- an id is not something to show a person.
--
-- 0 means the leg does not apply: a routine, a built-in surface (the
-- Console and the household dashboard are Connections a deployment HAS
-- rather than rows somebody added), or a task created before this
-- migration.
ALTER TABLE butler_tasks ADD COLUMN origin_connection_ref INTEGER NOT NULL DEFAULT 0;

-- Session revocation. butler_sessions has never had a lifecycle: a row
-- is written when a turn finishes and is never withdrawn, so "the
-- session behind this task lost authority" had nothing to read.
--
-- This is the REVOCATION half only. Re-keying sessions to
-- Person + Agent + session_id is slice 12 (#213), and deliberately not
-- attempted here: this adds the marker that an existing session was
-- withdrawn, which is what §10.1's "at its next boundary" needs, and
-- leaves what a session IS to the slice that owns it.
ALTER TABLE butler_sessions ADD COLUMN revoked_at TEXT NOT NULL DEFAULT '';
ALTER TABLE butler_sessions ADD COLUMN revoked_by TEXT NOT NULL DEFAULT '';
