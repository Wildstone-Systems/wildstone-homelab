-- ADR-W047 S17: no provider, selection or tool grant is seeded on upgrade.
CREATE TABLE search_providers (
 id TEXT PRIMARY KEY,
 config_json TEXT NOT NULL,
 config_revision TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('unconfigured','untested','ready','unavailable')),
 test_revision TEXT NOT NULL DEFAULT '',
 tested_config_revision TEXT NOT NULL DEFAULT ''
);
-- Keep selectors after deletion: an explicit missing provider must never
-- silently inherit a household default or be replaced by another provider.
CREATE TABLE research_selections (
 scope TEXT PRIMARY KEY,
 provider_id TEXT,
 config_revision TEXT NOT NULL
);
