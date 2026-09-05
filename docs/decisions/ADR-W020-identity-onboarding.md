# ADR-W020: Identity onboarding — the first admin, and how a household gets accounts

- **Status:** proposed
- **Relates to:** ADR-W007 (principals, bootstrap token), ADR-W012
  (secrets never read back), ADR-W016 (Console IA), ADR-W019 A5
  (legibility over scheduling), ADR-W005 A2 (`.Resolved` stays small)
- **Audience:** decision record AND implementation spec for the identity
  work package's human half.

---

# Part A — Decisions (binding)

### A1. There are two identity systems and the product says so

A **Keystone principal** (`admin`/`operator`/`butler`) is who may drive
Wildstone. An **LLDAP user** is who may log into the household's apps.
On day one they are the same person; they are never the same record, and
they have different lifetimes, different stores and different blast
radii.

Presenting them as one thing is the tempting simplification and it is
wrong in a way that surfaces late: revoking a family member's photo
access would revoke their ability to administer the house, or — far
worse the other way — adding a teenager to the media group would hand
them an approval verb. The Console shows two surfaces and names them.

### A2. First-run claim replaces the bootstrap token in the journal

Today the admin token is minted once, printed to stdout, and thereafter
exists only in the journal; `Store.Bootstrap()` returns `""` whenever
principals exist, and there is no offline mint. `docs/install.md`
recommends scrubbing the journal — do that a beat early and admin access
is unrecoverable short of destroying the runtime DB. This blocked
automated setup three separate times during v0.1.6-alpha VM testing
(friction log #8), and it is the first thing a new operator touches.

First start therefore writes a **claim secret** to a root-readable file
in the data dir, and `keystone claim-code --data-dir` regenerates one on
demand. The Console's first-run screen accepts it, and the operator sets
an admin username and password there. The token in the journal stops
being the mechanism.

The claim file is deleted once claimed. Possession of a file readable
only by root is the same proof of physical access the join-token pattern
already relies on (ADR-W003), so this adds no new trust assumption — it
only makes the existing one recoverable.

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

The first credential is a **single-use claim code**, generated on the
node and stored as a `Secret` — the same path every other secret takes,
so it inherits ADR-W012's guarantee that Keystone cannot read it back.
The person exchanges it for a password they choose.

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
A first-run screen is exactly where an unhelpful failure costs the most
trust, because the operator has nothing working yet to reason from.

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
