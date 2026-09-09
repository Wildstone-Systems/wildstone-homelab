-- 0024: selected-source retrieval index and compaction (ADR-W030 A3/A6/
-- A9.1, work package P9).
--
-- The implementation choice, made by measurement rather than taste:
-- SQLite FTS5 with bm25 ranking for the keyword half, and an optional
-- caller-supplied embedding for the vector half. Both are LOCAL — no
-- service, no network, no second datastore — which is what A9's
-- local-first data policy requires of anything that reads household
-- material. FTS5 and bm25() are present in the pure-Go driver this
-- database already uses, so the keyword half costs one virtual table and
-- no new dependency; the vector half is optional because an embedding
-- needs a model, and a deployment with no local model must still get
-- working retrieval rather than none.
--
-- The rule that shapes the schema: PERMISSION FIRST. Every indexed row
-- carries the owner, audience and label of the source it came from, and
-- retrieval filters on those in SQL before any content is read. There is
-- no path that ranks first and filters after — a match on material the
-- reader may not see must never exist, not even briefly, because
-- "briefly" is how it ends up in a model's context.

CREATE TABLE butler_index (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    -- memory links an indexed chunk to the record it came from, so
    -- forgetting the record removes the chunk (A3).
    memory      INTEGER REFERENCES butler_memory(seq),
    owner       TEXT NOT NULL,
    audience    TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    permission_label TEXT NOT NULL,
    content     TEXT NOT NULL,
    -- observed_at is when the underlying thing was true. Freshness is
    -- measured from this, not from indexed_at: reindexing an old
    -- conversation does not make it recent.
    observed_at TEXT NOT NULL DEFAULT '',
    indexed_at  TEXT NOT NULL,
    -- volatile marks a source whose CURRENT state cannot be answered
    -- from an index at all — a light that is on or off, a door that is
    -- locked or not. Retrieval returns these flagged for live
    -- verification rather than as answers (A6: changing app state is
    -- verified live, not trusted from RAG).
    volatile    INTEGER NOT NULL DEFAULT 0,
    -- embedding is an optional float32 vector, little-endian, supplied
    -- by whatever local model the deployment has. NULL means this
    -- deployment ranks by keyword alone, which is a working
    -- configuration and not a degraded one.
    embedding   BLOB,
    tokens      INTEGER NOT NULL DEFAULT 0,

    CHECK (content <> ''),
    CHECK (owner <> ''),
    CHECK (permission_label <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
);

CREATE INDEX butler_index_by_source ON butler_index (source_kind, source_id);
CREATE INDEX butler_index_by_memory ON butler_index (memory);
-- The permission filter's index. It leads with the columns retrieval
-- narrows on, because that narrowing has to happen before ranking.
CREATE INDEX butler_index_permission ON butler_index (audience, owner, seq);

-- The keyword half. content_rowid ties it to butler_index so the
-- permission columns are one join away and the FTS table itself holds no
-- household material beyond the text it must rank.
CREATE VIRTUAL TABLE butler_index_fts USING fts5(
    content,
    content='butler_index',
    content_rowid='seq'
);

-- Triggers keep the two in step. Doing it here rather than in Go means
-- a writer that forgets cannot desynchronise them, and a delete cannot
-- leave a searchable ghost of removed material — which is the failure
-- that matters most, because a ghost is exactly what a revocation was
-- supposed to remove.
CREATE TRIGGER butler_index_ai AFTER INSERT ON butler_index BEGIN
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

CREATE TRIGGER butler_index_ad AFTER DELETE ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
END;

CREATE TRIGGER butler_index_au AFTER UPDATE OF content ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

-- butler_index_measurements is where the implementation choice is
-- RECORDED rather than asserted (P9: "retrieval quality/resource limits
-- are recorded"). A fixture run writes a row; the Console reads it. A
-- claim about recall with no measurement behind it is a preference.
CREATE TABLE butler_index_measurements (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    at           TEXT NOT NULL,
    fixture      TEXT NOT NULL,
    strategy     TEXT NOT NULL CHECK (strategy IN ('keyword', 'vector', 'hybrid')),
    queries      INTEGER NOT NULL,
    hits         INTEGER NOT NULL,
    -- recall_pct and p95_micros are what a person actually compares.
    recall_pct   INTEGER NOT NULL,
    p95_micros   INTEGER NOT NULL,
    index_bytes  INTEGER NOT NULL DEFAULT 0,
    note         TEXT NOT NULL DEFAULT '',
    CHECK (fixture <> '')
);

-- butler_index_jobs is the local-first background work: indexing,
-- compaction and reindexing run at LOWER priority than anything a
-- person is waiting on, on local resources, and never as a side effect
-- of answering a question (A9.1).
CREATE TABLE butler_index_jobs (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL CHECK (kind IN ('index', 'compact', 'reindex')),
    payload      TEXT NOT NULL DEFAULT '{}',
    state        TEXT NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed')),
    -- priority is lower-is-sooner, and background work starts at 100 so
    -- anything interactive can be queued in front of it without
    -- renumbering.
    priority     INTEGER NOT NULL DEFAULT 100,
    created_at   TEXT NOT NULL,
    started_at   TEXT NOT NULL DEFAULT '',
    finished_at  TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX butler_index_jobs_queue ON butler_index_jobs (state, priority, seq);
