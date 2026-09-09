# ADR-W031: Butlers are user-created specialists, and a model is assigned rather than chosen

- **Status:** accepted
- **Relates to:** ADR-W030 A9 (model routing — this amends it), ADR-W030
  A2/A3 (owned memory and permission-first retrieval), ADR-W030 A7/A7.1
  (tool authority and Plan-mediated standing authority), ADR-W030 A5
  (connection scoping), ADR-W017 A4 (the startup probe and the QA-only
  verdict), ADR-W026 A1 (family and admin surfaces)
- **Audience:** decision record.

---

# Part A — Decisions (binding)

### A1. A Butler is a user-created, named specialist

There is not one Butler. There are as many as the household creates, and
the household creates them: a health Butler, a nutrition Butler, an
accounting Butler, a household Butler.

Each one carries, as its own configuration:

- a **name**, chosen by the person who made it
- a **directive** — what it is for, in that person's words
- exactly one **model**
- a **surface** it lives on (family or admin)
- the **tools** it may use
- the **connections** it may read
- its **own memory**
- the **routines and tasks** assigned to it

The alternative is one Butler that does everything, and the problem with
that one is not that it is inelegant. It is that it must hold every tool,
reach every connection, and carry one enormous instruction trying to
cover cooking, finances, health and the heating. Every failure it has is
a failure with the whole house in scope. A nutrition Butler that
misbehaves cannot unlock a door, because it was never given the tool —
not because a rule caught it in time.

Specialisation is therefore a containment decision before it is an
organisational one.

### A2. A Butler's model is assigned, not chosen at run time

This amends ADR-W030 A9. That decision had a router pick a model per
call from locality, readiness and capacity. It is withdrawn.

The reason is not that routing was complicated, though it was. It is
that **routing cannot deliver the guarantee it exists to provide**.
Anything that decides which model should handle a message must first
read the message in order to decide. So the sensitive material reaches a
model before anything has routed it, and the routing step is itself the
disclosure it was meant to prevent.

An assignment has no such step. The health Butler runs on the local
model because that is what it was created with, and nothing inspects
anything to work that out.

It is also the difference between an answer and a shrug. Asked which
model saw a given piece of material, an assignment answers by name. A
router answers "whichever was free at the time", which is not a property
anyone can rely on.

What this removes: locality classification at selection time, capacity
counting, escalation to a next-choice model, and the lease table those
required. The router is deleted rather than disabled, because a
selection path that exists and is not used is a selection path someone
re-enables.

What is deferred rather than forbidden: an orchestrating Butler that
delegates to others. It may turn out to be worth having. It is not
worth having *first* — it reintroduces a component that decides, and
therefore reads, before anything else runs, and a house with four
specialists does not need a manager to find them.

What survives: the startup probe and its verdict. A Butler whose model
has not passed its probe answers questions and performs no actions
(ADR-W017 A4). That was never about which model to pick; it is about
what a model that failed a check is trusted to do.

### A3. A Butler narrows a surface. It can never widen one

A Butler is created against a surface and inherits that surface's
structural ceiling.

A family-surface Butler cannot be given an admin tool, because the family
gateway does not contain one — there is nothing for configuration to
switch on. Creating a Butler selects from what the ceiling already
allows. It cannot add to it, and no directive, model or assignment
changes that.

This is stated because a system of user-created agents is exactly where
a ceiling would erode: it would be easy, and wrong, to let "create a
Butler" become the way to assemble a capability the surface was built to
withhold.

### A4. One Butler's material does not reach another Butler's model

This is what the design is for.

Memory recorded by a Butler is owned by that Butler. Retrieval filters
on that owner in SQL, before any content loads — the same
permission-first mechanism ADR-W030 A3 already applies to people, for
the same reason: material a reader may not see should never exist in the
process on their behalf.

There is no implicit sharing between Butlers, at any point, for any
reason. The health Butler's material does not reach the nutrition
Butler's cloud model, and the isolation is structural rather than a
filter that has to be remembered.

### A5. Sharing is explicit, and it names the models that gain access

Some things are genuinely common. "We are vegetarian" is useful to the
nutrition Butler and the household Butler both, and a design with no
shared pool at all would force the household to tell each Butler
everything separately.

So a shared pool exists. But a shared item is readable by every Butler
permitted to read it, and some of those Butlers run on cloud models.
Promoting an item to shared is therefore a disclosure decision, and it
is presented as one: the act names the Butlers that would gain access
and the models behind them.

Silent promotion to a shared pool would undo A4 quietly, which is the
worst way for a guarantee to fail — the property still looks true, and
the material has already left.

### A6. A Butler that cannot reach its model waits

It does not fall back to another Butler, or another model.

That fallback is precisely the disclosure A4 forbids: "the local one was
down, so the cloud one took it" is the exact sentence this design exists
to make impossible. A Butler whose model is unavailable reports the
outage and its work waits.

This is the behaviour ADR-W030 A5 already established for connections —
no eligible node means dependent work waits and nothing improvises a
substitute. The same reasoning applies with more force here, because the
substitute would be a different model seeing material chosen for this
one.

### A7. Each Butler is addressed directly

A Butler has its own conversation. You talk to the gym Butler in the gym
Butler's thread.

There is no dispatcher deciding which Butler should handle an incoming
message, for A2's reason: a dispatcher must read the message to route
it, so the material reaches a model before the routing that was supposed
to protect it.

### A8. A Butler cannot create or change a Butler. A person at the Console can

The rule that matters is about the actor, not the ceremony.

**A Butler cannot create or modify a Butler**, and that is
Plan-mediated like every other standing authority increase (ADR-W030
A7.1). Without it self-escalation has an obvious route: a Butler
creates a cloud-model Butler, assigns it the material it holds, and A4
is bypassed without a single rule being broken.

**A person acting at the Console does not need a Plan to change a
Butler they already own.** A10 gives the Console holder every Butler.
Asking them to raise a proposal and then approve it is self-approval
with extra steps: the click *is* the consent, and a ceremony that
cannot be refused by anyone teaches people that approvals are
paperwork.

So a direct Console change — the backend chip being the live case —
applies immediately. What it produces instead is **an activity-log
event**: which Butler, which model before, which model after, who, and
when. That is the property a Plan was standing in for. The record is
what makes a swap answerable afterwards; the approval step was adding
nothing, because the person approving was the person asking.

The line held here is between **who acts**, not between which field
changed. Tools, connections and memory scope stay Plan-mediated when a
Butler proposes touching them, for the reason in the first paragraph —
and are equally direct when the Console holder sets them on a Butler
they own.

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

Tools, connections and memory scope are separate, explicit and
Plan-mediated. Otherwise "you are the accounting Butler and may read
whatever you need" becomes an authority grant written in prose — a
grant no classifier reviewed, no Plan approved, and no one can audit.

### A10. Butlers belong to the Console; everyone else meets them in chat

There is one place Butlers are made and one place they are used, and
they are different places.

**The Console holder owns every Butler.** They create them, assign the
model, the directive, the tools and the connections, and they decide
which household members may talk to which Butler. There is no personal
ownership below that: a Butler is not "Sam's", even when Sam is the
only person who talks to it.

**Everyone else meets a Butler through a chat conversation** — Telegram
first, other services later as integrations. A household member needs
no Console account, no Keystone credential and no admin surface. They
have a conversation.

This is the split ADR-W030 A6 already drew for household routine review
— an admitted adult acts without receiving Console credentials — applied
to the Butlers themselves. It also decides what a per-Butler access list
is for: it is the Console holder's answer to "who may talk to Alfred",
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

# Part B — What this changes in ADR-W030

| W030 | Change |
|---|---|
| **A9** model routing | **Amended.** Runtime selection by locality, capacity and escalation is withdrawn. A model is a Butler's assigned property. The probe verdict survives unchanged. |
| **A2/A3** owned memory | **Extended.** A Butler is an owner, alongside a person. Retrieval filters on it the same way. |
| **A7** tool authority | **Extended.** Tool permissions are already stored per profile; a Butler becomes the thing they are stored against. |
| **A5** connections | **Extended.** A connection is scoped to the Butlers that may read it. |
| **A8.1** chat | **Extended.** A binding addresses one Butler. |
| Everything else | **Unchanged.** Durable work, traces, identity, admission, notices, offboarding and restore are unaffected. |

The code follows the same shape. `model_routes` already stores a name, a
provider, a model, an endpoint and a readiness verdict — most of a
Butler's model binding exists. Tool controls are already keyed on a
profile. Tasks already record the profile they ran under. This is
mostly a generalisation of structures that are in place, plus deletion
of the selection logic A2 withdraws.

---

# Part C — Implementation

Ordered so each step stands on its own.

All seven packages have landed. Each row names what proves it.

| # | Package | Landed as | Evidence |
|---|---|---|---|
| **B1** | The Butler record | `runtimedb/migrations/0032_butlers.sql`, `runtimedb/butlers.go` | `butlers_test.go`: a Butler carries its own name and model; the surface and profile it was created against cannot move; access is downstream of admission and rechecked at use; a Butler whose model is gone waits and names the model rather than substituting; a switch reports the crossing. |
| **B2** | Scope: tools, connections, memory | `migrations/0033_butler_scope.sql`, `runtimedb/butlerscope.go` | `butlerscope_test.go`: material held by a local-model Butler is not reachable by a cloud-model one — verified against a sabotaged filter, so it is a controlled comparison rather than a query returning nothing. Also: the scope never bypasses a person's own permission, omission falls back to the shared pool, tools only narrow, connections fail closed. |
| **B3** | Explicit sharing and its disclosure | `runtimedb/butlerassign.go`, `migrations/0034_butler_assignment.sql` | `butlerassign_test.go`: promotion is previewed before it happens and recorded after, naming which Butlers gain and whether any sends material out of the house. A private record cannot be promoted — that would widen an audience as a side effect of widening a scope. |
| **B4** | Assignment | `runtimedb/butlerassign.go` | `butlerassign_test.go`: an unassigned routine that needs a model is refused rather than given one; an assigned routine whose model is gone waits; a task snapshots its Butler so a later reassignment does not rewrite what answered it. |
| **B5** | Per-Butler addressing | `migrations/0034_butler_assignment.sql`, `runtimedb/butlerassign.go` | `butlerassign_test.go`: a message reaches a Butler by the address it arrived on; one address resolves to one Butler; arriving there is not permission, and the access list still decides. The address is `(transport, external id)` — a Telegram deployment can put each Butler on its own bot or on its own forum topic without a schema change, which is what settles Part D's open question. |
| **B6** | Console roster | `api/handlers_butlers.go`, `console/src/views/Butlers.tsx`, `console/src/api.ts`, `App.tsx` | `handlers_butlers_test.go`: every roster row names its model and whether it leaves the house; a switch applies at once and returns the crossing warning; shaping a Butler needs manage; the scope endpoint answers in one call. |
| **B7** | The router removed | `migrations/0035_remove_the_router.sql`, `runtimedb/modelroutes.go` | `modelroutes_test.go` now tests what a route IS rather than how one is chosen: the kind is an immutable boundary, only a probed route carries action authority, a disabled route stays disabled. Locality is proven where it now lives — in `butlerscope_test.go` and the Part C acceptance suite. |

### B6 in more detail

Today the Butler screen has a two-tab segmented control — Operator,
Household — and a conversation list under it. That control is a
*surface* switch and it works because there are exactly two surfaces.
It cannot become a Butler switch: eight named Butlers are not tabs.

What replaces it is a **roster** at the top of the existing sidebar,
with the conversations for the selected Butler below it, and a create
action on the roster.

Three things about that screen are load-bearing rather than cosmetic.

**The model is already shown, and stays shown.** `ThreadBar` renders a
`provider · model` chip in the conversation header today, in plain
sight rather than behind a click. That is A2 and A4 already visible at
the moment that matters — nobody is typing into a Butler without seeing
where the words go — and it needs no change beyond becoming per-Butler.
Adding the model to each roster row is worth doing for a different and
weaker reason: comparing Butlers side by side is how a person notices
that the health Butler is pointed somewhere it should not be. That is
verification, not protection, and it must not be mistaken for the thing
the header is already doing.

**The chip keeps switching the model, and it keeps doing it
immediately.** Its popover changes the backend today and that behaviour
stays. Per A8 the person at the Console owns every Butler, so a
proposal they would then approve themselves is ceremony with no
refusing party — and a change that takes three steps instead of one
teaches people that approvals are paperwork rather than decisions.

What the chip gains is an **activity-log event** on each switch:
Butler, model before, model after, who, when. That is what a Plan would
have been standing in for. A swap needs to be *answerable* afterwards,
which is a record; it does not need to be *permitted*, which is what
the owner clicking it already is.

**Switching mid-thread carries the history, and says so.** A session in
flight already finishes on the provider it started with, so a swap
cannot split one answer across two models. The next turn does go to the
new model with the conversation so far, and that is correct: a person
who switched Alfred from local to cloud mid-thread decided that, and
the consequence is theirs.

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

The surface does not disappear from the screen. A Butler is created
against one (A3), and the difference is worth seeing: an operator-surface
Butler can propose changes to the home lab and a household one cannot.
Grouping the roster by surface shows that without a word of
explanation.

Acceptance for the whole change is one demonstration: material given to
a Butler on a local model is not reachable by a Butler on a cloud model.
That is proven at the store, and it is proven again in the Part C
acceptance suite. It is NOT yet proven at the level that serves a live
conversation, because that path is still the one ADR-W030's wiring gap
describes — see #148. B1 to B7 make the guarantee expressible and
enforced; installing it on the live path is the work that issue tracks.

---

# Part D — Settled, and the one thing left

Four questions were open in the first draft of this record. They are
answered above and noted here so a later reader can see they were asked
rather than skipped.

- **Ownership** → A10. The Console holder owns every Butler and grants
  access to them. No personal ownership. Offboarding a member therefore
  removes their access to Butlers; it never deletes one.
- **Child surface** → unchanged. ADR-W030's guardian review and the
  child profile's structural ceiling apply to a Butler on that surface
  exactly as they applied before.
- **Migration** → none required. The deployment is pre-release, so the
  existing household and operator profiles are updated in place or
  removed. Whichever survives becomes the instance's first Butler.
  Writing a migration for state nobody depends on would be work spent
  preserving a shape this record replaces.
- **Naming** → A11. "Butler" is the role; each has its own name.

### Still open: how Telegram addresses more than one Butler

A7 says each Butler is addressed directly, and A10 says household
members reach them over Telegram. Telegram then has to make "which
Butler is this for" answerable **without reading the message**, because
reading to decide is the disclosure A2 removed.

Two shapes do that, and the choice is not obvious:

- **One Telegram bot per Butler.** Cleanest — the bot token *is* the
  address, and isolation is total. Costs a bot registration and a token
  per Butler, and a household with six Butlers has six contacts.
- **One bot, one forum topic per Butler.** The topic ID identifies the
  Butler structurally, so it still involves no reading. One contact,
  one token. Ties the design to a Telegram-specific feature, which sits
  awkwardly with the intent to add other chat services later.

Both satisfy A7, and B5 settled it by not deciding: an address is
`(transport, external id)`, so a bot id and a topic id are the same
shape and a deployment picks. The choice stayed a deployment's rather
than becoming a schema's.
