-- 0023: owned memory records (ADR-W030 A3/A3.1, work package P8).
--
-- Memory here is OWNED DATA with provenance, not a vector blob and not a
-- second source of truth. Four things follow from that, and each is a
-- column or a constraint rather than a convention:
--
--   Provenance. Every record says where it came from and when it was
--   observed. A record with no source cannot be corrected at its source,
--   cannot be invalidated when that source is deleted, and cannot be
--   weighed against a fresher reading.
--
--   Audience inheritance. A derivative carries the STRICTEST audience of
--   its sources. This is the rule that stops private material becoming
--   shared by being summarised, and it is enforced where derivatives are
--   created, not hoped for at retrieval.
--
--   No authority. There is no column here through which a memory record
--   could grant anything. Poisoned input can make the Butler believe
--   something false; it cannot make the Butler ALLOWED to do something,
--   because nothing in the authority path reads this table.
--
--   Forgetting that sticks. A forgotten record is marked, and its source
--   is suppressed, so the next extraction pass over the same source does
--   not helpfully put it back.

CREATE TABLE butler_memory (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    -- kind is closed and deliberately contains no "permission",
    -- "grant" or "rule" member. What the Butler may DO is decided by
    -- ADR-W030 A7's controls and grants, and there is no kind here that
    -- could be mistaken for one.
    kind       TEXT NOT NULL CHECK (kind IN ('fact', 'preference', 'inference', 'procedure')),
    -- subject is who or what the record is about. Retrieval filters on
    -- it before content ever reaches a model.
    subject    TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL,
    -- audience and permission_label are mandatory for the reason every
    -- other table in this ADR makes them mandatory: a record that cannot
    -- be permission-filtered can never be safely shown.
    audience   TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    permission_label TEXT NOT NULL,
    -- owner is the person the record belongs to: who may correct it,
    -- forget it, and export it. For household-shared records it is the
    -- household, and any admitted adult may correct it (A1).
    owner      TEXT NOT NULL,

    -- Provenance. source_kind names the sort of thing it came from —
    -- a conversation, a household event, an app read — and source_id
    -- identifies the exact one, which is what deletion invalidation and
    -- re-extraction suppression both key on.
    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT '',

    -- confidence separates a stated fact from a tentative inference. An
    -- inference presented with the confidence of a fact is how a
    -- household ends up arguing with its own house.
    confidence TEXT NOT NULL DEFAULT 'stated' CHECK (confidence IN ('stated', 'inferred', 'uncertain')),

    -- safety_relevant marks a record whose REMOVAL the household must
    -- hear about immediately rather than in the next digest — an
    -- allergy, a medication, a standing instruction (A1).
    safety_relevant INTEGER NOT NULL DEFAULT 0,

    created_at TEXT NOT NULL,
    author     TEXT NOT NULL,
    -- superseded_by points at the record that corrected this one, so a
    -- correction is a new row and history is not rewritten.
    superseded_by INTEGER REFERENCES butler_memory(seq),
    forgotten_at  TEXT NOT NULL DEFAULT '',
    forgotten_by  TEXT NOT NULL DEFAULT '',
    envelope_version INTEGER NOT NULL,

    CHECK (content <> ''),
    CHECK (permission_label <> ''),
    CHECK (owner <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
);

CREATE INDEX butler_memory_eligible ON butler_memory (subject, forgotten_at, seq);
CREATE INDEX butler_memory_by_source ON butler_memory (source_kind, source_id);
CREATE INDEX butler_memory_by_owner ON butler_memory (owner, seq);

-- butler_memory_derivations records which records a derivative was built
-- from. It is what makes "the strictest audience of its sources" a
-- checkable claim afterwards, and what lets deleting a source invalidate
-- everything downstream of it rather than only the record itself.
CREATE TABLE butler_memory_derivations (
    derivative INTEGER NOT NULL REFERENCES butler_memory(seq),
    source     INTEGER NOT NULL REFERENCES butler_memory(seq),
    PRIMARY KEY (derivative, source)
);

CREATE INDEX butler_memory_derivations_by_source ON butler_memory_derivations (source);

-- butler_memory_suppressed_sources is why a forget sticks.
--
-- Marking a record forgotten stops it being retrieved. It does not stop
-- the next extraction pass reading the same conversation and cheerfully
-- learning it again — which is how "I asked you to forget that" becomes
-- a thing a household says three times. A suppression row makes the
-- source itself off limits for that content.
CREATE TABLE butler_memory_suppressed_sources (
    source_kind  TEXT NOT NULL,
    source_id    TEXT NOT NULL,
    -- content_key is a normalized form of what was forgotten, so
    -- suppressing one fact from a long conversation does not suppress
    -- the whole conversation.
    content_key  TEXT NOT NULL,
    suppressed_at TEXT NOT NULL,
    suppressed_by TEXT NOT NULL,
    PRIMARY KEY (source_kind, source_id, content_key)
);

-- butler_memory_digest is the record of what changed in SHARED memory,
-- for the periodic digest other admitted adults see (A1). Private
-- corrections never appear here: they are nobody else's business, and a
-- digest that leaked them would make correcting a private record a
-- disclosure.
CREATE TABLE butler_memory_digest (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    memory     INTEGER NOT NULL REFERENCES butler_memory(seq),
    change     TEXT NOT NULL CHECK (change IN ('recorded', 'corrected', 'forgotten')),
    summary    TEXT NOT NULL,
    actor      TEXT NOT NULL,
    at         TEXT NOT NULL,
    -- immediate marks a change the household must hear about now rather
    -- than in the next digest — the safety-relevant removals of A1.
    immediate  INTEGER NOT NULL DEFAULT 0,
    delivered_at TEXT NOT NULL DEFAULT '',
    CHECK (summary <> ''),
    CHECK (actor <> '')
);

CREATE INDEX butler_memory_digest_pending ON butler_memory_digest (delivered_at, seq);

-- Memory content is corrected by SUPERSEDING, never by editing: a record
-- that can be rewritten in place has no history, and "what did it think
-- last week, and why" is exactly the question a correction raises.
CREATE TRIGGER butler_memory_content_immutable
BEFORE UPDATE OF content, source_kind, source_id, author, kind ON butler_memory
BEGIN
    SELECT RAISE(ABORT, 'a memory record is corrected by superseding it, not by editing it (ADR-W030 A3)');
END;

-- A derivative may never be less strict than a source it was built
-- from. The audience column is checked at write time in Go, where the
-- sources are known; this trigger stops a later UPDATE loosening one.
CREATE TRIGGER butler_memory_audience_cannot_loosen
BEFORE UPDATE OF audience ON butler_memory
WHEN OLD.audience = 'private' AND NEW.audience = 'shared'
BEGIN
    SELECT RAISE(ABORT, 'private memory cannot be widened to shared by an update (ADR-W030 A2/A3)');
END;
