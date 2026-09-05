# Wildstone concepts

The mental model, in the order the pieces depend on each other. Each
section cites the ADR that decided it.

## Desired state lives in the Statefile

The **Statefile** is a git-backed store of typed resources
(ADR-W004): `Volume`, `ConfigFile`, `LdapGroup`, `OidcClient`,
`Container`, `Route`, `DnsRecord`, `BackupPlan`, and the household's
`Automation` (ADR-W022). Specs only — observed status never touches
it. Container images must be digest-pinned (`repo@sha256:...`); the
agent refuses anything else.

Every resource but `DnsRecord` and `Automation` names the node it is
applied at, in `metadata.node`, and that field is required: the
reconciler groups desired state by node and dispatches each group at one
agent, so a resource with no node is never dispatched anywhere.
`DnsRecord` is an exception because it is applied through an external
provider rather than an agent; `Automation` because Keystone itself acts
on it. Neither may carry a node.

For most resources the node is obvious — the machine running the
container. Two are less so: `OidcClient` and `LdapGroup` are applied at
the node running the **identity provider**, not the node running the app
that consumes them, because what they produce is configuration Authelia
and LLDAP read. App packages expose this as an optional `identity_node`
param that defaults to the package's own `node`, which is correct for a
single-node homelab; set it when identity runs on a different machine.

### Identity, and what an `OidcClient` has to say

An `OidcClient` registers one app with the identity provider. It carries
more than a client id and a redirect URI because real apps disagree about
almost everything, and the disagreements are not preferences:

- `tokenAuthMethod` — some apps send the client secret in the POST body,
  some use the OIDC default. There is no value that suits both.
- `requirePkce`, `consentMode` — a client with no browser cannot complete
  an explicit-consent flow, which is how a phone app fails.
- `idTokenClaims` — requesting a scope puts its claims at `/userinfo`, not
  in the ID token. An app that reads groups from the ID token needs the
  claim named here.
- `requireGroups` — restricts the client to members of those LLDAP groups,
  enforced by the provider refusing to issue a token. **For some apps this
  is the only gate there is.** An app with no group support admits the
  user and gives them nothing, which looks like a broken install rather
  than a refusal.

Wildstone writes every client into **one** merged file. Authelia does not
merge config files: several files that each define a clients list leave
only the last one standing, silently, so one file per client would
register exactly one client. Wildstone still keeps a per-client file of
its own for bookkeeping — that is what lets a plan say *this* client
drifted rather than reporting all of them — but it is not what Authelia
reads.

Each capability's containers mount only that capability's subtree of the
managed prefix, read-only. The identity provider has no business reading
the reverse proxy's config, or the reverse.

### A package writes its own app's config

An app's configuration is the package's business. Authelia needs an LDAP
base DN and a storage backend; Jellyfin's LDAP plugin needs an XML
document; Immich reads JSON. None of that belongs in the control plane,
which is meant to know nothing about any particular profile — so a
package declares a `ConfigFile` and Wildstone renders it.

**The path is derived, never supplied**: `<prefix>/<capability>/<filename>`,
with the directory taken from the owning capability. A package therefore
cannot write into another capability's subtree, and the derived path
matches the read-only mount that capability's containers already get, so
a file lands exactly where its own app can read it.

Secret references in the content are resolved on the node like anywhere
else — and whether to resolve them is decided by looking at the content,
not by a flag the author might forget. A config shipped with the literal
text `secret/…` where a secret belongs is an app that starts and rejects
every login, with nothing in any log to explain it.

### Secrets reach a config file only on the node

Wildstone renders every config, and Wildstone never holds a secret value.
So a rendered file carries a **reference**, and the node substitutes it
when it writes the file:

```yaml
client_secret: 'pbkdf2:secret/immich-oidc'   # -> $pbkdf2-sha512$310000$...
some_password: 'secret/immich-db'            # -> the stored value
```

Two forms, because one secret genuinely has two representations in a
working deployment: the identity provider stores an OIDC client secret as
a digest and checks the client's presented value against it, while the
app itself must send that value in the clear. The store holds **one**
plaintext secret and the node derives the other form. Storing both would
be two things that can disagree about one fact.

A dangling reference fails the write rather than being left in place: a
config with the literal text `secret/…` where a secret belongs is not a
partial success, it's a provider that starts and rejects every login.

**Reading such a file back gives you a hash, not its contents.** The
bytes on disk hold live secrets, so the node returns the checksum of what
was rendered instead — enough to answer "has this drifted?", and nothing
that could carry a secret into a plan, an error message, or the Butler.

A `Container`'s published ports use Docker's own syntax and may name the
host address to bind:

```yaml
ports:
  - "8443:80"                      # every interface — Docker's default
  - "100.64.0.1:8443:80"           # this address only, e.g. the tailnet one
  - "[fd7a:115c:a1e0::1]:8443:80"  # IPv6 goes in brackets
```

The address must be a literal IP, not a hostname: a listener whose real
reach depends on what `/etc/hosts` resolves to at container-create time
is not a listener you can reason about. Both ingress profiles require
you to name the address for their private surface rather than defaulting
it, because a surface documented as private that silently binds a public
interface is the worst of both.

## Plans are the only write path

Every change to the Statefile is a **plan**: proposed (by a human, a
package install, or the Butler), reviewed, and **approved by a human**
before anything happens (ADR-W004 A3, ADR-W007). A plan lists its
steps with a risk grade (`Low`/`Medium`/`High`) and blast radius.
Approval commits the change; the reconciler then converges nodes to
it. There is no unmediated write anywhere — not for the UI, not for
the CLI, not for the AI.

## Routing is generated, never hand-written

A `Route` is desired state like anything else. The reconciler renders it
to a Caddy config fragment at `/etc/wildstone/routes/<name>.caddy` and
maintains the `Caddyfile` beside it that imports them all; both ingress
profiles mount that directory **read-only**, so those generated files
are Caddy's entire configuration.

Two consequences worth knowing:

Both ingress profiles run Caddy with `--watch`, so a Route added later
takes effect on its own: the reconciler writes the fragment and Caddy
re-reads it within about a second. No restart, and nothing for you to do.

- **Editing a Caddyfile by hand does not stick.** That covers the base
  `Caddyfile` and the routes placeholder as well as the fragments: they
  are not resources, so they get no plan and no approval step, but the
  reconciler re-asserts them on every tick and logs a `prereq.repaired`
  event when it has to. The base file carries the default-deny block, so
  "nobody noticed it was removed" is not an acceptable failure mode.
- **Editing a route fragment by hand does not stick either.** The reconciler
  compares the live bytes against a fresh render every tick and repairs
  the difference. Change the `Route`, not the file.
- **Apps are reached by container name.** Every managed container is
  attached to a user-defined Docker network (`wildstone`), which is what
  makes `container/<name>:<port>` upstreams resolve — Docker's default
  bridge has no DNS. The agent refuses to attach a container to a
  network it did not create.

A route declaring `auth: authelia_oidc` gets a real `forward_auth`
directive; the other values in the closed set (`app_oidc`, `app_ldap`,
`portal`, `none`) mean the proxy passes through by definition, because
the app authenticates itself or the surface is honestly public.

## The reconciler converges, and repairs drift

The reconcile service (ADR-W008) diffs desired against observed on
every tick (and immediately on approval/ingest — event-driven pokes).
Approved plans authorize their Medium/High steps; unattended drift
repair applies **Low-risk steps only**. Resources it did not create are
foreign: the drift classifier never deletes them (this is what makes
side-by-side migration safe — see [migration.md](migration.md)).

## Wardstone is a closed op surface

Nodes run **wardstoned**, which accepts a fixed set of signed
operations over mTLS (ADR-W003): ensure/stop/remove container, ensure
volume, write managed file (under a managed prefix only), health,
stats, logs, probes, and the write-only secret ops. There is no Exec.
A compromised Keystone still cannot run arbitrary commands on a node.

## Principals, roles, one API

Everything — Console, `wild`, Butler — consumes the ONE public
HTTP+JSON API (ADR-W007). Callers are **principals** with closed
roles: `admin` (everything), `operator` (read/propose/approve),
`butler` (read/propose — structurally cannot approve), `publisher`
(emit household events, nothing else — ADR-W022). Tokens are
opaque `wst_` bearers, stored hashed. Every request runs
authn → RBAC → rate limit → handler → audit, and every response body
passes a redacting encoder that strips secret-shaped values.

Redaction is deliberately blunt — over-redacting is the safe direction —
so a handful of shapes that look secret but are not carry explicit
shelters: `secret/<name>` references, `sha256:` digest pins, a Statefile
commit under a key that names one, `Kind/name` resource references, and
separated identifiers — a kebab-case or `SCREAMING_SNAKE` run such as a
secret's name (`authelia-storage-encryption-key`) or an env-var key
(`IMMICH_MACHINE_LEARNING_URL`). The `Kind/name` shelter is why the shelter list matters rather than being an
implementation detail: without it an ordinary `Volume/immich-postgres-data`
is a 27-character run of base64's alphabet, and a plan step reads
`Create [REDACTED]`. A plan a human cannot read is a plan they cannot
review, and review is the only thing standing between a proposal and
your data. `wild secret list` and an identity plan's env block were the
same failure without the identifier shelter.

A shelter always declines when the thing under it is not what it claims:
a `Kind/name` whose name half is high-entropy, and an identifier whose
letters are all hex digits, are both taken by the value rules instead. A
passphrase-shaped password is the one thing shape cannot separate from an
identifier, and it stays covered by the keyword rule that redacts the
value of any `password`/`secret`/`token`/`api_key` key regardless of what
it looks like.

## Capabilities and packages

A **capability** ("photos", "media") is a set of resources owned by
`capability/<name>`, delivered as a signed package (ADR-W005,
ADR-W010, ADR-W011/W013). Installing or upgrading a package produces —
a plan, like everything else.

## Secrets are write-only

Secret values transit `wild secret set` once, travel to the node over
mTLS, and are stored encrypted in the node's local store (ADR-W012).
The protocol has no read-back operation; Keystone never persists a
value; specs reference `secret/<name>` and the agent materializes the
value only into a container's environment at create time. Rotation is
re-set + container re-create. Consequence: **your password manager is
the secret backup** — see [recovery.md](recovery.md).

## Backups run themselves, restores restore alongside

A `BackupPlan` (ADR-W011) names source volumes, a repository volume, a
`secret/...` repository password, and a schedule. The backup engine
runs pinned restic containers through the same closed op surface as
everything else, records every run in a ledger, and runs a periodic
restore **drill**. Restores go to a fresh `<plan>-restore` volume —
never over live data; moving data in place is a deliberate human act.

## Household events: facts flow in, from publishers

Alongside the operational event stream there is a second, separate
one for the household itself (ADR-W022): "Mika left home", "breakfast
logged", "school is closed Friday". Each is a small, structured fact —
a publisher-supplied id, when it happened, whom it is about, a
namespaced type such as `person.left_home`, and a bounded payload.
Application databases stay authoritative for application state; the
stream records that something changed, in order, replayably.

Facts arrive through one intake, `POST /api/v1/household/events`, from
**publishers**: principals with a fourth role that holds exactly one
verb, `emit`. A publisher cannot read state, propose, or approve; the
worst a compromised shim can do is lie about the house under its own
name, because Keystone stamps the event's source from the token, never
from the body. A retried delivery (same publisher, same id) is a no-op
that returns the original sequence number.

A service that publishes says so in its package, in a `publishes`
block naming the event types and how they reach the intake: `native`
(the app posts itself), `sidecar` (an adapter container the package
declares), or `config` (the app calls a webhook itself, configured by
a `ConfigFile`). Whatever the shape, the credential is the same:
`wild household publisher <capability> -node <id>` mints the
principal `publisher-<capability>` and places its token on the node as
the provided secret `<capability>-publisher` — the name the capability
engine already synthesised, so the package's shim reads it from
`.Resolved.PublisherSecretRef` with nothing else to wire. Minting
again rotates it: the old token stops working the moment the new one
exists, and containers referencing the secret are re-created.

Consumers follow the stream with the same cursor and long-poll shape
as the operational one (`GET /api/v1/household/events?after=N`, or
`wild household events -follow`).

### A rule is approved once, then runs on its own

What acts on those facts is an **`Automation`**: a household rule, and
a Statefile resource like any other (ADR-W022 A1). It reads in order —
*when* this event arrives (`trigger`), *if* these checks all hold
(`conditions`: an event absent or present in a window, a time of day, a
weekday, a cooldown), gather *this much* context (`context`), optionally
ask the Butler to phrase or decide (`escalation`), then *do* this
(`action`: a capability's tool, at a risk tier, with params). A rule
with no `escalation` never involves a model.

Creating or changing a rule is a plan, and it is always Medium: a rule
acts on the household by itself once approved, so it must always pass a
human, never unattended drift repair. The Butler may draft one — it is
the expected author — but it holds no approve verb, so a human signs it.
Removing a rule is Low; it just stops.

Policy holds every rule, whoever wrote it, to one line (ADR-W022 A5): an
action's tier must be `read` or `domestic`. A rule cannot restart a
container or change a route; those are plans, and stay plans. Its params
are held to the same rule as a container's environment: a secret goes in
by `secret/<name>` reference or not at all.

An `Automation` carries no node. Keystone owns it: it is present exactly
when it is desired, the reconciler has nothing to apply, and the
automation engine reads it from the Statefile.

### The engine decides in code, and writes its decisions down

The automation engine follows the household stream — poked by every new
event, and on a timer otherwise — and for each event evaluates every
enabled rule whose trigger matches. Conditions run in order, in code,
against the stream itself: "no `pantry.meal_logged` for this subject
in the last 6h" is a query, not a judgment. The first condition that
fails stops the evaluation.

Every decision is recorded back onto the household stream as an
`automation.fired` or `automation.suppressed` event from the source
`system/automation`, with the rule as its entity, the trigger's subject,
and a payload naming the reason and what a firing would do. The record
has a deterministic id (rule name plus trigger sequence), so a tick
retried after a crash writes nothing twice. A firing never triggers a
rule: the engine's own records are not triggers, which is what keeps a
rule from chaining into itself.

Two guards keep catch-up honest. A first tick over a stream with history
adopts its end rather than acting on the past; and a trigger older than
an hour by the time it is read is recorded as suppressed with a stale
reason, because a "left home" from six hours ago must not ask about
breakfast now.

**Replay before trust** (ADR-W022 A7). `wild automation replay
<name>` or `replay -file draft.yaml` runs a rule over the last week of
real events and reports what it would have done, writing nothing. A
draft is validated and policy-checked on the way, and its violations
come back with the verdicts; cooldowns are simulated from the replay's
own firings. It is the only way to test a rule, and the Butler will use
the same endpoint to check its own drafts before proposing them.

Nothing acts on a fired decision yet; its payload says `executed:
false`. Actions through the domestic gateway are the next work package.

## The Butler proposes; humans approve

The Butler (ADR-W006, ADR-W017) is an LLM operator with read tools
(overview, status, health, diagnose, explain, plan preview) and
exactly one write tool: `propose_change`, which produces a plan in
state `proposed`. Its ceiling is structural, not conventional: the
butler role holds no approve verb, tool results are redacted,
bounded, and enveloped as untrusted data, and every conversation is a
persisted, principal-bound session (`butler` acting-for you). The
morning report is deterministic — no LLM involved — and keeps working
with the Butler off.
