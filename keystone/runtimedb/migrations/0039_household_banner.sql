-- The household's banner photo and how it is framed.
--
-- ONE ROW, ALWAYS. The banner belongs to the household rather than to a
-- member: everybody standing in front of the wall tablet sees the same
-- picture, so there is nothing to key it by and the primary key is a
-- constant. A table rather than a settings blob because the image is
-- bytes, and bytes in a JSON column are a base64 round-trip on every
-- read of anything else in it.
--
-- WHY NOT DESIRED STATE. The design puts the household's theme in the
-- Statefile, and for the hue it is right: it is small, declarative, and
-- an operator should be able to read it. A photograph is none of those.
-- It is a few hundred kilobytes that would land in a git repository the
-- control plane rewrites, and putting it there would make every
-- statefile diff unreadable and every restore carry it. The hue stays
-- declarative; the picture lives here.
CREATE TABLE IF NOT EXISTS household_banner (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    image       BLOB    NOT NULL,
    media_type  TEXT    NOT NULL,
    -- How the picture is framed, as CSS object-position percentages.
    -- A photograph is arbitrary and a banner is a wide, short crop of
    -- it, so "which part of it do you want to see" is a real question
    -- and 50/50 is only a guess at the answer.
    position_x  REAL    NOT NULL DEFAULT 50,
    position_y  REAL    NOT NULL DEFAULT 50,
    -- Who last changed it, so the household can tell.
    set_by      TEXT    NOT NULL DEFAULT '',
    updated_at  TEXT    NOT NULL
);
