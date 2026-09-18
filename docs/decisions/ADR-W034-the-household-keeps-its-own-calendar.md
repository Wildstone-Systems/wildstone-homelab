# ADR-W034: The household keeps its own calendar

- **Status:** accepted for phases 1, 2 and 4; phases 3 and 5 are a plan,
  not a commitment.
- **Relates to:** ADR-W005 (package format), ADR-W013 A4 (backups),
  ADR-W019 A10 (a Route is not always served at the host it declares),
  ADR-W024 (household events and automations), ADR-W029 (a package is
  checked against the app it configures), ADR-W030 A5/A5.1 (scoped
  integrations and the adapter contract), ADR-W031 (Butlers).
- **Audience:** decision record for the `calendar` capability, and the
  brief for whoever builds the phases that are not here yet.

---

# Part A — Decisions (binding)

### A1. `calendar` is one capability, and Radicale is an implementation detail of it

A household installs **Calendar**. It does not install a CalDAV server
and then wire an API to it.

Radicale is the store and the protocol; everything Wildstone offers on
top of it — the agent's reads, the agent's writes, the events it will
publish — is reached through the integration adapter contract, never by
reaching into Radicale. Nothing outside `packages/calendar-radicale` and
`keystone/adapter/caldav` knows the name Radicale, and that is the
property this decision exists to hold. Replacing the server later is
then a new profile of one capability rather than a migration every
consumer participates in.

The boundary in one line:

```
                Calendar capability
                        │
          keystone/adapter (the contract)
                        │
        ┌───────────────┼───────────────┐
        ▼               ▼               ▼
  caldav (Radicale)   caldav        a future
  on this machine    elsewhere      provider
```

`keystone/adapter/caldav` is already both of the first two: it speaks
CalDAV to a collection URL, and whether that URL is the household's own
Radicale or a mailbox provider's server is configuration rather than a
different adapter.

It is reached by two roads, and the choice between them is about where
the application is, not about what an agent may do:

```
agent ─► domestic gateway ─► caldav ─────────────► Radicale, here
              (in Keystone)                        (no extra process)

agent ─► connection + work queue ─► adapter ─────► a calendar elsewhere
              (ADR-W030 A5)         (a process)    (Google, Fastmail)
```

For a service on Wildstone's own Docker network the first is the whole
answer: one process, one credential, no queue to run dry. The second
exists for applications Keystone cannot reach directly, and it is where
C3's providers will land.

### A2. It exists as a separate thing from `mail`, because it depends on nothing

`mail-roundcube` already serves calendars, through Davis, at
`calendar.<domain>`. It is not the same offer. Davis authenticates every
request against the household's IMAP provider (`AUTH_METHOD=IMAP`), so
it works only for a household that has mail somewhere else, and it stops
the day that provider does.

`calendar-radicale` authenticates against the household's own directory
and stores collections on the household's own disk. That is what makes
it the *durable local* calendar the goal asks for, and it is why a
household with no external mail at all can still have one.

Both may be installed. They are separate capabilities on separate
addresses, and the new one defaults to `cal.<domain>` rather than
`calendar.<domain>` precisely so that installing it does not take the
other one's name — or, under the tailnet posture, its pinned port
(ADR-W019 A10).

### A3. The agent never gets the calendar's credentials, its filesystem, or its database

An agent names a household action — `calendar.get_agenda`,
`calendar.create_event` — and Keystone does the rest. It sends and
receives plain values (a title, a date, a time), never iCalendar, and it
holds no CalDAV password.

This is the same rule ADR-W030 already makes, restated here because a
calendar is the most tempting place to break it: the store is a
directory of text files, and handing an agent a path would be one line.

It holds on both roads. On the in-process road the credential is in
Keystone's environment and the model sees an action name; on the
work-queue road it is in an adapter's environment and the model sees the
same action name. Which road a deployment uses is not a fact the agent
can observe, which is what makes A1's boundary worth having.

### A4. Radicale is the source of truth. There is no second copy of the household's events

The Calendar capability keeps operational state only — what a connection
is, what it was verified to do, what work was queued and answered, and
the audit of it. Not a table of events.

An events table would be a second thing that can be right, and the
household edits the first one from a phone whenever it likes. The cost
of not having one is that an agenda question is a live read, which is
the correct cost: "your calendar is unreachable" and "your calendar is
empty" are different answers and the adapter contract already keeps them
apart.

### A5. Writing is a person's decision, on whichever road it arrives by

There are two roads to this calendar and they have different gates,
because they are different people deciding.

**The work-queue road** (an adapter process, for a calendar somewhere
else) is gated twice: Keystone refuses action work for a connection
nobody has widened — `w.Kind == WorkAction && !conn.ActionsEnabled` — so
an adapter is never handed one until a person decides; and the adapter
itself must be run writable. The second is a deliberate second copy of
the first, for the same reason `keystone/domestic` keeps its own copy of
the tier rule: a call that arrived some other way still cannot write.

**The in-process road** (Keystone's own action gateway, for the calendar
this household runs) is gated by the deployment: the six tools are
registered only when Keystone is started with `-calendar-url`,
`-calendar-user` and `CALENDAR_PASSWORD`. That is the same gate
`-ntfy-url` is, and it is the operator saying "the household's agents may
use this calendar."

What is NOT a gate on either road is a per-call confirmation, because
there is no such mechanism. See A10.

RSVPing to an invitation stays out. It is a different act with a
different blast radius — it sends mail, to people, as the household —
and remains on ADR-W030's follow-up list.

### A6. iCalendar is generated in Go, never by a model

An agent resolves "dinner with Giulia next Thursday at half six for two
hours" into a start and an end. It stops there.

Folding at 75 octets, CRLF line endings, escaping a comma in a location,
and the difference between an all-day `DATE` and a midnight `DATETIME`
are not judgement calls. A model asked to make them is right most of the
time, which is the worst available outcome for a household calendar:
the failures are rare, silent, and land on somebody's phone.

### A7. A change edits the server's copy; it does not replace it

`keystone/adapter/caldav` understands four properties. An event on a real
calendar routinely carries a dozen more — attendees, recurrence, the
organiser, an alarm. So an update fetches the stored object and rewrites
only the lines it was asked to change, inside the `VEVENT` component and
nowhere else.

Regenerating the object from what we parsed would drop the rest. Dropping
the alarm on somebody's medication reminder is not an acceptable cost of
correcting a typo in its title — and a `VTIMEZONE` has a `DTSTART` of its
own, so even the component scoping is load-bearing.

### A8. A UID is not an address

A CalDAV server names the resource it stores. Only the events this
adapter created are at the `<uid>.ics` a write would guess; one added on
a phone is wherever that phone put it. Reads therefore carry the `href`
the server reported, and changing or deleting an event the household made
starts by reading it.

### A9. The agent acts on the household calendar, not on anybody's own

Everybody's own calendars are their own: `rights` is anchored on the name
they signed in with, and nothing — including the service account — reads
another person's. Alongside them is one **household** calendar, under its
own principal, readable and writable by everyone in `calendar_users`, and
that is what the six tools act on.

It is deliberately not "the agent acts on your calendar", which would
mean a standing grant over every member's private appointments for the
convenience of a create. That grant is a decision for the permissions
phase (C2), not a default.

Two costs, both real, both preferred to that grant:

- **The household calendar does not appear on an iPhone by itself.** A
  phone discovers the calendars under the principal it signed in as, and
  this one is under none of them. It is added once by address, as a
  subscribed calendar; DAVx5 and desktop clients take it read-write. This
  is how CalDAV principals work, not something the package can fix.
- **An agent cannot answer "what's on MY calendar tomorrow"** — only
  what is on the household's. Until C2, that is the honest boundary.

### A10. Delete asks first, on a tier that now exists

Deleting an event wants "do it, but ask first", and when this capability
was first written there was no such tier. There is now: **ADR-W024 A5.1**
adds `confirm` and the attendance axis it needs, because the shape of the
want is general — it is every destructive household action — and a tier
only the calendar understood would have been the same mistake in a
smaller box.

So `calendar.delete_event` is declared at `confirm`, and two things
follow. A rule firing at 3am cannot carry it at all, because there would
be nobody to ask. A person in chat gets a question naming the actual
event — "Delete 'Dentist for Sam' from Fri 18 Sep?", via the executor's
`Describe` — which they answer on the Home page, and their yes
authorises that call and nothing else.

Three things the executor does regardless, because a confirmation is not
a substitute for an operation that behaves:

- it removes at most one event per call;
- it refuses, naming the candidates, when the title and date match none
  or several — and it does so BEFORE anybody is asked, so an ambiguous
  request costs one message rather than a question, an answer and then a
  refusal;
- every dispatch lands on the household stream, audited, like all the
  others.

### A11. The calendar works with no agent at all

A household that configures no Butler, no connection and no adapter still
has a calendar: Radicale, on the tailnet, in everyone's phone. Every
agent-facing phase below is additive to that, and none of them is allowed
to become a dependency of it.

The corollary is in the failure behaviour: if Radicale is unreachable the
adapter reports `unavailable` and dependent work waits. Nothing anywhere
reconstructs an agenda from memory.

---

# Part B — What shipped

### B1. Phase 1 — the capability

`packages/calendar-radicale`: Radicale on a persistent volume, behind the
private ingress, signed in against the household directory
(`auth: app_ldap` — the media-jellyfin posture, and the only one a phone's
calendar app can use, since it sends HTTP Basic per request and cannot
follow a login redirect), with `calendar_users` as the grant and a daily
backup of the collections.

Three findings came out of checking the package against the app
(ADR-W029), and each of them would have shipped a converged install that
did not work:

1. **Radicale reads no environment variables.** Its configuration is one
   ini file and nothing else, so a setting passed as `env:` would be
   inert — the `photos-immich` failure exactly.
2. **The published image is not the image its Dockerfile describes.** The
   Dockerfile defaults to `DEPENDENCIES=bcrypt`, under which
   `[auth] type = ldap` dies on `ImportError`. The release workflow
   passes `bcrypt,argon2,ldap`. Reading the Dockerfile and stopping there
   would have rejected the whole LDAP design on evidence that was wrong.
3. **The reader password cannot live in the config file.** A managed file
   carrying a secret reference is narrowed to `0600` root-owned when the
   node materialises it, and this image runs as uid 1000. Authelia is the
   one package that escapes that, by pinning `PUID=0`, and Radicale has
   no such knob. So the credential is placed by a Job that runs as root
   and hands the file to the user that has to read it.

The third is a **gap, not a workaround to be proud of**: Wildstone has no
way to say what user a managed file belongs to. The Job is the shape that
gap forces. Closing it — an owner on `ConfigFile`, or a container-user
field — would delete this Job and is worth doing before the next
non-root app needs a secret in a file.

The install steps this leaves are **one account and its password**, used
by both halves: Radicale searches the directory as it, and Keystone
writes the household calendar as it. A second account would be a second
thing to create, rotate and forget, and it would gain nothing —
`owner_only` still applies to every person's own calendars and this
account is in none of them.

That one manual step cannot be removed today, and the reason is worth
naming: `kind: LdapUser` declares that a person must exist and
deliberately carries no credential, because a password cannot live in a
spec (ADR-W020 A4). Nothing lets a package create a service account with
a password it knows. It is the same gap
`wild household publisher home-automation` works around, and it is worth
closing once rather than per package.

### B2. Phase 2 — reads, and writes, in the CalDAV layer

Reading was already built: `keystone/adapter/caldav` has answered
`caldav.today` and `caldav.events` since ADR-W030 P12.

Writing is new: `caldav.create_event`, `caldav.update_event` and
`caldav.delete_event`, under A5's gates, with A6's generation, A7's
editing and A8's addressing. `EventWrite`, `Events`, `CreateEvent`,
`UpdateEvent` and `DeleteEvent` are exported so the two roads share one
implementation: the work-queue road arrives with JSON, the in-process
road with a struct, and neither gets its own copy of the protocol. Creates are conditional on `If-None-Match: *`
so a colliding UID cannot silently replace an event; updates are
conditional on the etag they read, so two people editing one event is a
refusal a caller can act on rather than a lost update.

Fixing the round trip turned up a **pre-existing bug in the read path**:
`unfold` stripped every leading space from a continuation line, where RFC
5545 adds exactly one. "Dentist for␍␊␠␠Sam" was read as "Dentist forSam".
It had been invisible because a fold only loses a character when it lands
next to a space; the write path's own round trip made it impossible to
miss. The contract test had been written to accept either answer, and now
accepts one.

### B3. Phase 4 — the six tools, on

`packages/calendar-radicale` declares six `agentTools` and
`keystone/domestic` registers an executor for each, so a Butler can use
the calendar the day the capability is installed:

| tool | tier | what it does |
| --- | --- | --- |
| `calendar.get_agenda` | read | what is on, for a day or a window |
| `calendar.search_events` | read | find by title |
| `calendar.check_availability` | read | is that window free, and what is in it |
| `calendar.create_event` | domestic | add one |
| `calendar.update_event` | domestic | change one |
| `calendar.delete_event` | domestic | remove one |

No new agent surface was needed. `butler.list_household_actions` and
`butler.do_household_action` already route to the domestic gateway, which
dispatches any tool a package declares *and* a deployment has wired — so
these six are discoverable and callable through the path
`notifications.notify` already uses, and household automation rules can
name them too.

Three things about the executors are worth recording because they are not
obvious from the tool list:

- **The summary is the whole answer.** `do_household_action` hands the
  model `Result.Summary` and drops `Result.Detail`, so a read's summary
  is written to be read aloud — "Tomorrow: 3 things. 08:00 school run…" —
  rather than as a log line with the answer in a field nothing downstream
  can see.
- **An event is named by what it is, not by an id.** Changing and
  deleting take a title and a date. A CalDAV UID is not something a
  person says out loud, and it would have to survive a round trip through
  a chat message to be useful. Ambiguity refusal falls out of it: "2
  events on Fri 18 Sep match 'dinner': …. Say which one" is a refusal
  that names the candidates, where an id would silently have picked one.
- **A time with no offset is the household's clock.** A model asked for a
  time produces `2026-09-25T18:30` far more often than an RFC3339 string
  with an offset, and reading that as UTC puts a six-thirty dinner on
  every phone at the wrong hour. It is the single most consequential line
  in the package and it has a test that fails on the mutation.

`TestCalendarToolsAreDeclaredByThePackage` pins the package's declaration
against `domestic.CalendarTools`, names and tiers both. A tool in one and
not the other fails at somebody's dispatch, where a Butler has already
told the household it can do the thing.

### B4. What walking the paths found

Five defects, all of the same family: everything reconciled, every test
passed, and the thing did not work. Recorded because the shape repeats.

1. **The shared calendar did not exist.** Radicale does not create a
   collection on demand — `app/put.py` returns 409 Conflict when a PUT's
   parent is not already a collection — and nothing created it. The
   household's first "add dinner on Friday" would have failed, as a
   protocol error that reads like the server being down. The credential
   Job now creates it, with the `VCALENDAR` tag and the right ownership,
   only when absent.
2. **The confirmation question was discarded.** `NoticeRequest.Body` had
   been accepted and dropped since migration 0030, because there was no
   column — invisible while every question's KIND said everything there
   was to say. A confirm-tier action therefore rendered as "Household
   confirm" with a yes and a no beside it. Consent to an unnamed action
   is not consent, so this was a correctness fix, not a presentation one.
3. **There was no way to reach the profile.** The API returned
   `addToPhone` and the page never drew it. The one path to the feature
   existed only for somebody who knew the URL.
4. **The card claimed "Nothing in the next three days"** whenever it was
   available, having looked at nothing. It now reads the shared calendar,
   and an unreadable calendar is `Available: false` with a reason —
   because "nothing on" and "could not look" are different answers and a
   wall tablet must not render the second as the first.
5. **A household running both calendars could get the wrong address.**
   `calendarServer` returned the first matching Route, so a profile built
   for the Wildstone calendar could carry davis's host: an account that
   installs cleanly, fails to connect, and blames a password the person
   does not have. Ours is preferred explicitly now, and a test pins it in
   both orderings.

The through-line: each was a claim about something OUTSIDE the code under
test — what Radicale does with a PUT, what the notices table stores, what
the page draws — and that is exactly the class ADR-W029 says a package's
own checks cannot catch. Walking the paths found all five in one pass.

---

# Part C — The plan (not binding)

Ordered, and each phase is useful on its own.

### C1. The household event bus

Publish `calendar.event.created` / `.updated` / `.deleted` on change, and
`calendar.event.starting` / `.started` / `.ended` on time, onto the
existing household timeline (ADR-W024) — metadata, not the appointment
body, with consumers querying back for detail subject to authorisation.

The shape is forced by the app: Radicale's `[hook]` emits to RabbitMQ or
to email, and it can call no webhook, so the "config" shim this repo
prefers does not apply. It is a `sidecar` — an adapter container that
polls the collections and posts to the intake — which is why
`calendar-radicale` declares no `publishes` block today rather than
declaring one nothing honours.

The temporal half needs a small scheduler, and idempotent publication:
`starting` must fire once for an appointment, not once per tick.

### C2. Per-member calendars, and the confirmation tier

Two things B3 deliberately left, and they are the same shape: both need a
permission Wildstone has not got.

**A member's own calendar.** Today an agent acts on the household
calendar and cannot see anybody's private one (A9). Changing that means
either the service account holding a standing grant over everyone's
collections — which is exactly the thing A9 refuses to make a default —
or a per-member credential Keystone can act with, which is a real design
problem: a household member's calendar password is theirs.

The permissions this wants, riding the existing member/operator model
rather than a new one: `calendar.read.own`, `calendar.write.own`,
`calendar.read.household`, `calendar.write.household`, `calendar.admin`.

**A confirmation tier.** The policy this capability wants is:

| action | default |
| --- | --- |
| read the agenda, search, check availability | allow |
| create an event | allow |
| change an event the agent created | allow |
| change an event a person created | confirm |
| delete an event | confirm |
| touch another member's calendar | per policy; deny by default |
| bulk operations | confirm |

Four of those rows say **confirm**, and ADR-W024 A5's tier set has no
such value — a household action is `read` or `domestic`, and both mean
"do it". A10 records what stands in for it here. The tier belongs in
ADR-W024, where every capability's actions can use it, not in a calendar
executor.

### C2a. The adapter runtime has no runner

Recorded because it surprised the author and will surprise the next
person: `keystone/adapter` is library code with contract tests and **no
caller**. Nothing in the repository builds a binary that runs
`adapter.Runner`, and no package deploys one. The work-queue road
described in A1 is therefore a design that compiles, not a road anything
currently travels.

Nothing today needs it — Radicale is on Wildstone's own network and the
in-process road reaches it — so this is not a gap to close on the
calendar's account. It is the first thing C3 needs.

### C3. Phase 5 — other people's calendars

Establish the provider boundary now (done: it is `adapter.Upstream`) and
add a Google provider later, **aggregation before synchronisation**.

This is where the adapter runner (C2a) earns its keep: a Google calendar
is not reachable from Keystone's own process on the household's terms —
it wants OAuth, token refresh, and a failure domain of its own — which is
the case the connection and work-queue design was drawn for.

Aggregation answers "what do I have tomorrow" across a work calendar and
this one, and can be wrong only by omission. Bidirectional replication
can be wrong by duplicating, deleting, or overwriting somebody's
appointment, and needs its own design review before anyone writes it.
They are different problems and conflating them is how the second one
arrives without having been decided.

### C4. Still not doing

A proprietary calendar UI; a Google Calendar clone; bidirectional
synchronisation; direct agent access to the store; iCalendar generated by
a model; scheduling optimisation; cross-household federation. Each is
addable to the capability later, and none of them is on the path to a
household having a calendar that works.
