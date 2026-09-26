-- 0088: household lists — shopping, chores, packing — kept by Wildstone
-- itself (packages/lists-wildstone), for the household's Agents and the
-- dashboard's Lists card.
--
-- Every list belongs to the HOUSEHOLD, not to a person: any member who
-- can reach the Lists card, and any household Agent a member talks to,
-- reads and changes the same lists. added_by / checked_by record who did
-- what, for the card and the audit trail; they are not an access rule.
--
-- In the runtime database rather than the Statefile because items change
-- by the minute and every Statefile change is a reviewed Plan. That also
-- puts lists under the runtime state's own recovery input (ADR-W030;
-- docs/recovery.md), not the three-input contract.
--
-- name_key / text_key are the case-folded, space-collapsed spellings the
-- store matches on, so "Milk" and " milk " are one item and "Shopping"
-- and "shopping" are one list. The text people typed is kept as typed.
CREATE TABLE household_lists (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL CHECK (name <> '' AND length(name) <= 60),
    name_key   TEXT NOT NULL UNIQUE CHECK (name_key <> ''),
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

CREATE TABLE household_list_items (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    list       INTEGER NOT NULL REFERENCES household_lists(seq) ON DELETE CASCADE,
    text       TEXT NOT NULL CHECK (text <> '' AND length(text) <= 200),
    text_key   TEXT NOT NULL CHECK (text_key <> ''),
    checked    INTEGER NOT NULL DEFAULT 0 CHECK (checked IN (0, 1)),
    added_at   TEXT NOT NULL,
    added_by   TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL DEFAULT '',
    checked_by TEXT NOT NULL DEFAULT '',
    UNIQUE (list, text_key)
);

CREATE INDEX household_list_items_by_list ON household_list_items (list, seq);
