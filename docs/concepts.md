# Wildstone concepts

The mental model, in the order the pieces depend on each other. Each
section cites the ADR that decided it.

## Desired state lives in the Statefile

The **Statefile** is a git-backed store of typed resources
(ADR-W004): `Volume`, `ConfigFile`, `LdapGroup`, `OidcClient`,
`Container`, `Route`, `DnsRecord`, `BackupPlan`, and the household's
`Automation` (ADR-W024). Specs only — observed status never touches
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
(emit household events, nothing else — ADR-W024). Tokens are
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
one for the household itself (ADR-W024): "Mika left home", "breakfast
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

**The first adapter is Home Assistant** (`packages/home-automation`),
in the config shape. It runs nothing: a household with home automation
already has a Home Assistant, and adopting that is a capability package
of its own. The package templates one managed file — a `rest_command`
and an automation — that makes Home Assistant post `person.arrived_home`
and `person.left_home` into the intake itself, with the publisher token
resolved on the node and never held by Keystone. Install it in two
steps, credential first, then include the rendered file from
`configuration.yaml`; the package header carries the exact sequence.

Its **subject mapping** is the honest kind ADR-W024 A8 asks for: the
subject is the Home Assistant person's object id, so `person.mika`
publishes events about `mika`. That is the only stable per-person
identifier Home Assistant exposes to a template, so the adapter declares
it rather than pretending to a lookup table. A household that wants
these to match its Wildstone member names names its Home Assistant
people accordingly.

Consumers follow the stream with the same cursor and long-poll shape
as the operational one (`GET /api/v1/household/events?after=N`, or
`wild household events -follow`).

### A rule is approved once, then runs on its own

What acts on those facts is an **`Automation`**: a household rule, and
a Statefile resource like any other (ADR-W024 A1). It reads in order —
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

Policy holds every rule, whoever wrote it, to one line (ADR-W024 A5): an
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

**Replay before trust** (ADR-W024 A7). `wild automation replay
<name>` or `replay -file draft.yaml` runs a rule over the last week of
real events and reports what it would have done, writing nothing. A
draft is validated and policy-checked on the way, and its violations
come back with the verdicts; cooldowns are simulated from the replay's
own firings. It is the only way to test a rule, and the Butler will use
the same endpoint to check its own drafts before proposing them.

### Acting: one narrow door, and a question that can be answered

A fired rule's action goes through the **domestic gateway**, and there
is no other way for a rule to touch anything (ADR-W024 A5). The gateway
resolves `<capability>.<tool>` to a registered executor, refuses any
tier but `read` or `domestic`, checks the owning package actually
declares that tool in its `agentTools` block, bounds the call, and
records it on the operator's event stream. The tier check is a second
copy of the policy rule on purpose: policy refuses such a rule at
proposal, and the gateway refuses the call at dispatch, so a rule that
arrived by some path nobody anticipated still cannot restart a
container.

The first executor is `notifications.notify`, over the ntfy capability.
Its params are rendered against the trigger before dispatch, so a rule
can say `topic: "household-{{ .Subject }}"` and reach a person by name.
A typo in a template fails the action rather than sending
`<no value>` to somebody's phone.

**A notification can carry answer buttons**, and that is what closes the
loop. Given `replies: "Yes,No"`, each button is a plain HTTP POST back
to the household intake, carrying a short-lived reply token and a
ready-made envelope. Tapping one records `household.answered` on the
same stream that triggered the question, correlated to it — so the
trigger, the decision, the action, and the answer read as one thread. No
chat surface, no polling, no second channel. A rule that asks for
replies where no reply URL is configured fails loudly rather than
quietly sending a question nobody can answer.

Three follow-on events say what happened after a firing, because the
stream is append-only and a decision record never changes:

- `automation.acted` — the action was **dispatched**. It is written
  *before* the call, under a deterministic id, so a tick retried after a
  crash cannot send it twice. Dispatch is at-most-once: the household
  may miss an action if Keystone dies at exactly the wrong moment, and
  will never get it twice.
- `automation.action_failed` — it was refused or it failed, with the
  reason and the stage.
- `automation.escalated` / `escalation_failed` / `escalation_skipped` —
  see below.

### Escalation: the model chooses words, never whether

A rule may ask the Butler to phrase one thing (ADR-W024 A6). That is the
whole of it, and the shape is deliberately narrow:

```yaml
  context:
    - { kind: recentEvents, eventType: pantry.meal_logged, sameSubject: true, limit: 3 }
  escalation:
    prompt: "Ask, in one sentence, whether today matched the usual breakfast."
    into: message
```

By the time the Butler is asked, the engine has already settled that the
rule fires, which tool runs, at which tier, and for whom. The one thing
the Butler affects is the text of the action param named by `into` —
which must already exist and carry serviceable wording, because that
wording is both what a reviewer reads before approving and what the
household receives if no model answers. So an escalating rule still
works with the Butler switched off; it just says the plainer thing its
author wrote. The stream records which happened: `automation.escalated`,
`escalation_failed` (the Butler was asked and did not answer usefully),
or `escalation_skipped` (none configured).

The packet is the rule's own `context` providers, retrieved fresh: the
last three meals for this member, not a history dump. The call itself
carries **no tools at all** — not a read tool, not `propose_change`,
nothing — so there is nothing for a hostile household event to reach
for. Every fact passes through the shared redactor and the
untrusted-content envelope on the way in, and the answer is stripped to
one bounded line on the way out, at the engine's own boundary rather
than on the Butler's word.

That is the containment claim, and it is worth stating plainly: **an
attacker who fully controls a household event payload can, at most,
influence the wording of a message the household was already going to
receive on their own device.** They cannot make a rule fire, change what
it does, change who it reaches, or stop it. A red-team test in
`keystone/butler` pins it.

### Writing rules in plain English

The other direction: a household member says *"whenever I get home, ask
whether I got the mail"*, and the Butler writes the rule (ADR-W024 H7).
It gains two read tools to do it, and no new way to make anything
happen:

- **`get_automation_schema`** describes how a rule is written *on this
  deployment* — the closed grammar read straight out of the validator,
  plus the event types packages actually publish here and the actions
  actually wired up. A model that guesses at a closed vocabulary writes
  rules that get rejected, and a description that drifts from the
  validator is worse than none.
- **`dry_run_automation`** replays the draft against real recorded
  events and reports what it would have done. It is the same call
  `wild automation replay -file` makes, so the Butler and the operator
  cannot get different verdicts on one rule.

Then `propose_change`, exactly as for any other resource. The Butler
never enables a rule: what comes out is a plan in `proposed`, Medium by
grading, which a human approves. After that the deterministic engine
runs it through the same path a hand-written rule takes — there is no
second execution route for a rule an AI happened to write, which is the
whole point of compiling intent into the schema the runtime already
understands rather than into a prompt.

The system prompt tells the Butler to dry-run before proposing and to
report what it found — *"this would have fired eleven times last week"*
is something the household needs to hear before approving, not after —
and to ask when a request is ambiguous rather than guessing. A rule is
approved once and then runs on its own, so a wrong assumption inside one
is not a wrong answer in a conversation; it is a wrong answer every day
until someone notices.

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

### Two surfaces: the operator's butler and the household's

The Butler answers on two surfaces (ADR-W026), and they are two
gateways rather than one gateway with a filter:

| | `admin` | `family` |
|---|---|---|
| Endpoint | `POST /api/v1/butler/ask` | `POST /api/v1/household/butler/ask` |
| Reads | the Statefile, plans, health, diagnosis | the household timeline |
| Writes | `propose_change` — a plan a human approves | one `domestic`-tier action, through the same gateway an approved household rule uses |
| Cannot | approve or apply anything | see or touch the home lab at all |

The family surface holds no Statefile, no node reader and no plan
pipeline — not a filtered one, none — so a household conversation has
no path to infrastructure to be talked into taking. Its system prompt
does not describe one either: naming a capability a surface lacks is an
invitation to try it.

Which surface answered is stamped on the session record from the
endpoint the request arrived on — never from the request body — so the
audit trail separates the two forever after:
`wild butler sessions -surface family`, or the Console's Butler screen,
which offers the two side by side with the ceilings written out. The
family surface binds each session to the caller as its member, and every
action it dispatches carries that member into the domestic gateway's
audit record.

### Chat: bindings, then a queue

A chat account reaches a Butler only through a **binding** — a row an
operator created by issuing a single-use claim code (`wild chat code`)
that the person sends to the bot once. Bindings are per-surface and
revocable, and every refusal (unknown code, expired, spent, wrong
transport) is the same answer, so somebody guessing learns nothing. An
unbound account gets exactly one reply: how to get a code.

Neither half of an invite is free text. A code names a person — a
declared household member on the family surface (see **The household's
people** below), a Keystone principal on the operator's — and a
transport this deployment has an adapter installed for
(`GET /api/v1/chat/transports`). Anything else is refused when the code
is issued rather than when the person it was meant for says nothing ever
happened: a code for a bot that does not exist is valid, unredeemable,
and looks exactly like a working one.

An adapter's whole job is protocol translation. It posts what it heard
to `POST /api/v1/chat/messages`, polls `GET /api/v1/chat/outbound` for
what to say, and acks what it delivered. It never chooses a surface —
its credential does, from its role — and it never asserts who somebody
is; the binding does that. Replies and rule-driven pushes share the one
queue, so both are delivered and retried the same way, deduped on the
transport's own message id.

Group chats: the operator surface answers in a direct message only. The
family surface may answer in a group, and is told in its prompt that the
audience is shared.

A household rule can ask over chat as well as over ntfy: a package
declares `send` as a `domestic` tool, the rule names it like any other
action, and the member's reply — free text, not a button — is matched
back to the question that is still open for that conversation and
recorded as `household.answered` with the rule's own correlation id.
Recording an answer involves no model, so that loop closes with the
Butler switched off.

### The household's people

A household member is an `LdapUser` in the Statefile (ADR-W020 A3) — not
a row somebody typed into LLDAP's own admin UI. The admin adds people
from the Console's Household screen or `wild household member`, which
**proposes a plan**; approving it is what declares the person, and the
identity capability's provisioning job then creates the account and adds
the group memberships it is missing. The job only ever adds: no plan
removes an account, because a control plane that deletes people because
a YAML file changed is one bad plan away from deleting the household.

No password travels with any of that (ADR-W020 A4). The account exists
and cannot sign in until the person sets their own credential through
the sign-in page's reset flow.

Group memberships are offered from the `LdapGroup`s capabilities have
declared, and a group nothing declares is refused: the provisioning job
is additive and best-effort, so an invented group name fails one
membership in a container log rather than the request, and the person
quietly cannot open the app they were added for.

Chat transports (Telegram, Matrix, and whatever comes next) are the
point of the split:
they arrive as packages, and each one is two separate bots against these
two endpoints, never one bot deciding for itself which side of the house
a message belongs to.
