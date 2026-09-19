-- 0060: a person's chosen theme, and the themes a household pasted in.
--
-- WHY PER PERSON AND NOT PER HOUSEHOLD. The household already has a
-- colour: `accentHue` in the dashboard package's settings.yaml, which
-- is desired state, which is right — it is small, declarative, and an
-- operator should be able to read it out of the statefile. That is the
-- house's colour, on the wall tablet everybody walks past.
--
-- A theme is not that. It is the ground somebody reads on, and the
-- people in one household do not agree about it: one reads at night and
-- wants Vantablack, one has low vision and wants Flexoki Light at AAA,
-- one just likes Gruvbox. Making that a household setting would mean
-- the last person to change it decides for everybody, which is the
-- behaviour of a thermostat, not of a preference.
--
-- So it is here rather than in desired state, and the split with the
-- hue is deliberate: the household's colour stays declarative, and a
-- person's theme — which no operator needs to read and no restore needs
-- to reproduce byte-for-byte — lives in the runtime database beside the
-- other things a person chose for themselves (member_notice_preferences
-- is the same shape and the same reasoning).
--
-- KEYED ON person_id, per 0043 and 0051: a handle is a label that can
-- be renamed, and a theme that detached when somebody changed their
-- name would be a bug nobody could explain.
CREATE TABLE person_theme (
    person_id  TEXT PRIMARY KEY REFERENCES butler_members(person_id) ON DELETE CASCADE,
    -- The theme's name, which is either one shipped in the binary or
    -- one in household_themes. NOT a foreign key onto household_themes,
    -- because most names are not rows there — and because a household
    -- removing an imported theme should leave the people who chose it
    -- back on the design's own look rather than failing their write.
    -- The resolver treats an unknown name as no choice at all.
    theme      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (theme <> '')
);

-- The themes a household pasted in, as the file they pasted.
--
-- STORED AS THE SOURCE, NOT AS DERIVED TOKENS. What a person pasted is
-- what a person can be shown back and what they can diff against the
-- theme's upstream; the token block is a pure function of it and is
-- recomputed on read. Storing the derivation instead would freeze every
-- imported theme at the contrast rules in force the day it was added,
-- so a later fix to the derivation would silently skip them.
--
-- ONE ROW IS ~600 BYTES. The parser bounds a palette at 16 KiB before
-- anything reaches here, and a household has a handful of these.
CREATE TABLE household_themes (
    -- The slug, validated by theme.ValidName before it is written: it
    -- ends up in a URL and in a picker.
    name       TEXT PRIMARY KEY,
    -- The colors.toml, verbatim.
    colors     TEXT NOT NULL,
    -- Who added it, so the household can tell. An actor column: it
    -- records whoever did it, which on this path is always an Owner.
    added_by   TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    CHECK (name <> ''),
    CHECK (colors <> '')
);
