-- ADR-W007 A8 / #157 (W050 slice 0b): one journal for every desired-state
-- publication, so the Git commit and the accepted-revision record can no
-- longer disagree after a crash between them. A candidate is durable here
-- before it is accepted; acceptance is one transaction that compares the
-- expected revision, selects the candidate and moves its Plan.
CREATE TABLE statefile_publications (
    publication_id      TEXT PRIMARY KEY,
    source              TEXT NOT NULL
                        CHECK (source IN ('api-plan', 'household-audit', 'out-of-band')),
    plan_id             TEXT NOT NULL DEFAULT '',
    expected_revision   TEXT NOT NULL DEFAULT '',
    candidate_revision  TEXT NOT NULL DEFAULT '',
    tree_digest         TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL DEFAULT 'prepared'
                        CHECK (state IN ('prepared', 'accepted', 'mirrored', 'abandoned')),
    detail              TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,

    CHECK (publication_id <> '')
);

CREATE INDEX statefile_publications_open ON statefile_publications(state)
    WHERE state IN ('prepared', 'accepted');

-- The accepted revision record gains what was accepted and by which
-- publication. Rows written before this migration keep an empty digest,
-- which recovery reads as "not recorded" rather than as a mismatch.
ALTER TABLE statefile_head ADD COLUMN tree_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE statefile_head ADD COLUMN publication_id TEXT NOT NULL DEFAULT '';
