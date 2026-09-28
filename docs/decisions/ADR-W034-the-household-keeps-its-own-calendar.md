# ADR-W034: The household keeps its own calendar

- **Status:** accepted. Built: A1–A14, C2 (a member's own calendars on
  the Calendar page). Open: C1 (event bus), C3 (other providers).
- **Relates to:** ADR-W005 (package format), ADR-W013 A4 (backups),
  ADR-W019 A10 (a Route is not always served at the host it declares),
  ADR-W024 (household events and automations), ADR-W029 (a package is
  checked against the app it configures), ADR-W030 A5/A5.1 (scoped
  integrations and the adapter contract), ADR-W031 (Butlers), ADR-W038
  (declared shared calendars).
- **Governed by:** ADR-W033 where they overlap.
- **Terminology:** "connection" here means an integration connection
  (ADR-W030 A5), not an ADR-W033 Connection (how a Person reaches an
  Agent).
- **Audience:** decision record for the `calendar` capability. Which
  calendars exist and who may read or write each is ADR-W038.

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
the audit of it. Not a table of events. A commitment booking (ADR-W035
A7) keeps its date, the calendar ID and the event href it follows; it is
a pointer to the event, not a copy of it.

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
this household runs) is made available by the deployment: the seven tools
are registered when Keystone is started with `-calendar-url`, the same way
`-ntfy-url` is. A missing `-calendar-user` or `CALENDAR_PASSWORD` is logged
as a warning at startup, and every call then fails upstream.
Wiring makes the tools available; it grants them to no Agent. Each Agent
uses them only under an Owner's grant to that Agent, by tool class,
bundle or explicit selection, with opt-in tools excluded from broad grants
(ADR-W033 A0, B3–B5).
_Code not yet conformant: calendar actions are reached through the single
`do_household_action` tool and filtered by declared tier, not granted per
action to each Agent._

On either road, deleting an event is additionally gated per call by the
`confirm` tier (A10).

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

### A9. The agent acts on declared shared calendars, not on anybody's own

Everybody's own calendars are their own: `rights` is anchored on the name
they signed in with, and no other person reads them. The service account
does, for one reader only — the Calendar page shows a member their own
(C2) — and no agent tool reaches them. Alongside them are the declared shared calendars of
ADR-W038, each a group collection with its own read and write access;
`household` is the default one. The seven tools act on the default, or on
the calendar named by their optional `calendar` argument, within the
acting Person's access to it.

It is deliberately not "the agent acts on your calendar", which would
mean a standing grant over every member's private appointments for the
convenience of a create. So an agent cannot answer "what's on MY
calendar tomorrow" — only what is on the shared calendars that Person can
read. That is open (C2).

### A10. Delete asks first

Deleting an event uses the `confirm` tier of **ADR-W024 A5.1**, the
general tier for every destructive household action.

`calendar.delete_event` is declared at `confirm`, and two things
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

### A12. A shared calendar is a GROUP collection, so a phone finds it

A CalDAV client discovers the calendars under the principal it signed in
as and nothing else. Radicale yields a collection stored under
`collection-root/<group_collections_folder>/<base64 of a group name>` as
a child of the principal of **everybody in that group**. So the household
calendar lives at `GROUPS/Y2FsZW5kYXJfdXNlcnM=` — `calendar_users`,
encoded — and a member signs in as themselves and finds it beside their
own. Further declared calendars follow ADR-W038's discovery and rights.

This needs Radicale 3.8.0 or later:

- `[group] type = from_auth` feeds Radicale the memberships the directory
  returned.
- `ldap_groups_attribute = memberOf` returns group **DNs**, and Radicale
  takes the RDN value of each — so `cn=calendar_users,ou=groups,…`
  becomes `calendar_users`, which is what both the rights file's
  `groups:` clause and the base64 folder name are written against.
- The rights file grants that group `rw` on that path.

The base64 is spelled twice — in the rights file and in the job that
creates the collection — because the package template language has no
base64 to derive it with.
`TestTheHouseholdCalendarIsTheGroupsOwnCollection` pins both against the
LdapGroup's own name; a mismatch would create the calendar where no
member's discovery looks.

**Not verified on a device.** If a phone does not follow the group
collection's href, add the household calendar by its own address as a
second account.

### A13. A member's first calendar is made for them

Radicale creates a member's principal on their first authenticated
request, and it was empty — so somebody who installed the profile and
signed in met an account with no calendars in it and no obvious next
step. `[storage] predefined_collections` fills it at the same moment.

Group membership is already the gate, with nothing extra to enforce: the
`ldap_filter` means only somebody in `calendar_users` can authenticate at
all, so only they ever reach it.

### A14. Pin the release, not `latest`

`ghcr.io/kozea/radicale:latest` is a nightly build, not the latest
release. The image is pinned to the `stable` release tag's digest, and
`TestTheCalendarImageIsTheReleaseNotTheNightly` holds it there.

---

# Part B — Implementation rules

### B1. The capability

`packages/calendar-radicale`: Radicale on a persistent volume, behind the
private ingress, signed in against the household directory
(`auth: app_ldap`, since a phone's calendar app sends HTTP Basic and
cannot follow a login redirect), with `calendar_users` as the grant and a
daily backup of the collections.

- **Radicale reads no environment variables.** Its configuration is one
  ini file; a setting passed as `env:` is inert.
- **The reader password is placed by a root Job.** A managed file carrying
  a secret reference is materialised `0600` root-owned, and this image
  runs as uid 1000; Wildstone cannot yet say what user a managed file
  belongs to, so the Job hands the file to the user that reads it.
- **One service account.** Radicale searches the directory as it and
  Keystone writes the shared calendars as it. `owner_only` still applies
  to every person's own calendars, and this account is in none of them.

### B2. Reads and writes in the CalDAV layer

`keystone/adapter/caldav` reads (`caldav.today`, `caldav.events`) and
writes (`caldav.create_event`, `caldav.update_event`,
`caldav.delete_event`) under A5's gates, with A6's generation, A7's
editing and A8's addressing. Both roads share one exported
implementation. Creates are conditional on `If-None-Match: *` so a
colliding UID cannot silently replace an event; updates are conditional
on the etag they read, so a concurrent edit is a refusal rather than a
lost update.

### B3. The seven tools

`packages/calendar-radicale` declares seven `agentTools` and
`keystone/domestic` registers an executor for each:

| tool | tier | what it does |
| --- | --- | --- |
| `calendar.get_agenda` | read | what is on, for a day or a window |
| `calendar.search_events` | read | find by title |
| `calendar.check_availability` | read | is that window free, and what is in it |
| `calendar.how_to_add` | read | how to get the calendar onto a phone |
| `calendar.create_event` | domestic | add one |
| `calendar.update_event` | domestic | change one |
| `calendar.delete_event` | confirm | remove one, after the person says yes (A10) |

They are reached through `butler.list_household_actions` and
`butler.do_household_action`, and household automation rules can name
them. Wiring is not a grant (A5; ADR-W033 B3–B5).

Executor rules:

- **The summary is the whole answer.** `do_household_action` hands the
  model `Result.Summary` only, so a read's summary is written to be read
  aloud.
- **An event is named by title and date, not by an id.** Ambiguity is a
  refusal that names the candidates.
- **A time with no offset is in the household's time zone**, never UTC.

`TestCalendarToolsAreDeclaredByThePackage` pins the package's declaration
against `domestic.CalendarTools`, names and tiers both.

### B4. Runtime facts

- The credential Job creates the shared collection only when it is
  absent (Radicale does not create a collection on PUT).
- An unreadable calendar is reported as `Available: false` with a reason,
  never as an empty agenda.
- When both calendar capabilities are installed, the portal prefers
  Wildstone's own calendar route over Davis's.

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

### C2. Personal calendars

Access to shared calendars is decided by ADR-W038, and the `confirm`
tier is built (A10).

**Amendment (2026-09-28): a member sees and keeps their own calendars on
the Calendar page.** Decided with the household owner. Keystone never
holds a member's own calendar password. It reaches their calendars with
the calendar service account instead, which the generated rights file
lets read and write every person's own collections and create their
principal. That is an administrator's reach, and it is stated rather than
hidden: whoever runs the machine could already read the collections
volume.

- **The principal is the proven member.** The page derives it from the
  member the identity proxy asserted, lower-cased as Radicale's login is.
  No request field names a principal, and every collection the server
  reports is confined to a direct child of it before use.
- **Their own, and only there.** A member's own calendars are listed on
  their Calendar page beside the shared ones, with a colour and a
  visibility choice. They never appear on the dashboard or the agenda
  (shared screens, ADR-W033 A10), in a booking, or in any Butler tool
  (A9 is unchanged). Household activity records that a member changed
  their own calendar, never what the event was.
- **No event crosses kinds.** An event moves between two shared calendars
  or between two of the member's own, never between the two kinds. A
  shared event may carry a booking, and moving a private event onto a
  shared calendar would publish it by a side effect.
- **New calendar.** The page can make the member a new calendar in their
  own account, so it reaches their phone as well. For somebody who has
  never signed in to the calendar, the principal is created first.
- **A rename keeps them.** Radicale keeps a person's calendars under the
  login that made them and does not move them when the login changes. The
  page lists calendars under every login the Person has had (#372) and
  makes new ones under the current login, which is the one a phone signs
  in with.
- **Undeclared shared collections stay out.** The service account's
  personal rules exclude `GROUPS/`, so a shared calendar removed from the
  registry is not the service's to read.

Changes to the calendar registry go through an Owner-approved package
Plan (ADR-W033 E2; ADR-W038).

### C2a. The adapter runtime has no runner

`keystone/adapter` has no deployed `adapter.Runner`, so the work-queue
road of A1 is not yet travelled; C3 needs it first.

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
