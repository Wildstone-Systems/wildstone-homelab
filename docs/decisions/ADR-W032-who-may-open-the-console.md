# ADR-W032: Who may open the Console

- **Status:** accepted in part — B2 built, scoping not
- **Relates to:** ADR-W007 A2 (the closed RBAC table), ADR-W007 A3 (token
  minting), ADR-W026 A3 (chat claim codes bind a person to a surface),
  ADR-W030 A2.1 (offboarding immediately denies access), ADR-W031 A10
  (the Console holder owns every Butler — this would amend it)
- **Audience:** decision record. Nothing here is built.

---

# Part A — The finding

### A1. There are three identity systems, and nothing bridges the first to the second

A person in this house can exist in three places, and the three do not
know about each other.

| | What it is | Where it lives | What it grants |
|---|---|---|---|
| **LDAP user** | the household directory account | `KindLdapUser` in the Statefile, rendered to lldap | signing in to *apps*, through Authelia over OIDC |
| **Keystone principal** | an API identity with a role | `principals` table | everything the Console can do, via a bearer token |
| **Butler member** | an admission record | `butler_members` | being granted a Butler, and reaching it over chat |

On this deployment the directory holds `admin`, `giulia` and `mika`.
The principals table holds `admin` and four machine accounts — two chat
adapters, a publisher, a valet. `butler_members` holds `admin` and
`Admin`.

So `giulia` and `mika` are real people with real directory accounts who
have no principal and no admission. They cannot open the Console, and
they do not appear on a Butler's Reach tab.

The second of those is a data gap: Butler authority → *Record a person*
offers the directory as its choices precisely so this can be fixed in
four clicks. The first is not a gap in the data. It is a gap in the
design, and it is bigger than it looks.

### A2. Adding an LDAP group would do nothing

The natural first thought — declare a `wildstone-console` group, put
people in it — has no effect, because nothing reads groups when a
request is authenticated.

`keystone/api/api.go:547` is the whole authentication path. It takes a
bearer token, looks it up against the `principals` table, and returns
an identity or fails. There is no OIDC verification, no session cookie,
no directory lookup, and no group check anywhere in it.

Nor is the Console behind anything that could do the check for it.
Keystone serves the Console itself, from `-console-dir`, on its own API
listener. There is no Caddy route file for it, so Authelia never sees a
request for the Console at all — unlike every other app in the house,
each of which has a route like:

```
forward_auth authelia:9091 {
    uri /api/authz/forward-auth
    copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
}
```

The Console is the only thing in the deployment not sitting behind the
identity provider this house already runs, and which Keystone itself
renders the configuration for.

### A3. Authentication is the small half. Scope is the large one

**Implementation reconciliation (slice 24, #225).** The description below
records the Console at the time of this ADR. Later slices added Person and
audience checks to memory, tasks, and Plan inspection, but A3 is not fully
resolved: `listButlers` still returns every Agent, and `butlerScope` still
accepts any Agent name for a Console caller. These endpoints need explicit
Person-scoped review before broadening Console access. `RoleOperator` remains a
legacy machine/API principal role (`TestLegacyRoleOperatorIsUntouched`),
separate from the Person's Owner or Member role and Agent class. Its name
does not grant a human Owner status. New human authorization must use the
Person role and scoped checks, never infer it from `RoleOperator`.

This is the part that decides how much work the change is.

ADR-W031 A10 gives the Console holder **every** Butler, and the API is
built that way: `listButlers` returns all of them, `butlerScope` answers
for any name, `butlerMemory` is not filtered by caller. That is a
coherent design for a single-operator control plane, and it is exactly
what makes a second Console user dangerous.

If `giulia` signs in tomorrow, she sees Alfred and Operator — the
operator-surface Butlers that propose changes to the home lab — next to
every household Butler's memory and conversations. Not because a check
was missed, but because there is no check to miss: the endpoints were
written for a caller who is entitled to all of it.

There is also no role for her to hold. The RBAC table
(`keystone/principal/rbac.go`) is closed and has six entries. `admin`
holds everything. `operator` holds read, propose, **approve** and
converse — the ability to approve a plan, which is authority over the
home lab, not over a Butler. `butler`, `concierge`, `valet` and
`publisher` are machine roles. Nothing in the table describes "a person
who may see their own Butlers and nothing else."

**So the sequence is fixed: a household role and per-principal scoping
come first, and the authentication bridge is the last step, not the
first.** Building the bridge first produces a working sign-in to a
screen that shows everybody everything.

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

# Part B — Options for the bridge

Each assumes A3 is done first.

### B1. Mint a token for a directory user, from the Console

The Console gains a screen that mints a bearer token for a principal,
closing the `POST /api/v1/principals/{name}/tokens` gap already recorded
in `docs/console-coverage.md`. An operator creates a principal named for
the directory account, mints a token, and hands it over.

- **For:** no new authentication path, no new trust boundary, nothing to
  get wrong in the verification of an assertion. It also fixes the
  recovery problem in the coverage doc — an operator who has mislaid
  their token currently has no route back through the Console.
- **Against:** every household member now handles a bearer token, in
  chat or a password manager, with no expiry story and no revocation
  they can perform themselves. The directory account and the Console
  identity stay two separate things that happen to share a name, so
  removing somebody from the directory does not remove their Console
  access. That last point is the same class of failure as A4.

### B2. Forward-auth, and trust Authelia's assertion

Give the Console a Caddy route like every other app. Keystone gains a
second authentication path that accepts `Remote-User` and
`Remote-Groups` **only** from the proxy, maps a group to a role, and
constructs an identity from it.

- **For:** it is the pattern this house already runs, and Keystone
  already renders these route files itself. Sign-in, session lifetime,
  MFA and lockout all become Authelia's job, which is what Authelia is
  for. Removing somebody from the directory removes their Console access
  at the next request. An LDAP group finally *means* something, which is
  the thing that was expected to work in the first place.
- **Against:** trusting a header is only safe if the header cannot be
  set by a client. That means the API listener must not be reachable
  except through the proxy, or the trusted path must be a separate
  listener — and today `-api-listen 0.0.0.0:9441` is reachable directly
  on the LAN and the tailnet. Getting this wrong is total: anyone who
  can reach the port sets `Remote-Groups: admins` and is an
  administrator. The bearer path must also survive for the CLI and the
  adapters, so there would be two ways in, and the weaker one is
  whichever is misconfigured.

### B3. OIDC in Keystone

The Console performs an authorisation-code flow against Authelia
directly. Keystone validates the ID token, reads `groups`, and maps to a
role.

- **For:** no trusted-header boundary, so no way to forge an identity by
  reaching the port. Works the same whether the request arrives through
  the proxy or not, which suits a control plane that must stay reachable
  when the proxy is the thing that is broken.
- **Against:** the most code — client registration, discovery, key
  rotation, token validation, refresh, clock skew. It also puts a
  dependency on the identity provider into the control plane that
  administers it, and the recovery story then has to answer what happens
  when Authelia is down and the Console is how you would fix it. B1's
  token has to survive as the break-glass path either way.

---

# Part C — What was decided and built

**The group model, not per-person tokens.** Minting a bearer token by
hand for each person does not scale past the operator, has no expiry
story, and leaves a credential that outlives the directory account it
was minted for. Every other service in this house is reached by being in
a group a package declared. The Console is now one of them.

Built:

1. **`packages/wildstone-console`** — the Console is a package. It
   declares the `console_users` LdapGroup and a Route with
   `auth: authelia_oidc`, pinned to port 47443 in
   `keystone/ingressorigin`. It installs nothing; Keystone still serves
   the Console itself. What it adds is the two declarations that were
   missing — who may reach it, and how a browser gets there.
2. **Forward-auth in `authenticate()`** — a request with no bearer
   token, arriving from the trusted ingress, carrying `Remote-User` and
   a `Remote-Groups` list containing `console_users`, is authenticated
   as that person with admin authority. The identity is ephemeral: no
   principal row, because the group IS the permission and a persisted
   row would be a second place to revoke and a second place to forget.

Deliberately not built: **per-principal scoping (A3)**. The Console
group is therefore all-or-nothing — everyone in it is an administrator
and sees every Butler. That is a decision, not an oversight: a narrower
role without scoping would ship a screen that renders and then answers
403 to every control, because the Butler endpoints are almost entirely
manage-verb. The narrower group becomes possible once A3 is done.

### The trust boundary is shared, and that is worth knowing

`-household-ingress` names one proxy and now gates two things: the
household review page, and Console sign-in. Enabling it for the first
does not grant anyone the Console — that still needs `console_users`
membership, which an operator controls — but the coupling is real and
the flag says so, as does the startup log.

The peer check is an exact remote-address match, so the header cannot be
set by an arbitrary caller on the network: they would have to originate
a connection appearing to come from the proxy's own address. The
residual risk is a local process on the proxy's host, not the LAN at
large.

# Part D — What is still open

1. **A4** — normalise member names. `Admin` and `admin` are two records
   for one person and offboarding one leaves the other admitted.
2. **A3** — per-principal scoping, which amends ADR-W031 A10 from "the
   Console holder owns every Butler" to "a caller sees the Butlers they
   hold". Until then the Console group is all-or-nothing.
3. **A household role** in the RBAC table, which only becomes useful
   after A3.
4. **Token minting in the Console** — still the recorded gap in
   `docs/console-coverage.md`, and still the break-glass path for when
   the identity provider is the thing that is broken.

# Part E — Superseded recommendation



**B2, with B1 kept as break-glass, and A3 done before either.**

The order matters more than the choice:

1. **A4** — normalise member names. Small, independent, and it is a
   revocation hole today.
2. **A household role** in the RBAC table: read, converse, and nothing
   else. No propose, no approve, no manage.
3. **Per-principal scoping** of the Butler endpoints, which amends
   ADR-W031 A10 from "the Console holder owns every Butler" to "a
   caller sees the Butlers they hold, and an admin holds all of them".
   This is the real work, and it is worth doing for its own sake: it is
   also what makes the audit trail answer "who could have seen this".
4. **B1**, because it is small, closes a recorded gap, and is the
   recovery path the deployment currently lacks.
5. **B2**, once the API listener's exposure is settled — the trusted
   header is only as good as the guarantee that a client cannot set it.

The thing not to do is B2 first because the infrastructure is already
there. It is, and that is exactly what makes it tempting: it would take
an afternoon and produce a Console that signs `giulia` in and shows her
everything.

---

# Part D — What is deliberately not decided here

- **Whether a household member should open the Console at all.** They
  may not need to. Everything a household member does today happens over
  chat, and A3's work may be better spent on making that surface fuller
  rather than on a second door into the operator's screen. That is a
  product decision and this document does not make it.
- **What a household member's Console would show.** If the answer to
  the above is yes, "the operator's screen with rows removed" is
  probably the wrong shape.
