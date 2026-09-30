-- 0098: Personal mailbox credentials materialized on nodes, their
-- verified erasure, and the Person's authorization for unattended use
-- (epic #326 S02; ADR-W030 A5.1/A5.4, ADR-W033 D1 amendment).
--
-- Three things this migration adds, and one it deliberately does not.
--
-- It does NOT add a column for a credential value. The value is typed by
-- its Person into the protected intake, forwarded once over mTLS to the
-- node's write-only store, and forgotten. Nothing here can hold it, and
-- nothing that could is added "for later".

-- 1. A credential now has a lifecycle. Before this, integration_secrets
--    only recorded that a reference existed ('recorded', the default for
--    every earlier row, which keeps its old meaning). A credential
--    onboarded through the intake is STAGED while it is being sent to
--    the placement nodes, ACTIVE once every node holds it, SUPERSEDED
--    when a replacement becomes active, REVOKED when its Person (or an
--    Owner administering, offboarding, or a restore replay) withdraws
--    it, and FAILED when staging did not reach every node.
--
--    version counts credentials per connection and need. login is the
--    account NAME the adapter signs in as — not secret, but personal,
--    and only ever shown to its Person.
ALTER TABLE integration_secrets ADD COLUMN need TEXT NOT NULL DEFAULT '';
ALTER TABLE integration_secrets ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE integration_secrets ADD COLUMN login TEXT NOT NULL DEFAULT '';
ALTER TABLE integration_secrets ADD COLUMN state TEXT NOT NULL DEFAULT 'recorded'
    CHECK (state IN ('recorded', 'staged', 'active', 'superseded', 'revoked', 'failed'));

-- One live credential per connection and need. A second active one would
-- be two passwords the adapter might be using, and "which one did it
-- read with" is not a question an erasure should have to answer.
CREATE UNIQUE INDEX integration_secrets_one_active
    ON integration_secrets (connection, need) WHERE state = 'active';

-- 2. Where each credential version was SENT, and whether its erasure has
--    been PROVEN there. A row is written before the value leaves
--    Keystone ('sending'), so a crash mid-fan-out still leaves a record
--    that something may exist on that node and has to be erased.
--
--    erase_state is empty while the copy is wanted, 'pending' from the
--    moment it is fenced, 'erased' only when the node returned evidence
--    that verifies (secreterase.Verify) AND the adapter container that
--    could have held the value in memory is gone, and 'unverified' when
--    either could not be shown — the connection stays degraded until
--    every row is 'erased'.
CREATE TABLE integration_secret_copies (
    secret         INTEGER NOT NULL REFERENCES integration_secrets(seq),
    node           TEXT NOT NULL,
    provision_state TEXT NOT NULL CHECK (provision_state IN ('sending', 'provisioned', 'failed')),
    -- The store version the node reported for this name; the erase is
    -- fenced to it so it can never destroy a newer write.
    node_version   INTEGER NOT NULL DEFAULT 0,
    sent_at        TEXT NOT NULL,
    erase_state    TEXT NOT NULL DEFAULT '' CHECK (erase_state IN ('', 'pending', 'erased', 'unverified')),
    -- The connection lease epoch that fenced this copy. Recorded so the
    -- record shows the fence came first.
    fenced_epoch   INTEGER NOT NULL DEFAULT 0,
    erase_requested_at TEXT NOT NULL DEFAULT '',
    erased_at      TEXT NOT NULL DEFAULT '',
    -- hex(nonce) ":" hex(evidence) of the verified erase. Evidence, not
    -- a value: it is a digest over the name, the nonce and "absent".
    evidence       TEXT NOT NULL DEFAULT '',
    erase_detail   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (secret, node),
    CHECK (node <> '')
);

-- A proven erasure stays proven. Reopening one would let a stale restore
-- or a retry race turn "gone" back into "we should check".
CREATE TRIGGER integration_secret_copies_erased_final
BEFORE UPDATE OF erase_state ON integration_secret_copies
WHEN OLD.erase_state = 'erased' AND NEW.erase_state <> 'erased'
BEGIN
    SELECT RAISE(ABORT, 'a proven credential erasure cannot be reopened (ADR-W030 A5.1)');
END;

-- 3. The adapter executable a connection's placements run, as
--    "<package>@<version>/<program>". Part of what a Person authorizes:
--    a different executable is different behaviour, so a change
--    invalidates the authorization (and the Owner's approval with it).
ALTER TABLE integration_connections ADD COLUMN executable TEXT NOT NULL DEFAULT '';

-- 4. A Person's authorization for UNATTENDED use of their own Personal
--    credential (ADR-W033 D1 amendment). Before this, an unattended run
--    could not use a Personal secret at all; this is the narrow,
--    exact-version delegation D1 said would need its own design.
--
--    Every material field is a column, and the digest is computed over
--    all of them. A subscription the Owner approves names that digest,
--    so the Owner's approval covers exactly what the Person authorized
--    and nothing else; the Person's row names the subscription and its
--    content digest, so the Person's authorization covers exactly what
--    the Owner approved. Background use is the INTERSECTION, evaluated
--    live on every dispatch (runtimedb.AuthorizeBackgroundUse).
--
--    Rows are immutable except for revocation (trigger below). A
--    material change is a NEW row, which is what makes "a material edit
--    invalidates the authorization" structural rather than a rule each
--    writer has to remember.
CREATE TABLE personal_background_authorizations (
    seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
    connection          INTEGER NOT NULL REFERENCES integration_connections(seq),
    -- The Person who authorized. Always the connection's owner: nobody
    -- else's authorization means anything for a Personal credential.
    person              TEXT NOT NULL,
    credential          INTEGER NOT NULL REFERENCES integration_secrets(seq),
    credential_version  INTEGER NOT NULL,
    -- Canonical JSON of the source scope (folders, sender and subject
    -- filters), sorted and de-duplicated before it is stored.
    source_scope        TEXT NOT NULL,
    processing_location TEXT NOT NULL CHECK (processing_location IN ('local', 'cloud')),
    -- Who may receive or learn from results: the owning Person alone.
    result_audience     TEXT NOT NULL CHECK (result_audience = 'aud_owner'),
    -- The Agent (butlers.seq) that runs it.
    agent               INTEGER NOT NULL,
    target_calendar     TEXT NOT NULL,
    executable          TEXT NOT NULL,
    -- The Owner-approved subscription this is for, and a digest of its
    -- content at authorization time (approval fields excluded, since the
    -- Owner approves after the Person authorizes).
    subscription        INTEGER NOT NULL,
    subscription_digest TEXT NOT NULL,
    -- sha256 over every material field above except the subscription
    -- binding. This is what the subscription names.
    digest              TEXT NOT NULL,
    created_at          TEXT NOT NULL,
    revoked_at          TEXT NOT NULL DEFAULT '',
    revoked_by          TEXT NOT NULL DEFAULT '',
    revoked_reason      TEXT NOT NULL DEFAULT '',
    CHECK (person <> ''),
    CHECK (source_scope <> ''),
    CHECK (target_calendar <> ''),
    CHECK (executable <> ''),
    CHECK (digest <> ''),
    CHECK (revoked_at = '' OR revoked_reason <> '')
);

-- One live authorization per connection: one mailbox, one monitor.
CREATE UNIQUE INDEX personal_background_authorizations_live
    ON personal_background_authorizations (connection) WHERE revoked_at = '';

CREATE TRIGGER personal_background_authorizations_immutable
BEFORE UPDATE OF connection, person, credential, credential_version, source_scope,
    processing_location, result_audience, agent, target_calendar, executable,
    subscription, subscription_digest, digest, created_at
ON personal_background_authorizations
BEGIN
    SELECT RAISE(ABORT, 'a background authorization cannot be edited; a material change is a new authorization (ADR-W033 D1)');
END;

-- Revocation is final. A restored or replayed row cannot be revived by
-- clearing revoked_at.
CREATE TRIGGER personal_background_authorizations_revocation_final
BEFORE UPDATE OF revoked_at ON personal_background_authorizations
WHEN OLD.revoked_at <> '' AND NEW.revoked_at <> OLD.revoked_at
BEGIN
    SELECT RAISE(ABORT, 'a revoked background authorization stays revoked (ADR-W033 D1)');
END;
