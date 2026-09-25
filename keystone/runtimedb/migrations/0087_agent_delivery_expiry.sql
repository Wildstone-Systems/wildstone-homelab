-- 0087: agent_deliveries.expired_at, the long-poll consumer's own
-- marker (ADR-W039 §3.6, Phase 4, #341).
--
-- expires_at (0084) says WHEN an unacknowledged delivery should stop
-- being offered; nothing before this recorded that it actually did. A
-- plain column addition — expired_at carries no CHECK, and nothing else
-- references this table — so no table rebuild is needed.
ALTER TABLE agent_deliveries ADD COLUMN expired_at TEXT NOT NULL DEFAULT '';
