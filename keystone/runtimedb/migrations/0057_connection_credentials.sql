-- Which platform credential each chat bot runs on, as a fingerprint
-- (ADR-W033 B7.1).
--
-- Two bots on one token are one bot polled twice: Telegram hands each
-- update to whichever poller asks, so each bot steals the other's
-- messages and a person's key lands at the wrong one. Keystone holds no
-- credential values, so it records a SHA-256 fingerprint of the token
-- when a bot is made — enough to refuse the same token for a second
-- bot, and nothing that could be used as the token. Bot tokens are long
-- random strings; a fingerprint of one does not lead back to it.
CREATE TABLE connection_credentials (
    connection  INTEGER PRIMARY KEY REFERENCES butler_addresses(seq),
    fingerprint TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE INDEX connection_credentials_fingerprint ON connection_credentials (fingerprint);
