-- #257: local generated deployment history is not the product Statefile head.
CREATE TABLE deployment_promotions (
    operation_id TEXT PRIMARY KEY,
    binding TEXT NOT NULL,
    expected_revision TEXT NOT NULL,
    candidate_revision TEXT NOT NULL,
    tree_digest TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('prepared', 'promoted')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
