-- Where a completion came from, so the same household fact closes a
-- cycle once (ADR-W035, `satisfiedBy`).
--
-- A commitment may name the household event that closes it — "the truck
-- was serviced" rather than somebody tapping Done. The pass runs daily
-- and re-reads the stream each time, so without a key the same
-- `vehicle.serviced` would close a cycle every morning forever.
--
-- `external_id` is the household event's own id, which is already
-- unique per event, and the partial index leaves hand-recorded
-- completions alone: a member may answer the same commitment twice in a
-- day, and two people ticking off the bins is a duplicate the household
-- can live with. An event replayed is not.
ALTER TABLE commitment_completions ADD COLUMN external_id TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX commitment_completions_by_external
    ON commitment_completions (commitment, external_id)
    WHERE external_id <> '';
