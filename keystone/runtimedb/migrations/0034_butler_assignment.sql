-- 0034: assignment and addressing (ADR-W031 A5/A7, work packages
-- B3/B4/B5).
--
-- B1 made a Butler. B2 scoped what it can see. This is how work and
-- conversations reach one.
--
-- Three things, and they are one migration because they are one idea:
-- everything a Butler does is something that NAMED it beforehand.
-- Nothing is dispatched by inspection at run time, because inspecting
-- to decide means reading the material before the decision that was
-- supposed to protect it (A2).
--
--   A ROUTINE names its Butler. That is what "assign the sensitive
--   routine to the local Butler" means, and it is checked when the
--   routine is written rather than when it fires.
--
--   A TASK names its Butler, snapshotted at creation the way surface
--   and profile already are, so a task's model is answerable from the
--   task rather than from whatever the Butler is pointed at today.
--
--   A CONVERSATION reaches a Butler by ADDRESS, not by content. The
--   address is (transport, external id) — deliberately not "Telegram
--   bot" or "Telegram topic", because both are just addresses and a
--   deployment should be able to pick either without a schema change.
--   The next transport is then a new row rather than a new column.

-- A routine runs as one Butler. Empty means it needs no model at all —
-- a deterministic rule with no reasoning step, which is most of them.
ALTER TABLE household_routines ADD COLUMN butler TEXT NOT NULL DEFAULT '';

-- A task's Butler is snapshotted, like its surface and profile. What
-- answered a task is a fact about that task, not a lookup that changes
-- when somebody reassigns the Butler next week.
ALTER TABLE butler_tasks ADD COLUMN butler TEXT NOT NULL DEFAULT '';

CREATE INDEX household_routines_butler ON household_routines (butler);
CREATE INDEX butler_tasks_butler ON butler_tasks (butler, state);

-- butler_addresses is how a message finds its Butler.
--
-- A7 says each Butler is addressed directly and there is no dispatcher,
-- for A2's reason: a dispatcher must read the message to route it, so
-- the material reaches a model before the routing meant to protect it.
-- An address is therefore structural — it is known from where the
-- message arrived, before anything looks at what it says.
--
-- The external_id is whatever the transport uses to distinguish one
-- destination from another. For Telegram that is a bot id if the
-- deployment gives each Butler its own bot, or a forum topic id if it
-- gives them one bot and a topic each. This table does not care, and
-- that is the point: the choice is a deployment's, not a schema's.
CREATE TABLE butler_addresses (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    butler      INTEGER NOT NULL REFERENCES butlers(seq),
    transport   TEXT NOT NULL,
    external_id TEXT NOT NULL,
    -- label is for a person reading the list: "the gym bot",
    -- "#nutrition". The system never matches on it.
    label       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',

    CHECK (transport <> ''),
    CHECK (external_id <> ''),
    CHECK (created_by <> '')
);

-- One live address resolves to exactly one Butler. Two Butlers sharing
-- an address would mean the same incoming message could be answered by
-- either, which is a dispatcher with the decision hidden in a race.
CREATE UNIQUE INDEX butler_addresses_live
    ON butler_addresses (transport, external_id) WHERE revoked_at = '';
CREATE INDEX butler_addresses_butler ON butler_addresses (butler, revoked_at);

-- A retired Butler cannot be given a new address. Retiring it means
-- messages stop reaching it; handing it an address afterwards would
-- quietly undo that.
CREATE TRIGGER butler_addresses_live_butler
BEFORE INSERT ON butler_addresses
WHEN (SELECT retired_at FROM butlers WHERE seq = NEW.butler) <> ''
BEGIN
    SELECT RAISE(ABORT, 'that butler has been retired, so it cannot be given an address (ADR-W031 A7)');
END;

-- butler_pool_disclosures records what a promotion into the shared pool
-- exposed, and to which models.
--
-- A5 makes promotion a disclosure decision. A decision with no record
-- is one nobody can revisit: "who could read this, and did any of them
-- send it out of the house" has to be answerable a month later, when
-- the Butler roster has changed and the answer is no longer visible by
-- looking at it.
CREATE TABLE butler_pool_disclosures (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    memory      INTEGER NOT NULL,
    -- butlers and models are recorded as text, as they were AT THE
    -- TIME. A join would answer today's question; this answers the one
    -- that was actually decided.
    butlers     TEXT NOT NULL,
    left_house  INTEGER NOT NULL DEFAULT 0,
    promoted_at TEXT NOT NULL,
    promoted_by TEXT NOT NULL,

    CHECK (promoted_by <> '')
);

CREATE INDEX butler_pool_disclosures_memory ON butler_pool_disclosures (memory, seq);
