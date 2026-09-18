# ADR-W034: Commitments and meters — the household keeps track of what it owes itself

- **Status:** proposed, and built. K1 through K7 are implemented:
  `Meter` and `Commitment` resource kinds, the readings store and its
  four doors, the occurrence ledger, the daily pass, the household
  homepage's rows and answer paths, the Console screen, and the agent's
  two read tools. Every decision in Part A is made.
- **Number:** W034. Taken after W032 and W033 landed on main (#228); the
  earlier `WXXX` placeholder existed because both were then unmerged.
- **Relates to:** ADR-W024 (household events, the `Automation` resource,
  the domestic gateway — this builds directly on it), ADR-W033 A1/A4
  (Owner and Member, Console and the household plane), E2/E3 (Agents
  propose, Owners activate), E5/E6 (what a routine is, and so what a
  commitment is not), E8 (deterministic steps and inference steps),
  ADR-W032 (who may open the Console), ADR-W004 (plans are the only write
  path), ADR-W007 (one API, one event stream), ADR-W009 (policy tiers),
  ADR-W030 A4 (durable work, completion requires evidence), ADR-W011 A4
  (scheduled duty belongs to Keystone, not cron). Surface decisions are
  written against PR #238, the Wildstone-served household homepage.
- **Audience:** decision record for a core Keystone subsystem. Part A is
  the shape; Part B is the build order. Not an implementation spec — each
  work package gets its own spec when it is scheduled.

---

## The intent, in one sentence

The home lab keeps a durable list of what the household owes itself, and
works out what is due from numbers that count up — the calendar being
only one of them.

The occasion, stated plainly because it is the acceptance test: a child's
annual checkup was never booked, because the practice was supposed to
call and nobody was tracking that they hadn't. A truck went past its oil
change, because the interval is measured in miles and nothing was
counting miles. A furnace filter and a water filter went unchanged,
because nothing said when.

These are one problem. Each is a thing that comes due against a number
that goes up, and each is currently tracked in somebody's head.

---

# Part A — Decisions

### A1. Two new resource kinds: `Meter` and `Commitment`

**A `Meter` is a number that only goes up.** Miles on a vehicle, hours on
a furnace blower, gallons through a filter, days on the calendar. A meter
has a name, a unit, and a series of readings, each stamped with when it
was taken and who said so.

**A `Commitment` is something the household owes, that comes due when a
meter reaches a number.** It names one or more meters, an interval on
each, and what counts as having done it.

Both are Statefile resources, Keystone-owned, applied at no node — the
same arrangement `Automation` already has (`keystone/reconcile/
reconcile.go:459`). Creating or changing one is a plan a human approves;
after that, code runs it. This is ADR-W024 A1's rule, unchanged: the
*definition* is what gets approved, never each firing.

**Rejected:** one kind instead of two. Meters outlive commitments and are
shared by them — the truck's odometer feeds oil, tires, brakes, and
registration. Folding the meter into each commitment means four places to
record one reading, and four of them disagreeing by Tuesday.

### A2. The clock is a meter, and it is the one every household already has

"Every 5,000 miles" and "every 365 days" are the same sentence with a
different meter. Treating the calendar as a special case produces two code
paths, two state machines, and a `Commitment` schema with a `dueDate`
field *and* a `dueMileage` field that are mutually exclusive in a comment.

So there is a built-in meter, `clock`, whose readings are supplied by
Keystone's own clock and which needs no configuration, no publisher, and
no package. A household that never defines a meter can still track every
calendar obligation it has.

Consequence: a purely time-based commitment is not a lesser form. It is
the ordinary case, running on the meter that is always there.

### A3. A reading is a core write. Publishers are one door, not the door

This is the decision the rest of the design bends around, and the one an
earlier sketch of this ADR got wrong.

ADR-W024 A3 routes household facts through `POST /api/v1/household/events`,
which requires a `publisher` principal — minted per capability by the
capability engine. That is right for facts a *service* observes. It is
wrong as the only way to record a number a *person* reads off a dashboard,
because it makes an installed capability a prerequisite for tracking a
truck.

A meter reading may therefore be written four ways, all equal:

| Door | Requires an installed capability? |
|---|---|
| Console — a field on the commitment's card | no, but Console is Owner-only |
| `wild meter read f150-odometer 84210` | no, but it is an operator tool |
| Household chat — "truck's at 84,210" | **yes** — a chat capability |
| A family-facing web surface | **yes** today — see the correction below |
| A household event from a publisher | **yes — and this is the optional one** |

**Correction, recorded rather than quietly fixed.** An earlier version of
this table claimed the first three doors were free. Reviewing it against
the surfaces actually being built showed otherwise: chat is a capability,
and the family dashboard is a capability, so today a household **Member**
has no capability-free way to write a reading at all. A4's promise holds
for the Owner and fails for the family, which is the half that matters.

The fix is available and is not a change to A4 — it is what A4 requires.
`keystone/portal` is Wildstone serving small household surfaces of its
own, behind a door that produces an identity holding no RBAC verb — and
PR #238 extends it into the family front page itself. **A22 settles what
that costs**, and A19 settles where a commitment appears on it. The short
version: a family door needs the `dashboard` capability on its
`wildstone` profile, and that is a dependency this ADR accepts and names
rather than one it can design away.



The engine does not care which door a reading came through. It records
the value, the meter, when it was taken, and the principal that wrote it,
and that is the whole contract. A publisher event is a *fourth door*,
not the foundation.

**Rejected:** synthesising a built-in publisher principal for
Console/CLI writes. It would preserve one intake at the cost of a lie —
a principal named as a publisher that no capability owns, no token backs,
and no uninstall removes.

### A4. Capabilities extend the tracker; they never enable it

The layering rule, stated once so every future package answers to it:

> **Core decides what is owed and when. Capabilities only improve when
> you get asked, and how easily you answer.**

Worked through on the driving example:

- **With nothing installed.** The commitment knows the truck is due in
  about 300 miles based on the last reading. It asks, on its own
  schedule, for a fresh number, and tells you when the number it has is
  getting old.
- **With Home Assistant installed.** HA already emits `person.arrived_home`
  (`packages/home-automation`). An `Automation` the household writes
  notices that event, sees a commitment wanting a reading, and asks
  *right then* — when you are sitting in the driveway looking at the
  odometer. You reply to the notification; that is a reading.
- **With a vehicle integration installed.** The reading arrives on its
  own and nobody is asked at all.

Uninstall any of it and nothing breaks. The nudges get dumber, which is
the correct failure mode. This is the deterministic-backbone principle
ADR-W024 already holds to, applied to capabilities rather than to models.

### A5. Due-ness is a pure function, and the engine decides nothing else

**Nothing that happened after the moment being asked about is visible to
it** — not a meter reading, not a completion, not the making of an
appointment. That sentence is what makes the rest of this decision true:
a replay that can see a fact from after the point it is replaying is not
a replay, and the same bound keeps a publisher with a fast clock from
moving today's answer.

Two rounds of review were needed to get it, which is itself the lesson.
The first found the function taking the newest reading outright. The
second found that fixing *readings* had left completions and bookings
open, so a recurring commitment still advanced into a future cycle and a
one-off still reported itself done before the work. The bound belongs on
every input the ledger supplies, and stating it that way — rather than
per-field — is what closes the class instead of the instance.

Given a commitment, the readings on its meters, and the record of what
has been done, "is this due" is a pure function. No I/O, no model, no
clock beyond the reading it is handed. That makes it replayable and
dry-runnable from the first commit, for the same reason ADR-W024 A7 makes
automations replayable: it is the only way to trust a rule before
approving it.

The engine's entire output is a state per occurrence and, on transitions,
an event on the household timeline. **It does not notify anybody.**

### A6. An occurrence is the unit of state, not the commitment

The definition is permanent; each cycle is its own durable record with
its own history. "When was the oil last changed, at what mileage, by
whom" is a question about occurrences, and a commitment that carries only
a current state cannot answer it.

```
   pending ──────► due_soon ──────► overdue
                       │               │
                       ▼               ▼
                   scheduled ──────────┘        (only when booking is required)
                       │
                       ▼
              satisfied  │  waived
                       │
                       └──────► the next occurrence opens
```

| State | Means |
|---|---|
| `pending` | Known, not close yet |
| `due_soon` | Within the lead window — this is where nagging starts |
| `scheduled` | An appointment exists: a date, and who booked it |
| `overdue` | Past the threshold, not done |
| `satisfied` | Done, with evidence (A8) |
| `waived` | Deliberately skipped this cycle, recorded with who and why |

`lead` is per-commitment and per-meter: sixty days before a checkup, three
hundred miles before an oil change. A commitment may set both.

**`satisfied` and `waived` close a cycle rather than describe one**, so
for a recurring commitment they are never where things stand: the moment
one cycle closes the next has already opened. The exception is a one-off
(A9), which has no next cycle — there the closing state *is* the answer,
and "it was done on the third" is what the household should be told. The
exception is exactly as wide as the case that needs it.

### A7. `scheduled` is a real state, because booking is a separate failure

The checkup was not missed because nobody knew it was due. It was missed
because knowing it was due and *having an appointment* are different
facts, and only the first was tracked.

So a commitment declares `requiresBooking`. When true, `due_soon` does not
quiet down when somebody acknowledges it — only an actual appointment
moves it to `scheduled`, and until then it keeps asking. When false —
the furnace filter — the state is skipped entirely and the path is
`due_soon → satisfied`.

`waived` is not `satisfied`. Skipping a cycle on purpose is a recorded
decision with a person attached, and the next occurrence still opens.

### A8. Completion requires evidence, and the evidence carries the reading

A commitment is satisfied by a recorded completion: what was done, when,
by whom, and **the meter reading at the time**. That last part is not
optional — without it the next occurrence cannot be computed, and an oil
change recorded with no mileage is a note, not a maintenance record.

This is ADR-W030 A4's rule, which `keystone/runtimedb/butlertasks.go`
already enforces for durable work: the state change and the evidence are
written in one transaction, so a crash leaves work to finish rather than
a lie about finished work. It arrives again as W033 E10.

A completion may be declared the same four ways a reading may (A3).

### A9. Three rollover modes, all named

When an occurrence is satisfied, what happens next is one of three
things, and the household says which:

| Mode | Next due measured from | Example |
|---|---|---|
| `once` | nothing — the commitment is finished | the dentist on the third; a recall; a brake inspection at 90,000 miles |
| `fromCompletion` | where the meter stood at completion | oil change — drift is correct, the clock starts when the oil is new |
| `fromSchedule` | the original anchor, ignoring when it was done | annual checkup — a visit in November does not move next year's to November |

Getting the last two confused is the most common defect in every tool
that attempts this, so the field is required and has no default.

**`once` is spelled out rather than expressed by leaving the field
empty.** A missing value that silently means "one time" is how a
household writes a recurring commitment, forgets the mode, and is told
about the furnace filter exactly once — which is the failure this
decision refuses a default in order to prevent.

A one-off is not a lesser kind of commitment. It has a due point, a lead
window, optionally a booking, and the same states as anything else; the
whole of its difference is that satisfying it opens no next cycle. So
"remind me about the dentist" and "track the truck's oil" are one
feature, and a household never has to know which kind of thing it is
creating.

**A one-off may name its due point absolutely** — a date for the clock,
a number for a counter — because "the dentist on the third" is the
commonest one-off there is and expressing it as an interval from an
anchor would mean doing calendar arithmetic to say a date already known.
An absolute point belongs only to a one-off: a fixed point cannot repeat.

### A10. Several meters, earliest wins

Real maintenance is "every 6 months or 5,000 miles, whichever comes
first". A commitment names a list of due rules; the occurrence comes due
when the *first* of them is crossed, and the record says which one
tripped it.

This is not a novelty. Fleet and aircraft maintenance have tracked
calendar, hours, and cycles against one item, earliest wins, for
decades. The vocabulary is worth borrowing rather than reinventing.

### A11. "I don't know" is a state, and an estimate never triggers anything

If nobody has read the odometer in six weeks, the honest answer to "is the
truck due" is *I do not know, go and look*.

So a meter declares `staleAfter`. Past it, the commitment enters a
`reading_stale` condition: the household is asked for a number rather
than told a conclusion.

The engine may **project** for display — 12,000 miles a year puts the next
oil change around March — and a projection is labelled as one everywhere
it appears. **A projection never moves a state.** Only a real reading
does. The alternative is a system that confidently reports a truck as
fine because of arithmetic about a truck it has not heard from.

Meters also go backwards in the real world: a replaced odometer, a sold
vehicle, a new furnace. That is a declared `rebase` on the meter, with a
reason, not a reading that silently invalidates every interval computed
before it.

**An undeclared regression is a fault, not a position.** A counter only
counts up, so a reading lower than the one before it with no rebase
recorded is a mistyped digit or a meter replaced without anybody saying
so. Taking the lower number is how an overdue commitment walks quietly
back to "not due yet" — the exact failure this decision exists to
prevent, arriving through the front door. So the rule reports itself
unresolved and names the two readings, and the household is asked about
the number rather than told a conclusion drawn from it.

### A12. The engine emits; automations nag

When an occurrence changes state, the engine writes a household event —
`commitment.due_soon`, `commitment.overdue`, `commitment.reading_stale`,
`commitment.satisfied` — onto the existing timeline, correlated to the
occurrence.

Who hears about it, on which surface, at what hour, with what wording, is
an `Automation` the household writes, running through the domestic
gateway exactly as ADR-W024 H4 built it. The engine ships with no
opinion about notification.

This is the whole reason the design stays small. A deterministic due-ness
engine has one job and can be tested exhaustively. Quiet hours,
escalation to a second person, "tell my wife instead", and "ask me when I
get home" are all rules on top, written by the household, changed without
touching the engine.

### A13. The Butler drafts and phrases; it never moves a state

Unchanged from ADR-W024 A6 and arriving again as W033 E8's split between
deterministic steps and inference steps:

- The Butler may **draft** a `Commitment` or `Meter` from a sentence
  ("track the truck's oil every five thousand miles"), dry-run it, and
  propose it. A human approves the plan.
- The Butler may **phrase** a nudge, through an automation's declared
  escalation, replacing exactly one action param with the author's own
  wording as the fallback.
- The Butler may **read** commitments and meters through read-graded
  tools, to answer "what's coming up on the truck".

It cannot mark a commitment satisfied, record a reading it was not
given, waive an occurrence, or change a due threshold. Those are writes
by a person or a plan.

A household whose Butler is switched off loses the phrasing and the
drafting. It keeps the tracker.

### A14. Ownership is who it is about; satisfying is who may do it

A commitment carries an owner in the existing vocabulary
(`household`, `household/<member>`, `capability/<name>` — see
`resource.AutomationOwnerAllowed`). Kai's checkup is owned by
`household/kai`; it is booked and recorded by a parent. So "whom it is
about" and "who may act on it" are separate fields, and the second
defaults to the adults rather than to the subject.

Per-member visibility follows whatever the identity layer says at the
time — which is precisely what W032 and W033 are rewriting, so this
decision is deliberately thin and is flagged below for re-reading.

### A15. Daily is fast enough

The engine evaluates on a timer, once a day, plus immediately on any
write that could change an answer (a reading, a completion, an approved
change).

Nothing here needs minute precision: a yearly checkup with a sixty-day
lead and an oil change with a three-hundred-mile lead are both indifferent
to whether the pass ran at 07:00 or 07:04. The pattern already exists —
`nextMorning` at `keystone/cmd/keystone/main.go:1045` fires the daily
morning report — and ADR-W011 A4's rule holds: scheduled duty belongs to
Keystone, not to cron on a host.

### A16. Risk and policy

- Creating or changing a `Commitment` or `Meter`: **Medium**, for
  ADR-W024's reason — it will act on its own once approved.
- Deleting one: **Low** — it stops; nothing else changes. Occurrence
  history is retained, not purged with the definition.
- Recording a reading or a completion is **not a plan**. It is an
  ordinary authenticated write, the way emitting a household event is.
  Requiring a plan to write down a mileage would mean nobody ever writes
  down a mileage.
- A plan containing only commitment and meter steps classifies as
  `ClassHouseholdRoutine` (`keystone/plan/household.go`), so an admitted
  adult can approve "track the water filter" without Console credentials.

### A17. Backup and restore

Definitions live in the Statefile and are covered by it. Occurrences,
readings, and completions live in `runtimedb` and are covered by the
existing backup path. On restore, the engine re-derives every state from
the readings and completions rather than trusting stored states — the
function is pure (A5), so the ledger is the truth and the state column is
a cache.

Restore raises one honest problem: a backup restored a month late will
compute a truck as overdue using a month-old reading. A11 already covers
it — the reading is stale, so the household is asked for a number rather
than told a conclusion.

---

### A18. A Commitment is not a routine

ADR-W033 landed while this was drafted, and the first question it raises
is whether a commitment should be one of its routines (E5) rather than a
resource kind of its own.

It should not. W033 E6 binds an approved routine to its **tools, targets,
parameter limits, trigger/schedule, output audience, delivery policy,
assigned Agent, and per-resource grants**. A commitment has none of
those. It names no tool, reaches no service, delivers nothing, and has no
Agent: it declares that something is *owed*, and a Keystone-owned engine
works out whether it is due. Making it a routine would mean inventing all
eight fields so that a thing which executes nothing could be described as
if it did.

The nearer relative is `BackupPlan` (ADR-W011 A4): a declaration that a
duty exists, serviced by Keystone on its own schedule, rather than a
document that runs.

**The routine is the other half.** A commitment changes state; what
happens next — who is told, how, in what words, at what hour — is a
routine or an `Automation`, and that is where W033's tools, audience,
delivery policy and Agent all belong. Keeping them apart satisfies E8's
deterministic/inference line by construction rather than by care: the
commitment engine is wholly deterministic and has no Agent to be blocked
on, so the household still knows the truck is due with every model in the
house switched off.

### A19. The household homepage is the surface, and it already has the card

PR #238 serves the family-facing dashboard from Keystone itself, with a
`needsYou` card on the Home page. A commitment occurrence maps onto its
`needsYouItem` field for field:

| `needsYouItem` | Commitment occurrence |
|---|---|
| `Summary` | the engine's `Reason`, which is already household language |
| `WaitingOn` | who has to act |
| `Waited` | how long it has been `due_soon` or `overdue` |
| `Consequence` | what has not happened yet |
| `Decidable` | whether this member may satisfy it — A21's `mayComplete` |
| `Tone` | derived from the state |

So **commitments get no surface of their own.** They contribute
`needsYou` items and Activity rows to a page the household already opens,
which is the difference between a tracker people pass every day and one
they have to remember to visit.

Two things that page decided independently are worth recording, because
agreement reached twice from different directions is the strongest
evidence either is right:

- Its `homeCard` carries `Available` and `Problem` under the rule *"never
  render an unreachable service as nothing to do; silence and all-clear
  must not look the same"*. That is A11 exactly: a meter nobody has read
  is not a meter reading "not due".
- Its `Decidable` renders an item a member may not act on as read-only —
  *"the affordance is absent, never present-and-refused"*. That is the
  checkup case, where the item is about a child and the action belongs to
  a parent.

**Honestly: the shape fits and the plumbing does not exist.**
`homeNeedsYou` builds from notices and owner-side plans, and an item's
`Seq` addresses a notice. Contributing commitments means extending the
item source and the answer path, not reusing them as they stand. That is
K5's work and it is real work.

### A20. The write boundary is the connection ceiling, not a new role

PR #238 makes `household_portal` a **connection ceiling** that decorates
the authority resolver: it can only turn a yes into a no, is bound from
the turn, and is never consulted for an already-denied tool. That is the
mechanism this ADR needs, and it means no new role, verb or review page
has to be invented for commitments.

Three weights of write, and conflating any two of them breaks something:

| Action | Weight | Why |
|---|---|---|
| Create or change a definition | Plan, Owner approves (W033 E2) | a durable rule that will nag people for years |
| Record a reading, mark done, book, waive | ordinary member write, under the portal ceiling | **a reading is evidence, not authority**: it changes no rule and grants nothing |
| Propose a definition | any member → Owner Review Queue (W033 E3) | members are blocked from activating, never from starting |

Where ADR-W030 A6 said any trusted admitted adult could change a shared
routine without an owner veto, **W033 E2 supersedes it**: it is the later
and more specific decision, and this ADR follows it without reopening it.

The middle row is the one a literal reading of W033 E2 gets wrong.
Requiring a Plan to write down a mileage means nobody writes down a
mileage, and a tracker nobody answers is worse than no tracker, because
it also teaches the household to ignore its notifications.

### A21. Three fields, not one owner

`owner` alone cannot express the case this ADR exists for. Kai's checkup
is **about** Kai, **satisfied by** a parent, and Kai is not the person who
should be nagged about it. So:

- **`owner`** — who it is about. The existing vocabulary
  (`household`, `household/<member>`, `capability/<name>`).
- **`mayComplete`** — who may satisfy, book or waive it. This is what the
  homepage renders as `Decidable`.
- **`notify`** — who hears about a state change. Separate again, because
  the person who must act and the person who should be told are not
  always the same.
- **`scope`** — `household` or `personal`, mirroring W033 C1's memory
  scopes rather than inventing a parallel vocabulary. The furnace filter
  is household; a medication refill is personal and opt-in.

The earlier sketch's `mayComplete: [adults]` is withdrawn: W033 A1 makes
`child` a constrained Member profile rather than a role, so "adults" is
not a thing to name. These fields carry Person, Group and role-class
references from W033's one namespace.

### A22. What A4 costs, stated exactly

A3's correction asked where a household Member writes a reading when
nothing is installed. PR #238 answers it: the `dashboard` capability
gains a `wildstone` profile that Keystone serves itself, `exclusive: true`
with the gethomepage profile, and **only the wildstone profile declares
the `LdapGroup`** the portal's routes are gated on.

That is a good answer. `dashboard` is no longer an app launcher whose
absence costs a household some convenience; under the wildstone profile
it *is* the family front page, and depending on it is much closer to
depending on ingress — which no web surface can avoid — than to depending
on an optional extra.

**The residue, recorded rather than smoothed over:** a household running
the gethomepage profile gets 404 on the portal routes, so its Members
have no door at all and the tracker is Console-and-CLI only, which is to
say Owner-only. A4 therefore reads, precisely: the engine, the readings,
the ledger and the Owner's access need nothing installed; a **family**
door needs the `dashboard` capability on its `wildstone` profile. That is
a capability dependency this ADR accepts, named here so nobody later
discovers it as a surprise.

# Part B — Work packages (ordered)

Each lands nothing the previous one does not need, and each is testable
with fixtures and fakes. `K` for *keeping*, to avoid colliding with
W024's `H` packages and W030's `P` packages.

| # | Package | Proves |
|---|---|---|
| **K1** | `Meter` resource kind + readings store: schema, validation, plan integration, the built-in `clock` meter, `POST /api/v1/household/meters/{name}/readings` under an ordinary authenticated write, `wild meter read\|list\|history` | A number can be recorded and read back by a household with nothing installed (A2, A3) |
| **K2** | `Commitment` resource kind: schema, validation, plan integration, risk grading, the `commitment-*` policy rules, household plan classification | A commitment is a plan, approved once, policy-checked like any resource (A1, A16) |
| **K3** | The due-ness function: pure evaluation over (commitment, readings, completions) → occurrence state, multi-meter earliest-wins, both rollover modes, staleness. Unit-tested against a table of fixtures. No storage, no timer. **Built — `keystone/commitment`** | The hard logic is correct and provable in isolation (A5, A9, A10, A11) |
| **K4** | Occurrence ledger + the daily pass: `runtimedb` tables, state transitions, the timer, `commitment.*` events onto the household timeline, completion-with-evidence in one transaction | The state machine survives a restart and the timeline records why (A6, A8, A12, A15) |
| **K5** | The answering paths: commitments contribute `needsYou` items and Activity rows on the household homepage, and an answer path that records a reading, a completion, a booking or a waive. Extends the item source and the answer verb — neither exists for anything but notices today (A19) | A member answers where they already are, without a Console credential (A19, A20) |
| **K6** | Console → Commitments, Owner-side: the definition list, an occurrence's history and ledger, a dry run, projection shown as projection | An Owner can see and change what the household owes (A20) |
| **K7** | The extension seam: an example `Automation` that turns `person.arrived_home` into "what's the odometer say", shipped as documentation rather than built-in behaviour; Agent read/draft tools with a dry run | Home Assistant extends the tracker without the tracker depending on it (A4, A13) |

K1–K4 are the product. K5–K7 are what make it pleasant.

---

## Schema sketch

Illustrative, not binding — K1 and K2 settle the field names.

```yaml
# The meter. Nothing installed; a person types the number in.
kind: Meter
metadata: { name: f150-odometer, owner: household/mika }
spec:
  description: Odometer on the F-150
  unit: miles
  monotonic: true         # a lower reading is a fault or a rebase, never a value
  staleAfter: 45d         # past this, ask rather than conclude
  estimate: 12000/yr      # display only; never moves a state (A11)
```

```yaml
# The one that was missed because nobody booked it.
kind: Commitment
metadata: { name: kai-annual-checkup, owner: household/kai }
spec:
  description: Kai's annual well-child visit
  due:
    - { meter: clock, every: 365d, lead: 60d }
  requiresBooking: true         # due_soon persists until an appointment exists
  rollover: fromSchedule        # stays in September however late it is done
  satisfiedBy:
    eventType: health.visit_completed
    subject: kai
  mayComplete: [adults]
```

```yaml
# The one that was missed because the interval is in miles.
kind: Commitment
metadata: { name: f150-oil, owner: household/mika }
spec:
  description: F-150 oil and filter
  due:                          # earliest wins (A10)
    - { meter: f150-odometer, every: 5000, lead: 300 }
    - { meter: clock, every: 6mo, lead: 14d }
  requiresBooking: false
  rollover: fromCompletion      # the clock starts when the oil is new
  satisfiedBy:
    eventType: vehicle.serviced
    entity: f150
```

```yaml
# The one-off. Same states, same card, no next cycle.
kind: Commitment
metadata: { name: kai-dentist, owner: household/kai }
spec:
  description: Kai's dentist appointment
  due:
    - { meter: clock, at: 2026-10-03, lead: 7d }
  requiresBooking: true
  rollover: once                # satisfying it finishes it
  mayComplete: [household/mika, household/sarah]
```

---

## Design principles this ADR holds to

- **Core decides, capabilities extend.** Every feature here works with
  nothing installed. Installing something makes it nicer, never possible.
- **Approved once, run by code.** The definition passes a human; the
  firings do not.
- **One primitive, not two.** The calendar is a meter. There is no
  separate time-based path.
- **The ledger is the truth.** States are derived, so they can be
  rebuilt, replayed, and dry-run.
- **Silence about a number is a state, not a value.** No projection ever
  triggers anything.
- **Evidence, or it did not happen.** Completion carries who, when, and
  the reading.
- **The engine does not notify.** Due-ness and nagging are separate
  systems, and only the first is hard.

---

## Costs, and what was rejected

**Costs.** A third durable subsystem in `runtimedb` with its own backup
and restore cases. A resource kind that is meaningless until a person
writes a number down — the tracker cannot fix a household that will not
answer it, and a commitment nobody satisfies becomes a permanently
overdue row that trains people to ignore the notifications. Mitigating
that is a product problem (make answering one tap) and not solvable in
the schema.

**Rejected: running Grocy for this.** It is self-hostable, already
solves recurring chores, and has the `fromCompletion`/`fromSchedule`
distinction in its own vocabulary ("track date only" versus "track each
execution"). It has no meters, no scheduling state, and no connection to
the household timeline — so the truck stays untracked and the checkup
stays un-booked, which are the two cases that prompted this. Worth
running anyway if the household wants a chore app; it is not this.

**Rejected: maintenance entries in Homebox** (`packages/inventory-homebox`,
already packaged). A good home for *what the household owns* and a record
of what was done to it. It computes nothing about what is due next, and
it is a capability — so by A4 it may extend this, and may not be it.

**Rejected: building it in Home Assistant** with `utility_meter` helpers
and template sensors. It would work, and the state would live in a
UI-configured entity nobody can review, back up with the household, or
reason about in a plan. It also inverts A4 exactly.

**Rejected: numeric conditions on `Automation` instead of a new kind.**
Adding a `meterAtOrAbove` condition to `AutomationCondition` looks
cheaper and is not: an automation is stateless by design, and the whole
difficulty here is the durable occurrence — the thing that remembers a
checkup is due and *still has no appointment*. That state has to live
somewhere, and scattering it across rule firings is how it gets lost.
Automations remain the nagging layer (A12).

---

## What the K3 prototype settled

The due-ness function is built, in `keystone/commitment` — a leaf package
on the standard library alone, no storage, no clock of its own. 41 tests,
91.8% statement coverage, and every one of 18 deliberate mutations to the
logic is caught by at least one of them.

Building it answered seven things Part A left open. They are refinements
to the decisions above, not departures from them, and they belong in the
ADR because each one is a place where the obvious implementation is
wrong.

1. **Staleness undermines only the negative conclusion.** A11 says a
   stale reading cannot support "not due yet". It does not follow that a
   stale reading proves nothing: a reading of 84,900 against a threshold
   of 84,300 is past due whether it was taken this morning or in June.
   So a crossed threshold is definitive regardless of age, and staleness
   blocks only the conclusion that something is *not* due.

2. **Comparing a month against a mile needs a common scale.** A10 says
   "earliest wins" without saying how to compare units. The prototype
   uses progress as a fraction of each rule's own interval, so a rule 96%
   through five thousand miles and one 96.2% through six months are
   directly comparable, and the answer does not depend on the order the
   rules were written in.

3. **A missed appointment returns to overdue.** A7 gave `scheduled` as a
   state; it did not say what happens when the appointment date passes
   with nothing recorded. Staying `scheduled` would reproduce exactly the
   failure the state exists to catch, so a booking in the past with no
   completion is `overdue` again.

4. **`fromSchedule` must skip to the next future slot.** A9 says the
   schedule ignores when the work was done. Taken literally, a checkup
   done fourteen months late would compute a next occurrence already in
   the past. The schedule advances to the first slot after the
   completion — which is what "it stays in September" has to mean in a
   year it slipped.

5. **A rebase needs a prior value, and will not guess one.** A11's
   coordinate reset can only be translated if something records what the
   meter read before it was replaced. The reading before the rebase
   serves, or the rebase carries the old number explicitly; with neither,
   the rule reports itself unresolved rather than inventing a threshold.

6. **A calendar month is not `AddDate`.** Go's `time.Time.AddDate`
   normalises 31 February forward to 3 March. A filter changed on the
   31st is due at the end of February, so calendar steps clamp to the
   last day of the target month. Related: an interval of `6mo` and one of
   `180d` are genuinely different across daylight saving, and the parser
   keeps them apart — `m` is minutes, and a month is `mo`, because that
   ambiguity is the one way to write an interval that parses and is wrong
   by a factor of forty-three thousand.

7. **A completion is recorded, then the day is evaluated.** The cycle a
   completion closes is already closed by the time anything asks where
   things stand. Stated because the opposite order is the natural one to
   write and produces a commitment that reports itself overdue on the
   day it was done.

8. **A fixed schedule is measured from its anchor, never from the slot
   before it.** Stepping compounds the month-end clamp of finding 6: a
   schedule anchored on 31 January is due 28 February, and a step from
   *that* lands on 28 March, so one short month moves every occurrence
   for the rest of the commitment's life. Each slot is computed from the
   anchor, and only the month it lands in may shorten it.

9. **A coordinate reset is scoped to the mode that anchored the cycle.**
   A `fromCompletion` cycle is anchored at the position the last
   completion recorded, so only replacements after that completion have
   moved anything. A `fromSchedule` cycle is walked from the
   commitment's original anchor, so it carries every replacement since
   *then* — and the completion's own recorded position has to be
   translated into that same space before deciding which slots it is
   past. Getting either half wrong moves a due point by the size of the
   replacement.

10. **A meter outlives the commitments that read it.** One odometer
    feeds the oil, the tyres and the brakes, and each starts tracking on
    its own day, so a commitment with no declared starting position must
    anchor at the reading nearest *its* start — not at the meter's
    oldest reading ever. The naive fallback anchors a June commitment in
    January, and worse when the meter was replaced in between, because
    that pre-anchor reading sits in a coordinate space the rebase window
    deliberately excludes and the anchor and the translation then
    disagree.

11. **A due point behind where tracking began is refused, on a counter
    as on the clock.** "Due at 50" on a meter that read 100 the day the
    commitment was written is not a very overdue commitment; it is one
    nobody can ever meet. The calendar rule refused this from the start
    and the counter rule did not, which is the same asymmetry as finding
    9 in a different place.

None of this needed storage, an API, a resource kind or a model to
settle, which is the argument for having built K3 first.

Findings 8 to 11, and the amendments to A5 and A11 above, came from two
rounds of review on this PR rather than from writing it. They share a
shape worth naming: each is a case where the function produced a number
instead of saying it could not, which is the one behaviour this package
exists to refuse.

Findings 10 and 11 add a second shape, and it is the more instructive
one: a rule stated for the clock and not for counters, or fixed for one
input and not its siblings. Both rounds found one of each. A guarantee
worth having is worth stating over every input it covers, which is how
A5 is now written.

---

## What landed, and what it settled

This ADR was drafted against a `main` that did not yet have the agent
system, and deferred four decisions to it. They are now made.

**ADR-W032 and ADR-W033 landed** on main in #228, together with the
Person core (#202). That settles:

- **Whether a commitment is a W033 routine.** It is not — A18, and the
  reasoning is now against landed text rather than a branch.
- **A20 and A21**, which replace what A14 and A16 said thinly. Console is
  Owner-only (W033 A4), Plans go to an Owner Review Queue (E2, E3), and
  `child` is a Member profile rather than a role, so the earlier
  `[adults]` shorthand is withdrawn.
- **The number.** W034, taken once W032 and W033 were no longer in
  flight.

**The household homepage merged** as #238. A19, A20 and A22 were written
against it in review, so each was re-checked against the merged tree
rather than assumed to have survived: `needsYouItem` still carries
`Summary`, `WaitingOn`, `Waited`, `Consequence`, `Decidable` and `Tone`;
`homeCard` still carries `Available` and `Problem`; the connection is
`household_portal` in `keystone/portal/scope.go`; and the family front
page is `capability: dashboard, profile: wildstone, exclusive: true`,
gated by the `LdapGroup` only that profile declares.

**Both questions this ADR left open are now closed.**

- **One-off obligations** are a `Commitment` whose rollover is `once`
  (A9), with the A6 exception for how they finish. Prototyped and
  tested. The alternative — a separate reminder kind — was rejected
  because it would have split one household concept across two schemas,
  two surfaces and two review paths on the strength of a difference that
  turns out to be one field.
- **Whether a Member's proposal needs Owner approval** is settled by
  W033 E2 superseding ADR-W030 A6, per A20. The later ADR governs;
  reconciling the older text is not this ADR's work.

Nothing now stands between this ADR and K1 but the decision to start it.
