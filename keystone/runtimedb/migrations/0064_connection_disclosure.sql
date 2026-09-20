-- 0064: a Connection is a DISCLOSURE boundary, and rooms are not people.
--
-- 0063 recorded who a Connection admits. This records what may be said
-- over it, which design §8.4 calls the other half of the same object:
-- "Connections are disclosure boundaries... The exact schema is
-- implementation-specific; the policy effect is not."
--
-- #212 leaves the schema open and asks for one to be chosen and
-- documented. This is it: the four characteristics §8.4 names, as four
-- closed enums, plus the one piece of delivery policy §8.3 demands be
-- explicit.
--
-- WHY THE CHARACTERISTICS ARE COLUMNS AND NOT A CLASS. §8.4 lists them
-- separately -- local vs external transport, encryption class, provider
-- logging/retention class, trusted UI vs external messaging -- and they
-- genuinely vary independently: a Matrix room on a homeserver in the
-- house is local AND end-to-end AND external messaging. Collapsing
-- them into one "sensitivity" grade would make the runtime unable to
-- reason about the case it most needs to, which is a transport that is
-- local but still a room full of people.

CREATE TABLE connection_disclosure (
    connection INTEGER PRIMARY KEY REFERENCES butler_addresses(seq),

    -- Where the bytes go. 'local' is a service this household runs;
    -- 'external' leaves the house.
    reach      TEXT NOT NULL CHECK (reach IN ('local', 'external')),

    -- What protects them in transit. 'none' is deliberately spellable:
    -- a plaintext transport should be describable rather than
    -- unrepresentable, because the policy answer for it is to refuse
    -- things, and you cannot refuse what you cannot name.
    encryption TEXT NOT NULL CHECK (encryption IN ('none', 'transport', 'end_to_end')),

    -- Who keeps a copy. 'unknown' is NOT a synonym for 'none' and is
    -- the conservative default: an operator who has not said must be
    -- treated as a provider that retains, never as one that does not.
    retention  TEXT NOT NULL CHECK (retention IN ('none', 'provider', 'unknown')),

    -- What the person is looking at. The Homepage is a surface this
    -- deployment renders; Telegram is somebody else's client on
    -- somebody else's phone.
    surface    TEXT NOT NULL CHECK (surface IN ('trusted_ui', 'external_messaging')),

    -- MAY A MESSAGE MEANT FOR ANOTHER CONNECTION BE DELIVERED HERE.
    --
    -- §8.3: "Cross-Connection delivery fallback occurs only when
    -- explicitly configured -- never as a default." So the default is
    -- 0, and it is a column rather than an inference because the
    -- alternative is what the code did before: ChatRecipientFor walked
    -- every live binding on a transport and returned the first match,
    -- so a household with two bots on Telegram delivered a rule's
    -- message to whichever Connection sorted first. That is a
    -- cross-Connection fallback nobody configured and nobody could see.
    accepts_fallback INTEGER NOT NULL DEFAULT 0 CHECK (accepts_fallback IN (0, 1)),

    set_at TEXT NOT NULL,
    set_by TEXT NOT NULL,
    CHECK (set_by <> '')
);

-- Backfill, conservatively, from the only thing on record: the
-- transport.
--
-- CONSERVATIVELY MEANS THE STRICTEST READING THAT IS NOT A LIE. Every
-- chat transport here is somebody else's service reached over the
-- internet, so reach is 'external', the surface is external messaging,
-- and retention is 'unknown' -- not 'provider', which would assert
-- something about a vendor nobody has checked, and not 'none', which
-- would be the permissive guess. Encryption is 'transport': these are
-- all HTTPS APIs, which is true and is not a claim about end-to-end.
--
-- An operator who knows better corrects it. Nothing here is a
-- measurement, and the columns say what was assumed rather than
-- pretending to have looked.
INSERT INTO connection_disclosure
    (connection, reach, encryption, retention, surface, accepts_fallback, set_at, set_by)
SELECT a.seq, 'external', 'transport', 'unknown', 'external_messaging', 0,
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'migration:0064'
  FROM butler_addresses a;

-- Every new Connection gets a classification when it is created, for
-- 0063's reason: a branch the runtime answers with a refusal should not
-- be reachable by ordinary use, and a policy row that only some write
-- paths remember to create is a policy row that goes missing.
CREATE TRIGGER connection_disclosure_default
AFTER INSERT ON butler_addresses
BEGIN
    INSERT INTO connection_disclosure
        (connection, reach, encryption, retention, surface, accepts_fallback, set_at, set_by)
    VALUES (NEW.seq, 'external', 'transport', 'unknown', 'external_messaging', 0,
            NEW.created_at, NEW.created_by);
END;

-- chat_rooms: the conversations we have SEEN be a room.
--
-- WHY THIS EXISTS. §8.3 refuses group and multi-recipient destinations
-- in MVP, and the inbound path can enforce that directly because the
-- adapter tells it `group: true`. The OUTBOUND path cannot: a household
-- rule queues to a target string, and a target string does not say
-- whether it is one person or forty. Without a record, "never send
-- household memory to a room" is a rule the delivery side has no way to
-- evaluate.
--
-- It is an OBSERVATION LOG, not a configuration. Nothing an operator
-- sets, nothing that grants anything -- a row here only ever subtracts,
-- by making a destination refuse. That is why a row is never deleted
-- when a room goes quiet: forgetting that a conversation was a room
-- would silently re-permit delivery into it, and the one thing this
-- table must never do is become less certain over time.
CREATE TABLE chat_rooms (
    connection    INTEGER NOT NULL REFERENCES butler_addresses(seq),
    conversation  TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    PRIMARY KEY (connection, conversation),
    CHECK (conversation <> '')
);
