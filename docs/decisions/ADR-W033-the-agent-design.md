# ADR-W033: The Wildstone agent system

- **Status:** accepted design direction, rewritten after architectural review on 2026-09-16
- **Scope:** identity, authority, agents/Butlers, Connections, memory, secrets, plans, routines, lifecycle, and recovery
- **Design of record:** [`docs/agent-system-design.md`](../agent-system-design.md)
- **Audit detail:** remains in [`ADR-W033-review.md`](ADR-W033-review.md) and the existing audit documents. This ADR links decisions to that work but does not carry the full findings matrix.
- **Implementation:** partly built; where code differs from a decision here, the decision governs and the code is not yet conformant.

`Butler` and `Agent` mean the same architectural object. Product-facing text may say Butler; technical text may say Agent.

---

# 0. Why this ADR exists

The agent audit surfaced many individual findings, but most of them shared a smaller set of unanswered architectural questions. Fixing findings one by one would have let implementation order decide the architecture accidentally. This ADR does the reverse: settle the architecture first, then derive remediation from it.

The governing principle is that identity, authority, knowledge, work, and external reach stay separate. Models may interpret requests and propose work, but they do not create identity, manufacture authority, widen memory audiences, or approve their own durable automation.

Where this ADR conflicts with an older ADR, this ADR governs. Older ADRs have been edited so they no longer state conflicting rules; Part G lists what changed.

---

# A. Human identity, roles, and surfaces

## A0. Authority is decided in three layers

Stated once, up front, because a **specific** resource rule can be stronger than the **general** authorization rule it sits under, and a literal implementation could then satisfy the general rule while violating the specific one.

The layers, in order, each never widening the one before:

1. **General execution authority** — may this actor do work at all, and within what bounds?
2. **Resource-specific authority** — may it reach *this* record, *this* secret, *this* tool?
3. **Disclosure and audience policy** — may the result reach *this* audience, on *this* Connection?

Layer 1 has three kinds of actor, and they are genuinely different principals rather than one impersonating another: an interactive **Person**; a **Task**, whose authority is *derived* — it inherits the bounded authority of the request that created it, is re-evaluated at every boundary, can never widen, and is not a grant object anyone issues or holds; and an approved **routine version** running unattended.

The general rules **require layers 2 and 3 without enumerating them**. That is the part that closes the class rather than one instance of it: a new resource brings its own grant semantics, and the general rule cannot silently permit what that resource forbids because it never claimed to know the whole list.

## A1. There are two fundamental human roles

Wildstone has exactly two fundamental human roles:

- `owner`
- `member`

`child` is not a third RBAC role. It is a constrained Member profile/policy preset. The same mechanism may later support profiles such as `guest` without adding another fundamental authority rung.

An Owner administers the deployment. A Member consumes household services and capabilities.

## A2. Every human is one canonical Person

A Person has one stable internal identity regardless of how they authenticate or communicate. Directory login, web session, Telegram identity, Discord identity, and future bindings all attach to the same Person.

A claimed name, display name, phone number, or chat username establishes nothing by itself. External identifiers become identity bindings only after authenticated enrolment or explicit Owner administration.

## A3. The first Owner is created during installation

A fresh install must not complete without at least one active Owner.

The installer asks for the initial administrative username and password and creates the first Owner and directory identity before setup completes. Wildstone does not use a "first browser visitor becomes admin" flow.

After installation, the user is directed to the network-bound Wildstone URL and signs in normally.

## A4. Homepage and Console are different trust surfaces

### Homepage

Homepage is the household plane. Every authenticated Owner and Member may reach it, subject to service and capability entitlements.

Homepage uses a persistent household SSO session so normal navigation among Homepage and services does not repeatedly ask for credentials.

Homepage is not an administrative surface. Even for an Owner, it remains the household experience. It may offer a visible link to Console, but it does not become an admin dashboard.

Only Member Agents appear on Homepage.

### Console

Console is the control plane. Only Owners may enter it. There is no delegated Member exception: if a Person can enter Console, they are an Owner.

Console requires fresh or recent step-up authentication even when the household SSO session is still valid. Highly consequential administrative actions **require** a currently fresh Owner session — not "may require": an elevation window that expires without closing the action is not an administrative boundary. Wildstone does not require a new password or second factor for every individual click inside that window.

### A4.1 Amendment (2026-09-19, #205): how freshness is established and what it gates

Decided with the household owner while working #205. Where an older ADR says otherwise about Console sign-in, this amendment governs.

- **The assertion is a signed token, not a header.** Authelia's forward-auth answers with `Remote-User`, `Remote-Groups`, `Remote-Name` and `Remote-Email` only; it has no authentication-time header to forward. So the Console signs in as an OpenID Connect client of Authelia, and Wildstone reads `auth_time` from the ID token after verifying its signature against Authelia's published keys. Nothing arriving in a request header can make a session fresh. Step-up is a new authorization request with `prompt=login`, which Authelia (4.39.0 and later) answers by making the person authenticate again. Passwords and second factors stay in Authelia, as decision 7 of #201 requires.
- **The Console client is public with PKCE.** Keystone holds no client secret, keeping to the rule that Keystone never holds secret values.
- **Two windows, both configurable.** Staying in the Console requires an authentication no older than **12 hours**. A consequential operation requires one no older than **15 minutes**. A future-dated `auth_time` is never fresh.
- **Consequential means detrimental, and the list is deliberately short.** A household reaches its homelab over a network it controls, so the gate covers what is destructive or hard to undo, not every write: changing a Person's role, disabling or offboarding a Person, purge, deleting an Agent, restoring a backup and restore quarantine, removing access groups, rotating node secrets, Household-secret `manage` and `reveal`, and **minting any bearer credential** (Console and principal tokens, join tokens). Minting is on the list because bearer callers are exempt, so an ungated mint would let a stale session escape the gate. Admission, adding access groups, models and provider keys are ordinary.
- **Bearer tokens are exempt.** The `wild` CLI, the installer and host recovery authenticate with bearer tokens and have no browser session. They are the host's own path (A7), not a browser session going stale. The gate applies to identities that arrive through the trusted ingress.
- **A refused operation is resumed by the person, not replayed.** The response tells the Console to step up. After re-authenticating, the person comes back to the same action and confirms it with one click. A write is never replayed automatically after a redirect.
- **Household SSO is long-lived: 30 days by default, configurable.** Homepage and household services keep a persistent session (A4, Homepage), independent of the Console's windows. Per-device credentials for native phone and TV apps are separate future work.
- **Homepage shows Owners a link to Console.** Members do not see it, since they would only be refused. Without the link the Console has no discoverable entry point after installation.

## A5. Service access is group-based and enforced outside the UI

Each app/package declares one or more Wildstone access groups, projected into the directory/SSO layer using the strongest compatible integration mechanism available to the service: OIDC, LDAP, or safe proxy/forward-auth where compatible.

A package must not rely on Homepage card visibility as authorization. A user who knows the direct service URL is still denied unless their effective entitlement allows access.

New Members inherit a default household access group, expected to be something like `Family`, and may receive additional service/group assignments.

Higher-level groups such as `Adults`, `Kids`, `Media`, `Documents`, or `Home Automation` may be expressed in Wildstone and projected to effective directory memberships. Explicit deny/restriction overrides inherited allow.

## A6. Demotion and last-Owner protection are immediate

Changing an Owner to Member immediately removes Console authority and invalidates active Console sessions.

Wildstone must always retain at least one active Owner. The last Owner cannot demote, disable, soft-delete, or otherwise remove their own active Owner status until another active Owner exists.

## A7. Host privilege is the final recovery authority

If web authentication is lost, recovery is performed from the host through the CLI by a root-privileged caller, whether local or over SSH.

The product must support flows equivalent to:

```text
wildstone owner reset-password <username>
wildstone owner create <username>
```

Recovery actions are audited. This is the break-glass path; no hidden web backdoor is required.

---

## A8. A Person can be scheduled without being able to sign in

A household schedules people who do not have accounts. A six-year-old has swimming on Tuesday, a reading book to finish, and a bag to pack; none of that requires them to authenticate, and most of it will be entered by somebody else.

So being a **subject** of household data and being a **principal** who acts are separate, and this ADR separates them:

- A **subject** is named, scheduled, assigned things, and shown on a screen. It needs a Person record and nothing else.
- A **principal** signs in, holds credentials, reaches an Agent, and accumulates memory of its own. It needs admission.

A Person may be the first without being the second, and for a child that is the normal case rather than a degraded one.

The record already has most of this shape. `butler_members` carries `profile` (`NULL` or `child`), a `guardian`, and `admitted`; `RecordButlerMember` requires a guardian for a child and refuses any role but Member; `AdmitButlerMember` refuses to admit a child record by any path, including a direct `INSERT`. What was missing was the statement that an un-admitted child is a **supported, complete state** rather than a half-finished admission, and that household features may rely on it.

**BEING UNADMITTED IS NOT THE SAME AS BEING UNABLE TO SIGN IN.** `admitted` governs Butler runtime membership and nothing else. Directory authentication and `principal.Store.Authenticate` do not consult a Person's profile — `ProfileChild` is read in exactly one place that gates anything, `AdmitButlerMember` — so a child Person who also holds an LLDAP account or an API principal can still authenticate.

So it is required here rather than assumed. **A scheduled subject carries no authentication binding of any kind**: no directory account, no API principal, no chat binding, no session. Recording a Person as a child, or converting an existing Person to one, must remove any it already has; a deployment that cannot remove them must refuse the conversion rather than leave usable credentials behind a profile that claims there are none.

_Code not yet conformant: `RecordButlerMember` checks the guardian and the role but no identity link._

**THIS IS NOT THE CHILD SURFACE, AND DOES NOT OPEN IT.** ADR-W030 A2.1 gates a future Child surface on its own bot credentials, principal and role, endpoint, structurally limited gateway, transport eligibility, and boundary tests, and states that no child deployment is authorized there. Every one of those is a requirement on a child **acting** — talking to an Agent, holding a credential, reaching a gateway. A child who is scheduled does none of them. The gate stands exactly as written, `AdmitButlerMember` keeps refusing, and nothing in this section may be read as evidence for that surface.

What a scheduled-but-unadmitted Person explicitly does not get: a login, a chat binding, an Agent, a session, memory of their own, a Connection, or any cloud routing (ADR-W030's Child cloud-routing clause is untouched).

## A9. A guardian is their child's standing on household records

`child` has required a `guardian` since the record existed, and nothing read it. This section says what it means.

For a Person carrying the `child` profile, their recorded guardian is treated as that child for **visibility and completion** of the child's own household records: the guardian sees the child's personal items and may answer them.

The reason it must be said here and not in the code that needs it: the personal-scope rule is "a personal commitment is the owner's business and nobody else's", and a child's items are owned by the child. Under that rule alone a parent cannot see their six-year-old's reading task, which is the wrong answer in every household that has one. `handlers_portal_commitments.go` reached this exact problem and correctly declined to solve it locally, recording that "W033 A1 makes `child` a Member profile rather than a role, so there is no adults-versus-children vocabulary here to lean on, and inventing one in this file would put it in the wrong place." This is that place.

Three properties this has and a general adult/child split would not:

- **It is a recorded relationship, not an inferred one.** ADR-W030 requires that guardian mappings and child classification be explicit admission records, never inferred from an LDAP group. An adult is not a guardian by being an adult; they are a guardian of a named child or of nobody.
- **It is asymmetric.** Guardian reaches child. Child never reaches guardian, and the child has no session to reach from in the first place (A8).
- **It is not a third role.** A1 still holds: two fundamental roles, and `child` is a constrained Member profile. A guardian is an ordinary Member or Owner who happens to be named on somebody's record.

A sole guardian still cannot be removed while the child record is active — ADR-W030 already says so for admission, and it is now also true for these records, because removing them would leave a child's items with nobody able to see or answer them.

## A10. An agenda on a shared screen discloses to the room, and says so

The household's own screens are shared: a wall tablet in a kitchen, a phone handed round. A per-person agenda rendered there is not protected by the person it is about — it is protected by the door the household already came through.

So a per-person agenda is a **household surface with a subject selector**, not a per-person authenticated one. "Whose day is this" chooses what to render; it does not establish who is asking. The portal's existing boundary — everyone through the door is the household — is the boundary the agenda inherits.

**THE SURFACE IS CLASSIFIED UNIFORMLY, because nothing can tell it apart.** Withholding adult personal items "on a shared screen" is not implementable: a request carries an authenticated member and a surface, and no request, session, Connection or device attribute says whether the physical screen is currently being handed round a kitchen. A rule conditioned on a fact the server cannot observe is not a rule.

The rule is therefore about standing, not about hardware, and holds identically in every session:

- A reader may see a subject's **household-scope** records whenever they can open the surface.
- A reader may see a subject's **personal-scope** records when they are that subject, when the commitment names them, or when A9's recorded guardian relationship makes the subject their child.
- Nothing else. Selecting an adult from the picker grants no standing over that adult's personal records, in any session, on any device.

The cost is accepted deliberately: an adult reading their **own** agenda on the kitchen tablet will see their own personal items there, because the surface cannot distinguish that tablet from their phone. A household that wants those unseen keeps them off a shared screen the same way it keeps anything else off one. What the design guarantees is the part it can: the selector never widens what a reader may be shown.


# B. Agents, tools, models, and Connections

## B1. Agents belong to the household

Agents are household-owned resources, not creator-owned resources.

Any Owner may administer any Agent. Members cannot create, modify, clone, pause, retire, delete, or reconfigure Agents.

## B2. There are two Agent classes

- `owner` Agent
- `member` Agent

A Member Person may interact only with Member Agents.

An Owner Person may interact with Owner or Member Agents.

This is structural, not merely a runtime preference. A normal Member-to-Owner-Agent path is not admitted in the first place.

## B3. Tools have authority classes

Tools are classified at least as:

- Member tool
- Owner tool

Member tools may be granted to either Agent class. Owner tools may be granted only to Owner Agents. Configuration that assigns an Owner tool to a Member Agent is invalid and must be rejected.

A tool may additionally be marked `opt-in`, meaning it is excluded from broad default grants even when its class otherwise matches.

> **As built (2026-09-26).** Two tiers of Agent: a **Member** (household) Agent holds the household's tools; an **Owner** (operator) Agent holds the operator's tools **and** the household's. An Owner Agent's session is `butler.OwnerToolset` — the operator gateway joined to a household gateway built for the same person — and its tool screen and grant checks (`toolExistsOnClass`) use the same union. This supersedes ADR-W026 A1's one-gateway-per-surface for Owner Agents; see the amendment there.

## B4. Tool grants support dynamic service bundles and explicit selections

An Owner can grant a whole service/class bundle or individual tools.

A dynamic grant such as "all Home Assistant Member tools" automatically includes future Home Assistant tools that are both Member-class and not opt-in. It does not absorb future Owner-class or opt-in tools.

An explicit list remains static.

## B5. Agent creation, upgrade, and downgrade are intentionally asymmetric

A newly created Owner Agent receives all currently available Member and Owner tools by default except tools marked opt-in.

A newly created Member Agent can receive only Member tools.

Downgrading an Owner Agent to Member automatically strips all tool grants. The Owner may then re-grant appropriate Member tools.

Upgrading a Member Agent to Owner preserves its existing Member grants but does not automatically add Owner tools. This avoids silent privilege expansion through Connections that may still admit Members.

## B6. Model fallback is configured per Agent

An Agent owns an ordered model/provider chain, not one global model and not a deployment-wide fallback order.

Example:

```text
Alfred
1. local Ollama model
2. OpenAI model
3. OpenRouter model

Sandy
1. different local model
2. Anthropic model
```

Model fallback stays within the assigned Agent. Wildstone does not automatically substitute another Agent when one Agent cannot perform an inference step.

Privacy requirements still constrain the model chain. A local-only task must fail rather than silently fall through to a cloud model.

For an Agent served by an external runtime (ADR-W039), the model chain is the runtime's own configuration. Wildstone records the Owner-declared processing class for that runtime and applies it as a disclosure filter: `local_only` content is withheld from any runtime not declared local-only.

## B7. Connection is the common reach abstraction

Every way a Person communicates with an Agent is a first-class `Connection`, including Homepage, Telegram, Discord, Slack, voice, and future transports.

A Connection belongs to exactly one Agent. An Agent may have many Connections, including multiple Connections of the same transport type.

Each Connection has its own lifecycle, credential or secret reference, admission policy, configuration, and disclosure/privacy classification.

Disabling a Connection does not pause the Agent.

### B7.1 Amendment (2026-09-19, #246 and chat connections): how a chat Connection is made

Decided with the household owner. Where an older ADR (W026, W031) says otherwise about chat adapters or bots, this amendment governs.

- **A chat Connection connects a Person to an Agent, and nothing else.** There are no "family" and "admin" bots. What a Connection may admit follows the Agent's class and the Person's role (B2, B9): an Owner-class Agent's Connections admit Owners only. The transport package does not encode an audience.
- **One package per platform, installed once per bot.** The chat packages are `chat-telegram`, `chat-matrix`, `chat-discord` and `chat-slack`. Each is *instanced* (#246): an install carries an instance name, its resources and settings belong to that instance, and a household can install as many as it has bots. The instance name defaults to the Agent's name. B7 allows several bots per Agent on one platform, so the name is suffixed when taken.
- **One token is one bot, and one bot serves many people.** Connecting a second Person to an Agent that already has a bot on the platform invites them to that bot; no credential is asked for. A credential already running another live bot is refused, by a SHA-256 fingerprint Keystone records in place of the value, because two installs polling one bot steal each other's messages. Making a new bot mints a credential and removing one ends people's way to the Agent, so both are consequential (A4.1). Inviting to an existing bot is admission, which is ordinary. Removing a bot revokes its address and its own credential and withdraws its invites. A removed bot's name is not reused, so its credential is never revived.
- **The Owner supplies only what cannot be derived:** the Agent, the Person, and the platform's credential. Telegram and Discord take a bot token. Slack takes a bot token and an app-level token, because Socket Mode needs both. Matrix takes a homeserver address and the bot account's access token. The machine defaults to the Keystone host, and the Keystone address is derived. The adapter's image is not asked for.
- **The adapter ships with the release, not through a registry.** The release already builds the `chat-adapter` program for each architecture. The installer places it on every host, and a chat package runs it in a public, digest-pinned distroless base image with the program bind-mounted read-only and run as a non-root user. Nothing of Wildstone's is published to a container registry, which keeps a private repository private and needs no registry credentials on a node.
- **The Agent delivers the invite (A2's authenticated enrolment).** When the Owner creates a Connection for a Person, the Agent leaves that Person a notice where they are already signed in: the household dashboard for a Member-class Agent, the Console for an Owner-class one. The notice says a Connection is waiting and carries nothing secret. On that page the Person presses **Get my key**, and only then is the single-use key minted, for them, and stored only as a hash. It is never kept in readable form, and the Owner who made the Connection never sees it. The Person sends the key to the bot, and the first account to present it becomes the Person's binding on that Connection, which also closes the invite. A key sent in chat proves possession of the chat account, which is what enrolment needs. Collecting it on their own signed-in page is what makes it the right Person's key.
- **The reverse, a Person typing their platform user ID, is deferred.** An ID typed on a dashboard proves nothing about who holds that chat account, so the bot would have to confirm with the account first. Most platforms do not let a bot message someone who has never messaged it, which brings the flow back to a key sent by the Person. It can be added later as a convenience on top of that confirmation. It is not a way around it.

### B7.2 Amendment (2026-09-28): a Matrix bot on the household's own homeserver

Decided with the household owner. It narrows B7.1's "the Owner supplies the platform's credential" for one case.

- **When the household runs its own Matrix homeserver, a Matrix bot needs no credential.** The Connect form offers **This household's Matrix server** beside the existing token option, and chooses it by default. The Owner picks the Agent and the Person. Nothing else is asked.
- **The bot is an application service, not a person's account.** Before this, the only way to get a Matrix access token for a homeserver with single sign-on only was to make a household person for the bot, enrol them in two-factor, sign in as them in a browser and copy the token out. That bot was a directory user, with a family member's reach into every other household app. Its token also depended on a browser session nobody may sign out of. An application-service user has no password, cannot use single sign-on and is not in the directory, so it cannot sign in to anything.
- **The bot owns its registration.** A `chat-matrix` instance with no homeserver address generates its token on the node and declares a `MatrixAppService` resource: its user (`@wildstone-<instance>`), and references to its token and to the token the homeserver would use in return. The registration's sender is a separate, unused `@wildstone-<instance>-as`, because Synapse refuses `/sync` to an application service's sender. The bot acts as its own user with the registration's token and a `user_id` parameter, which is what an application-service namespace is for. The namespace is derived from the bot's user alone (the bot and its sender), and it is exclusive, so a registration cannot claim another bot's user or a person's. Keystone merges every registration into one file in the homeserver's managed subtree, the same way it merges OIDC clients for the identity provider. The homeserver package installs that file for Synapse with a root Job, because the file holds tokens (written 0600 root) and Synapse runs as an unprivileged user. The file is in the config digest, so adding or removing a bot re-runs the Job and restarts Synapse, which loads application services only at start.
- **Nothing is pushed, and nothing new listens.** The registration's `url` is null. The adapter still reads its rooms with `/sync` and reaches Synapse by container name on the node's container network, so the adapter's only additions are registering its user once, acting as that user on every request, and setting its display name to the Agent's name. B7.1's "the adapter dials out" still holds.
- **The bot runs on the homeserver's machine.** Its token is a node secret, and the file Synapse loads is on Synapse's node. Placing the bot anywhere else is refused, not silently moved. A bot on another machine, or on somebody else's homeserver, keeps B7.1's token flow.
- **One token is still one bot.** There is no pasted credential to fingerprint, so Keystone records a fingerprint of the bot's own capability name in its place. B7.1's refusal of a credential already in use has nothing to compare here, and each local bot is its own bot.
- **Removal keeps the user.** Removing the bot drops its registration, and Synapse forgets the application service on restart. The user stays on the homeserver so its past messages keep their sender, as a person's would.

## B8. Homepage is a first-class trusted Connection

Homepage is not a special bypass around the Connection model.

A Person using Homepage may select and converse with any Member Agent to which they are admitted. Owner Agents are not exposed on Homepage.

## B8.1 Groups are one household principal namespace

Service entitlements (A5), memory audiences (C-series) and secret grants (D3) all reference the same groups — `Adults`, `Kids` — rather than each keeping a parallel notion of who they mean.

One namespace is not one permission. Membership is an identity primitive; what it authorizes is each resource's own decision. Being in `Adults` does not confer every adult service, every adults-only record and every adults-only secret; it means that when those rules each say "adults" they mean the same people. The alternative is two group systems that drift, and a household unable to explain why someone is in one and not the other.

## B9. Connection admission supports Person and Group subjects

The admission schema supports both Person and Group subjects from the beginning so group-oriented capability does not require a schema redesign later.

MVP admission is Person-based. True group-conversation semantics are future work because safe group chat requires speaker identity, changing audience membership, destination-aware disclosure, and per-speaker authority.

Unknown or unbound external identities are refused before model inference.

**So group and multi-recipient destinations fail closed in MVP**, and this is the consequence of deferring group semantics rather than a separate rule. Person admission authenticates the speaker; it says nothing about who else is in the room. A *recognised* Person addressing an Agent from a group destination is refused and directed to a one-to-one Connection, because otherwise they could pull their own Personal memory in front of unadmitted participants while every disclosure rule here still read as satisfied. "The sender is known" is not "the audience is known".

Owner-Agent Connections admit Owners only. Configuration and runtime both enforce this.

### B9.1 Amendment (2026-09-19): strangers get nothing; groups need the bot addressed

Decided with the household owner.

- **An unknown account gets as little as possible: nothing.** Any reply — a greeting, a hint, "that code did not work" — tells a stranger the bot is a household's assistant and how to get in. The bot says nothing to an account it does not know, not even during an outage. The one reply such an account can earn is "you're connected", for sending a valid key in a direct message, which only a Person the household invited has. Keystone records that an unknown account wrote to a bot (the account and the bot, never the text), so the household can see a bot being probed.
- **A group or server channel is allowed, but only what is addressed to the bot is the bot's.** A message there reaches the Agent only when it mentions the bot or replies to it: `@bot` on Telegram, a mention on Discord and Slack, `m.mentions` or the bot's id on Matrix. Everything else said in the room never leaves the adapter, and Keystone refuses an unaddressed group message as a backstop. The mention is removed before the Agent reads the message. Admission is unchanged: an addressed message from an account that is not bound is still silence, and the Agent is told it is speaking in front of a shared audience. An Owner Agent still answers in direct messages only.
- **A stranger cannot flood the household through a bot.** The adapter drops floods before they reach Keystone:
  - each account has a small budget;
  - an account Keystone called unknown is dropped in the adapter for ten minutes, so a stranger costs one request rather than one per message;
  - accounts not yet seen bound share one budget per bot, so many throwaway accounts count as one;
  - invite-key attempts have their own small budget, per account and per bot.

  Keystone is the backstop. It checks an account before claiming its message, so a stranger's message writes nothing. It notes an unknown account at most once per account per ten minutes, and not at all past a per-bot budget. Key attempts are limited per account, and refused keys are noted once per account per ten minutes. Every table is bounded.
- **This relaxes B9's "group destinations fail closed in MVP"** for Member Agents, on the owner's decision: a Person the household admitted may talk to their Agent in a group they are in, when they address it. The disclosure concern B9 names still holds, and the audience note is how the Agent is told about it. Full group semantics, such as speaker-aware disclosure and per-destination policy, remain future work (#212).

## B10. Conversations follow Person + Agent, not transport

A session is fundamentally associated with:

```text
Person + Agent + session_id
```

Messages record which Connection they arrived on and left on, but the Connection is not the session identity.

If the same Person talks to the same Agent on another Connection within a configurable recency threshold, and exactly one session is eligible, Wildstone continues it. Outside that threshold, it starts a new session.

Where more than one session is eligible, Wildstone asks rather than choosing. Two recent conversations with the same Agent are two conversations the Person kept apart, and continuing the arbitrarily-more-recent one, or merging them, is a decision the system is not entitled to make on the Person's behalf. No candidate's context is loaded until they answer; `/new` resolves it without choosing.

The **whole candidate** is disclosure, not merely its subject, and it is governed separately from context. A conversation can be revealed by its subject, by its timestamp, by a neutral placeholder holding a slot in a list, or simply by the list being one item longer; "no context is loaded" covers context rather than metadata, and the candidate is precisely what is shown before anyone has chosen.

So the current Connection's disclosure policy — its disclosure/privacy classification per B7, and the delivery boundary of D7; design contract §8.4 carries the detail — is evaluated against each part separately before display: **existence** — omit the candidate entirely if not permitted; **subject** — replace with a neutral label; **timestamp and activity metadata** — omit; and likewise any other identifying metadata. Existence is the leg that has to be policy-dependent, because checking only the subject leaves a protected session disclosed by the placeholder that replaced it — a slot in a list still says there is a conversation here you are not being told about. Candidate count and ordering are taken from what survived filtering and must not imply what was removed.

After filtering: exactly one safe candidate continues unambiguously; several safe candidates go to the Person to choose among those only; and where none are safe while protected candidates exist, the system **fails closed** — a generic redirect to a trusted Connection such as Homepage that reveals no number, subject, timestamp or existence, and reads identically whether one protected session was hidden or five. The case is sessions begun on Homepage and continued ambiguously over Telegram: safe where they were written, not safe where they would now be shown.

A command such as `/new` starts a new independent session immediately. Other session commands such as listing or resuming sessions may be added without changing the architecture.

The most recent accepted inbound Connection is the default reply path.

## B11. Cross-Connection fallback is explicit policy

If the current reply Connection becomes unavailable, Wildstone does not silently switch to another Connection by default.

Fallback may be explicitly configured between approved Connections, for example Discord to Telegram while Homepage to Telegram remains disallowed. This allows convenience without erasing privacy boundaries.

---

# C. Memory and knowledge

## C1. There are four memory scopes

Wildstone distinguishes:

1. **Session context** — transient conversation/task state.
2. **Agent-local memory** — durable knowledge belonging to one Agent.
3. **Personal memory** — durable knowledge scoped to one Person.
4. **Household memory** — durable knowledge intentionally shared across the household.

They may live in one physical memory service, but their logical ownership and authorization are distinct.

## C2. Session context is not durable memory by default

A conversation may influence the current session without automatically becoming durable memory.

When a Person explicitly says "remember this", that instruction itself is sufficient authorization to create Personal memory. Wildstone does not ask a redundant confirmation.

## C3. Agents may autonomously create Personal memory

An Agent may extract and save relevant Personal memories from ordinary conversation without asking for confirmation each time, because the resulting memory remains scoped to that Person and under that Person's control.

The Person can inspect, correct, forget, export, or restrict their Personal memory.

The Agent must preserve provenance and must distinguish stated information from inference rather than silently promoting guesses to facts.

## C4. Household memory is the sharing boundary

Agents do not autonomously promote Personal memory to Household memory.

An Agent may propose promotion, but the Person whose Personal memory it is must approve that promotion through normal product controls.

Owner status does not make one Person the owner of another Person's Personal memory. Owners may administratively inspect server-held data, but inspection is not ownership and does not grant normal-product authority to promote another Person's memory.

## C5. Agent-local memory is real and isolated

Each Agent may autonomously create durable Agent-local memory that it considers useful for its own operation and patterns.

Agent-local memory is not directly readable by other Agents initially. Cross-agent continuity should primarily come from Personal and Household memory rather than one Agent reaching into another Agent's private store.

**Isolation from other Agents is not isolation between people, and the second is the likelier leak.** One Agent serves several people, so a record it learned from Person A sits in the store that answers Person B; cross-Agent isolation says nothing about that. A derived Agent-local record therefore inherits the strictest audience and privacy constraints of its sources and may never widen them, and on a shared Agent retrieval and prompt assembly evaluate the active Person against those inherited constraints. Otherwise Agent-local memory becomes a way to launder a source's audience by copying it somewhere that has none of its own.

On Agent deletion, an Owner can delete, export, promote selected records, or transfer Agent-local memory to another Agent. Transfers preserve provenance indicating the source Agent and the authorized transfer.

## C6. Personal-memory access follows Agent admission by default

When a Person is admitted to use an Agent, that Agent receives access to that Person's Personal memory by default. The access can be explicitly restricted.

Those admissions never combine. An Agent serving several people resolves Personal memory for the **Person on the current request** and no one else; the union of what it may reach across everyone it serves is not a set anything is evaluated against. Otherwise an Agent admitted by two people answers one of them out of the other's memory, and every rule in this section still reads as satisfied.

This avoids a second configuration step merely to make the Agent useful while keeping the scope tied to that Person.

## C7. Member Agents receive Household memory by default

Member Agents receive Household-memory access by default, subject to explicit restriction and per-record privacy policy.

That access is **eligibility to query the household namespace, not authority to disclose every household record to every Person admitted to the Agent.** Retrieval and prompt assembly evaluate each candidate record against the active Person on the current request, including the record's audience and privacy policy; a record whose audience excludes that Person is excluded from both, whatever the Agent can reach.

The case that makes it concrete is one Member Agent shared by an adult and a child. The Agent is eligible for Household memory and the child is not in an adults-only record's audience — so without the active-Person check the Agent's eligibility would be doing the child's authorization for them. This is the same rule C6 applies to Personal memory, and the reason it has to be said twice is that Household memory is *meant* to be shared, which makes the missing check look like correct behaviour.

The per-Agent restriction is unchanged and is a second limit rather than a substitute: it can narrow what an Agent reaches below what its people could see, and can never widen it above them.

**An unattended routine has no active Person, so it is its own memory principal.** A scheduled routine runs with nobody present (E6), which leaves the active-Person rule above with no subject; the answer is not to borrow one. An approved routine version is a bounded principal for memory retrieval, as it already is for Household-secret `use` (E6.0), and it never inherits or impersonates its proposer, approver, or any other Person. Effective unattended access intersects the exact routine-version authorization, that version's memory grant, the record's own audience and privacy policy, the assigned Agent's eligibility, **the routine's approved output audience**, and current privacy and safety policy.

The output-audience leg is what makes this safe rather than merely well-typed: a routine reads in order to deliver, so what it may read is bounded by who will read its output. An adults-only Household record must not feed a routine delivering to a Child audience, and Agent-local source inheritance (C5) is not bypassed by running unattended.

Personal memory is the boundary worth stating on its own: **Owner approval of a routine does not grant another Person's Personal memory.** Approval authorizes the work; it does not transfer ownership of the knowledge the work may consume. Such routines are not prohibited — they may simply require grant support that does not exist yet.

Owner classification does not imply unrestricted access to every Person's Personal memory. Administrative authority and disclosure authority remain separate.

## C8. Memory records preserve audience, provenance, and correction history

A durable memory record carries enough metadata to support at least:

- scope/owner
- subject
- content
- kind
- audience
- provenance
- confidence or stated/inferred status
- lifecycle status
- sensitivity/privacy policy

Corrections supersede rather than silently rewrite history.

Derivatives inherit the strictest disclosure/privacy requirements of their sources.

Retrieval filters inside the candidate query and rechecks the destination before delivery.

## C9. Retrieval combines exact and semantic candidates under one permission filter

Keyword and semantic retrieval are both useful, but neither may bypass audience/privacy filtering.

Live applications remain authoritative for live state. Memory is contextual knowledge, not a replacement for source-of-truth service data.

## C10. Forgetting survives restore

Forgetting/deletion must reach active memory, derived indexes, summaries, and caches and must be journaled so a backup restore cannot resurrect intentionally forgotten data.

## C11. Sensitive semantic memory is not a secret

Semantic information that an Agent needs to reason about remains memory even when sensitive. It may carry stronger policy, for example:

```text
sensitivity: protected
cloud_processing: local_only
external_delivery: restricted
```

Memory also preserves processing provenance, including whether cloud inference was already involved when the information was captured.

A privacy rule can become stricter later, but provenance is not rewritten to pretend earlier disclosure did not occur.

---

# D. Secrets and privacy

## D1. Household secrets use a store separate from infrastructure/node secrets

The existing node/infrastructure secret store keeps its own properties. Household/user secrets are a separate domain because they require different ownership, use, reveal, and recovery semantics.

Secret scopes include at least Personal and Household, and they do not share authorization rules.

A Personal secret belongs to exactly one Person and its audience is that Person alone. No other Person may use or reveal its raw value, **including an Owner acting through normal product authority**.

Secret permissions are product-level: **use** — interactively, with the owning Person active on the request — by an Agent that Person has admitted *and* explicitly granted; **manage** — create, edit, grant, revoke, control reveal — by that Person only.

**Unattended routines cannot use Personal secrets; they fail closed.** A Person's grant to an Agent authorizes that Person's own interactive use through that Agent; it is not a standing capability the Agent carries into work the Person is not part of. When a routine runs with no active Person (E6), a Personal secret does not resolve, and nothing substitutes for the absent owner — not the Agent's grant, not Owner approval of the routine, not the routine's own approval or version, not a shared-Agent relationship, not the proposer's identity, not the approver's. That list is exhaustive deliberately: every entry is a plausible-looking stand-in, and a Personal secret has exactly one legitimate authority, who is by definition not there.

The hole this closes is concrete: Person A grants a shared Agent use of A's Personal secret, an Owner approves an unattended routine on that same Agent, and the routine resolves A's secret with A never involved. Household secrets remain the supported type for unattended work, through the exact-version principal in D3/E6.0.

An MVP boundary rather than a permanent prohibition. Personal-secret use by routines would need explicit Person-authorized exact-version delegation with its own revocation, audit and interface semantics; none of that is designed here, so until it is the answer is no.

Administrative lifecycle operations are deliberately a separate concept. An Owner runs the server, so an Owner can quarantine, disable, delete and resolve ownership at offboarding. Those act on the object and **never reveal the raw value or confer `manage`**: an Owner can make a Personal secret stop existing without being able to read it, grant it, or decide who else may. Collapsing the two would make "Owner" a reveal path by accident, which is the same boundary C7 keeps for Personal memory — administration is not ownership.

**Personal-secret raw reveal is deferred for MVP.** Members may propose Personal secrets and use authorized ones through tools; direct create/edit/delete controls are deferred, and there is no Personal reveal surface at all unless a separately approved trusted one is added later. Household secrets are the shared case the rules below describe, and their reveal rules are unchanged.

## D2. Models receive references, not raw secret values

By default, a model receives an opaque secret reference rather than the value.

Trusted runtime/tool code resolves the reference below inference. Resolution rechecks identity, authorization, destination, and validity.

This remains the default even for local models so secrets do not casually leak into transcripts, memory extraction, embeddings, summaries, or a later cloud fallback.

## D3. Use, manage, and reveal are distinct permissions

A tool may use a secret without the Agent/model seeing it.

The permission model distinguishes at least:

- use
- manage
- reveal

Member Agents may be allowed to use authorized secrets through tools without receiving manage or reveal authority.

**For Household secrets, authorization is principal/object rather than an Agent-wide boolean.** A Household secret grants its permissions to explicit principals, and an Agent grant makes that Agent technically capable of using the secret through a tool without ever substituting for authorizing the caller.

There are **two kinds of principal because there are two kinds of execution**, and this general rule recognises both rather than leaving routines as an exception carved out of it. **Interactive**: the principal is the active Person, directly or through Group membership, and effective `use`/`reveal` intersects that Person or Group, the Agent/tool capability, the secret's own permission grant, and the trusted Connection or reveal-surface policy. **Unattended** — Household secrets only, since Personal secrets fail closed with no active Person (above): the principal is the exact approved routine version (E6.0), no active Person is required, the runtime never borrows or impersonates the proposer or approver, and effective `use` intersects that exact-version principal, the Agent/tool capability, a `use` grant held by that exact routine version, current service capability, and current privacy and safety policy.

Across both: a routine principal gets `use` only and never human raw-value `reveal`; `manage` stays Owner-controlled; and changing a routine's executable behaviour or its required secret grants creates a new version needing reapproval.

Stating only the interactive form here would make this rule contradict E6.0, and a literal implementation would have to either refuse every unattended secret-backed routine or quietly bypass this section — which is how a bypass becomes the real authorization rule.

Without this, a Member Agent admitted by two people becomes a way around the audience: Person B uses or reveals a secret granted only to Person A, or only to an adults-only group, because the Agent they share holds a grant and the Agent was treated as the principal. Reveal surfaces are unchanged (D4) — this decides who may reveal, not where.

## D4. Secret reveal depends on both Agent class and trusted Connection

Owner Agents are not reachable on Homepage. An Owner Agent may reveal an authorized secret only through trusted Console chat/UI, subject to current Owner authorization and fresh administrative session requirements where applicable.

A Member Agent may reveal an authorized Household-shared secret through the trusted Homepage Connection.

Raw reveal should be rendered by trusted UI/runtime code as a protected component rather than inserted into ordinary model-authored transcript text where possible.

External chat Connections such as Telegram or Discord are not secret-reveal surfaces merely because the sender is authenticated.

## D5. Member secret management is intentionally narrower in MVP

The long-term model may allow Members to manage their own Personal secrets while Owners manage Household secrets.

For MVP, Wildstone may be stricter:

- Members may propose secrets.
- Members may use authorized secrets through tools.
- Members may reveal authorized Household-shared secrets through trusted Homepage.
- Owners create/edit/delete Household secrets and approve proposals.
- Owners reveal through trusted Console.

This is an implementation simplification, not an architectural prohibition on future Personal-secret management.

## D6. A secret disclosed over an external Connection is already disclosed

If a Person types a secret into Telegram or another external Connection, Wildstone must not pretend it remained local.

The system should warn, stop repeating/using the raw value in conversation, offer a secret-storage proposal, and preserve provenance that the secret was received through that external Connection.

## D7. Processing and delivery are separate privacy boundaries

Every sensitive operation asks two independent questions:

1. May this content be processed by this model/provider?
2. May this content be delivered over this Connection?

A local model does not make Telegram private. A trusted local Homepage Connection does not make a cloud model local.

Connections therefore carry disclosure/privacy characteristics sufficient for policy evaluation, such as local/external transport, encryption properties, and retention/logging class.

If privacy requirements cannot be satisfied, execution fails loudly. Wildstone does not silently downgrade privacy.

---

# E. Plans, approvals, routines, and work

## E1. Plan is a first-class primitive

Durable or consequential proposed changes are represented as versioned Plans rather than informal chat intent.

A Plan carries at least:

- proposer Person
- proposing Agent
- exact version
- requested tools/capabilities
- trigger/schedule where relevant
- targets/parameters
- delivery behavior
- approval requirement
- status
- resulting resource/action

Plan states include at least `draft`, `submitted`, `awaiting_review`, `approved`, `rejected`, and `superseded`.

Pending Plans remain pending indefinitely in MVP rather than expiring automatically.

## E2. Agents propose; human Owners activate

Member Agents may draft and submit Member-class Plans.

Owner Agents may draft and submit Member- or Owner-class Plans.

No Agent may approve or activate its own Plan. A human Owner must approve the exact executable version.

There is no standing grant under which an Agent may activate a routine itself.

### E2.1 Amendment (2026-09-19, #269): household settings an Agent can propose

The household's time zone is the first household setting an Agent can propose, and the pattern is meant for the rest.

- **The time zone is a setting, not a startup flag.** It lives with the household's other runtime settings (the zone quiet hours already used). The `-household-timezone` flag only seeds it while it has never been set, and the installer asks for it. Everything that keeps the household's time reads it live: the Agent's "it is now", calendar tools, household rules, upkeep and the household pages. A change applies to the next thing that asks, with no restart. A host clock is not the household's clock: a VM running UTC would put "3pm" hours off.
- **An Agent proposes; an Owner decides.** An Agent that hears the clock is wrong calls `household.propose_timezone`. That records a Plan carrying the setting change and nothing else, which waits on the Plans screen like any proposal. Approving it applies the setting. The Agent changes nothing itself, and tells the person so.
- **An Owner may also set it directly**, on the Household screen. It is not a consequential operation under A4.1.

## E3. Submitted Plans enter an Owner Review Queue and notify a designated Owner

The persistent Owner Review Queue is the source of truth and is visible to all Owners.

Notification delivery follows designated Owner/reviewer preferences. For example, Alfred may notify the designated Owner over Telegram that Abigail and Sandy submitted a routine Plan.

The Agent/Connection delivering the review request is not the approver. The human Owner is.

The first valid Owner decision resolves the submitted version.

## E4. Editing and approval are separate actions

Editing an executable Plan creates a new version. That version requires an explicit approval action.

No material edit inherits approval from an older version.

## E5. Approved routines are household-owned

Once approved, a routine is a durable Household resource rather than property of the proposing Person or assisting Agent.

It records provenance such as:

- proposed_by_person
- proposed_through_agent
- approved_by_owner
- assigned_agent

but its continued existence does not depend on those actors remaining present.

## E6. Approval creates bounded routine authority

An Owner approves a routine once; each normal run does not require another approval.

Approval binds the exact routine version and its executable bounds, including tools, targets, parameter limits, trigger/schedule, **output audience**, delivery policy, assigned Agent, and the **per-resource grants** the version requires.

Output audience and delivery policy are deliberately separate. Output audience is *who may receive or learn from the result*, and it is an authorization input — it bounds what the routine may read (C7), because a routine reads in order to deliver. Delivery policy is *where and how* the result goes. A routine can have a correct destination and a wrong audience, which is why the audience is approved rather than inferred from the destination.

The runtime still checks current service/tool availability and current safety/privacy policy on every run.

## E6.0 An approved routine is its own principal for secret use

A scheduled routine runs unattended: no active Person, and by E6 it does not keep borrowing its proposer's authority. Household-secret authorization grants to Person and Group principals (D3), so without a third kind of principal every routine needing a secret either fails that intersection or impersonates a proposer indefinitely — and the second is worse for being invisible.

So an approved routine is a principal in its own right, **bounded to its exact approved version**, and a Household secret may grant `use` to that routine version as it would to a Person or Group. Effective unattended `use` intersects the exact-version routine authorization, the routine/version secret-use grant, the assigned Agent/tool capability, current service capability, and current privacy and safety policy.

Any change to executable behaviour **or to the secret grants a routine requires** creates a new version needing Owner reapproval: a routine cannot acquire a secret it was not approved with by being edited around it. This covers `use` only — raw-value reveal to a human is unchanged, since a routine resolves a secret below inference to reach a service and never reveals one.

An Owner-approved Agent **subscription** version (ADR-W039 §3.6) is the same kind of unattended principal: every rule here and in C7, D3, E6 and E6.1 that names an approved routine version applies to it identically.

## E6.1 Any Owner can stop a routine immediately

Everything in E5 and E6 makes an approved routine durable: it runs without per-run approval, and it outlives its proposer and its approving Owner. Durability without a stop is a routine nobody can turn off, so the stop is stated here rather than left to be inferred from lifecycle.

**Stopping needs no Plan and no approval.** It is deliberately asymmetric with starting: an Owner who has decided a recurring consequential effect must end does not wait in a review queue behind the thing they are trying to stop.

`disabled` preserves the bounded authorization so the routine can be re-enabled without a new approval. `revoked` withdraws it, and returning to service takes a new approved version like any first activation. Soft deletion ends execution authority at the soft delete, not at the purge. In-flight runs stop at their next boundary on the same terms as F1, and an effect already committed past a boundary is recorded rather than assumed away. A household-wide stop halts all routine execution at once.

## E7. Member-class routines contain only Member-class tools

A Member Agent cannot propose or execute a routine requiring Owner tools.

An Owner Agent may propose Member- or Owner-class routine work, but human Owner approval is still required for activation.

## E8. Routines separate deterministic steps from inference steps

The routine engine/scheduler owns routine execution. An Agent is invoked only where inference is required.

If the assigned Agent is paused or unavailable:

- deterministic steps continue normally
- inference-dependent steps become blocked or fail loudly with visible status
- the routine itself is not destroyed or silently paused as a whole

Wildstone does not automatically substitute another Agent.

Changing the assigned Agent is an authorization-relevant edit and requires a new routine version and Owner approval.

## E9. Tasks outlive turns; turns own fresh ledgers

A Task represents the thing a Person asked to accomplish and may span multiple turns.

Each turn gets a fresh ledger of observations, attempts, tool results, and evidence. A later turn must not silently reuse stale readings from an earlier turn.

Budget, overall status, and uncertainty belong to the Task.

## E10. Completion requires evidence; uncertainty is explicit

A model claiming an action completed is not evidence.

External effects should use idempotency/deduplication where supported. After a timeout, Wildstone inspects the external outcome before retrying. If confirmation is impossible, the Task records an uncertain outcome rather than blindly repeating a consequential effect.

## E11. Learned procedures are structured definitions

A reusable learned procedure is a versioned structured definition, not a remembered sentence reinterpreted from scratch on every run.

Saving a procedure grants no new authority. Activating it follows the same Plan and Owner-approval rules as any other routine.

---

# F. Lifecycle, revocation, deletion, and recovery

## F1. Authority revocation is immediate; durable-resource resolution is explicit

**Demotion is not offboarding.** Owner → Member demotion recomputes authority *downward*: Console access, elevated Owner sessions, Owner-only admissions and pending Owner-bound approvals and artifacts go immediately, while the Person's Member identity, Homepage and entitled-service access, Member-Agent admissions, Personal memory and every other Member-level authority remain. A demoted Owner is still a Member of this household, and running the full path below on them would disable the identity that is supposed to remain. Demotion borrows the mechanism — recompute now, recheck at every boundary — without borrowing the scope.

Revoking, disabling, or deleting a Person immediately removes active authority:

- revoke SSO/web/Console sessions as applicable
- update directory/group entitlements
- revoke Agent Connection admission
- invalidate Person-bound approvals that have not yet been consumed
- invalidate prepared private context where necessary
- deny new actions
- stop queued and in-flight work at its next boundary

The last of those is not covered by "deny new actions", where the operative word is *new*. At the moment of revocation there is already authorized work in motion — a reply in a delivery queue, a model turn mid-flight, a retry about to fire, private context already assembled for a prompt. Each rechecks authority at its next boundary and fails there, and a delivery stopped this way reports the failure without disclosing what it carried. Revocation is prompt because it sweeps and correct because everything rechecks anyway; neither half replaces the other.

"Invalidate approvals" reaches authorization still bound to that Person — pending and unconsumed approvals, queued Plans awaiting their review, approval sessions, and prepared artifacts such as issued secret references. It does **not** reach an already-materialized Household routine authorization that was validly approved beforehand: that belongs to the Household (E-series), not to its proposer or approver, and F2 below depends on the distinction. Without it, an Owner leaving would quietly take the household's routines with them.

The one case resolved rather than assumed: a routine that delivers to the revoked Person, or that depends on an admission only they held, pauses for Owner resolution. Not deleted, and not left delivering to someone who has gone.

Durable/shared resources are not silently destroyed merely because the Person left.

## F2. Person deletion inventories durable resources

Before final deletion/purge, Wildstone shows Owners the durable resources associated with the Person and allows explicit resolution such as transfer, retain, export, retire, block, or delete as appropriate.

Personal work/data may be exported or deleted. Household routines may continue because they are Household-owned.

## F3. Agents have explicit lifecycle states

Agents support at least:

- active
- paused
- retired
- pending deletion / soft-deleted
- purged/deleted

A retired Agent remains inspectable in Console, including configuration/history and exportable memory, but cannot execute again.

## F4. Agent deletion resolves memory, routines, Connections, and work

Deleting an Agent presents an explicit resolution flow:

- Agent-local memory: delete, export, transfer, or promote selected records
- assigned routines: reassign, leave blocked, or retire
- Connections: disable/revoke
- outstanding work: cancel or block with recorded reason

Personal and Household memory are not deleted merely because an Agent is deleted.

## F5. Service/package removal makes bindings dormant rather than erasing them

When a package/service disappears, its service access groups, user assignments, Agent tool grants, and routine definitions remain but become inactive/unavailable.

A routine depending on a missing capability becomes `blocked` with a visible reason.

When the exact compatible capability returns, dormant bindings reactivate and a routine blocked only for that reason automatically returns to service, with the transition recorded in history/audit.

Materially changed semantics must not be treated as the same capability merely because a label is similar.

## F6. Soft deletion persists until explicit Owner purge

People, Agents, Connections, routines, and similar durable resources are soft-deleted by default and remain recoverable indefinitely until an Owner explicitly performs final purge.

There is no automatic retention timer required for MVP.

---

# G. Amendments and supersessions

This ADR is authoritative where it conflicts with earlier decisions. The older ADRs have been edited to remove the conflicting clauses and now state the rules below directly; git history holds their earlier wording.

What changed, and where:

- **Routine approval (ADR-W030 A1/A6, and the clauses copied from it into W007, W009, W020, W024–W027, W035):** no Agent activates a routine, and there is no Member or "admitted adult" approval or household-reviewer page. A human Owner approves the exact version (E2–E4); any Owner stops a routine without a Plan (E6.1).
- **ADR-W030 A7/A8:** authority remains an intersection but now uses the Person, Agent, Connection, and Task/routine authorization as distinct required legs (A0). Task authority is derived, not an issued grant.
- **Surfaces and bots (ADR-W026 A1/A3/A4, W029 A7, W031 A1/A3/A7, W030 A1):** the Household/Operator/family/admin surfaces are replaced by Owner/Member Agent classes, tool classes and Connections (B2–B5, B7, B7.1). Each Agent keeps its own bot and credential. Unknown accounts get no reply (B9.1).
- **ADR-W031 A1/A4/A10:** Agents are household-owned and administered by any Owner (B1). Memory uses the Session, Agent-local, Personal and Household scopes (C1–C11).
- **ADR-W032, ADR-W016 A3:** exactly two human roles; the Console is Owner-only, signed in through OIDC with step-up (A1, A4, A4.1).
- **Single model per Agent (ADR-W017 A4/A9, W030 A9, W031 A2/A6):** replaced by a per-Agent ordered model/provider chain (B6). ADR-W039 amends B6 for external runtimes.
- **First administrator (ADR-W007, W014, W020 A2/A4, W023 A3):** the installer creates the first Owner; recovery is the host CLI (A3, A7).
- **Offboarding (ADR-W020, W024, W030 A4):** Household routines survive the Person who proposed or approved them (E5, F1, F2).
- **Agent pause and routines:** Household routines survive Agent loss; deterministic steps continue and inference steps block (E8).
- **ADR-W012 infrastructure secret-store properties:** retained; Household/user secrets are a separate store (D1).
- **ADR-W030 A2.1 / the Child surface:** **not** superseded. A8 states that a Person can be a scheduled subject without being admitted; A2.1 continues to gate a child *acting* — credentials, principal, endpoint, gateway, transport eligibility — and no child deployment is authorized by this ADR either. `AdmitButlerMember` still refuses a child record by every path. Guardian records are retained and given meaning by A9.
- **Calendars (ADR-W034 A9/A12):** the single shared calendar became a registry of declared calendars with per-calendar access (ADR-W038), which narrows A10 household-scope visibility and never widens it.
- **ADR-W039 (later amendment of this ADR):** external runtimes own their model chain, gated by the Owner-declared processing class (B6); an approved subscription version is an unattended principal like a routine version (E6.0).

Detailed finding-by-finding coverage remains in the audit documents.

---

# H. Costs and rejected alternatives

The design intentionally accepts several costs:

- Persistent household SSO and strict Console reauthentication require two distinct session behaviors.
- Dynamic service/tool bundles require stable capability identity and compatibility checks.
- Four memory scopes and disclosure policies are more complex than one generic memory store, but they match the actual ownership boundaries.
- Agent-local memory introduces lifecycle work at Agent deletion, including transfer provenance.
- A separate Household secret domain adds operational complexity but keeps raw credentials out of normal memory/model paths.
- Requiring human Owner approval for every newly activated routine adds friction but prevents Agents from silently creating durable automation.
- Cross-Connection session continuity requires careful privacy-aware reply routing.
- Fail-closed privacy means some work will stop rather than "helpfully" use an unsafe fallback.

Rejected alternatives include:

- a third fundamental `child` role
- first-browser-visitor bootstrap
- blanket forward-auth for every app regardless of compatibility
- Members administering Agents
- automatic Owner-tool grants when upgrading a Member Agent
- one global model fallback order
- Connection-bound conversation identity
- silent cross-Connection reply fallback
- eliminating Agent-local memory
- treating sensitive semantic memory as a secret
- allowing Agents to self-activate durable routines
- automatic Agent substitution
- hard deletion by default

---

# I. Dependency-oriented implementation order

The ADR records only the dependency order. Detailed implementation planning belongs in issues or a separate implementation plan.

1. Human identity and surfaces
2. Service entitlements
3. Agent classes and tools
4. Connections and sessions
5. Memory scopes and retrieval policy
6. Secrets and privacy boundaries
7. Plans and Owner Review Queue
8. Routine execution
9. Lifecycle and soft deletion
10. Audit remediation, migrations, and rollout

Detailed acceptance criteria live in [`docs/agent-system-design.md`](../agent-system-design.md), which is the clean design contract.

---

# J. Open implementation choices

The architecture intentionally does not freeze every implementation detail. Still open are items such as:

- exact session-recency timeout
- exact slash-command vocabulary beyond the required ability to start a new session
- exact directory/group projection implementation per app
- exact protected-memory sensitivity taxonomy
- exact Connection disclosure fields
- exact model/embedding products
- numeric Task/routine budgets
- exact UI for Plan review and protected secret reveal
- exact soft-delete purge UX

Those choices must preserve the decisions above and may be made during implementation without reopening this ADR unless they change an architectural boundary.
