-- 0063: what a Connection ADMITS, as configuration rather than as a
-- property derived from the Agent that happens to answer on it.
--
-- Design §8.1 lists "admission policy" among the things every
-- Connection has of its own, and §8.3 says what it decides: who may
-- address an Agent here, with subjects of type Person or Group, and
-- "Owner-Agent Connections admit Owners only".
--
-- WHAT THIS REPLACES. Admission was a DERIVATION, not a record:
-- Connection.AdmitsOwnersOnly() in connection.go read `surface ==
-- 'admin'` off the Agent and answered from that. Two things are wrong
-- with deriving it, and only the second is about a bug.
--
--   1. It is not configurable. A deployment cannot say "this Telegram
--      bot admits Owners only" about a Member Agent, because the only
--      input is a column on `butlers`. §8.1 wants the Connection to
--      carry it, and slice 11 is where it starts to.
--
--   2. A DERIVATION FROM AN UNPARSED VALUE FAILS OPEN. `surface ==
--      'admin'` is false for 'admin ', for 'ADMIN', and for a value
--      that is simply not one of the two — and false there means
--      "household", which admits Members. The narrower answer is the
--      one an unreadable value should produce. A closed enum with its
--      own CHECK, read as a closed enum in Go, is how that stops being
--      a matter of whether the row happened to be well-formed.
--
-- NOTHING HERE WIDENS ANYTHING. Every backfilled row states exactly
-- what the derivation already answered, so admission on this
-- deployment is unchanged by the migration itself. What changes is
-- that it is now written down, can be narrowed by hand, and has a
-- defined answer — refusal — when it is missing or unreadable.

CREATE TABLE connection_admission (
    -- One policy per Connection. PRIMARY KEY rather than an indexed
    -- column: two admission rows for one Connection would be two
    -- answers to "who may address this Agent here", and a resolver
    -- picking between them is a permission decided by ORDER BY.
    connection   INTEGER PRIMARY KEY REFERENCES butler_addresses(seq),

    -- admits is the class ceiling: which Person role may address the
    -- Agent over this Connection.
    --
    -- 'owners'    — Owners and nobody else (§8.3).
    -- 'household' — any admitted Person, Owner or Member.
    --
    -- It is a CEILING and not a grant. Being of an admitted role does
    -- not by itself let somebody in: they still need a live binding on
    -- this Connection, which is what chat_bindings records. This says
    -- who MAY be admitted, never who IS.
    admits       TEXT NOT NULL CHECK (admits IN ('owners', 'household')),

    -- subject_kind is §8.3's seam, present so the schema supports what
    -- the design says it supports rather than being retrofitted when
    -- group conversations arrive.
    --
    -- MVP IS 'person' AND THE RESOLVER REFUSES ANYTHING ELSE. §8.3:
    -- "Group and multi-recipient destinations fail closed in MVP...
    -- Person admission authenticates the speaker, not everyone who
    -- will receive the reply." A 'group' row is therefore legal to
    -- write and denied at admission, which is the honest way to carry
    -- a seam: the column exists, and nothing is admitted through it
    -- until the audience rules that make it safe do.
    subject_kind TEXT NOT NULL DEFAULT 'person' CHECK (subject_kind IN ('person', 'group')),

    set_at       TEXT NOT NULL,
    set_by       TEXT NOT NULL,

    CHECK (set_by <> '')
);

-- Backfill: exactly what the derivation answered, for every address,
-- revoked ones included.
--
-- REVOKED ONES TOO, because a revoked address is still referenced.
-- butler_tasks.origin_connection_ref (0062) keeps the seq it was
-- written with, and the FK here is to butler_addresses(seq) rather
-- than to the live index. Backfilling only live rows would leave a
-- later revocation looking identical to a Connection that never had a
-- policy, and the two deserve different refusals: "that Connection was
-- withdrawn" is a fact about the household, "no policy is on record"
-- is a fact about the deployment.
INSERT INTO connection_admission (connection, admits, subject_kind, set_at, set_by)
SELECT a.seq,
       CASE b.agent_class WHEN 'owner' THEN 'owners' ELSE 'household' END,
       'person',
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
       'migration:0063'
  FROM butler_addresses a
  JOIN butlers b ON b.seq = a.butler;

-- A new Connection gets a policy at the moment it exists.
--
-- A TRIGGER RATHER THAN A LINE IN AddButlerAddress, because the
-- absent-row case is the one Go answers with a refusal, and a refusal
-- reachable by ordinary use is a bug report waiting to happen. Every
-- path that inserts an address -- the CLI, the Console, a test fixture
-- building a deployment by hand -- gets the row without knowing this
-- table exists. Go still fails closed on an absent row: the trigger
-- makes that branch unreachable in practice, and unreachable is not
-- the same as unnecessary.
--
-- It derives the default from the Agent class, which is the narrowest
-- correct default: an Owner Agent's new Connection admits Owners, and
-- a Member Agent's admits the household. Narrowing it further is then
-- an edit somebody makes, not a default they have to remember.
CREATE TRIGGER connection_admission_default
AFTER INSERT ON butler_addresses
BEGIN
    INSERT INTO connection_admission (connection, admits, subject_kind, set_at, set_by)
    VALUES (
        NEW.seq,
        CASE (SELECT b.agent_class FROM butlers b WHERE b.seq = NEW.butler)
            WHEN 'owner' THEN 'owners' ELSE 'household' END,
        'person',
        NEW.created_at,
        NEW.created_by
    );
END;

-- An Owner Agent's Connection admits Owners only (§8.3), and that is
-- not negotiable by configuration.
--
-- The Connection carries the policy, but it cannot carry a WIDER one
-- than the Agent behind it allows -- the same rule ADR-W031 A3 states
-- for surfaces ("a Butler narrows a surface and can never widen one"),
-- applied to the Connection. Without this, "admission is now
-- configurable" would be a way to reach an Owner Agent as a Member,
-- which is the escalation §8.3 names.
--
-- Go checks the same intersection independently. That is deliberate
-- duplication, for the reason portalMayReach gives about checking both
-- halves of a pair: a defence that is correct only because of an
-- invariant maintained elsewhere stops being a defence on the day that
-- invariant is relaxed.
CREATE TRIGGER connection_admission_owner_agent_insert
BEFORE INSERT ON connection_admission
WHEN NEW.admits <> 'owners'
 AND (SELECT b.agent_class FROM butler_addresses a JOIN butlers b ON b.seq = a.butler
       WHERE a.seq = NEW.connection) = 'owner'
BEGIN
    SELECT RAISE(ABORT, 'an owner agent''s connection admits owners only, so it cannot be set to admit the household (ADR-W033 B9, design 8.3)');
END;

CREATE TRIGGER connection_admission_owner_agent_update
BEFORE UPDATE OF admits ON connection_admission
WHEN NEW.admits <> 'owners'
 AND (SELECT b.agent_class FROM butler_addresses a JOIN butlers b ON b.seq = a.butler
       WHERE a.seq = NEW.connection) = 'owner'
BEGIN
    SELECT RAISE(ABORT, 'an owner agent''s connection admits owners only, so it cannot be set to admit the household (ADR-W033 B9, design 8.3)');
END;
