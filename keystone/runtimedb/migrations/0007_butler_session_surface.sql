-- 0007: the surface a Butler session ran on (ADR-W026 A1). Two surfaces
-- exist and the set is closed: 'admin' (the operator's butler — read and
-- propose over the Statefile) and 'family' (the household's butler —
-- household context and domestic-tier actions, no Statefile at all).
--
-- The surface is chosen by the endpoint the session arrived on, never by
-- a request body, an adapter or the model, and it is stored so an audit
-- read can tell the two apart forever after. Existing rows are admin:
-- every session written before this migration came in on
-- /api/v1/butler/ask, which is the admin surface.
ALTER TABLE butler_sessions ADD COLUMN surface TEXT NOT NULL DEFAULT 'admin';

CREATE INDEX butler_sessions_by_surface ON butler_sessions (surface, seq);
