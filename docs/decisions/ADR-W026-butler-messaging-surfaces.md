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

## Conditional amendment from ADR-W030

This amendment becomes binding when ADR-W030 is accepted. Household and
Operator remain separate bot credentials, principals, endpoints, and
structurally different gateways. Handoff is explicit every time and never
executes the pending operation in the destination automatically. They present
one assistant identity and may continue one of several shared conversations
only after current identity, profile, person, and audience checks.

Trusted admitted adults may read the household-shared timeline in direct
sessions. Private sources retain narrower access, and group sessions keep the
existing conservative member/audience filter. Previously built privileged
model context is never reused across a surface change.

A future Child bot is a third surface, not a mode switch: it requires separate
credentials, role, endpoint, gateway, explicit child/guardian admission, and
boundary tests. It is disabled until those checks and transport eligibility
are complete.

Chat may approve only an exact household-action proposal in an authenticated
direct session, using a proposal ID/revision, intended approver, expiry, and
single-use token. Group chat, silence, and ordinary recent-question replies do
not approve. A Plan link may open authenticated Console review, but no chat
callback approves infrastructure or standing authority. Proactive task and
continuation notices follow quiet-hours, privacy, routing, and deduplication
policy.

Offboarding revokes every binding and active session for the member before any
cleanup Plan runs. Queued replies/proactive notices addressed to that member
expire without delivery, and no conversation context becomes visible to the
remaining household merely because its owner was removed.

A routine draft or behavioral edit may include a deep link to the authenticated
Console household-routine review view. The link carries only validated routing
parts and a Plan ID. An admitted adult authenticates there and may approve only
the server-classified household-routine Plan; no approval returns through the
chat adapter. An authenticated direct Household control may immediately disable
an exact routine/version, while activation, editing, deletion, and Plan approval
remain unavailable to chat.

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

- `chat-<transport>-family` → a new role `concierge`, holding one new
  verb `converse` and nothing else. Not read. Not emit. It can open a
  family session for a bound member and deliver the answer back; it
  cannot read the household stream itself, and a compromised family
  adapter cannot enumerate the house.
- `chat-<transport>-admin` → a new role `valet`, holding `converse` and
  nothing else either.

**Amended after review.** The admin adapter was first specified as the
existing `butler` role, on the reasoning that it relays a surface which
reads and proposes. That was wrong, and wrong in a way that only shows
on upgrade: ADR-W024 A5 makes every external agent — Hermes, Codex,
anything else — a `butler`-role principal, so putting `converse` on that
row would have handed each of them the operator's private chat queue and
the ability to post as bound accounts. A new capability must not appear
on credentials minted before it existed.

Neither adapter needs more than `converse`. A session's proposals are
submitted under an internally constructed `butler` identity acting-for
the bound person, never under the adapter's own credential — so the
admin adapter holds no propose verb of its own, and (A5) no approve.

The capability's NAME decides which role is minted, and the request must
agree with it: `chat-<transport>-<surface>` is checked at mint time, so
`-surface admin` against the family package is refused rather than
quietly placing an operator credential in the household bot's secret.

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
- A group is a shared audience: the surface may read the household's own
  facts, its devices, and the ASKER's own — never another member's.

**Amended after review.** The second rule was first written as prompt
text, which review rightly called what it was: a request, not a
boundary. It is now a capability. A group session is built on a
different gateway, and its reads are narrowed before they reach the
model.

The narrowing is by WHOSE fact a thing is, and that comes from a
declaration a package already makes — the `subject: member | device |
household | none` on each event type it publishes (ADR-W024 A10). An
event about a person is withheld unless it is the asker's; an event
about a device or about the household is not. Nothing here reads the
model's mind or the message's tone.

A first attempt narrowed instead to "the asker plus facts with no
subject at all", which is the same idea done with the wrong key: it
hid the front door along with the other member, and "is the door still
unlocked?" is exactly what a kitchen group asks. Whose fact it is, not
whether it has a subject.

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
  private — the dependency this amendment exists to remove. A rule that
  must say something privately already has the deterministic path: a
  `chat.send` to that member's own conversation (A8), which never passes
  through the group session.

With no catalog, nothing is declared, and every attributed fact except
the asker's is withheld. A deployment that has declared nothing is not
one where everyone's movements are public.

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
| | **Status: implemented.** `butler_sessions.surface` (migration 0007) is required on every insert — there is no default, because both defaults are wrong. `butler.HouseholdGateway` is a second gateway holding three injected seams (household timeline, domestic actions, deployment vocabulary) and no Statefile, node reader or plan pipeline; a reflection test pins that field set. `Session` drives either surface through a `Toolset` interface that carries the system prompt with the tools, so a family session cannot run on the operator's prompt. Both surfaces share one `finishOutcome` redact/envelope/bound pipeline. The endpoint binds the session's member to the authenticated caller and stamps `family`; `GET /api/v1/butler/sessions?surface=` separates the audit trails. Consumers: `wild butler ask\|status\|sessions -surface`, and the Console's Butler screen. | |
| **G2** | Bindings and claim codes: the table, issue/redeem/revoke on the API, `wild chat bind`, the Console pane, the `concierge` role and `converse` verb | A chat account can become a bound member, and a stranger cannot |
| | **Status: implemented.** `chat_bindings` and `chat_claim_codes` (migration 0008), the latter storing only a sha256 like every other secret. A code is three readable groups from a confusable-free alphabet, single-use, short-lived, and carries its own surface and identity — the redeem request has no field that could redirect it. Every refusal (unknown, expired, spent, wrong transport) is one indistinguishable 403. Issuing and revoking are `manage`; redeeming is the new `converse` verb, held by the new `concierge` role and by nothing else on that role. Revoking takes effect on the next message; re-binding an account revokes the old row rather than failing on the unique index. Consumers: `wild chat code\|bindings\|revoke`, and the Console's Chat pane. | |
| **G3** | The chat contract: the per-surface outbound queue with its cursor, the inbound message endpoint the adapter posts to, per-binding rate limits, the A6 red-team cases | An adapter can be written against a documented, testable contract with no vendor in it |
| | **Status: implemented.** Three endpoints, all `converse`: `POST /chat/messages` (post what you heard), `GET /chat/outbound` (poll, long-polling cursor) and `POST /chat/outbound/{seq}/ack`. The surface each acts on comes from the caller's ROLE, not the request — a concierge has no spelling for the admin surface — so a stolen family-adapter token cannot open an operator session. `chat_outbound` (migration 0009) is one queue for replies and rule pushes alike, at-least-once with a dedupe key anchored on the transport's own message id, and messages expire rather than accumulate. Unbound accounts get one guidance reply and no session row; rate limits are per chat ACCOUNT, ahead of the binding lookup. Group rules: the admin surface refuses with one line, the family surface answers with the audience declared in its prompt. | |
| **G4** | `packages/chat-telegram`: the adapter sidecar (long-poll out, no ingress), both surface instances, `chat.send` declared as a `domestic` tool, the A7 group rules | The whole loop end to end on one transport, as data |
| | **Status: implemented,** with one deviation recorded below. The adapter is `cli/cmd/chat-adapter`: a small binary behind a three-method `Transport` interface, with the Telegram driver as its first implementation — long-polling `getUpdates`, so it dials OUT and the deployment gains no port, no Route and nothing at the edge. It holds one token whose ROLE picks its surface, never sends a `surface` field, and knows one special case (a message that is exactly a claim code is redeemed, and never in a group). The two surfaces ship as two packages — `chat-telegram-family` (concierge) and `chat-telegram-admin` (butler) — because two bots with two credentials is the decision, and one package with a mode param would be the thing A3 refuses. `POST /api/v1/chat/adapters/{capability}` mints the right-roled principal and places its token on the node, reusing the publisher-mint executor; re-minting cannot move an adapter to the other surface. **Deviation:** this repository publishes no container images — the release pipeline builds binaries, and publishing an image is a distribution decision nobody has made — so `adapter_image` is a required digest-pinned param and `cli/cmd/chat-adapter/Dockerfile` is how a household builds one. (An earlier draft of this line cited ADR-W010's closed-registry posture; that is the *package* registry, and says nothing about container images. The decision stands, the citation was wrong.) The image must be pullable WITHOUT credentials: Keystone pulls before every container start, a failed pull stops the step, and the pull carries no registry auth — so a private registry does not work and neither does side-loading. Recorded as F94. | |
| **G5** | Second transport (`chat-discord` or `chat-slack`) written by the same author against G3 with no Keystone change | The contract is a contract, not a Telegram-shaped hole |
| | **Status: implemented as Matrix, not Discord or Slack.** The claim holds: adding it took one driver file (`cli/cmd/chat-adapter/matrix.go`) plus two packages, and the `keystone/` tree is untouched by the commit — no endpoint, no field, no role. **Why Matrix:** Discord and Slack both receive over a gateway websocket, and Slack's alternative is an inbound webhook wanting a public endpoint; either would trade away the property that makes the sidecar shape safe (A2: the adapter dials out, the deployment gains no inbound port). Matrix's `/sync` is a long poll over plain HTTPS — the same shape as Telegram's `getUpdates` — and a household can self-host the homeserver. Adding Discord later is that file again with a websocket library; nothing above the driver moves. The driver skips its own echoed messages, takes a cursor rather than the backlog on first sync, and treats any room that is not exactly two people as a group, erring toward A7's more careful rules. | |
| **G6** | Household integration: chat as an ADR-W024 H4 delivery target, replies landing as `household.answered` with the trigger's correlation id | The family surface closes the same loop ntfy already closes |
| | **Status: implemented.** `domestic.ChatSender` is the ntfy notifier's sibling — declared by a package, tier-checked, timed out and audited through the same gateway — registered once per installed `chat-<transport>-family` capability that declares `send`, so a deployment running two transports has two unambiguous tools. Its two capabilities arrive as injected functions, so `keystone/domestic` still imports neither the runtime DB nor any vendor. A rule reaches a member only through a live FAMILY binding; an unbound member is a refusal naming the `wild chat code` that fixes it. Because a chat reply is free text rather than a button, `chat_questions` (migration 0010) remembers the correlation id, and the member's next message within the window is claimed once and recorded as `household.answered` carrying it. Recording an answer runs no model: the loop a rule opens still closes with the Butler switched off, which is the deterministic-backbone principle applied to the one place chat could have quietly broken it. | |

**All six work packages are implemented.** What remains outside this ADR: an image pipeline (G4's recorded deviation), per-member read scoping on the household timeline (A7 decides the group case; per-member privacy is not decided anywhere yet), and a Discord or Slack driver whenever a websocket dependency is worth taking on.

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
