# ADR-W031: Butlers are named specialists, and models are assigned rather than chosen

- **Status:** accepted; amended by ADR-W033 (B1–B8, C1–C8, E8) and, for
  external runtimes, ADR-W039
- **Governed by:** ADR-W033 where they overlap.
- **Relates to:** ADR-W030 A9 (model routing — this amends it), ADR-W030
  A2/A3 (owned memory and permission-first retrieval), ADR-W030 A7/A7.1
  (tool authority), ADR-W030 A5 (integration connection scoping),
  ADR-W017 A4 (the startup probe and the QA-only verdict), ADR-W026
  (chat adapters, as amended by ADR-W033 B7.1)
- **Audience:** decision record.

**Terminology.** In this ADR an *integration connection* is an adapter to
an app or service (CalDAV, Home Assistant). A *Connection* is how a
Person reaches an Agent (Homepage, a Telegram bot), as ADR-W033 B7 uses
the word. Butler and Agent mean the same thing.

---

# Part A — Decisions (binding)

### A1. A Butler is a named specialist the household's Owners create

There is not one Butler. There are as many as the household creates: a
health Butler, a nutrition Butler, an accounting Butler, a household
Butler. Butlers are household-owned, and only Owners create and
administer them (ADR-W033 B1).

Each one carries, as its own configuration:

- a **name**, chosen when it is created
- a **directive** — what it is for
- an ordered **model/provider chain** (ADR-W033 B6)
- an **Agent class**, `owner` or `member` (ADR-W033 B2), and the
  **Connections** people reach it on (ADR-W033 B7)
- the **tools** it is granted
- the **integration connections** it may read
- its **Agent-local memory**; Personal and Household memory are separate
  scopes it may be eligible for (ADR-W033 C1, C5–C7)
- the **routines and tasks** assigned to it

The alternative is one Butler that does everything, and the problem with
that one is not that it is inelegant. It is that it must hold every tool,
reach every integration connection, and carry one enormous instruction
trying to cover cooking, finances, health and the heating. Every failure
it has is a failure with the whole house in scope. A nutrition Butler
that misbehaves cannot unlock a door, because it was never given the
tool — not because a rule caught it in time.

Specialisation is therefore a containment decision before it is an
organisational one.

### A2. A Butler's models are assigned, not chosen at run time

This amends ADR-W030 A9. That decision had a router pick a model per
call from locality, readiness and capacity. It is withdrawn.

The reason is not that routing was complicated, though it was. It is
that **routing cannot deliver the guarantee it exists to provide**.
Anything that decides which model should handle a message must first
read the message in order to decide. So the sensitive material reaches a
model before anything has routed it, and the routing step is itself the
disclosure it was meant to prevent.

An assignment has no such step. Each Butler has an ordered model/provider
chain that an Owner configured (ADR-W033 B6), and nothing inspects a
message to pick from it. Fallback runs down that chain in order and
stays within the Butler. Privacy still constrains it: a local-only task
fails rather than falling through to a cloud model. There is no
deployment-wide fallback order.

Asked which model saw a given piece of material, an assignment answers
with a model the Owner configured for that Butler. A router answers
"whichever was free at the time", which is not a property anyone can
rely on.

What this removes: locality classification at selection time, capacity
counting, and the lease table those required. The router is deleted
rather than disabled, because a selection path that exists and is not
used is a selection path someone re-enables.

What is deferred rather than forbidden: an orchestrating Butler that
delegates to others. It may turn out to be worth having. It is not
worth having *first* — it reintroduces a component that decides, and
therefore reads, before anything else runs, and a house with four
specialists does not need a manager to find them.

What survives: the startup probe and its verdict. A Butler whose model
has not passed its probe answers questions and performs no actions
(ADR-W017 A4). That was never about which model to pick; it is about
what a model that failed a check is trusted to do.

ADR-W039 amends this for external runtimes, which own their model chain
under an Owner-declared processing class.

_Changed by ADR-W033 B6: one assigned model became a per-Agent ordered chain._

### A3. A Butler narrows its class. It can never widen it

Tools have an authority class, Member or Owner, and Butlers have an
Agent class (ADR-W033 B2, B3). Member tools may be granted to either
class; Owner tools only to Owner Agents. Configuration that gives an
Owner tool to a Member Agent is invalid and rejected. Creating a Butler
selects from what its class allows. It cannot add to it, and no
directive, model or assignment changes that (ADR-W033 B5 covers
upgrade and downgrade).

This is stated because a system of user-created agents is exactly where
a ceiling would erode: it would be easy, and wrong, to let "create a
Butler" become the way to assemble a capability the class was built to
withhold.

### A4. Memory isolation follows the four scopes

Memory has four scopes: Session, Agent-local, Personal and Household
(ADR-W033 C1–C8). Session context stays in the Person + Agent + session
tables and is not implicitly durable memory.

Agent-local memory is isolated to its holding Agent. Personal memory follows
its owning Person across admitted Agents; Household memory is eligible across
Agents. Both are subject to explicit Agent restrictions. Eligibility never
widens the active Person's audience. All derivatives retain source audience
constraints, including group constraints whose membership can change.

The creating Butler stamp on Personal and Household records is provenance,
not a retrieval fence. Existing records were classified as follows: stated
private/Person records are Personal, shared/household records are Household,
and Butler-held inferences, procedures, and their derivatives are Agent-local.
Ownership, source metadata, and audience constraints remain intact.

### A5. Promotion requires the knowledge owner's consent

Personal-to-Household promotion requires the owning Person's authenticated
approval from the household dashboard. An Owner's administrative role cannot
approve another Person's memory, and a model tool cannot select Household
scope. Approval creates a provenance-linked record and audit receipt. It may
release that Person's private constraint, never another Person's constraints
or inherited group restrictions. Forgetting the source also removes the copy.

An Owner may transfer Agent-local records when retiring an Agent. Transfer
preserves original source, author, owner, and derivation links, and records
the source and destination Agents. Personal and Household memory survive
retirement and remain reachable under their own constraints. This does not
change model fallback rules or grant authority through remembered content.

### A6. A Butler that cannot reach its models waits

It does not fall back to another Butler (ADR-W033 B6). Within its own
chain it moves only to the next model the Owner configured, and only
where the material's privacy policy allows that model; when nothing in
the chain is both reachable and allowed, it reports the outage and its
work waits.

"The local one was down, so the cloud one took it" is the sentence this
design exists to make impossible for local-only material.

This is the behaviour ADR-W030 A5 already established for integration
connections — no eligible node means dependent work waits and nothing
improvises a substitute. The same reasoning applies with more force
here, because the substitute would be a model nobody chose for this
material.

_Changed by ADR-W033 B6: fallback within the Butler's own ordered chain is allowed._

### A7. Each Butler is addressed directly, and on chat that means its own bot

A Butler has its own conversation. You talk to the gym Butler in the gym
Butler's thread.

There is no dispatcher deciding which Butler should handle an incoming
message, for A2's reason: a dispatcher must read the message to route
it, so the material reaches a model before the routing that was supposed
to protect it.

On Telegram, and on any chat transport with the same shape, that means
**one bot per Butler, with its own credential**. Butlers do not share one.

Chat Connections follow ADR-W033 B7.1: one package per platform
(`chat-telegram`, `chat-matrix`, `chat-discord`, `chat-slack`),
installed once per bot as a named instance. There are no family or
admin bots; who a bot admits follows the Agent's class and the Person's
role.

The bot token *is* the address: there is no field to mistype, no topic
id to reuse, and nothing that could deliver one Butler's message to
another.

The cost is real and worth stating: a household with six Butlers
registers six bots, manages six tokens as secrets, and has six contacts
in its phone. That is the price of the isolation, and it is the right
trade for material that must not cross — but it is a reason to make
Butlers per JOB rather than per whim.

The stored address is still `(transport, external id)` rather than
"bot": the schema does not encode this decision, so a transport whose
natural unit is something else — a channel, a room — needs a row, not a
migration.

### A8. A Butler cannot create or change a Butler. An Owner can

The rule that matters is about the actor, not the ceremony.

**A Butler cannot create or modify a Butler.** At most it proposes a
Plan, which only a human Owner approves (ADR-W033 E2). Without that,
self-escalation has an obvious route: a Butler creates a cloud-model
Butler, assigns it the material it holds, and A4 is bypassed without a
single rule being broken.

**An Owner acting directly does not need a Plan to change a Butler.**
Any Owner administers every Butler (A10). Asking them to raise a
proposal and then approve it is self-approval with extra steps: the
click *is* the consent, and a ceremony that cannot be refused by anyone
teaches people that approvals are paperwork.

So a direct Console change — the backend chip being the live case —
applies immediately. What it produces instead is **an activity-log
event**: which Butler, which model before, which model after, who, and
when. That is the property a Plan was standing in for. The record is
what makes a swap answerable afterwards; the approval step was adding
nothing, because the person approving was the person asking.

The line held here is between **who acts**, not between which field
changed. Tools, integration connections and memory scope stay
Plan-mediated when a Butler proposes touching them, for the reason in
the first paragraph — and are equally direct when an Owner sets them.

The general form, because it decides more than this one control: where
the owner is acting directly, the system's job is to make the
consequence **visible**, not to withhold the control or soften what it
does. A warning that says what is about to happen respects a decision.
Blocking it, or quietly doing something narrower than what was asked,
substitutes the system's judgement for the owner's and leaves them
surprised by their own action.

### A9. A directive instructs; it does not authorise

A Butler's directive says what it is for and shapes how it talks. It
grants nothing.

Tools, integration connections and memory scope are separate, explicit
grants that an Owner makes (ADR-W033 B4), or that an Agent may only
propose as a Plan. Otherwise "you are the accounting Butler and may read
whatever you need" becomes an authority grant written in prose — a
grant no classifier reviewed, no Plan approved, and no one can audit.

### A10. Butlers belong to the household, and Owners administer them

Butlers are household-owned. Any Owner creates them and sets the
models, the directive, the tools and the integration connections, and
decides which Persons are admitted to which Butler. There is no personal
ownership: a Butler is not "Sam's", even when Sam is the only person who
talks to it (ADR-W033 B1).

A Member reaches only Member Agents they are admitted to, on Homepage
and over their chat Connections (ADR-W033 B2, B8). An Owner may reach
Owner or Member Agents; Owner Agents are not on Homepage. A Member needs
no Console account and no Keystone credential.

The admission list is the Owner's answer to "who may talk to Alfred",
and it is the thing that stops Sam reaching the accounting Butler at
all, rather than reaching it and being refused per tool.

### A11. A Butler is the role. The name is the Butler's own

"Butler" names the kind of thing: a bot with a model, a directive and a
scope. It is not a proper noun and there is no longer a singular "the
Butler".

Each one is given a name when it is created — Alfred, Robert — and that
is what a person says and what a routine is assigned to. A household
has Butlers, plural, the way it has people.

This is recorded as a decision because the alternative is ambiguity in
every sentence written afterwards: "ask the Butler" stops meaning
anything once there are four, and documentation that says "the Butler
retrieves memory" leaves the reader unable to tell whether it means one
bot or the facility.

---

### A2.1. The word "route" goes with the router (2026-09-10)

Migration 0035 removed the router and deliberately kept the rows,
noting "what a route is FOR now: the model a Butler is assigned to". It
did not rename them, and the leftover word earned its confusion:
nothing routes, so a log line reading `route=cloud-witness
model=witness-1` looks like two competing answers to "which model".

`Route` was also already taken — `resource.KindRoute` is the
reverse-proxy route, and a codebase with two unrelated Routes makes
every conversation about either of them start with which one.

Migration 0037 renames the table to `models`, `butlers.route` to
`butlers.model`, and the API paths from `/butler/routes` to
`/butler/models`. The endpoint-discovery call that held that path moves
to `/butler/models/offered`, which is the more accurate name for it
anyway: it lists what a given endpoint could give you, not what this
house has configured.

The bundle survives, because a bare model string on the Butler cannot
carry the data boundary. One Ollama endpoint serves `gemma4:e4b` off
local disk and `gemma4:31b-cloud` from Ollama's own infrastructure — so
neither the endpoint nor the provider says where the material goes. Only
`kind` does, and it is a property of the pairing rather than of either
half.

The single `butlers.model` column is the legacy shape. The rule is a
per-Agent ordered chain (ADR-W033 B6); code not yet conformant.

# Part B — What this changes in ADR-W030

| W030 | Change |
|---|---|
| **A9** model routing | **Amended.** Runtime selection by locality and capacity is withdrawn. A Butler's models are its Owner-configured ordered chain (ADR-W033 B6). The probe verdict survives unchanged. |
| **A2/A3** owned memory | **Amended.** Memory has four scopes (ADR-W033 C1–C7). Only Agent-local memory is held by a Butler; on Personal and Household records the creating Butler is provenance, not a retrieval fence. |
| **A7** tool authority | **Extended.** A Butler is the thing tool grants are stored against. |
| **A5** integration connections | **Extended.** An integration connection is scoped to the Butlers that may read it. |
| **A8.1** chat | **Extended.** A binding addresses one Butler. |
| Everything else | Not changed by this ADR. ADR-W033 governs identity, admission and lifecycle. |

---

# Part C — Implementation

Ordered so each step stands on its own.

All seven packages have landed. Each row names what proves it.

| # | Package | Landed as | Evidence |
|---|---|---|---|
| **B1** | The Butler record | `runtimedb/migrations/0032_butlers.sql`, `runtimedb/butlers.go` | `butlers_test.go`: a Butler carries its own name and model; access is downstream of admission and rechecked at use; a Butler whose model is gone waits and names the model rather than substituting another Butler; a switch reports the crossing. An Owner may upgrade or downgrade a Butler's Agent class (ADR-W033 B5); code not yet conformant. |
| **B2** | Scope: tools, integration connections, memory | `migrations/0033_butler_scope.sql`, `runtimedb/butlerscope.go` | `butlerscope_test.go`: Agent-local material held by a local-model Butler is not reachable by a cloud-model one — verified against a sabotaged filter, so it is a controlled comparison rather than a query returning nothing. Also: the scope never bypasses a person's own permission, tools only narrow, integration connections fail closed. Locality of Personal and Household memory is enforced by each record's own privacy policy (ADR-W033 C11, D7). |
| **B3** | Explicit sharing and its disclosure | `runtimedb/butlerassign.go`, `migrations/0034_butler_assignment.sql` | `butlerassign_test.go`: promotion is previewed before it happens and recorded after, naming which Butlers gain and whether any sends material out of the house. A private record cannot be promoted — that would widen an audience as a side effect of widening a scope. |
| **B4** | Assignment | `runtimedb/butlerassign.go` | `butlerassign_test.go`: an unassigned routine that needs a model is refused rather than given one; a task snapshots its Butler so a later reassignment does not rewrite what answered it. When an assigned routine's Butler cannot run, deterministic steps continue and inference steps block visibly, with no substitution (ADR-W033 E8). |
| **B5** | Per-Butler addressing | `migrations/0034_butler_assignment.sql`, `runtimedb/butlerassign.go` | `butlerassign_test.go`: a message reaches a Butler by the address it arrived on; one address resolves to one Butler; arriving there is not permission, and the admission list still decides. The address is `(transport, external id)`, so the schema does not encode the one-bot-per-Butler choice (Part D). |
| **B6** | Console roster | `api/handlers_butlers.go`, `console/src/views/Butlers.tsx`, `console/src/api.ts`, `App.tsx` | `handlers_butlers_test.go`: every roster row names its model and whether it leaves the house; a switch applies at once and returns the crossing warning; shaping a Butler needs manage; the scope endpoint answers in one call. |
| **B7** | The router removed | `migrations/0035_remove_the_router.sql`, `runtimedb/modelroutes.go` | `modelroutes_test.go` now tests what a route IS rather than how one is chosen: the kind is an immutable boundary, only a probed route carries action authority, a disabled route stays disabled. Locality is proven where it now lives — in `butlerscope_test.go` and the Part C acceptance suite. |
| **B8** | Butlers on the live conversational path | `butlerlive/butlerlive.go`, `butlerlive/serve.go`, `api/handlers_butler_named.go`, `api/handlers_chat_message.go`, `cmd/keystone/main.go` | `butlerlive/butlerlive_test.go`: a message addressed to a local-model Butler reaches that model and the cloud model receives nothing; the address decides even when the message claims to be for another Butler; an unreachable Butler waits and no other Butler's model is called; the session's authority resolver is bound to one Butler before the surface can run; a Member cannot reach an Owner Agent (ADR-W033 B2); a conversation survives a restart and the read taken before it is served from the ledger rather than dispatched again. |

### B6 in more detail

The Butler screen has a **roster** at the top of the sidebar, with the
conversations for the selected Butler below it, and a create action on
the roster.

Three things about that screen are load-bearing rather than cosmetic.

**The model is shown.** `ThreadBar` renders a `provider · model` chip in
the conversation header, in plain sight rather than behind a click.
That is A2 and A4 visible at the moment that matters — nobody is typing
into a Butler without seeing where the words go. Adding the model to
each roster row is worth doing for a different and weaker reason:
comparing Butlers side by side is how a person notices that the health
Butler is pointed somewhere it should not be. That is verification, not
protection, and it must not be mistaken for the thing the header is
already doing.

**The chip switches the model, immediately.** Per A8 any Owner
administers every Butler, so a proposal they would then approve
themselves is ceremony with no refusing party — and a change that takes
three steps instead of one teaches people that approvals are paperwork
rather than decisions.

Each switch writes an **activity-log event**: Butler, model before,
model after, who, when. That is what a Plan would have been standing in
for. A swap needs to be *answerable* afterwards, which is a record; it
does not need to be *permitted*, which is what the owner clicking it
already is.

**Switching mid-thread carries the history, and says so.** A session in
flight finishes on the provider it started with, so a swap cannot split
one answer across two models. The next turn does go to the new model
with the conversation so far, and that is correct: a person who switched
Alfred from local to cloud mid-thread decided that, and the consequence
is theirs.

What the screen owes them is the consequence stated at the moment of
the decision — a warning on a **local-to-cloud switch with an open
thread**, naming what happens: this conversation's history goes to that
model with the next message. Not a confirmation dialog standing between
them and a control they own; a sentence telling them what the click
does.

Only that direction warns. Cloud to local narrows where material can
go, and warning about it would train people to click through the
warning that matters.

**Create is a proposal, not a creation.** Creating a Butler is where a
Plan still earns its place — it is not a change to something that
already exists but the granting of a new scope, and it is the act A8
must keep a Butler out of. The button drafts one and it lands on Plans
as `proposed`, the same card and approve handler this screen already
uses. The label should say so; a button reading "Create" that produces
a pending plan teaches the wrong thing about what just happened.

The roster is grouped by **Agent class**. The difference is worth
seeing: an Owner Agent may hold Owner tools and propose Owner-class
Plans, and a Member Agent cannot (ADR-W033 B3, E2, E7).

Acceptance for the whole change is one demonstration: Agent-local
material given to a Butler on a local model is not reachable by a Butler
on a cloud model. That is proven at the store, again in the Part C
acceptance suite, and — since #148 — at the level that actually serves
a conversation. Personal and Household memory reach any admitted,
eligible Butler, so their locality is enforced by each record's privacy
policy (ADR-W033 C11, D7), not by which Butler holds them.

The serving-level proof is the one that took the guarantee from
expressible to true. `keystone/butlerlive` resolves the Butler from the
address a message arrived at, builds the client from the model that
Butler was ASSIGNED, and runs the turn through the durable runner with
an authority resolver bound to that Butler alone. Its tests assert the
strong form rather than the convenient one: not "the right model was
chosen", which a router would also satisfy after the fact, but that the
other model received nothing at all.

Two things about how that is tested are worth keeping, because both were
wrong first and passed anyway. A test that disables a route proves the
STORE's refusal, not the serving path's — the serving path's own
contribution to A6 is that it contains no substitute Butler, so that is
what is sabotaged to check the test bites. And a resumed turn must
re-ask for the read it already has; a resume where the model requests
nothing proves nothing about whether a verified result would have been
served from the ledger.

---

# Part D — Settled

- **Ownership** → A10. Butlers are household-owned and any Owner
  administers them. Offboarding a Person revokes their admission to
  Butlers; it never deletes one.
- **Child surface** → a child has no Agent and no session (ADR-W033 A8).
  The Child-surface gate in ADR-W030 A2.1 stands, and no child
  deployment is authorized.
- **Migration** → none required. The deployment is pre-release, so the
  existing household and operator profiles are updated in place or
  removed.
- **Naming** → A11. "Butler" is the role; each has its own name.
- **Telegram addressing** → one bot per Butler, per A7. Reading a
  message to decide which Butler it is for is the disclosure A2
  removed, so the bot itself is the address. A forum topic per Butler
  on one bot was rejected: it ties the design to a Telegram-specific
  feature. The schema still stores `(transport, external id)` and does
  not encode the choice, so a transport whose natural unit is a channel
  or a room needs a row rather than a migration.
