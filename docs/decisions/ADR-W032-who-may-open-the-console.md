# ADR-W032: Who may open the Console

- **Status:** accepted; amended by ADR-W033 (A1, A4, A4.1, A7). Console
  sign-in over OIDC is built; member-name normalisation (A4) is not.
- **Governed by:** ADR-W033 where they overlap.
- **Relates to:** ADR-W007 A2 (the closed RBAC table), ADR-W007 A3 (token
  minting), ADR-W030 A2.1 (offboarding immediately denies access)
- **Audience:** decision record.

---

# Part A — The finding

### A1. Three identity records, one Person

A person in this house can appear in three places:

| | What it is | Where it lives | What it grants |
|---|---|---|---|
| **LDAP user** | the household directory account | `KindLdapUser` in the Statefile, rendered to lldap | signing in to *apps*, through Authelia over OIDC |
| **Keystone principal** | an API identity with a role | `principals` table | bearer-token access for the CLI, installer, adapters and other machines |
| **Person / Butler member** | the household's record of the person, with their Owner or Member role | `butler_members` | household standing, Agent admission, and — for an Owner — the Console |

A directory account on its own grants no Console access. The Person
record and its role are what decide (Part C).

### A2. A directory group is not a permission

Every other app in the house is reached by being in a group a package
declared. For the Console that is not enough: Wildstone, not the
directory, is the authority on who administers it (ADR-W033 A4). A
group membership may be a precondition the ingress checks; it never
makes anyone an Owner.

### A3. Console access is Owner-only

The Console is the control plane and only Owners may enter it. There is
no household role, delegated Member exception or per-caller Console
scoping: a Person who can enter the Console is an Owner, and an Owner
administers every Agent (ADR-W033 A4, B1).

`RoleOperator` is a legacy machine/API principal role, separate from the
Person's Owner or Member role and from Agent class. Its name never
implies a human Owner; human authorization reads the Person role
(`TestLegacyRoleOperatorIsUntouched`).

_Changed by ADR-W033 A1/A4: two human roles, and Console is Owner-only._

### A4. One thing to fix regardless: a member is one string, compared exactly

`butler_members.member` is `TEXT NOT NULL UNIQUE`, and SQLite collates
case-sensitively by default. Every lookup in
`keystone/runtimedb/butleridentity.go` is `WHERE member = ?`.

This deployment has both `Admin` (household) and `admin` (operator), and
`butler_access` grants `Sandy` and `Operator` to both. They are two
admission records, with two generations, for one person.

That matters beyond tidiness. ADR-W030 A2.1 says offboarding is one
audited action that **immediately denies access**. Offboarding `admin`
bumps that record's generation and revokes its access rows; `Admin`
keeps its admission and its grants. The operator would reasonably
believe the revocation was complete. It would not be.

The fix is to normalise on write, or to make the uniqueness constraint
`COLLATE NOCASE` and migrate the existing pair. It is small, and it is
independent of everything else here.

---

# Part B — Mechanisms considered

### B1. Mint a token for a directory user, from the Console

Minting a bearer token per person was rejected as the way people reach
the Console: it has no expiry story, and the credential outlives the
directory account it was minted for.

Minting any bearer credential (Console and principal tokens, join
tokens) is a consequential operation and needs a fresh Owner session
(ADR-W033 A4.1). Break-glass recovery is the host CLI run by a
root-privileged caller (ADR-W033 A7), not a Console-minted token.

### B2. Forward-auth, and trust Authelia's assertion

Forward-auth headers (`Remote-User`, `Remote-Groups`) say who is asking,
from the trusted ingress only. They never make a session fresh and never
grant Console authority on their own.

_Changed by ADR-W033 A4.1: freshness comes from a verified ID token, not a header._

### B3. OIDC in Keystone

This is the chosen mechanism. The Console signs in as an OpenID Connect
client of Authelia:

- a **public client with PKCE** — Keystone holds no client secret;
- Keystone verifies the ID token against Authelia's published keys and
  reads **`auth_time`** from it; a future-dated `auth_time` is never
  fresh;
- step-up is a new authorization request with **`prompt=login`**;
- staying in the Console needs an authentication no older than **12
  hours**; a consequential operation needs one no older than **15
  minutes**; both are configurable.

Bearer tokens remain for the CLI, installer and host recovery, and are
exempt from the freshness gate (ADR-W033 A4.1).

---

# Part C — What is decided and built

A request reaches the Console as a person only when all of these hold:

1. it arrives from the trusted ingress;
2. it names a user, and that user is an active **Owner** Person in
   Wildstone's own record — not disabled, offboarded or a Member;
3. it carries a Console session created from a verified ID token whose
   `auth_time` is inside the 12-hour window (B3).

Consequential operations additionally require the 15-minute window.
Demoting an Owner removes Console access immediately (ADR-W033 A6).

`packages/wildstone-console` declares the Console's route and OIDC
client. Its `console_users` LdapGroup is a precondition the ingress
path checks, not the permission (A2).

**A household Member does not open the Console.** Members reach Member
Agents on Homepage and over their Connections (ADR-W033 A4, B8). Homepage
shows the Console link to Owners only.

### The trust boundary is shared, and that is worth knowing

`-household-ingress` names one proxy and gates Console sign-in along
with other household routes. The peer check is an exact remote-address
match, so the forwarded identity cannot be set by an arbitrary caller on
the network: they would have to originate a connection appearing to
come from the proxy's own address. The residual risk is a local process
on the proxy's host, not the LAN at large — and even there, a forwarded
name alone gets no Console session without a verified ID token.

# Part D — What is still open

1. **A4** — normalise member names. `Admin` and `admin` are two records
   for one person and offboarding one leaves the other admitted.
2. **Console token minting** — still the recorded gap in
   `docs/console-coverage.md`; when built it is consequential and needs
   a fresh Owner session (B1).
