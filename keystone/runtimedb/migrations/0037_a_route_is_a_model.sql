-- 0037: a route is a model (ADR-W031 A2, finishing what 0035 started).
--
-- 0035 removed the ROUTER and kept the routes, deliberately: "what a
-- route is FOR now: the model a Butler is assigned to". What it did not
-- do is rename the thing, and the leftover word has been earning its
-- confusion ever since.
--
-- Two reasons it has to go.
--
-- The first is that "route" now describes nothing the row does. Nothing
-- routes. A Butler is ASSIGNED one of these and talks to it, and a log
-- line reading `route=cloud-witness model=witness-1` looks like two
-- competing answers to "which model", because one field is named after
-- a decision that no longer happens.
--
-- The second is that Route is already taken. resource.KindRoute is the
-- reverse-proxy route — host, upstream, auth, classification — and a
-- codebase with two unrelated Routes makes every conversation about
-- either of them start with which one.
--
-- What the row is: a named model, where it runs, and whether asking it
-- something leaves the house. That last part is why the bundle exists
-- rather than a bare model string on the Butler. One Ollama endpoint
-- serves gemma4:e4b from local disk and gemma4:31b-cloud from Ollama's
-- own infrastructure, so neither the endpoint nor the provider tells
-- you where the material goes — only this row does.
ALTER TABLE model_routes RENAME TO models;

-- The Butler's assignment. SQLite rewrites the foreign key and the
-- trigger bodies that reference it, which is why this is a rename and
-- not a rebuild.
ALTER TABLE butlers RENAME COLUMN route TO model;

-- Indexes and triggers carry the old word in their names, where it is
-- inert but still the first thing a reader of the schema sees.
DROP INDEX model_routes_assignable;
CREATE INDEX models_assignable ON models (enabled, kind, name);

DROP TRIGGER model_routes_kind_immutable;
-- A model's kind is the data boundary. It cannot change, because a
-- Butler assigned to it was assigned on the strength of where its
-- words go, and flipping that under an existing assignment would move
-- the boundary without anybody deciding to.
CREATE TRIGGER models_kind_immutable
BEFORE UPDATE OF kind ON models
WHEN NEW.kind <> OLD.kind
BEGIN
    SELECT RAISE(ABORT, 'a model cannot change kind: local and cloud are different destinations, and a butler was assigned to it on the strength of which one (ADR-W031 A2)');
END;

DROP TRIGGER butlers_route_usable_insert;
DROP TRIGGER butlers_route_usable_update;

CREATE TRIGGER butlers_model_usable_insert
BEFORE INSERT ON butlers
WHEN (SELECT enabled FROM models WHERE seq = NEW.model) = 0
BEGIN
    SELECT RAISE(ABORT, 'that model is disabled, so a butler assigned to it would have nowhere to run (ADR-W031 A6)');
END;

CREATE TRIGGER butlers_model_usable_update
BEFORE UPDATE OF model ON butlers
WHEN (SELECT enabled FROM models WHERE seq = NEW.model) = 0
BEGIN
    SELECT RAISE(ABORT, 'that model is disabled, so a butler assigned to it would have nowhere to run (ADR-W031 A6)');
END;
