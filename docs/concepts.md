# Wildstone concepts

The mental model, in the order the pieces depend on each other. Each
section cites the ADR that decided it. Reviewed 2026-10-22 against
`60df9dcbc1daa99d43f7692eb83f7d72f7070956`; use the [glossary](glossary.md)
for shared terms and [documentation review](design/documentation-refresh-2026-10-22.md)
for evidence and limits. Proposed placement/contract semantics are not runtime guarantees.

## Desired state lives in the Statefile

The **Statefile** is a git-backed store of typed resources
(ADR-W004): `Volume`, `ConfigFile`, `LdapGroup`, `OidcClient`,
`Container`, `Route`, `DnsRecord`, `BackupPlan`, and the household's
`Automation` (ADR-W024). Specs only — observed status never touches
it. Container images must be digest-pinned (`repo@sha256:...`); the
agent refuses anything else.

Node-scoped resources name the node they are
applied at, in `metadata.node`, and that field is required: the
reconciler groups desired state by node and dispatches each group at one
agent, so a node-scoped resource without a node cannot be dispatched.
`DnsRecord` uses an external provider; `Automation`, `Meter` and `Commitment`
are Keystone-owned. Those kinds do not carry a node. The closed registry in
`keystone/resource/resource.go` is authoritative; the kinds listed above are
examples rather than the complete registry.

For most resources the node is obvious — the machine running the
container. Two are less so: `OidcClient` and `LdapGroup` are applied at
the node running the **identity provider**, not the node running the app
that consumes them, because what they produce is configuration Authelia
and LLDAP read. Some app packages, including Immich and Jellyfin, expose an optional
`identity_node` parameter that defaults to their own `node`. Inspect each
manifest: this parameter is not universal, and placement on another machine
also needs valid endpoints and node-local secrets.

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
document; Immich reads JSON. The architectural goal is to express app configuration in package data, so a
package declares a `ConfigFile` and Wildstone renders it. Current code still
contains application-specific synthesis and controllers; this goal is not a
claim that Keystone is already entirely profile-agnostic.

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

## Infrastructure desired-state changes use Plans

Infrastructure proposals are **Plans**: proposed by a human, a package
install or an Agent, reviewed, and **approved by a human** before execution
(ADR-W004 A3, ADR-W007). Household tracker edits have a distinct audit-only
Plan path and do not grant host-execution authority. A plan lists its
steps with a risk grade (`Low`/`Medium`/`High`) and blast radius.
Approval commits the change; the reconciler then converges nodes to
it. This is the infrastructure proposal path. Household evidence writes, granted
application actions and the separately scoped opt-in `host.*` action family
have their own authorization/confirmation rules; they are not authority to
approve or apply infrastructure Plans. No new bypass is introduced here.

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
The protocol has no arbitrary host-shell operation. This does not make a
compromised Keystone harmless: allowed container creation, commands, mounts,
networks and approved device/capability access still carry substantial power.

## Principals, roles, one API

Console, `wild`, automation and Agents use authorized public API/gateway
boundaries. A **Person** has an Owner or Member role; an **Agent** has an
Owner or Member class; an API **principal** has a machine credential role.
These are different axes. The current machine-role set is in
`keystone/runtimedb/principals.go`, including adapter/publisher roles as well
as `admin`, `operator` and `butler`. Do not infer a Person's authority from a
legacy machine role name.

Console uses the Owner OIDC/PKCE session and freshness boundary (ADR-W033);
opaque bearer tokens remain for CLI and machine callers. Agent HTTP/MCP
transport uses the existing Agent API (ADR-W039/W040/W049); runtime identity
does not manufacture a Person's authority. Redaction is defense in depth,
alongside route-specific authorization and response handling.

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
The protocol has no individual secret read-back operation. Specs reference
`secret/<name>`; Wardstone resolves values locally for supported environment
and managed-file uses. Whole-site recovery can carry sealed node bundles and
the sealed household-vault key; Personal credentials have separate exclusions.
Keep recovery keys and required credentials off-host — see [recovery.md](recovery.md).

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

## Commitments: what the household owes itself

Alongside rules that react to events there is a second household
subsystem for things that come *due* (ADR-W035). A **`Meter`** is a
number that only counts up — miles on a vehicle, hours on a blower,
gallons through a filter. A **`Commitment`** is a thing the household
owes itself, due when a meter reaches a number.

The decision the rest turns on is that **the calendar is a meter**, built
in and declared nowhere. "Every 5,000 miles" and "every 365 days" become
the same sentence with a different meter, so a checkup and an oil change
are one feature rather than two, and a household never has to know which
kind of thing it is creating.

```yaml
kind: Commitment
metadata: { name: f150-oil, owner: household/mika }
spec:
  description: F-150 oil and filter
  rollover: fromCompletion        # the clock starts when the oil is new
  anchor: "2026-03-01"
  anchorAt: { f150-odometer: 79300 }
  due:                            # earliest wins
    - { meter: f150-odometer, every: "5000", lead: "300" }
    - { meter: clock, every: 6mo, lead: 14d }
```

Each cycle is its own occurrence, and `scheduled` is a state of its own
because knowing a checkup is due and *having an appointment* are
different facts — only the first was ever tracked, which is how a
checkup goes unbooked for a year. A one-off (`rollover: once`) has the
same states and simply opens no next cycle.

Three fields answer three questions. `metadata.owner` is who it is
**about**, `mayComplete` is who may **answer** it, `notify` is who
**hears**. A child's checkup is about the child, booked by a parent, and
the child is not who should be nagged.

### Nothing is stored except what happened

Completions and bookings are facts and are recorded. Where a cycle stands
is not: it is recomputed from the readings and those facts every time
anybody asks, so a restored backup works it out again rather than
believing a stale verdict.

A pass runs once a day, and again whenever a meter is read. It writes
`commitment.due_soon`, `commitment.overdue` and the rest onto the
household timeline **and stops**. Who hears about one, on which surface,
at what hour is an `Automation` the household writes — which is why the
truck's oil is still knowable with every model in the house switched off.

Because being told the same thing every morning is how a household stops
reading its notifications, the pass announces *transitions* and says
nothing on a quiet day.

### Not knowing is an answer

A meter nobody has read recently cannot support the conclusion "not due
yet". So a stale reading produces a request for a number rather than a
verdict, and every surface keeps the two apart: the household's own page
renders it as its own item, the Console gives it its own pill, and `wild
meter list` prints "too old to rely on" rather than a reassuring number.
A projection — "probably March, at twelve thousand miles a year" — is
shown as an estimate everywhere it appears and never moves a state.

### Recording a number is not a plan

A reading is evidence, not authority: it changes no rule and grants
nothing. So it is an ordinary authenticated write through any of four
doors — the Console, `wild meter read`, the household's own page, or a
capability publishing an event — and the engine cannot tell which one it
came through. That is the promise that keeps a capability an extension
rather than a prerequisite.

```
wild meter read f150-odometer 84210
```

### Home Assistant extends it; nothing requires Home Assistant

The seam is a rule the household writes, not behaviour built into
Keystone. With `home-automation` installed, Home Assistant already
publishes `person.arrived_home`. An `Automation` can turn that into a
question asked at the one moment somebody is sitting in the driveway
looking at the odometer:

```yaml
kind: Automation
metadata: { name: odometer-when-home, owner: household/mika }
spec:
  description: Ask for the odometer when Mika gets home and the truck is nearly due
  trigger: { eventType: person.arrived_home, subject: mika }
  conditions:
    - { kind: eventPresent, eventType: commitment.due_soon, window: 720h }
    - { kind: cooldown, window: 72h }
  action:
    tool: notifications.notify
    tier: domestic
    params:
      topic: household
      message: "What's the odometer say? The truck is coming up for an oil change."
```

Uninstall Home Assistant and nothing breaks. The commitment still knows
what it is owed and still asks — on its own schedule, and less cleverly.

## Agents, Connections and scoped authority

Agent and Butler refer to the same architectural object (ADR-W033). Owners
administer through Console; Members use the household surface. Owner Agents
can receive Owner and Member tools; Member Agents cannot receive Owner tools.
Actual grants, resource-specific rules, processing class and disclosure
audience narrow every call. Having an Owner Agent is not permission to approve
its infrastructure proposals.

Infrastructure changes use human-approved Plans. Permitted household actions
use existing application gateways, including confirmation where required.
The opt-in `host.*` family includes diagnostics and selected operational
actions under separate tier/confirmation checks; it is absent from Member
manifests (`keystone/butler/hostops_manifest_test.go`). The old statement that
every Agent has exactly one write tool is no longer an accurate global model.

Connections bind a Person and Agent to a conversation route. They do not grant
every recipient access to every fact. Chat adapters translate transports;
the server resolves identity and authority. External runtimes consume the
canonical Agent API rather than receiving an unrestricted node shell.
Credential enrollment, task admission and live compatibility must be checked
against their implementation and tests, not inferred from accepting an ADR.

People are canonical Person records; directory accounts and machine
principals are bindings, not synonyms for a Person. Themes, grants, admission
and role changes use that identity. Consult [agent system design](agent-system-design.md)
and [ADR-W033](decisions/ADR-W033-the-agent-design.md) for the current design,
including the limits on child admission and Owner authority.

## Deployment and application management boundaries

Today a Wardstone node is an enrolled execution environment, not a Proxmox
physical node or a claim of high availability. Multiple guests may share one
physical failure domain. The installer does not provision the guests.

[ADR-W050](decisions/ADR-W050-proxmox-first-deployment-and-recovery.md)
proposes independent management and application recovery groups. The
[Application Management Contract 0.1](design/application-management-contract-v0.1.md)
proposes common operational facets; it complements existing package traits
and service-domain adapters. Both remain drafts. A provider manages approved
hosting operations; an application adapter understands supported app behavior.
Neither is a second authority source or an arbitrary executable plugin.

Guest running, process running and application ready are different states.
A container restart does not repair corrupt data. Recovery requires coherent
backups, alongside restore and application checks; monitoring of host loss
must survive that host's outage.

## Themes belong to a person

A household has a colour: `accentHue` in the dashboard capability's
`settings.yaml`, which is desired state, which is where the house's own
colour belongs.

A **theme** is not that. It is the ground somebody reads on, and the
people in one household do not agree about it. So a theme is chosen per
person, stored against their `person_id`, and it follows them across
every Wildstone screen — the household dashboard, the Assistant, Files,
the calendar, the Console. Nobody else in the house sees it, and there
is no administrative override: an Owner decides which themes *exist*,
each person decides which one they read on (ADR-W036).

Members choose from the household dashboard itself — the portal door
holds no API permission, so switching theme needs no Console account —
and whoever is on the Console chooses there. Adding a theme, or a
picture for one, is an Owner's act on the Console.

Themes are [Omarchy](https://omarchy.org) palettes. Twenty-two ship in
the Keystone binary — Tokyo Night, Catppuccin, Everforest, Gruvbox,
Kanagawa, Nord, Rosé Pine and the rest — and an Owner can add any other
by pasting its `colors.toml` (Console → the palette button in the rail →
Add a theme). Nothing is cloned, fetched or executed: a theme is
twenty-odd hex values, and the rest of an Omarchy theme configures
programs Wildstone does not run.

A theme can also bring a **picture**. Omarchy themes ship a
`backgrounds/` directory, and an Owner can upload one of those
wallpapers for any theme; it becomes the background of the household
dashboard for everybody who reads on that theme. The wallpapers are not
vendored — they are megabytes of other people's art, where a palette is
a few dozen hex values — so the household supplies the one it likes.

Because the page is then drawn over an arbitrary photograph, the theme's
ground stays in front of it at a fixed opacity and every ink is derived
against the lightest and darkest thing that picture can put underneath.
A theme with a wallpaper therefore has slightly brighter ink than the
same theme without one. The Console never wears a wallpaper: it is the
screen you open when the house is broken.

What a palette does not supply, Wildstone derives: card grounds, drawer
grounds, muted ink, hairlines, status tints and shadow rings. Every pair
that carries text is held to WCAG AA against every ground it appears on,
and body copy to AAA — a palette that misses is nudged along its own hue
until it clears, rather than refused. Status colours stay semantic
whatever the palette calls them, because an operator has to be able to
tell "this is fine" from "this is broken".

Choosing nothing is the resting state, and it renders as Wildstone's own
look: the household's hue, through the stylesheets that already ship.
