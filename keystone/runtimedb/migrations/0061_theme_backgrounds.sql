-- 0061: the picture a theme brings with it.
--
-- An Omarchy theme is not only a palette. Every one of them ships a
-- `backgrounds/` directory — tokyo-night has eight wallpapers in it —
-- and a theme without its picture is half a skin.
--
-- WHY THE PICTURES ARE NOT IN THE BINARY, when the palettes are. A
-- palette is twenty-six hex values; the twenty-two that ship cost a few
-- kilobytes and no licensing question, because a list of colours is not
-- somebody's artwork. The wallpapers are between half a megabyte and a
-- megabyte and a half EACH, so vendoring one per shipped theme would
-- add fifteen megabytes to a control-plane binary that gets downloaded
-- onto home servers, and vendoring the sets would add over a hundred.
-- They are also art by various hands with various provenance, which is
-- exactly the call `packages/dashboard/README.md` already declines to
-- make incidentally about third-party logos.
--
-- So a household brings its own: the same act as pasting a colors.toml,
-- one file further. Nothing is fetched by the control plane, which is
-- the decision ADR-W036 A2 already made and this does not reopen.
--
-- KEYED ON THE THEME NAME, NOT ON household_themes. A picture can
-- attach to a theme that ships in the binary as easily as to a pasted
-- one — otherwise adding a wallpaper to Nord would mean re-pasting
-- Nord's palette first, which is a silly thing to make somebody do. The
-- two tables are independent and the API keeps them consistent.
--
-- A TABLE RATHER THAN A COLUMN ON household_themes, and rather than a
-- blob in some settings JSON, for the reason 0039 gives about the
-- household banner: bytes in a column beside text mean a base64 round
-- trip on every read of the text.
CREATE TABLE theme_backgrounds (
    -- The theme this picture belongs to. Validated by theme.ValidName
    -- before it is written.
    name       TEXT PRIMARY KEY,
    image      BLOB NOT NULL,
    -- Decided from the BYTES on the way in, never from what the upload
    -- claimed — see handlers_portal_banner.go, which is the other place
    -- this surface accepts an image and the reasoning it records.
    media_type TEXT NOT NULL,
    -- Who added it, so the household can tell. An actor column.
    set_by     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    CHECK (name <> ''),
    CHECK (media_type <> '')
);
