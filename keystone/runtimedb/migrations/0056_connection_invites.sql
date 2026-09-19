-- A Person invited to a chat Connection (ADR-W033 B7.1).
--
-- When an Owner connects a Person to an Agent's bot, the Agent leaves the
-- Person a notice, and the Person fetches their single-use key from their
-- own signed-in page. This row is what says they were invited, and to
-- which Connection. It holds no key: the key is minted when the Person
-- asks for it, and stored only as a hash in chat_claim_codes like every
-- other.
--
-- Open until the Person binds an account on the Connection (closed by the
-- redemption itself) or an Owner withdraws it. One open invite per Person
-- per Connection.
CREATE TABLE connection_invites (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    connection    INTEGER NOT NULL REFERENCES butler_addresses(seq),
    person_id     TEXT NOT NULL REFERENCES butler_members(person_id),
    invited_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    closed_at     TEXT NOT NULL DEFAULT '',
    closed_reason TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX connection_invites_open
    ON connection_invites (connection, person_id) WHERE closed_at = '';
