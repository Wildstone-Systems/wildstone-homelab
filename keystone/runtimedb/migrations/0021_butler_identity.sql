-- 0021: identity, admission, and shared continuity (ADR-W030 A1/A2/
-- A2.1, work package P6).
--
-- Until now "who is this" was answered differently on every surface. An
-- API token carried a principal, a chat message carried a binding, and
-- a household member was an LdapUser in the Statefile. Equal names
-- across those were treated as the same person, and A2.1 says plainly
-- that equal names are not proof of identity.
--
-- Three tables close that:
--
--   butler_members       — Butler admission, which is runtime state and
--                          NOT LDAP membership. Deleting an LDAP account
--                          and disabling someone's Butler are different
--                          acts with different urgencies, and conflating
--                          them means an offboarding waits on a
--                          directory write.
--   butler_identity_links— the explicit link from an authenticated
--                          surface identity to a member. A named link
--                          type, an external id, and a generation.
--   butler_conversations — the conversation identity that survives a
--                          handoff between the Household and Operator
--                          bots, so "continue what we were saying" means
--                          something across surfaces.
--
-- generation is the fence. Every retrieval and dispatch checks the
-- member's current generation against the one the link was resolved
-- under, so unlinking, disabling or offboarding takes effect at the next
-- check rather than whenever a cache happens to expire. Relinking
-- requires fresh authorization and produces a NEW generation, so old
-- derived grants cannot be revived by re-adding someone.

CREATE TABLE butler_members (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    member       TEXT NOT NULL UNIQUE,
    -- profile is the ceiling, not a preference. 'child' is accepted and
    -- stored so guardian relationships can be recorded now; the CHECK
    -- below refuses to ADMIT one, because the Child surface has not
    -- passed its own checks (ADR-W030 Part C: "child behaviour is
    -- specified but must not be enabled until its own checks pass").
    profile      TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
    admitted     INTEGER NOT NULL DEFAULT 0,
    -- generation increments on every admission, disable, unlink and
    -- offboarding. It is what makes revocation immediate.
    generation   INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    admitted_at  TEXT NOT NULL DEFAULT '',
    admitted_by  TEXT NOT NULL DEFAULT '',
    disabled_at  TEXT NOT NULL DEFAULT '',
    disabled_by  TEXT NOT NULL DEFAULT '',
    -- Offboarding is one audited administrator action that immediately
    -- denies access. Desired-state cleanup follows through Plans; failed
    -- cleanup never restores access (A2.1), which is why this column is
    -- here and not derived from whether the cleanup succeeded.
    offboarded_at TEXT NOT NULL DEFAULT '',
    offboarded_by TEXT NOT NULL DEFAULT '',
    -- guardian is required for a child record and meaningless otherwise.
    -- A sole guardian cannot be removed while a child admission is
    -- active (A2.1); with no child surface yet, this records the
    -- relationship so that rule has something to enforce against.
    guardian     TEXT NOT NULL DEFAULT '',

    CHECK (member <> ''),
    -- A child is never admitted. Not "not admitted by default" — the
    -- schema refuses the row, so no code path, no migration and no
    -- console form can produce an admitted child before that surface
    -- has its own evidence.
    CHECK (NOT (profile = 'child' AND admitted = 1)),
    CHECK (profile <> 'child' OR guardian <> '')
);

CREATE INDEX butler_members_admitted ON butler_members (admitted, member);

-- butler_identity_links is the explicit surface-to-member link.
--
-- link_type is closed. Each names a DIFFERENT way of being
-- authenticated, and the difference matters: an API principal is a
-- minted token, a chat binding is a redeemed claim code, and trusted
-- ingress is a header from the identity proxy. Collapsing them would
-- make "how do we know this is them" unanswerable per link.
CREATE TABLE butler_identity_links (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    link_type   TEXT NOT NULL CHECK (link_type IN ('api-principal', 'chat-binding', 'trusted-ingress')),
    -- external_id is the principal name, the chat binding id rendered as
    -- text, or the ingress subject. Stored as text so one column serves
    -- all three without a nullable pair.
    external_id TEXT NOT NULL,
    member      TEXT NOT NULL REFERENCES butler_members(member),
    -- generation is the member generation this link was created under.
    -- A link whose generation is behind the member's is stale and
    -- resolves to nothing.
    generation  INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    -- created_by is the administrator who approved it. Butler cannot
    -- grant itself manage authority (A2.1), and there is no code path
    -- that writes this row with the Butler as the creator.
    created_by  TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',
    CHECK (external_id <> ''),
    CHECK (created_by <> '')
);

-- One live link per (type, external id): a chat account belongs to one
-- person at a time. Revoked rows stay for the audit trail and are
-- excluded from the constraint.
CREATE UNIQUE INDEX butler_identity_links_live
    ON butler_identity_links (link_type, external_id) WHERE revoked_at = '';
CREATE INDEX butler_identity_links_by_member ON butler_identity_links (member, seq);

-- butler_conversations is the identity a conversation keeps across an
-- explicit handoff between bots. Sessions stay exactly as they were
-- (0003/0014 are untouched); this joins them.
CREATE TABLE butler_conversations (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    member     TEXT NOT NULL REFERENCES butler_members(member),
    created_at TEXT NOT NULL,
    subject    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX butler_conversations_by_member ON butler_conversations (member, seq);

CREATE TABLE butler_conversation_sessions (
    conversation INTEGER NOT NULL REFERENCES butler_conversations(seq),
    session      INTEGER NOT NULL REFERENCES butler_sessions(seq),
    attached_at  TEXT NOT NULL,
    PRIMARY KEY (conversation, session)
);

-- butler_handoffs records an explicit move between surfaces. It is a
-- row rather than a flag because a handoff is an event a person caused,
-- with a time and a direction, and an audit asks about it that way.
CREATE TABLE butler_handoffs (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation INTEGER NOT NULL REFERENCES butler_conversations(seq),
    member       TEXT NOT NULL,
    from_surface TEXT NOT NULL CHECK (from_surface IN ('admin', 'family')),
    to_surface   TEXT NOT NULL CHECK (to_surface IN ('admin', 'family')),
    from_session INTEGER REFERENCES butler_sessions(seq),
    at           TEXT NOT NULL,
    -- carried is how many turns the destination was given after
    -- filtering. Recorded because "no privileged context crossed" is a
    -- claim, and a claim with a number behind it can be checked.
    carried      INTEGER NOT NULL DEFAULT 0,
    withheld     INTEGER NOT NULL DEFAULT 0,
    CHECK (from_surface <> to_surface)
);

CREATE INDEX butler_handoffs_by_conversation ON butler_handoffs (conversation, seq);

-- Attribution recorded in history is never reinterpreted when a link
-- changes (A2.1). Links and handoffs are therefore append-only in the
-- same sense the trace is: revocation is a column, not an edit, and
-- there is no path that rewrites who did what.
CREATE TRIGGER butler_handoffs_immutable
BEFORE UPDATE ON butler_handoffs
BEGIN
    SELECT RAISE(ABORT, 'butler handoffs are append-only (ADR-W030 A2.1)');
END;

-- The member generation only moves forward, for the same reason every
-- other epoch here does: rewinding it revives access that was
-- deliberately withdrawn.
CREATE TRIGGER butler_members_generation_monotonic
BEFORE UPDATE OF generation ON butler_members
WHEN NEW.generation < OLD.generation
BEGIN
    SELECT RAISE(ABORT, 'butler member generation cannot move backwards (ADR-W030 A2.1)');
END;

-- An offboarded member cannot be re-admitted by flipping a column. It
-- takes an explicit new admission, which clears the offboarding and
-- bumps the generation — so re-admission is visibly a new decision
-- rather than an undo.
CREATE TRIGGER butler_members_offboarded_stays_denied
BEFORE UPDATE OF admitted ON butler_members
WHEN NEW.admitted = 1 AND OLD.offboarded_at <> '' AND NEW.offboarded_at <> ''
BEGIN
    SELECT RAISE(ABORT, 'an offboarded member cannot be re-admitted without a new admission (ADR-W030 A2.1)');
END;
