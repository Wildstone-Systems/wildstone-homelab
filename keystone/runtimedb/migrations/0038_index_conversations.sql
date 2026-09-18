-- 0038: a household decides whether its conversations are remembered,
-- and for how long (ADR-W030 A3/A3.1, A9.1).
--
-- Step 2 of docs/butler-memory.md made a Butler keep what somebody asked
-- it to keep. This is the other half, and it is a different act: the
-- Butler remembers what was SAID, without anyone asking. That is what
-- makes "what did I tell you about the boiler in March" answerable, and
-- it is also the largest thing this system does with a household's words
-- — so it is a setting rather than a constant, and the setting is here
-- rather than in a flag, because A3 asks for inspect and forget controls
-- and a control nobody can reach is not one.
--
-- It defaults ON. Memory that holds only what you dictate is barely
-- memory, and a Butler that answers "you never told me that" about a
-- conversation it had is worse than one with no memory at all. The
-- default is a decision, and the switch beside it is what makes it a
-- decision a household can disagree with.
ALTER TABLE butler_runtime_settings
    ADD COLUMN index_conversations INTEGER NOT NULL DEFAULT 1;

-- And it expires. Compaction has existed since 0023 and nothing
-- scheduled it, which is how an index that only grows gets written: the
-- code to prune was there the whole time and no clock ever called it.
--
-- The window is days rather than a size, because what a household means
-- is "stop keeping things from before last spring" and not "stop at
-- forty megabytes". Explicitly remembered notes are NOT covered by it —
-- those were asked for, and they last until somebody forgets them.
--
-- 180 days by default: long enough that "back in March" still answers in
-- September, short enough that a year of idle chat does not sit in a
-- retrieval index nobody audited.
ALTER TABLE butler_runtime_settings
    ADD COLUMN transcript_retention_days INTEGER NOT NULL DEFAULT 180;

-- SQLite cannot add a CHECK to an existing table, so the bounds for
-- these two live in ButlerSettings.Validate alone. Every other column
-- here is checked in both places on purpose — the schema is the backstop
-- that holds for any writer — and these two are the exception the
-- storage engine forced. SetButlerSettings is the only writer.
