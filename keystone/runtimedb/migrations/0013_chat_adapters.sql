-- 0013: which bot an adapter actually is (ADR-W026 A3/A4).
--
-- An adapter knows one thing nobody else in the deployment does: the
-- handle its bot answers on. Keystone holds the bot's TOKEN as a
-- write-only secret and can never read it back (ADR-W012), and the
-- catalog knows the transport but not the account — so without the
-- adapter saying so, nothing here can name the bot a person is supposed
-- to message, and an invite can only be "send this code to the household
-- bot" and hope they know which one that is.
--
-- One row per capability, because one adapter serves one surface on one
-- transport (ADR-W026 A3). The adapter cannot name a capability: the row
-- is keyed from its own principal, so a compromised adapter can lie
-- about its handle and not about which bot it is.
--
-- seen_at doubles as a liveness signal. An adapter re-registers on a
-- heartbeat, so a row that has gone stale is how the Console knows the
-- bot is not running — which otherwise looks exactly like a bot nobody
-- has messaged yet.
CREATE TABLE chat_adapters (
    capability   TEXT PRIMARY KEY,
    transport    TEXT NOT NULL,
    surface      TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    handle       TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    seen_at      TEXT NOT NULL
);
