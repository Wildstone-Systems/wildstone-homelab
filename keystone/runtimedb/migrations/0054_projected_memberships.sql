-- 0054: what Wildstone put in the directory (ADR-W033 design §4; work
-- slice 8 of the agent-system epic, second half).
--
-- Projection has to be able to REMOVE a directory membership. Until now
-- nothing could, and that was deliberate in three separate places — the
-- provisioning script never removes one, LdapGroupSpec carries no
-- roster, and the members API refuses a request that drops a group. The
-- rationale is recorded on LdapGroupSpec and worth quoting, because it
-- names a real failure rather than an inconvenience:
--
--     The household knows who lives in it; the control plane does not,
--     and a system that thinks it does is one that removes your sister
--     from photos_users because a YAML file did not mention her.
--
-- §4 makes Wildstone the source of truth and requires it to project
-- effective membership, so the additive-only posture cannot stand. This
-- table is how the reversal keeps that failure impossible anyway.
--
-- WILDSTONE OWNS WHAT IT PLACED, AND NOTHING ELSE. Every membership
-- projection asserts is recorded here. On the next pass it may remove a
-- membership in this table that it no longer wants — it put that one
-- there. A membership NOT in this table was added by a person, in
-- LLDAP's own UI, and projection reports it and leaves it alone. The
-- sister stays in photos_users, because Wildstone never claimed her.
--
-- Which is also why this is a table rather than a derivation. "What we
-- placed" is not recoverable from the current desired state: the moment
-- an entitlement is withdrawn, the membership it justified is exactly
-- the one no longer derivable, and the one that needs removing.
CREATE TABLE projected_memberships (
    -- The Person, by id. Projection resolves a directory account to a
    -- Person by handle and refuses to act when that is ambiguous; what
    -- it records is the identity, never the name it matched on.
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    -- The DIRECTORY group's name, e.g. 'docs_users' — not a slice-6
    -- group_id. This table is a record of what exists in LLDAP, and
    -- LLDAP has never heard of a group_id.
    ldap_group TEXT NOT NULL,
    placed_at  TEXT NOT NULL,
    PRIMARY KEY (person_id, ldap_group),
    CHECK (ldap_group <> '')
);

-- Read whole on every projection pass, and written rarely.
CREATE INDEX projected_memberships_by_group ON projected_memberships (ldap_group);
