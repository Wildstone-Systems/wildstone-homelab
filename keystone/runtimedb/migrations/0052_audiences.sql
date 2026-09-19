-- +wildstone: foreign_keys=off
-- 0052: an audience is a set of people, named in the household's one
-- group namespace (ADR-W033 design §4, §10 layer 3, §11; §18.5; work
-- slice 7 of the agent-system epic).
--
-- `audience IN ('private','shared')` cannot say "adults only". That is
-- not a missing convenience: §11 needs it for memory, §12 for secrets,
-- §15 for routine output and §9 for session disclosure, and it was
-- already recorded as finding 2 on #151 — "the audience vocabulary is
-- too narrow. AC01's adults-only audience cannot be expressed."
--
-- Done once, here, before memory and secrets are built on it. Per area
-- would produce two audience vocabularies that drift, which is the
-- failure §4 names in its own words.
--
-- SIX TABLES, not the four the issue lists. butler_task_evidence and
-- butler_work_trace carry an audience too, and a slice that widened
-- four of six would leave the old vocabulary alive in the two nobody
-- counted — the drift arriving inside the change meant to end it.
--
-- AN AUDIENCE IS NOT A PERMISSION. §10 puts it at layer 3, disclosure
-- policy, and layer 3 never widens layers 1 or 2: a record whose
-- audience includes somebody is still out of reach if they lack the
-- resource-specific grant. Nothing here grants anything, exactly as
-- slice 6's groups grant nothing.

-- An audience, named once and referenced by many records.
--
-- Shared rather than per-record: `aud_owner` and `aud_household` are
-- two rows the whole database points at, so mapping every existing
-- record is a constant rather than a row per record.
--
-- The kinds are the four §208 names:
--   owner      the owning Person alone — the old 'private'. Resolved
--              against the record's own owner_person_id, which is why
--              one row can serve every table.
--   household  everybody active — the old 'shared'.
--   set        the union of the groups and people in audience_members.
CREATE TABLE audiences (
    audience_id TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('owner', 'household', 'set')),
    -- What a household calls this audience, for the ones it makes
    -- itself. Empty on the two seeded rows: "the owner" and "the
    -- household" are roles, not names somebody chose.
    label       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    CHECK (audience_id <> '')
);

-- The members of a 'set' audience: groups, people, or both.
--
-- A group reference points at the slice-6 namespace rather than at a
-- name, so renaming `Adults` does not silently re-point every record
-- that named it — the same reason rules store group_id there.
CREATE TABLE audience_members (
    audience_id TEXT NOT NULL REFERENCES audiences(audience_id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('group', 'person')),
    ref         TEXT NOT NULL,
    PRIMARY KEY (audience_id, kind, ref),
    CHECK (ref <> '')
);

CREATE INDEX audience_members_by_audience ON audience_members (audience_id);

-- The two the migration maps onto. Fixed ids because every rebuilt
-- table below writes them as literals.
INSERT INTO audiences (audience_id, kind, label, created_at) VALUES
    ('aud_owner',     'owner',     '', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ('aud_household', 'household', '', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

-- ---------------------------------------------------------------------
-- The six rebuilds.
--
-- SQLite cannot drop a CHECK, so widening the column means rebuilding
-- every table that carries it — and with them 13 indexes and 12
-- triggers, including butler_index's three FTS sync triggers against an
-- external-content table.
--
-- The column lists below were generated from the live schema rather
-- than retyped. 0043 rebuilt these same tables by hand and silently
-- dropped two columns; 104 columns is more than anyone checks twice,
-- and a snapshot test over this migration asserts that every column
-- that existed before still exists after.
-- ---------------------------------------------------------------------

-- One trigger lives on ANOTHER table and reads one of these, so it is
-- dropped here and restored at the end with the rest.
--
-- butler_connection_scope_audience is on butler_connection_scope and
-- reads integration_connections.audience. #208 does not mention it —
-- it lists four audience tables and this trigger belongs to none of
-- them — and it announced itself by failing the migration with "no
-- such table: main.integration_connections" the moment the rebuild
-- dropped what it reads. A probe over the whole schema confirms it is
-- the only such reference, trigger or view.
DROP TRIGGER butler_connection_scope_audience;

-- ---- butler_memory ----
CREATE TABLE butler_memory_v52 (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL CHECK (kind IN ('fact', 'preference', 'inference', 'procedure')),
    subject    TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL,
    audience   TEXT NOT NULL REFERENCES audiences(audience_id),
    permission_label TEXT NOT NULL,
    -- The person the record belongs to: who may correct it, forget it,
    -- and export it. NULL is the household (A1).
    owner_person_id TEXT REFERENCES butler_members(person_id),

    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT '',

    confidence TEXT NOT NULL DEFAULT 'stated' CHECK (confidence IN ('stated', 'inferred', 'uncertain')),
    safety_relevant INTEGER NOT NULL DEFAULT 0,

    created_at TEXT NOT NULL,
    -- The person who stated it. Never the household and never a
    -- principal: a record nobody stated is not a record.
    author_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    superseded_by INTEGER REFERENCES butler_memory(seq),
    forgotten_at  TEXT NOT NULL DEFAULT '',
    -- forgotten_by is an actor column and keeps its recorded text.
    forgotten_by  TEXT NOT NULL DEFAULT '',
    envelope_version INTEGER NOT NULL,
    butler     TEXT NOT NULL DEFAULT '',

    CHECK (content <> ''),
    CHECK (permission_label <> ''),
    CHECK (author_person_id <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
);

INSERT INTO butler_memory_v52 (seq, kind, subject, content, audience, permission_label, owner_person_id, source_kind, source_id, observed_at, confidence, safety_relevant, created_at, author_person_id, superseded_by, forgotten_at, forgotten_by, envelope_version, butler)
SELECT seq, kind, subject, content, CASE audience WHEN 'private' THEN 'aud_owner' WHEN 'shared' THEN 'aud_household' END, permission_label, owner_person_id, source_kind, source_id, observed_at, confidence, safety_relevant, created_at, author_person_id, superseded_by, forgotten_at, forgotten_by, envelope_version, butler
  FROM butler_memory;

DROP TABLE butler_memory;

ALTER TABLE butler_memory_v52 RENAME TO butler_memory;

CREATE INDEX butler_memory_by_owner ON butler_memory (owner_person_id, seq);

CREATE INDEX butler_memory_by_source ON butler_memory (source_kind, source_id);

CREATE INDEX butler_memory_eligible ON butler_memory (subject, forgotten_at, seq);

CREATE INDEX butler_memory_scope ON butler_memory (butler, audience, owner_person_id, seq);

-- ---- butler_index ----
CREATE TABLE butler_index_v52 (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    memory      INTEGER REFERENCES butler_memory(seq),
    -- NULL is the household, as in butler_memory.
    owner_person_id TEXT REFERENCES butler_members(person_id),
    audience    TEXT NOT NULL REFERENCES audiences(audience_id),
    permission_label TEXT NOT NULL,
    content     TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT '',
    indexed_at  TEXT NOT NULL,
    volatile    INTEGER NOT NULL DEFAULT 0,
    embedding   BLOB,
    tokens      INTEGER NOT NULL DEFAULT 0,
    butler      TEXT NOT NULL DEFAULT '',

    CHECK (content <> ''),
    CHECK (permission_label <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
);

INSERT INTO butler_index_v52 (seq, source_kind, source_id, memory, owner_person_id, audience, permission_label, content, observed_at, indexed_at, volatile, embedding, tokens, butler)
SELECT seq, source_kind, source_id, memory, owner_person_id, CASE audience WHEN 'private' THEN 'aud_owner' WHEN 'shared' THEN 'aud_household' END, permission_label, content, observed_at, indexed_at, volatile, embedding, tokens, butler
  FROM butler_index;

DROP TABLE butler_index;

ALTER TABLE butler_index_v52 RENAME TO butler_index;

CREATE INDEX butler_index_permission ON butler_index (audience, owner_person_id, seq);

CREATE INDEX butler_index_scope ON butler_index (butler, audience, owner_person_id, seq);

-- ---- butler_tasks ----
CREATE TABLE butler_tasks_v52 (
    seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at          TEXT NOT NULL,     -- RFC3339Nano UTC
    updated_at          TEXT NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('conversation', 'routine')),
    state               TEXT NOT NULL CHECK (state IN (
                            'pending', 'running', 'waiting',
                            'completed', 'failed', 'cancelled',
                            'paused-offboarded')),

    -- The person who asked for the work, and the person who operates it
    -- now. Offboarding moves the second and never the first.
    author_person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    owner_person_id     TEXT NOT NULL REFERENCES butler_members(person_id),

    surface             TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    profile             TEXT NOT NULL,
    audience            TEXT NOT NULL REFERENCES audiences(audience_id),

    session             INTEGER REFERENCES butler_sessions(seq),
    origin_turn         INTEGER,

    correlation_id      TEXT NOT NULL DEFAULT '',

    objective           TEXT NOT NULL,
    completion_criteria TEXT NOT NULL,

    grants_json         TEXT NOT NULL DEFAULT '{}',
    budget_json         TEXT NOT NULL DEFAULT '{}',
    deadline_at         TEXT NOT NULL DEFAULT '',
    next_wakeup_at      TEXT NOT NULL DEFAULT '',

    lease_epoch         INTEGER NOT NULL DEFAULT 0,

    uncertainty         TEXT NOT NULL DEFAULT '',
    result_summary      TEXT NOT NULL DEFAULT '',

    retention           TEXT NOT NULL CHECK (retention IN ('manual', 'conversation', 'ephemeral')),
    envelope_version    INTEGER NOT NULL,
    butler              TEXT NOT NULL DEFAULT ''
);

INSERT INTO butler_tasks_v52 (seq, created_at, updated_at, kind, state, author_person_id, owner_person_id, surface, profile, audience, session, origin_turn, correlation_id, objective, completion_criteria, grants_json, budget_json, deadline_at, next_wakeup_at, lease_epoch, uncertainty, result_summary, retention, envelope_version, butler)
SELECT seq, created_at, updated_at, kind, state, author_person_id, owner_person_id, surface, profile, CASE audience WHEN 'private' THEN 'aud_owner' WHEN 'shared' THEN 'aud_household' END, session, origin_turn, correlation_id, objective, completion_criteria, grants_json, budget_json, deadline_at, next_wakeup_at, lease_epoch, uncertainty, result_summary, retention, envelope_version, butler
  FROM butler_tasks;

DROP TABLE butler_tasks;

ALTER TABLE butler_tasks_v52 RENAME TO butler_tasks;

CREATE INDEX butler_tasks_butler ON butler_tasks (butler, state);

CREATE INDEX butler_tasks_by_owner ON butler_tasks (owner_person_id, seq);

CREATE INDEX butler_tasks_by_session ON butler_tasks (session, seq);

CREATE INDEX butler_tasks_by_state ON butler_tasks (state, seq);

CREATE INDEX butler_tasks_by_wakeup ON butler_tasks (next_wakeup_at) WHERE next_wakeup_at <> '';

-- ---- butler_task_evidence ----
CREATE TABLE butler_task_evidence_v52 (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    recorded_at      TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('observation', 'tool-result', 'approval', 'delivery', 'external-verification')),
    description      TEXT NOT NULL,
    source_id        TEXT NOT NULL DEFAULT '',
    observed_at      TEXT NOT NULL DEFAULT '',
    permission_label TEXT NOT NULL,
    audience         TEXT NOT NULL REFERENCES audiences(audience_id),
    payload_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (permission_label <> ''),
    CHECK (description <> '')
);

INSERT INTO butler_task_evidence_v52 (seq, task, recorded_at, kind, description, source_id, observed_at, permission_label, audience, payload_json, envelope_version)
SELECT seq, task, recorded_at, kind, description, source_id, observed_at, permission_label, CASE audience WHEN 'private' THEN 'aud_owner' WHEN 'shared' THEN 'aud_household' END, payload_json, envelope_version
  FROM butler_task_evidence;

DROP TABLE butler_task_evidence;

ALTER TABLE butler_task_evidence_v52 RENAME TO butler_task_evidence;

CREATE INDEX butler_task_evidence_by_task ON butler_task_evidence (task, seq);

-- ---- butler_work_trace ----
CREATE TABLE butler_work_trace_v52 (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    at               TEXT NOT NULL,
    type             TEXT NOT NULL,
    step             INTEGER REFERENCES butler_task_steps(seq),
    attempt          INTEGER REFERENCES butler_task_attempts(seq),
    epoch            INTEGER NOT NULL DEFAULT 0,
    actor            TEXT NOT NULL DEFAULT '',
    -- Versions of the things that decide behaviour, so a before/after
    -- comparison in P5 knows what actually changed (A11).
    model_endpoint   TEXT NOT NULL DEFAULT '',
    model_verdict    TEXT NOT NULL DEFAULT '',
    prompt_version   TEXT NOT NULL DEFAULT '',
    policy_version   TEXT NOT NULL DEFAULT '',
    manifest_version TEXT NOT NULL DEFAULT '',
    source_ids       TEXT NOT NULL DEFAULT '',  -- comma-separated context source ids
    permission_label TEXT NOT NULL,
    audience         TEXT NOT NULL REFERENCES audiences(audience_id),
    redacted         INTEGER NOT NULL DEFAULT 1,
    payload_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (permission_label <> ''),
    CHECK (type <> '')
);

INSERT INTO butler_work_trace_v52 (seq, task, at, type, step, attempt, epoch, actor, model_endpoint, model_verdict, prompt_version, policy_version, manifest_version, source_ids, permission_label, audience, redacted, payload_json, envelope_version)
SELECT seq, task, at, type, step, attempt, epoch, actor, model_endpoint, model_verdict, prompt_version, policy_version, manifest_version, source_ids, permission_label, CASE audience WHEN 'private' THEN 'aud_owner' WHEN 'shared' THEN 'aud_household' END, redacted, payload_json, envelope_version
  FROM butler_work_trace;

DROP TABLE butler_work_trace;

ALTER TABLE butler_work_trace_v52 RENAME TO butler_work_trace;

CREATE INDEX butler_work_trace_by_task ON butler_work_trace (task, seq);

-- ---- integration_connections ----
CREATE TABLE integration_connections_v52 (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    -- adapter names the adapter implementation this connection uses.
    -- Application-specific behaviour lives there; this row is a
    -- neutral description of one instance of it.
    adapter     TEXT NOT NULL,
    endpoint    TEXT NOT NULL DEFAULT '',
    -- Ownership decides the defaults (A5). A personal connection starts
    -- private, read-only, live-query-only and local-processing-only
    -- until its owner explicitly widens it; a household one starts
    -- household-shared and read-only.
    ownership   TEXT NOT NULL CHECK (ownership IN ('personal', 'household')),
    owner       TEXT NOT NULL,
    audience    TEXT NOT NULL REFERENCES audiences(audience_id),
    -- Discovered actions stay disabled until separately enabled and
    -- granted (A5). This is that switch, and it defaults off.
    actions_enabled INTEGER NOT NULL DEFAULT 0,
    indexing_enabled INTEGER NOT NULL DEFAULT 0,
    cloud_allowed   INTEGER NOT NULL DEFAULT 0,

    state       TEXT NOT NULL CHECK (state IN ('configured', 'ready', 'degraded', 'unavailable')),
    -- lease_epoch fences adapters. Only the holder of the current epoch
    -- may claim; results from an older one are refused.
    lease_epoch INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    -- degraded_reason is why a connection is not fully working. A
    -- connection that is degraded for a reason nobody can read is a
    -- connection nobody fixes.
    degraded_reason TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (adapter <> ''),
    CHECK (owner <> ''),
    CHECK (created_by <> '')
);

INSERT INTO integration_connections_v52 (seq, name, adapter, endpoint, ownership, owner, audience, actions_enabled, indexing_enabled, cloud_allowed, state, lease_epoch, created_at, created_by, updated_at, degraded_reason)
SELECT seq, name, adapter, endpoint, ownership, owner, CASE audience WHEN 'private' THEN 'aud_owner' WHEN 'shared' THEN 'aud_household' END, actions_enabled, indexing_enabled, cloud_allowed, state, lease_epoch, created_at, created_by, updated_at, degraded_reason
  FROM integration_connections;

DROP TABLE integration_connections;

ALTER TABLE integration_connections_v52 RENAME TO integration_connections;

-- ---------------------------------------------------------------------
-- The triggers, recreated only now.
--
-- Dropping a table drops its triggers, and two of these reach ACROSS
-- tables this migration rebuilds: butler_tasks' evidence gate reads
-- butler_task_evidence. Recreated inline, the first rebuild would
-- install a trigger naming a table the third rebuild then drops, and
-- SQLite re-validates every trigger on the next ALTER TABLE ... RENAME
-- — which is exactly how the first draft of this migration failed, with
-- "no such table: main.butler_task_evidence".
--
-- So every table exists before any trigger is restored.
-- ---------------------------------------------------------------------

-- Rewritten for set-valued audiences.
--
-- The old rule was one comparison: private must not become shared. With
-- an audience naming groups or people there is no such pair to compare,
-- and SQL cannot decide whether one resolved set contains another — the
-- members live in another table and the answer depends on who is active
-- right now.
--
-- So the backstop fails CLOSED and refuses ANY change of audience. That
-- takes nothing away: no code in Keystone has ever issued an UPDATE of
-- this column, so the old trigger guarded a path nobody walks. A slice
-- that needs to narrow an audience adds a verb proving the narrowing in
-- Go and drops this deliberately — a decision somebody makes, rather
-- than a widening that slips through.
CREATE TRIGGER butler_memory_audience_cannot_loosen
BEFORE UPDATE OF audience ON butler_memory
WHEN OLD.audience <> NEW.audience
BEGIN
    SELECT RAISE(ABORT, 'a memory audience cannot be changed by an update; narrowing needs a verb that proves it (ADR-W030 A2/A3, design §11)');
END;

CREATE TRIGGER butler_memory_content_immutable
BEFORE UPDATE OF content, source_kind, source_id, author_person_id, kind ON butler_memory
BEGIN
    SELECT RAISE(ABORT, 'a memory record is corrected by superseding it, not by editing it (ADR-W030 A3)');
END;

CREATE TRIGGER butler_index_ad AFTER DELETE ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
END;

CREATE TRIGGER butler_index_ai AFTER INSERT ON butler_index BEGIN
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

CREATE TRIGGER butler_index_au AFTER UPDATE OF content ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

CREATE TRIGGER butler_tasks_author_immutable
BEFORE UPDATE OF author_person_id ON butler_tasks
WHEN NEW.author_person_id <> OLD.author_person_id
BEGIN
    SELECT RAISE(ABORT, 'butler task authorship is immutable (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_complete_needs_evidence_insert
BEFORE INSERT ON butler_tasks
WHEN NEW.state = 'completed'
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot be created completed (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_complete_needs_evidence_update
BEFORE UPDATE OF state ON butler_tasks
WHEN NEW.state = 'completed'
 AND NOT EXISTS (SELECT 1 FROM butler_task_evidence WHERE task = NEW.seq)
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot complete without evidence (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON butler_tasks
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'butler task lease epoch cannot move backwards (ADR-W030 A11)');
END;

CREATE TRIGGER butler_task_evidence_immutable
BEFORE UPDATE ON butler_task_evidence
BEGIN
    SELECT RAISE(ABORT, 'butler task evidence is append-only (ADR-W030 A4)');
END;

CREATE TRIGGER butler_work_trace_immutable
BEFORE UPDATE ON butler_work_trace
BEGIN
    SELECT RAISE(ABORT, 'butler work trace is append-only (ADR-W030 A11)');
END;

CREATE TRIGGER integration_connections_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON integration_connections
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'a connection lease epoch cannot move backwards (ADR-W030 A5)');
END;

-- Restored, generalised from 'private' to "anything narrower than the
-- household".
--
-- The rule it enforces (ADR-W031 A4) is that a connection belonging to
-- one person must not be handed to a Butler serving the whole family.
-- With two values that was audience = 'private'. With an audience that
-- can name Adults, the faithful reading is that anything short of the
-- whole household is too narrow for a family surface — so the test is
-- on the KIND, and 'set' is refused along with 'owner'.
--
-- Identical behaviour on every existing row, since the migration above
-- maps them all to 'owner' or 'household'. It fails closed on the new
-- case rather than admitting it by omission.
CREATE TRIGGER butler_connection_scope_audience
BEFORE INSERT ON butler_connection_scope
WHEN (SELECT a.kind
        FROM integration_connections c
        JOIN audiences a ON a.audience_id = c.audience
       WHERE c.seq = NEW.connection) <> 'household'
 AND (SELECT surface FROM butlers WHERE seq = NEW.butler) = 'family'
BEGIN
    SELECT RAISE(ABORT, 'that connection is not shared with the whole household, so a household-surface butler cannot be given it (ADR-W031 A4)');
END;
