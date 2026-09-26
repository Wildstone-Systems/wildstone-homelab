# ADR-W026: Butler messaging — chat Connections over dumb transports

- **Status:** implemented; amended by ADR-W033 (B2, B3, B7.1, B9.1)
- **Governed by:** ADR-W033 where they overlap.
- **Relates to:** ADR-W001 (acting identity), ADR-W007 (one API, one
  event stream, the RBAC table), ADR-W017 (Butler productionization —
  every session is a principal-bound record, the ask endpoint, degraded
  modes), ADR-W020 (identity onboarding, claim codes, the household
  member account), ADR-W024 (household events, the `domestic` tier
  gateway, A10's three shim shapes, A6 escalation/the Phraser),
  ADR-W025 (the catalog integration layer: integrating is data),
  ADR-W009 (policy), `docs/vision.md`
- **Audience:** decision record for the messaging layer. Part A is
  binding. Part B is the build order; each package gets its own spec
  when it is scheduled.

## Constraints from ADR-W030

- A child does not act over chat. A child is a Member profile with no
  binding (ADR-W033 A8), and ADR-W030 A2.1 still gates any future
  child-facing Connection.
- Proactive task and continuation notices follow quiet-hours, privacy,
  routing, and deduplication policy.
- Offboarding revokes every binding and active session for the Person.
  Queued replies and notices addressed to them stop at their next
  boundary without delivery, and no conversation context becomes visible
  to the remaining household because its owner was removed (ADR-W033 F1).

---

## The intent, in one sentence

A person should be able to talk to their Butler from the chat app they
already have open — an Owner about the homelab, anyone in the household
about the household — without a Member's conversation being able to do
what an Owner Agent can, and without Keystone learning a single chat
vendor's API.

## The problem

Today the Butler has exactly one front door: `POST /api/v1/butler/ask`
on the ONE API, consumed by the Console and `wild`. That is the right
door, and it is not a door anybody walks through from the sofa. Every
comparable agent (Hermes over Telegram, Discord, Slack) is reachable
where its user already is, and the Butler is not.

Adding chat naively breaks two things at once:

- **The audiences are not the same person.** An Owner Agent reads the
  Statefile, walks dependency chains, and proposes plans. A Member Agent
  asks whether the mail arrived. One bot doing both is
  one compromised phone away from being an infrastructure console.
- **Chat identity is a third party's.** A Telegram account is
  authenticated by Telegram, not by Wildstone. ADR-W017 A1 says there
  is no anonymous Butler session and no singleton admin session; a chat
  transport must not become the exception.

---

# Part A — Decisions (binding)

### A1. What a conversation can reach follows the Agent's class, not the transport

A chat Connection belongs to one Agent (ADR-W033 B7). What a message on
it can reach is decided by that Agent's class (B2) and the tool classes
it may hold (B3), never by the request body, the model, or the adapter.
A session is `Person + Agent + session_id` (B10).

The structural ceiling is the Member tool class: a Member Agent can
hold only Member tools, and configuration assigning it an Owner tool is
rejected (B3). The
infrastructure vocabulary — Statefile, plans, nodes, secrets,
capabilities, the operational event stream — is not filtered per caller
inside one gateway; it is simply not in a Member Agent's tool set.

**Rejected:** one tool set with per-caller filtering. The filter is then
the whole security boundary, in one code path, and every future tool has
to remember which audience it is for.

_Code still carries a `surface` field beside the Agent class; the class
is what governs._

### A2. The transport lives in a package; Keystone learns no chat API

A chat integration is the sidecar shim shape ADR-W024 A10 already
defined, and ADR-W025 already made writable as data: a package with a
small adapter container that speaks one vendor's protocol on one side
and the ONE API on the other. `chat-telegram`, `chat-matrix`,
`chat-discord` and `chat-slack` are packages. Keystone ships none of them and imports none
of their SDKs.

Consequences that make this the cheap shape:

- The adapter dials **out** (Telegram long-poll, Discord/Slack socket
  mode). No inbound port, no Route, no public hostname, nothing new on
  the ingress. The messaging layer adds no attack surface at the edge.
- A transport that breaks, gets rate-limited, or gets abandoned is a
  capability you disable. Its publisher and adapter credentials die
  with it (ADR-W024 A10).
- Adding a fourth chat app is a `package.yaml`, not a patch.

**Rejected:** built-in adapters in `cmd/keystone`. Same reason
ADR-W005 gave for packages being data, and the same reason ADR-W024
A10 rejected built-in publishers.

### A3. Each bot is its own adapter instance with its own credential

A chat package is one per platform — `chat-telegram`, `chat-matrix`,
`chat-discord`, `chat-slack` — installed once per bot, each install its
own instance, container, platform credential and Keystone principal
(ADR-W033 B7.1). There are no family or admin bots, and the package does
not encode an audience: who a bot admits follows the Agent it belongs to
(an Owner-class Agent's Connections admit Owners only).

Every adapter principal holds the `converse` verb and nothing else — no
read, no emit, no propose, no approve. It can relay messages for accounts
a human already bound and deliver answers back; it cannot read the
household stream itself, and a compromised adapter cannot enumerate the
house. The role minted follows the Agent's class (`concierge` for a
Member Agent, `valet` for an Owner Agent), both `converse`-only.

It is deliberately NOT the `butler` role. ADR-W024 A5 makes every
external agent a `butler`-role principal, so putting `converse` on that
row would hand each of them the private chat queue and the ability to
post as bound accounts. A new capability must not appear on credentials
minted before it existed.

A session's proposals are submitted under an internally constructed
`butler` identity acting-for the bound Person, never under the adapter's
own credential.

**Rejected:** one bot that switches Agent on who is speaking. The switch
would be a single `if` between a Member conversation and the Statefile.

### A4. Every chat message runs as a bound member, or it does not run

An adapter authenticates as itself and *names* the chat account that
spoke; Keystone resolves that account to a member or principal through
a **binding**, and refuses otherwise. The adapter never asserts who
somebody is.

A binding ties one chat account to one Person on one Connection.

Bindings are created by an **invite key** (ADR-W033 B7.1). When an Owner
creates a Connection for a Person, the Agent leaves that Person a notice
where they are already signed in; the Person presses **Get my key**, and
only then is a single-use key minted, stored only as a hash. The Owner
never sees it. The Person sends it to the bot, and the first account to
present it becomes their binding on that Connection.

An unknown or unbound chat account gets no reply at all (ADR-W033 B9.1).
The one reply such an account can earn is "you're connected", for a
valid key sent in a direct message. No enumeration, no error detail, no
session row. Revoking a binding is one call and takes effect on the next
message.

**Rejected:** a static allow-list of chat user ids in a package param.
It is a credential in a config file, it cannot be revoked without a
plan, and it makes onboarding a household member an Owner task with a
YAML editor in it.

### A5. No approve verb reaches any chat transport, ever

An Owner Agent can read and propose. Approving a plan — the single
human gate ground rule 4 exists to protect — is not available over
chat, for an Owner or for anyone.

The reason is a priori and not about trust in the Owner: a chat
account is authenticated by a vendor whose account-recovery flow
Wildstone does not control. SIM-swap someone's phone and you should
reach a Butler that can *describe* the homelab and *draft* changes.
You should not reach one that can apply them.

So the chat flow for a change is: ask in chat → the Butler proposes →
the reply carries the plan id and its risk grade → a human Owner approves
in the Console or `wild`, where the session is authenticated by a
Wildstone credential. This is a feature of the design, and the reply
says so rather than looking like a limitation.

### A6. Inbound chat content is untrusted input, on every Connection

A chat message is text an outside party wrote — in a group, possibly a
party who is not a household member at all.
It goes through the same treatment ADR-W017 A6 and ADR-W024 A4 already
specify: bounded before anything else (the existing `maxAskBytes`),
redacted, and wrapped in the untrusted-content envelope when it reaches
a model. The red-team suite grows the transport cases: an injected
message attempting to name an Owner Agent's tools, a group-chat
message impersonating an Owner, an envelope-forgery attempt in a
message body.

Rate limits are per binding, not per adapter: one member cannot exhaust
the household's provider budget, and a wedged adapter cannot loop.
Strangers are held back before that by the adapter's and Keystone's
flood budgets (ADR-W033 B9.1).

### A7. Group chats: Member Agents only, when addressed, and never a private fact

An Owner Agent answers only in a direct message from the bound account
(ADR-W033 B9.1).

A Member Agent may be used in a group (a household group chat is
exactly where "did anyone take the bins out" belongs), under these rules:

- Only a message that addresses the bot — a mention or a reply to it —
  reaches the Agent. Everything else said in the room never leaves the
  adapter (B9.1).
- Attribution is the **sending** account's binding. An unbound account
  gets no reply (A4).
- A group is a shared audience: the Agent may read the household's own
  facts, its devices, and the ASKER's own — never another member's.

The last rule is a capability, not prompt text. A group session is built on a
different gateway, and its reads are narrowed before they reach the
model.

The narrowing is by WHOSE fact a thing is, and that comes from a
declaration a package already makes — the `subject: member | device |
household | none` on each event type it publishes (ADR-W024 A10). An
event about a person is withheld unless it is the asker's; an event
about a device or about the household is not. Nothing here reads the
model's mind or the message's tone. Whose fact it is, not whether it
has a subject: "is the door still unlocked?" is exactly what a kitchen
group asks.

Two limits are deliberate and recorded rather than fixed:

- **Content inside a shared fact.** A household-wide calendar entry
  whose note names somebody is visible, because a subject filter cannot
  read a sentence. The prompt asks for discretion about content, and
  that part is model compliance — narrowed to the one place it cannot
  be structural.
- **No private routing.** A group answer goes to the group; there is no
  path that redirects part of it to a direct message. With the read
  narrowed there is no member-private material in the session to
  redirect, and building one would mean the model deciding what is
  private — the dependency this rule exists to remove. A rule that
  must say something privately already has the deterministic path: a
  `chat.send` to that member's own conversation (A8), which never passes
  through the group session.

With no catalog, nothing is declared, and every attributed fact except
the asker's is withheld. A deployment that has declared nothing is not
one where everyone's movements are public.

### A8. Outbound is a queue the adapter polls, and a `domestic` action a rule can name

Keystone does not dial a chat vendor, and it does not hold a
long-running connection per transport. Two paths, both existing shapes:

- **Replies** to a session land on the outbound queue for that bot that
  the adapter long-polls with a `seq` cursor — the same store-and-cursor
  pattern the event streams use (ADR-W024 A2: SQLite is the store and
  the bus). Delivery is at-least-once with a dedupe key, and an
  undeliverable message expires rather than accumulating.
- **Rules** address chat through the `domestic` tier gateway like any
  other action: the chat package declares a `chat.send` tool in its
  `agentTools` block (ADR-W005 A1), and an automation may name it
  exactly as it names `notifications.notify` today. Nothing new in
  `keystone/domestic` — a second executor registered by a package.

This makes the chat transport an alternative delivery for the ADR-W024
H4 loop, not a second one: a question asked over Telegram, answered in
Telegram, lands on the household timeline as `household.answered` with
the same correlation id an ntfy reply would have produced.

### A9. The deterministic half works with the provider off

ADR-W024's standing principle holds here. With no provider configured
(`-butler-provider off`, or a failed probe), the transports keep
working for everything deterministic: rule-driven notifications,
answer buttons, the morning report on request, plan and health
summaries from an Owner Agent. Only the conversational path answers
"no Butler provider is configured", with the same honest reason the
status endpoint already gives.

### A10. External agents are not chat Connections

Hermes, Codex, or any other outside agent remains what ADR-W024 A5
made it: a Keystone principal holding at most the `butler` role, read
and propose, going through the ONE API. It does not get a chat
Connection, a binding, or the domestic gateway. "Wildstone's Butler, reachable from
Telegram" and "somebody else's agent, calling Wildstone" are different
things and stay different things.

---

# Part B — Work packages (ordered)

Each lands nothing the previous one does not need. No real chat vendor
in CI: the adapters are tested against a recorded transport fake, and
Keystone's side is tested against a fake adapter.

| # | Package | Proves |
|---|---|---|
| **G1** | Surfaces in the core: `surface` on the session record, the family gateway (household context providers + `domestic` actions, no Statefile tools), the family system prompt, `POST /api/v1/household/butler/ask`. Console and `wild` are the first consumers | Two ceilings exist and are enforced structurally, before any transport can reach them |
| | **Status: implemented.** `butler_sessions.surface` (migration 0007) is required on every insert — there is no default, because both defaults are wrong. `butler.HouseholdGateway` is a second gateway holding three injected seams (household timeline, domestic actions, deployment vocabulary) and no Statefile, node reader or plan pipeline; a reflection test pins that field set. `Session` drives either surface through a `Toolset` interface that carries the system prompt with the tools, so a family session cannot run on the operator's prompt. Both surfaces share one `finishOutcome` redact/envelope/bound pipeline. The endpoint binds the session's member to the authenticated caller and stamps `family`; `GET /api/v1/butler/sessions?surface=` separates the audit trails. Consumers: `wild butler ask\|status\|sessions -surface`, and the Console's Butler screen. | |
| **G2** | Bindings: the table, bind/revoke on the API, the Console pane, the `converse` verb | A chat account can become a bound Person, and a stranger cannot |
| | **Status: implemented; binding reworked by ADR-W033 B7.1** — a binding is made by an invite key the Person collects on their own signed-in page (A4). Keys are stored only as a sha256, single-use, and every refusal is one indistinguishable 403. Redeeming is the `converse` verb, held only by adapter roles. Revoking takes effect on the next message; re-binding an account revokes the old row rather than failing on the unique index. | |
| **G3** | The chat contract: the outbound queue with its cursor, the inbound message endpoint the adapter posts to, per-binding rate limits, the A6 red-team cases | An adapter can be written against a documented, testable contract with no vendor in it |
| | **Status: implemented.** Three endpoints, all `converse`: `POST /chat/messages` (post what you heard), `GET /chat/outbound` (poll, long-polling cursor) and `POST /chat/outbound/{seq}/ack`. Which Agent a message reaches comes from the adapter's own credential, not the request, so a stolen Member-Agent adapter token cannot open an Owner-Agent session. `chat_outbound` (migration 0009) is one queue for replies and rule pushes alike, at-least-once with a dedupe key anchored on the transport's own message id, and messages expire rather than accumulate. Unknown and unbound accounts get no reply and no session row (ADR-W033 B9.1); rate limits are per chat ACCOUNT, ahead of the binding lookup. Group rules are A7. | |
| **G4** | `packages/chat-telegram`: the adapter sidecar (long-poll out, no ingress), `chat.send` declared as a `domestic` tool, the A7 group rules | The whole loop end to end on one transport, as data |
| | **Status: implemented.** The adapter is `cli/cmd/chat-adapter`: a small binary behind a three-method `Transport` interface, with the Telegram driver as its first implementation — long-polling `getUpdates`, so it dials OUT and the deployment gains no port, no Route and nothing at the edge. It holds one token and never names an Agent or audience itself. There is one instanced package per platform, installed once per bot (ADR-W033 B7.1). The adapter program ships with the release and the installer places it on every host; the package runs it read-only in a public, digest-pinned distroless image, so nothing of Wildstone's is published to a registry and no image is asked for. | |
| **G5** | Second transport (`chat-discord` or `chat-slack`) written by the same author against G3 with no Keystone change | The contract is a contract, not a Telegram-shaped hole |
| | **Status: implemented as Matrix, not Discord or Slack.** The claim holds: adding it took one driver file (`cli/cmd/chat-adapter/matrix.go`) plus a package, and the `keystone/` tree is untouched by the commit — no endpoint, no field, no role. **Why Matrix first:** its `/sync` is a long poll over plain HTTPS — the same shape as Telegram's `getUpdates` — and a household can self-host the homeserver. Discord and Slack drivers were added later over outbound websockets (Slack in Socket Mode), keeping A2's dial-out property; nothing above the driver moved. The driver skips its own echoed messages, takes a cursor rather than the backlog on first sync, and treats any room that is not exactly two people as a group, erring toward A7's more careful rules. | |
| **G6** | Household integration: chat as an ADR-W024 H4 delivery target, replies landing as `household.answered` with the trigger's correlation id | Chat closes the same loop ntfy already closes |
| | **Status: implemented.** `domestic.ChatSender` is the ntfy notifier's sibling — declared by a package, tier-checked, timed out and audited through the same gateway — registered once per installed chat instance that declares `send`, so a deployment running two bots has two unambiguous tools. Its two capabilities arrive as injected functions, so `keystone/domestic` still imports neither the runtime DB nor any vendor. A rule reaches a member only through a live binding on a Member Agent's Connection; an unbound member is a refusal naming how to introduce them. Because a chat reply is free text rather than a button, `chat_questions` (migration 0010) remembers the correlation id, and the member's next message within the window is claimed once and recorded as `household.answered` carrying it. Recording an answer runs no model: the loop a rule opens still closes with the Butler switched off, which is the deterministic-backbone principle applied to the one place chat could have quietly broken it. | |

**All six work packages are implemented.** Per-member read scoping on the household timeline outside groups is governed by ADR-W033's memory and disclosure rules (C-series, D7).

## Design principles this ADR holds to

- **The transport is dumb.** All authority, redaction, and audit are on
  Keystone's side of the seam; an adapter is a protocol translator that
  holds one narrow credential.
- **The audience comes from the Agent's class and the Person's role**
  (ADR-W033 B2, B7.1), never from a mode flag. Each bot has its own
  credential.
- **Identity is Wildstone's, not the vendor's.** A chat account reaches
  the Butler only through a binding a human created and can revoke.
- **Approval stays where a Wildstone credential is.** Chat can propose;
  chat cannot apply.
- **Integrating is data.** A new chat app is a package (ADR-W025), not
  a patch to the control plane.
