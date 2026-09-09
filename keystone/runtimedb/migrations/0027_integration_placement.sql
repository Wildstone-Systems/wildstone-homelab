-- 0027: adapter placement, credentials, and failover (ADR-W030 A5/A5.1,
-- work package P11).
--
-- A connection names one primary adapter node and an ordered set of
-- explicitly approved fallbacks. The word doing the work is PREPARED: a
-- node is eligible only after the approving Plan has deployed and
-- started its adapter container, provisioned the connection's secrets on
-- it, and verified readiness. Until all three are true it is not a
-- fallback, it is an intention.
--
-- That is what makes failover safe to automate. Keystone may move the
-- LEASE among prepared nodes and may do nothing else: it never creates a
-- container, never selects an undeclared node, and never copies a
-- credential during an outage — because all three of those are exactly
-- what you would want to do at 3am and exactly what turns one outage
-- into a credential on a machine nobody meant to put it on.
--
-- When no node is eligible, dependent work WAITS and Butler reports the
-- outage. Waiting is the correct answer; improvising is not.

CREATE TABLE integration_placements (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    node       TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('primary', 'fallback')),
    -- rank orders the fallbacks. The primary is rank 0.
    rank       INTEGER NOT NULL DEFAULT 0,

    -- The three facts that together make a node eligible. Each is
    -- recorded separately because each fails separately, and a node that
    -- has a container but no secret is a different problem from one with
    -- a secret and no container.
    container_running   INTEGER NOT NULL DEFAULT 0,
    secret_provisioned  INTEGER NOT NULL DEFAULT 0,
    readiness_verified  INTEGER NOT NULL DEFAULT 0,

    -- healthy_since supports the stabilization period: automatic
    -- failback waits for a node to be healthy for a while, so a
    -- flapping primary does not drag the lease back and forth.
    healthy_since TEXT NOT NULL DEFAULT '',
    last_health   TEXT NOT NULL DEFAULT '',
    -- holder marks the node currently holding the lease. Exactly one per
    -- connection, enforced by the partial unique index below.
    holder     INTEGER NOT NULL DEFAULT 0,

    added_at   TEXT NOT NULL,
    added_by   TEXT NOT NULL,
    -- Removal is not a DELETE. It is fenced, then proven: the container
    -- stopped and removed, the local secret erased, and where the remote
    -- service permits it, the node's credential revoked. Until all of
    -- those are proven the row stays, marked removing, and the
    -- connection reports degraded — because a credential that might
    -- still exist on a machine you have stopped watching is worse than
    -- one you know about.
    removing_at        TEXT NOT NULL DEFAULT '',
    container_removed  INTEGER NOT NULL DEFAULT 0,
    secret_erased      INTEGER NOT NULL DEFAULT 0,
    credential_revoked INTEGER NOT NULL DEFAULT 0,

    CHECK (node <> ''),
    CHECK (added_by <> '')
);

CREATE UNIQUE INDEX integration_placements_node ON integration_placements (connection, node);
-- One holder per connection. A second would be two adapters claiming the
-- same work, which is the duplicate this whole design exists to prevent.
CREATE UNIQUE INDEX integration_placements_holder
    ON integration_placements (connection) WHERE holder = 1;

-- integration_secrets records that a credential EXISTS and where it
-- lives. It never records the value.
--
-- There is deliberately no value column, and no nullable one that a
-- future change could start filling: ADR-W012's write-only secret path
-- holds the material, this holds a reference, and a column here would be
-- a place for a password to be written by accident during a debugging
-- session at midnight.
CREATE TABLE integration_secrets (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    -- member scopes a PERSONAL credential — an IMAP or CalDAV account
    -- that belongs to one person, onboarded through the authenticated
    -- secret-input surface and never collected through chat (A5.1).
    -- Empty means a household-level connection credential.
    member     TEXT NOT NULL DEFAULT '',
    -- secret_ref is the name in the secret path. A reference, resolved
    -- on the node into the adapter's scoped environment; it never
    -- travels through a work or result payload and never reaches a
    -- model.
    secret_ref TEXT NOT NULL,
    -- read_only records whether the upstream credential is genuinely
    -- read-only. Where a provider cannot issue one, adapter operations
    -- are enforced read-only and the credential's wider power is
    -- DISCLOSED rather than quietly accepted (A5.1).
    upstream_read_only INTEGER NOT NULL DEFAULT 0,
    disclosure TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',

    CHECK (secret_ref <> ''),
    CHECK (created_by <> ''),
    -- A credential that is not read-only upstream must say so. An
    -- undisclosed wider power is the thing a person would have wanted to
    -- know before handing it over.
    CHECK (upstream_read_only = 1 OR disclosure <> '')
);

CREATE UNIQUE INDEX integration_secrets_ref ON integration_secrets (connection, member, secret_ref);
