-- 0035: the router is removed (ADR-W031 A2, work package B7).
--
-- ADR-W030 A9 had a router pick a model per call from locality,
-- readiness and capacity. W031 A2 withdrew it, and the reason is worth
-- keeping next to the deletion rather than only in the ADR:
--
--   Anything that DECIDES which model should handle a message must
--   READ the message in order to decide. So the material reaches a
--   model before the routing step that was meant to protect it, and
--   the routing is itself the disclosure.
--
-- A Butler's model is assigned instead. Asked which model saw a piece
-- of material, an assignment answers by name; a router answers
-- "whichever was free at the time", which is not a property anyone can
-- rely on.
--
-- This drops rather than disables, because a selection path that exists
-- and is unused is a selection path somebody re-enables — most likely
-- without reading why it went.

-- The lease table counted in-flight calls per route so the router could
-- refuse a busy one. Nothing counts anything now: a Butler talks to its
-- model, and a model that is busy is the model's problem to report
-- rather than Keystone's to route around.
DROP TABLE model_route_leases;

-- max_concurrency and priority were the router's inputs and have no
-- meaning without it. max_concurrency bounded selection; priority
-- ordered it. Leaving them would leave two columns that look like
-- policy and are read by nothing.
DROP INDEX model_routes_selection;
ALTER TABLE model_routes DROP COLUMN max_concurrency;
ALTER TABLE model_routes DROP COLUMN priority;

-- What a route is FOR now: the model a Butler is assigned to. Lookups
-- are by name, and the enabled/kind pair is what a Butler's assignment
-- is checked against.
CREATE INDEX model_routes_assignable ON model_routes (enabled, kind, name);

-- Everything else about a route survives deliberately (A2): its
-- local/cloud kind is the data boundary and is still immutable, and its
-- probe verdict still decides whether a session may use tools that do
-- things (ADR-W017 A4). Neither of those was ever about CHOOSING a
-- model — they are about what a chosen one is trusted with.
