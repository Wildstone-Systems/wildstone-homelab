# ADR-WXXX (DRAFT): Commitments and meters — the household keeps track of what it owes itself

- **Status:** draft. Not proposed, not accepted, not built.
- **Number:** deliberately `WXXX`. W032 (*Who may open the Console*) and
  W033 (*The Wildstone agent system*) are taken on branches that have not
  landed; this file is numbered on merge, most likely W034. See
  "What lands on main will change this" at the end for the specific
  decisions that need re-reading once they do.
- **Relates to:** ADR-W024 (household events, the `Automation` resource,
  the domestic gateway — this builds directly on it), ADR-W004 (plans are
  the only write path), ADR-W007 (one API, one event stream), ADR-W009
  (policy tiers), ADR-W030 A4 (durable work, completion requires
  evidence), ADR-W011 A4 (scheduled duty belongs to Keystone, not cron)
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
| Console — a field on the commitment's card | no |
| `wild meter read f150-odometer 84210` | no |
| Household chat — "truck's at 84,210" | no |
| A household event from a publisher | **yes — and this is the optional one** |

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

### A9. Two rollover modes, and both are needed

When an occurrence is satisfied, the next one is computed one of two ways:

| Mode | Next due measured from | Example |
|---|---|---|
| `fromCompletion` | where the meter stood at completion | oil change — drift is correct, the clock starts when the oil is new |
| `fromSchedule` | the original anchor, ignoring when it was done | annual checkup — a visit in November does not move next year's to November |

Getting this wrong is the most common defect in every tool that attempts
this, so it is an explicit, required field rather than a default anybody
has to remember.

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

# Part B — Work packages (ordered)

Each lands nothing the previous one does not need, and each is testable
with fixtures and fakes. `K` for *keeping*, to avoid colliding with
W024's `H` packages and W030's `P` packages.

| # | Package | Proves |
|---|---|---|
| **K1** | `Meter` resource kind + readings store: schema, validation, plan integration, the built-in `clock` meter, `POST /api/v1/household/meters/{name}/readings` under an ordinary authenticated write, `wild meter read\|list\|history` | A number can be recorded and read back by a household with nothing installed (A2, A3) |
| **K2** | `Commitment` resource kind: schema, validation, plan integration, risk grading, the `commitment-*` policy rules, household plan classification | A commitment is a plan, approved once, policy-checked like any resource (A1, A16) |
| **K3** | The due-ness function: pure evaluation over (commitment, readings, completions) → occurrence state, multi-meter earliest-wins, both rollover modes, staleness. Unit-tested against a table of fixtures. No storage, no timer | The hard logic is correct and provable in isolation (A5, A9, A10, A11) |
| **K4** | Occurrence ledger + the daily pass: `runtimedb` tables, state transitions, the timer, `commitment.*` events onto the household timeline, completion-with-evidence in one transaction | The state machine survives a restart and the timeline records why (A6, A8, A12, A15) |
| **K5** | The answering paths: completion and reading via Console, via `wild`, and via a reply to a notification (reusing H4's reply tokens) | A person can answer from wherever they are, with nothing installed (A3) |
| **K6** | Console: the commitment list, an occurrence's history, a reading field that is one tap from the notification, projection shown as projection | The household can see what it owes and answer in one place (A11) |
| **K7** | The extension seam: an example `Automation` that turns `person.arrived_home` into "what's the odometer say", shipped as documentation rather than as built-in behaviour; Butler read/draft tools with a dry run | Home Assistant extends the tracker without the tracker depending on it (A4, A13) |

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

## What lands on main will change this

Written against `main` at `f8124b0`. Two branches are in flight and both
touch decisions above. Re-read these before proposing this ADR:

- **ADR-W032 (*Who may open the Console*)** rewrites how a household
  member is authenticated and scoped. **A14** (ownership and who may
  satisfy) and **A16**'s claim about household-classified plan approval
  both depend on it, and are written thin on purpose.
- **ADR-W033 (*The Wildstone agent system*)** restructures identity into
  Person/Owner/Member, makes an approved routine its own principal
  (E6.0), and gives any Owner an immediate stop (E6.1). Three
  consequences to work through: whether a `Commitment` should be a
  routine under E5 rather than a resource kind of its own; whether
  `mayComplete` should be expressed in W033's Member/Owner classes
  instead of an ad-hoc `[adults]`; and whether the daily pass needs the
  routine-principal treatment E6.0 describes. **A13** should survive
  intact — W033 E8 draws the same deterministic/inference line this ADR
  draws.
- The **number** becomes W034 or later, and the filename changes with it.

Nothing in Part A is expected to reverse, but A14 and A16 are the two
most likely to be reworded, and the K2 package should not be specced
until W033 lands.
