# ADR-W022: Household events and automations — the family's butler, deterministic first

- **Status:** proposed
- **Relates to:** ADR-W001 (concierge, acting identity), ADR-W004 (plans
  are the only write path), ADR-W005 A1 (`agentTools` parsed, not run),
  ADR-W006/W017 (the Butler's ceiling), ADR-W007 (one API, one event
  stream), ADR-W009 (policy, the `domestic` tier), implementation scope
  §7 (concierge epics C1–C5), `docs/vision.md` (the two-butler arc)
- **Source:** an external design synthesis ("Wildstone Home Lab Agent
  Architecture Summary", 2026-09-05), reconciled here against as-built.
  Where that document and this ADR disagree, this ADR wins.
- **Audience:** decision record for the household layer. Part A binding;
  Part B is the build order. Not an implementation spec yet — each work
  package in Part B gets its own spec when it is scheduled.

---

## The intent, in one sentence

The home lab runs the household on plain rules, and the AI is called in
only to talk to people and to help write those rules.

Four ideas sit under that sentence:

- **Facts flow in.** Apps report what happened — "Mika left home",
  "breakfast logged" — and Wildstone keeps them as a timeline.
- **Rules decide, cheaply.** Code watches the timeline and checks
  conditions. Most of the time the answer is "nothing to do", and no
  model is involved.
- **The AI handles the human parts.** When a rule needs to ask a
  question, phrase something, or resolve ambiguity, it hands the Butler
  a small packet of relevant facts.
- **Actions are narrow and permitted.** Anything that changes the house
  or an app goes through a typed, permissioned action. Never free-form
  model control.

This is the "family's butler" the vision doc describes, built on the
operator's butler that exists. Nothing here loosens the operator
butler's ceiling; everything here reuses its machinery.

# Part A — Decisions (binding)

### A1. A rule is approved once, then runs on its own

Ground rule 4 says every intentional change is a plan a human approves.
A household rule cannot go to the approval queue every time it fires —
"ask whether I got the mail" would be approved a hundred times a year.

So the **rule itself** is what gets approved. An `Automation` is a new
Statefile resource kind: trigger, conditions, the context it may read,
the action it may take, and an optional escalation to the Butler.
Creating or changing one is a plan, proposed by a human or by the
Butler, approved by a human. Once approved, the automation engine runs
it without asking again.

This is what "agent-authored does not mean agent-executed" means in
Wildstone terms: the Butler may draft a rule; a human signs it; code
runs it. Every plan-path guarantee (policy validation, audit, the
Butler's no-approve ceiling) applies to rules for free.

**Rejected:** a separate rule store outside the Statefile. It would be
a second write path with its own approval story, which is the thing
ground rule 4 exists to prevent.

### A2. Household events are a second stream, not the operational one

The runtime DB's `events` table (ADR-W007 A4) is shaped for
infrastructure: node, kind, name, message. It stays as it is.

Household facts get their own table and their own envelope:

| Field | Meaning |
|---|---|
| `event_id` | Publisher-supplied unique id; the intake enforces uniqueness, so a retried delivery is a no-op |
| `occurred_at` | When it happened, per the publisher |
| `recorded_at` | When Keystone stored it (stamped by Keystone, never supplied) |
| `subject` | The household member, device, or household the fact is about |
| `source` | The publishing principal |
| `type` | Canonical, namespaced: `person.left_home`, `pantry.meal_logged` |
| `entity` | Optional thing affected: a calendar entry, a device, a list item |
| `payload` | Small structured detail. Bounded in size |
| `correlation_id` | Groups events from one workflow, so an automation's outcome can be traced back to what fired it |
| `schema_version` | Lets the contract evolve |

Same access pattern as the operational stream: append-only, a `seq`
cursor, a long-poll `GET`. Application databases stay authoritative for
application state; the timeline records that something changed, not the
whole thing.

**Rejected:** a message broker (NATS, MQTT, Redis). One household does
not need one, and a broker is a second place events live. SQLite is the
store and the bus; in-process pokes, as the reconciler already uses,
are the subscription. Home Assistant may still speak MQTT — an adapter
translates it at the boundary.

### A3. Publishers are principals with one verb

Anything that emits household events holds a Keystone token with a new
`publisher` role and a single `emit` verb. It can post events and
nothing else: not read state, not propose, not approve. A Home Assistant
adapter that is compromised can lie about the house; it cannot touch the
Statefile.

The intake is `POST /api/v1/household/events` on the ONE API (ADR-W007).
First-party apps post to it directly. Third-party apps get a thin
adapter, shipped as a package (data, ADR-W005), that turns whatever the
app exposes into the canonical envelope. Preference order for what an
adapter listens to: native webhooks, then API polling isolated inside
the adapter, then database observation. Log parsing is not a contract
and is not accepted.

### A4. Household event content is untrusted input

An event payload is text an outside system wrote. When any of it reaches
the model — in an escalation packet, in a Butler read tool — it passes
through the same centralized redaction and untrusted-content envelope
that logs and health details already do (ADR-W006 A5, ADR-W017 A6). The
red-team suite grows a case for it: an event payload carrying "ignore
previous instructions" produces at most a proposal, never an action.

### A5. Actions run through the `domestic` tier gateway

ADR-W009 A4 defined the `domestic` tier and left it unused until the
concierge epics. This ADR is where it is used. An automation's action is
a typed operation — send a notification, call a first-party app's API,
set a Home Assistant entity — declared by a package in its `agentTools`
block (ADR-W005 A1) with an explicit risk grade, input schema, and
timeout. The gateway checks that the automation was approved with that
action, that the acting member is allowed it, and audits the call.

Two things do not run through it:

- **Infrastructure changes.** Anything that would touch the Statefile
  stays a plan. A household rule cannot restart a container.
- **External agents.** Hermes, Codex, or any other agent is a principal
  with at most the `butler` role: read and propose. Only the Wildstone
  Butler sits behind the domestic gateway, because only its sessions are
  principal-bound, redacted, and bounded (ADR-W017 A1).

### A6. Context providers are read tools, and the model is the last step

"Retrieve context, do not dump history" is already how the Butler
works: it has read tools, not a memory blob. A household context
provider is the same thing — a read-graded `agentTool` from a package
("last three breakfasts", "next two calendar events") — and an
automation names which ones it may call.

The order of evaluation is fixed and cheap-first:

1. Trigger matches an event type. Otherwise stop.
2. Conditions evaluate against the timeline and named providers, in
   code. Most rules stop here, and an `automation.suppressed` event
   records why.
3. Only if the rule declares an escalation does a Butler session open,
   with the trigger event and the providers' results as its packet.
4. The Butler's output is routed to the declared action, through A5.
5. The action's result is a new household event, correlated to the
   trigger. The loop closes on the timeline, where it can be replayed.

Vector or graph indexes over household data are a derived optimization
for later, never a source of truth. Not in this ADR.

### A7. Every automation is replayable without side effects

The engine evaluates a rule as a pure function over (event, context) and
returns a decision. A dry-run mode feeds historical events through a
rule and reports what would have fired, with actions stubbed. This
exists from the first commit, because it is the only way to test a rule
before approving it, and it is how the Butler's own drafts get checked
before they are proposed.

### A8. Identity mapping is declared best-effort

The `subject` on an event must map to a Wildstone member for per-member
rules and per-member audit to mean anything. Scope §7 C2 already expects
this to be negotiated per app, not solved uniformly. An adapter package
declares how it maps its app's notion of a person (a Home Assistant
`person` entity, a PantryOS user) to a member, and declares when it
cannot. An unmapped subject is a valid event; it just cannot fire a
per-member rule.

### A9. When this is built

Scope §7 says the concierge product waits for M5, the dogfood cutover,
and not to spend more than the foundation before then. This ADR is
design, and the envelope in A2 and the resource kind in A1 are cheap to
land early because they change no existing behaviour. The engine, the
gateway, and the adapters are the product, and building them before
alpha is a deliberate pull-forward the owner decides on and records here
— not a drift.

# Part B — Work packages (ordered)

Each package is small, tested with fixtures and fakes like everything
else in the repo, and lands nothing the previous package does not need.
No real Home Assistant, PantryOS, or LLM in CI.

| # | Package | Proves |
|---|---|---|
| **H1** | Household event intake: the A2 table and envelope, validation, uniqueness on `event_id`, the `publisher` role and `emit` verb, `POST`/`GET` on the API | Facts can flow in from a narrowly-trusted source and be read back in order |
| **H2** | `Automation` resource kind: schema, validation, plan integration, one policy rule (a `butler` principal may not author a rule whose action outranks `domestic`) | A rule is a plan, approved once, policy-checked like any resource |
| **H3** | Automation engine: trigger matching, condition evaluation, `fired`/`suppressed` events, dry-run and replay (`wild automation replay`). No escalation, no actions yet | The deterministic half works end to end and can be tested against history |
| **H4** | First action, through the domestic gateway: notify via the existing ntfy package. ntfy's action buttons post the reply back to H1's intake | A rule can ask a human a question and hear the answer, with no chat surface built |
| **H5** | First adapter: Home Assistant webhook → intake, as a package, with the A8 person mapping | A third-party app plugs in through the one contract |
| **H6** | Escalation: a rule opens a Butler session with its context packet; the Butler's answer routes to H4's action. Red-team case per A4 | The "left home without breakfast" flow, end to end |
| **H7** | Natural-language authoring: the Butler compiles a request into the H2 schema, dry-runs it against H3, and proposes it through `propose_change` | The AI writes rules the engine already understands; no second execution path |

The first end-to-end target is the breakfast check: Home Assistant
emits `person.left_home`; a rule checks the timeline for a
`pantry.meal_logged` that morning; if none, the Butler asks one
informed question over ntfy; the answer is written back through a typed
action and lands on the timeline as a new fact. It exercises every
layer above without touching a container.

## Design principles this ADR holds to

- **Deterministic backbone, probabilistic edge.** Rules keep working
  with the Butler off, exactly as the morning report does today.
- **Events are facts, not prompts.** Normalized before any model sees
  them, and untrusted when it does.
- **Approved once, run by code.** The plan pipeline gates the rule, not
  each firing.
- **Typed actions over arbitrary access.** The domestic gateway is
  narrow, audited, and the only executor for household actions.
- **Replay before trust.** No rule fires for real until it has been run
  against history.
- **Provider independence.** Nothing in this ADR depends on which model
  is configured, or on one being configured at all.
