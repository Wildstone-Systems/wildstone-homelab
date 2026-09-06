# ADR-W026: Butler messaging surfaces — one Butler, two audiences, dumb transports

- **Status:** proposed
- **Relates to:** ADR-W001 (acting identity), ADR-W007 (one API, one
  event stream, the RBAC table), ADR-W017 (Butler productionization —
  every session is a principal-bound record, the ask endpoint, degraded
  modes), ADR-W020 (identity onboarding, claim codes, the household
  member account), ADR-W024 (household events, the `domestic` tier
  gateway, A10's three shim shapes, A6 escalation/the Phraser),
  ADR-W025 (the catalog integration layer: integrating is data),
  ADR-W009 (policy), `docs/vision.md` (the two-butler arc)
- **Audience:** decision record for the messaging layer. Part A is
  binding. Part B is the build order; each package gets its own spec
  when it is scheduled.

---

## The intent, in one sentence

A person should be able to talk to their Butler from the chat app they
already have open — the operator about the homelab, the family about
the household — without either conversation being able to do what the
other one can, and without Keystone learning a single chat vendor's
API.

## The problem

Today the Butler has exactly one front door: `POST /api/v1/butler/ask`
on the ONE API, consumed by the Console and `wild`. That is the right
door, and it is not a door anybody walks through from the sofa. Every
comparable agent (Hermes over Telegram, Discord, Slack) is reachable
where its user already is, and the Butler is not.

Adding chat naively breaks two things at once:

- **The audiences are not the same person.** The operator's Butler
  reads the Statefile, walks dependency chains, and proposes plans. The
  family's Butler asks whether the mail arrived. One bot doing both is
  one compromised phone away from being an infrastructure console.
- **Chat identity is a third party's.** A Telegram account is
  authenticated by Telegram, not by Wildstone. ADR-W017 A1 says there
  is no anonymous Butler session and no singleton admin session; a chat
  transport must not become the exception.

---

# Part A — Decisions (binding)

### A1. A *surface* is a first-class property of a session, not a deployment convention

Two surfaces exist, and the set is closed:

| Surface | Talks to | Identity it binds to | Ceiling |
|---|---|---|---|
| `admin` | the operator | a Keystone **principal** (ADR-W007 A2) | the existing tool gateway: read + `propose_change`, never approve |
| `family` | a household member | a household **member** — the LDAP account of ADR-W020, the same string that is `subject` on household events | household read/context providers and `domestic`-tier actions only |

`surface` is a stored column on the session record (the `butler_sessions`
row of ADR-W017 A1), chosen by the endpoint the session came in on and
never by the request body, the model, or the adapter. Two surfaces are
two tool sets, two system prompts, two histories, and two credentials.

A family session holds **no Keystone verbs at all**. It is not a
read-limited operator; it is a different job that happens to share the
provider seam. It cannot see the Statefile, plans, nodes, secrets,
capabilities, health, or the operational event stream — not redacted,
not summarized. The vocabulary of infrastructure is absent from it.

**Rejected:** one surface with per-caller tool filtering. The filter is
then the whole security boundary, in one code path, and every future
tool has to remember which audience it is for. Two gateways forget
nothing.

### A2. The transport lives in a package; Keystone learns no chat API

A chat integration is the sidecar shim shape ADR-W024 A10 already
defined, and ADR-W025 already made writable as data: a package with a
small adapter container that speaks one vendor's protocol on one side
and the ONE API on the other. `chat-telegram`, `chat-discord`,
`chat-slack` are packages. Keystone ships none of them and imports none
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

### A3. Two surfaces means two adapter instances, two bots, two tokens — never one bot with a mode flag

The admin Telegram bot and the family Telegram bot are separate bot
accounts, separate adapter containers, separate Keystone principals,
and separate API endpoints. Nothing in the family adapter's environment
can reach the admin endpoint, because it does not hold a credential
that authenticates there.

The package declares which surface an instance serves as a param, and
the capability engine mints the matching principal at install:

- `chat-<transport>-admin` → role `butler` (read + propose, the
  ADR-W007 RBAC row that already cannot approve).
- `chat-<transport>-family` → a new role `concierge`, holding one new
  verb `converse` and nothing else. Not read. Not emit. It can open a
  family session for a bound member and deliver the answer back; it
  cannot read the household stream itself, and a compromised family
  adapter cannot enumerate the house.

**Rejected:** one bot that switches surface on who is speaking. The
switch is a single `if` between the family and the Statefile.

### A4. Every chat message runs as a bound member, or it does not run

An adapter authenticates as itself and *names* the chat account that
spoke; Keystone resolves that account to a member or principal through
a **binding**, and refuses otherwise. The adapter never asserts who
somebody is.

A binding is a runtime-DB row: `(transport, transport_user_id, surface,
identity, created_at, created_by, last_used_at)`, unique on
`(transport, transport_user_id, surface)`.

Bindings are created by a **claim code**, the mechanism ADR-W020
already uses to get a non-technical person onto their own account: the
operator issues a short-lived, single-use code from the Console or
`wild`, the person sends it to the bot once, the binding is written and
the code is burned. Codes are per-surface; an admin code is issuable
only by a principal holding `manage`.

An unbound chat account gets exactly one reply — how to get a code —
and nothing else. No enumeration, no error detail, no session row.
Revoking a binding is one call and takes effect on the next message.

**Rejected:** a static allow-list of chat user ids in a package param.
It is a credential in a config file, it cannot be revoked without a
plan, and it makes onboarding a family member an operator task with a
YAML editor in it.

### A5. No approve verb reaches any chat transport, ever

The admin surface can read and propose. Approving a plan — the single
human gate ground rule 4 exists to protect — is not available over
chat, for the operator, for the owner, for anyone.

The reason is A-priori and not about trust in the operator: a chat
account is authenticated by a vendor whose account-recovery flow
Wildstone does not control. SIM-swap someone's phone and you should
reach a Butler that can *describe* the homelab and *draft* changes.
You should not reach one that can apply them.

So the chat flow for a change is: ask in chat → the Butler proposes →
the reply carries the plan id and its risk grade → the human approves
in the Console or `wild`, where the session is authenticated by a
Wildstone credential. This is a feature of the design, and the reply
says so rather than looking like a limitation.

### A6. Inbound chat content is untrusted input, on both surfaces

A chat message is text an outside party wrote — on the family surface,
possibly a party in a group chat who is not a household member at all.
It goes through the same treatment ADR-W017 A6 and ADR-W024 A4 already
specify: bounded before anything else (the existing `maxAskBytes`),
redacted, and wrapped in the untrusted-content envelope when it reaches
a model. The red-team suite grows the transport cases: an injected
message attempting to name the other surface's tools, a group-chat
message impersonating the operator, an envelope-forgery attempt in a
message body.

Rate limits are per binding, not per adapter: one member cannot exhaust
the household's provider budget, and a wedged adapter cannot loop.

### A7. Group chats: family only, and never a private fact

The admin surface refuses group chats outright — it answers only in a
direct message from the bound account.

The family surface may be used in a group (a household group chat is
exactly where "did anyone take the bins out" belongs), under two rules:

- Attribution is the **sending** account's binding. An unbound member
  of the group gets A4's reply, not an answer.
- A group is a shared audience: the surface renders only facts already
  shared with the household. Anything scoped to one member goes to that
  member's direct message, with a one-line pointer in the group.

### A8. Outbound is a queue the adapter polls, and a `domestic` action a rule can name

Keystone does not dial a chat vendor, and it does not hold a
long-running connection per transport. Two paths, both existing shapes:

- **Replies** to a session land on a per-surface outbound queue that
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
summaries on the admin surface. Only the conversational path answers
"no Butler provider is configured", with the same honest reason the
status endpoint already gives.

### A10. External agents are not surfaces

Hermes, Codex, or any other outside agent remains what ADR-W024 A5
made it: a Keystone principal holding at most the `butler` role, read
and propose, going through the ONE API. It does not get a surface, a
binding, or the domestic gateway. "Wildstone's Butler, reachable from
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
| **G2** | Bindings and claim codes: the table, issue/redeem/revoke on the API, `wild chat bind`, the Console pane, the `concierge` role and `converse` verb | A chat account can become a bound member, and a stranger cannot |
| **G3** | The chat contract: the per-surface outbound queue with its cursor, the inbound message endpoint the adapter posts to, per-binding rate limits, the A6 red-team cases | An adapter can be written against a documented, testable contract with no vendor in it |
| **G4** | `packages/chat-telegram`: the adapter sidecar (long-poll out, no ingress), both surface instances, `chat.send` declared as a `domestic` tool, the A7 group rules | The whole loop end to end on one transport, as data |
| **G5** | Second transport (`chat-discord` or `chat-slack`) written by the same author against G3 with no Keystone change | The contract is a contract, not a Telegram-shaped hole |
| **G6** | Household integration: chat as an ADR-W024 H4 delivery target, replies landing as `household.answered` with the trigger's correlation id | The family surface closes the same loop ntfy already closes |

## Design principles this ADR holds to

- **The transport is dumb.** All authority, redaction, and audit are on
  Keystone's side of the seam; an adapter is a protocol translator that
  holds one narrow credential.
- **Two audiences, two ceilings, two credentials.** Never one bot with
  a mode flag.
- **Identity is Wildstone's, not the vendor's.** A chat account reaches
  the Butler only through a binding a human created and can revoke.
- **Approval stays where a Wildstone credential is.** Chat can propose;
  chat cannot apply.
- **Integrating is data.** A new chat app is a package (ADR-W025), not
  a patch to the control plane.
