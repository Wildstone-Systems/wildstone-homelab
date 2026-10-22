# ADR-W020: Identity onboarding — the first admin, and how a household gets accounts

- **Status:** proposed; A1, A2 and A4 amended by ADR-W033
- **Governed by:** ADR-W033 where they overlap.
- **Relates to:** ADR-W007 (principals, bootstrap token), ADR-W012
  (secrets never read back), ADR-W016 (Console IA), ADR-W019 A5
  (legibility over scheduling), ADR-W005 A2 (`.Resolved` stays small)
- **Audience:** decision record AND implementation spec for the identity
  work package's human half.

## Constraints from ADR-W030 and ADR-W033

- **Links are explicit.** Every authenticated binding — directory login,
  API principal, chat binding, trusted-ingress identity — attaches to
  exactly one canonical Person (ADR-W033 A2). These are distinct binding
  types, and equal names or LDAP group membership are not identity proof.
  The runtime projection carries a generation checked on every retrieval
  and dispatch.
- **An Owner administers identity directly**, in Console or the host
  CLI, not through a Plan. Admission is an ordinary operation; changing
  a Person's role, disabling or offboarding a Person are consequential
  and need a fresh Owner session (ADR-W033 A4.1). Chat bindings are made
  only by the Person's own invite-key enrolment (ADR-W033 B7.1).
- **Unlink, disable and offboarding take effect first.** Active
  authority is removed immediately: sessions and bindings revoked,
  admission withdrawn, unconsumed Person-bound approvals invalidated,
  queued and in-flight work stopped at its next boundary. Immutable
  historical attribution is preserved. Household routines keep running;
  only a routine that delivers to the departed Person, or needs an
  admission only they held, pauses for Owner resolution (ADR-W033 F1).
- **Offboarding resolves durable resources explicitly.** Owners see the
  Person's inventory and choose transfer, retain, export, retire, block
  or delete for each (ADR-W033 F2). Shared records retain original
  authorship. The Person is soft-deleted until an Owner purges them
  (ADR-W033 F6).
- **Children are never admitted.** A child is a Member profile recorded
  on a Person with an explicit guardian, as an un-admitted scheduled
  subject with no authentication binding (ADR-W033 A8, A9). Child
  classification and guardian mapping are explicit records, never
  inferred from an LDAP group. A sole guardian cannot be removed while
  the child record is active. The ADR-W030 A2.1 Child-surface gate
  stands.
- **Only a human Owner approves Plans**, including household routines,
  through the Owner Review Queue (ADR-W033 E2, E3). There is no Member
  review page and no review service principal.
- **Export.** While still admitted, a person may export their own
  conversations, personal memory/preferences/procedures, and task
  history with provenance/permissions. The export follows current read
  policy and excludes credentials, Owner-only context, and other
  people's private data.
- **Personal integration-connection credentials** (CalDAV, IMAP) are
  Personal-scope secrets in the Household/user secret store, not the
  node secret store (ADR-W033 D1). In MVP a Member proposes them and
  uses authorized ones through tools (D5). Adapters receive scoped
  references or materialized values below inference; chat, model
  context and work queues never receive the secret (D2), and unattended
  routines cannot use it (D1). Revocation immediately blocks future
  ingestion and dispatch.

---

# Part A — Decisions (binding)

### A1. One Person, two kinds of access, and the product says so

Every human is one canonical Person with one of two roles, Owner or
Member (ADR-W033 A1, A2). An LLDAP account, an API principal and a chat
binding are bindings attached to that Person, never separate people.

What a Person may do still comes from two different places, with
different lifetimes and blast radii: **service entitlement** — which
household apps they may log into — comes from access groups projected
into the directory (ADR-W033 A5); **Console authority** comes only from
the Owner role (ADR-W033 A4).

Presenting them as one thing is the tempting simplification and it is
wrong in a way that surfaces late: revoking a family member's photo
access would revoke their ability to administer the house, or — far
worse the other way — adding a teenager to the media group would hand
them an approval verb. The Console shows role and entitlements
separately and names them.

_Changed by ADR-W033 §A1–A2: human roles are Owner/Member on one Person; API principals are bindings, not a second identity._

### A2. The installer creates the first Owner; the host mints bearer tokens offline

The installer asks for a username and password and creates the first
Owner and their directory identity before setup completes (ADR-W033
A3). There is no Console first-run screen and no first-visitor claim.

A token printed once to the journal is not a recovery path: scrubbing
the journal loses it, and a journal that outlives a reinstall returns
tokens from installs that no longer exist (friction log F44, F66). So
`keystone claim-code --data-dir` is a root-only host tool: with Keystone
stopped, it mints a bearer token into a root-readable file in the data
dir, optionally with a TTL. The installer uses it to drive the install
and deletes the file when done; host recovery uses it or
`keystone owner` (ADR-W033 A7). It is never a way into the Console.

Possession of a file readable only by root is the same proof of
physical access the join-token pattern already relies on (ADR-W003),
so this adds no new trust assumption — it only makes the existing one
recoverable.

_Changed by ADR-W033 §A3, §A7: the first Owner is created by the installer, not claimed in a browser._

### A3. Users are a resource kind: `LdapUser`

`LdapGroup` already exists and users are its obvious sibling. Creating
people through a live LLDAP API instead would put the household's
account list outside the Statefile, outside plans, outside approval and
outside audit — and a Statefile restore would then bring back every
group with nobody in them, which reads as a working system and is not
one.

`LdapUser` declares that a person **exists** and which groups they
belong to. Like `LdapGroup` it is declared-only: no operation removes a
user as a side effect of a spec change, for the reason ADR-W012 gives
about destroying things a control plane cannot recreate.

### A4. Credentials are never declarative

A password cannot live in a spec, because specs live in the Statefile
and the Statefile is replicated, restored and read. `LdapUser` therefore
carries existence and membership and never a credential.

The first Owner's password is collected by the installer, set directly
on their directory user, held only for the length of the run, and never
written to the answers file or a spec (A2).

Everyone else's first credential is a **single-use claim code**,
generated on the node and stored as a `Secret` — the same path every
other secret takes, so it inherits ADR-W012's guarantee that Keystone
cannot read it back. The person exchanges it for a password they
choose (A5–A7).

_Changed by ADR-W033 §A3: a claim code cannot bootstrap the first Owner, because the claim surface needs an Owner to exist first._

### A5. The claim surface is the ingress, not the Console

The Console is the admin plane: plans, approvals, secrets, nodes. A
family member setting their own password must never authenticate to it.
The claim page is served by the ingress alongside the apps, and holds
exactly one capability: exchange a valid code for a password.

### A6. Delivery is out-of-band and human, by design

There is no mail relay and no public endpoint, and there will not be
one. There is also no remote recipient: the household is the delivery
channel. So the Console renders a new user's claim code **twice** — as
short speakable text, and as a QR code encoding the claim URL.

The QR is the load-bearing half. A non-technical family member points a
phone camera and lands on the page; nobody transcribes a URL or reads a
token aloud character by character. On a tailnet profile the same code
works from outside the house, with no change.

**Rejected: email invites** — no relay exists, and adding one makes an
identity provider that will not start when SMTP is unreachable, which
the identity package already deliberately avoids
(`disable_startup_check: true`). **Rejected: a public invite endpoint** —
it would be the only publicly-reachable write surface in the product,
to save a household from walking into the next room.

### A7. Drive Authelia's reset flow rather than building a second one

The identity package already configures Authelia's filesystem notifier
(`/config/notification.txt`) — the documented no-SMTP path, where
Authelia writes what it would have emailed. It is already pointed at a
file the agent can read. Directly above it, `password_reset: disable:
true` switches the flow off, so nothing is ever written there.

Enable it, and read the file. A second credential path owned by
Wildstone would have to reimplement expiry, single-use, rate limiting
and password policy that Authelia already implements and that a
household's security actually rests on.

### A8. A dead identity provider says so

Per ADR-W019 A5, ordering across capabilities stays convergent. The
claim page must therefore state plainly that identity is not yet serving
rather than presenting a login that fails or a spinner that never ends.
A claim page is exactly where an unhelpful failure costs the most
trust, because the person has nothing working yet to reason from.

---

# Part B — Implementation notes (non-binding)

- **B1. `LdapUser` spec**: `username`, `displayName`, `email`,
  `groups[]`. Validation mirrors `LdapGroupSpec` — the provisioning
  script interpolates these into GraphQL, so the charset must stay
  closed for the same reason.
- **B2. Provisioning reuses the existing job.** `lldap-provision-groups`
  already logs in, reads what exists and creates only what is missing.
  Users are the same shape; extend it rather than adding a second job
  with a second copy of the retry-and-login logic.
- **B3. Claim code properties**: single-use, short TTL (hours, not
  days), rate-limited per code and per source. Codes are compared in
  constant time; a code that has been spent is indistinguishable from
  one that never existed.
- **B4. QR rendering is client-side** in the Console, from the claim
  URL. No new dependency reaches the control plane, and the code never
  transits an image service.
- **B5. `notification.txt` is not reachable as written.** It lives at
  `/config/notification.txt`, inside the `authelia-config` volume, and
  both managed-prefix mounts on the Authelia container are
  `readOnly: true` — so the agent cannot read it and Authelia cannot be
  pointed at the prefix instead without a write path. This wants the
  same treatment ADR-W019 B2 gave Caddy's PKI: a subtree of the managed
  prefix made writable for the identity container specifically, with the
  notifier pointed there. The alternative — a new op that reads a file
  out of a volume — is the one W019 B2 explicitly declined to add, and
  declining it twice for the same reason is the consistent answer.
- **B6. Sequencing**: A2 first — it is self-contained, it unblocks
  automated setup immediately, and it removes the single worst step in
  the current install story. A3/A4 next. A5–A7 need the ingress
  amendments in ADR-W019 Part C, since a claim page behind a proxy that
  mangles `X-Forwarded-Proto` cannot complete a password set either.
