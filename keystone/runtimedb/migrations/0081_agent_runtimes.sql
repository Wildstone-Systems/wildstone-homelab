-- 0081: the Agent's runtime record and its runtime principal (ADR-W039
-- §3.3, §3.7, §4.1; Phase 1 of the migration plan in §8).
--
-- An Agent gains one `runtime` field: which kind of runtime backs it,
-- where that runtime is, which Keystone principal it authenticates as,
-- and how it is doing. Replacing the runtime replaces nothing else —
-- identity, members, tools, Connections, routines and memory all stay
-- on the Agent (§3.7) — so the record is a SIDE TABLE keyed by the
-- Butler rather than columns on `butlers`. `butlerColumns` is the join
-- every Butler read uses, and a runtime that lived there would be
-- scanned by every caller that never asked about it.
--
-- One row per Butler, never more: the primary key is the Butler.
CREATE TABLE butler_runtimes (
    butler            INTEGER PRIMARY KEY REFERENCES butlers(seq) ON DELETE CASCADE,
    -- type is the closed set the ADR names; each addition is a reviewed
    -- change here and in keystone/agentapi, which must agree.
    type              TEXT NOT NULL DEFAULT 'native'
                      CHECK (type IN ('native', 'hermes', 'openclaw')),
    -- endpoint is empty for native (in-process); a Route host or
    -- container/<name>:<port> for a packaged runtime.
    endpoint          TEXT NOT NULL DEFAULT '',
    -- principal is the CURRENT credential's name — the credentialRef of
    -- §3.3. It is a reference into runtime_principals below, not a
    -- foreign key, because a row exists before its first credential is
    -- minted and '' is the honest value then.
    principal         TEXT NOT NULL DEFAULT '',
    health            TEXT NOT NULL DEFAULT 'unregistered'
                      CHECK (health IN ('unregistered', 'connected', 'degraded', 'offline', 'revoked')),
    last_heartbeat    TEXT NOT NULL DEFAULT '',
    -- delivery and processing_class are the negotiated pair of §3.3.
    -- The processing class is declared by the Owner at registration and
    -- is never a claim the runtime makes; '' means undeclared, which for
    -- native means "read the assigned model's kind".
    delivery          TEXT NOT NULL DEFAULT 'push'
                      CHECK (delivery IN ('push', 'pull')),
    processing_class  TEXT NOT NULL DEFAULT ''
                      CHECK (processing_class IN ('', 'local_only', 'cloud')),
    -- execution_profile is #334's vocabulary and a reference here, not
    -- an enum: platform-only | sandboxed | host-admin today.
    execution_profile TEXT NOT NULL DEFAULT 'platform-only',
    updated_at        TEXT NOT NULL,
    updated_by        TEXT NOT NULL DEFAULT ''
);

-- The identity binding of §4.1: principal → Agent, stored here, and
-- IMMUTABLE for the principal's life. A credential that could be
-- re-pointed at another Agent would let one runtime act for a second
-- Agent by editing a row, which is exactly the authority the binding
-- exists to refuse. Rotation mints a successor and disables the
-- predecessor; it never rebinds.
--
-- Every credential the Agent ever held is a row, so the history of who
-- could act for it is evidence rather than something a rotation
-- overwrote.
CREATE TABLE runtime_principals (
    principal  TEXT PRIMARY KEY REFERENCES principals(name),
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    created_at TEXT NOT NULL
);
CREATE INDEX runtime_principals_by_butler ON runtime_principals (butler, created_at);

CREATE TRIGGER runtime_principal_binding_immutable
BEFORE UPDATE OF butler ON runtime_principals
WHEN NEW.butler <> OLD.butler
BEGIN
    SELECT RAISE(ABORT, 'a runtime principal is bound to one agent for its life; mint a new one (ADR-W039 §4.1)');
END;

-- Backfill: every Butler that exists today runs on the native runtime,
-- because until this migration there was no other kind.
--
-- A live Butler's native runtime is `connected` rather than
-- `unregistered`, and that is the truthful reading: the native runtime
-- is the in-process runner, which is loaded with the process and has
-- nothing to register or to heartbeat. `unregistered` would describe a
-- runtime that has yet to present itself, and this one cannot fail to.
-- A retired Butler's runtime is `revoked`, which is the state retiring
-- now leaves it in: nothing may act for a retired Agent.
--
-- The principal is NOT synthesised here. §3.3 says a native credential
-- is "minted, not synthesised", and minting goes through
-- keystone/principal's name discipline, which SQL cannot apply. The
-- process mints one for each live Butler that lacks one at startup,
-- the same way it enrols pre-0059 Agents in assigned tools.
INSERT INTO butler_runtimes (butler, type, principal, health, updated_at, updated_by)
SELECT seq, 'native', '',
       CASE WHEN retired_at = '' THEN 'connected' ELSE 'revoked' END,
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'migration'
  FROM butlers;
