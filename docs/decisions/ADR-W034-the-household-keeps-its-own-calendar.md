# ADR-W034: The household keeps its own calendar

- **Status:** accepted for phases 1 and 2; phases 3–5 are a plan, not a
  commitment.
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
Radicale or a mailbox provider's server is a connection's configuration
rather than a different adapter.

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

An agent reaches the calendar the way it reaches every other
application: by asking Keystone to queue work against a **connection**,
which an adapter claims and answers (ADR-W030 A5). It receives normalised
events — a title and two times — never iCalendar, and it holds no CalDAV
password.

This is the same rule ADR-W030 already makes, restated here because a
calendar is the most tempting place to break it: the store is a
directory of text files, and handing an agent a path would be one line.

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

### A5. Writing is off until two separate things say it is on

Calendar writes are real (phase 2, below), and they are gated twice:

- Keystone refuses action work for a connection nobody has widened —
  `w.Kind == WorkAction && !conn.ActionsEnabled` — so an adapter is never
  even handed one until a person decides; and
- the adapter itself must be run writable, so an adapter deployed to
  answer questions cannot be talked into changing a calendar by anything
  that reaches it over the wire.

The second is a deliberate second copy of the first, for the same reason
`keystone/domestic` keeps its own copy of the tier rule: a call that
arrived some other way still cannot write to the household's calendar.

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

### A9. The calendar works with no agent at all

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

The two install steps this leaves (create a read-only directory account;
`wild secret set calendar-ldap-reader`) are the same shape as
`wild household publisher home-automation`, and they fail loudly and
early rather than quietly and late.

### B2. Phase 2 — reads, and writes

Reading was already built: `keystone/adapter/caldav` has answered
`caldav.today` and `caldav.events` since ADR-W030 P12, and pointing it at
the household's own Radicale needs no new code — a connection and a
credential.

Writing is new: `caldav.create_event`, `caldav.update_event` and
`caldav.delete_event`, under A5's two gates, with A6's generation, A7's
editing and A8's addressing. Creates are conditional on `If-None-Match: *`
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

---

# Part C — The plan (not binding)

Ordered, and each phase is useful on its own.

### C1. Phase 3 — the household event bus

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

### C2. Phase 4 — the agent tools

`calendar.get_agenda`, `calendar.search_events`, `calendar.create_event`,
`calendar.update_event`, `calendar.delete_event`,
`calendar.check_availability`.

`calendar-radicale` declares no `agentTools` today, deliberately: the
domestic gateway dispatches a tool only to a *registered executor*, so a
declaration without one is a rule refused at dispatch with "no executor
is registered for this tool". The declaration and the executor land
together or not at all.

Confirmation policy, when they do — read broadly within scope, and put
the controls around the destructive end:

| action | default |
| --- | --- |
| read the agenda, search, check availability | allow |
| create an event | allow |
| change an event the agent created | allow |
| change an event a person created | confirm |
| delete an event | confirm |
| touch another member's calendar | per policy; deny by default |
| bulk operations | confirm |

Permissions ride the existing member/operator model rather than a new
one: `calendar.read.own`, `calendar.write.own`,
`calendar.read.household`, `calendar.write.household`, `calendar.admin`.

### C3. Phase 5 — other people's calendars

Establish the provider boundary now (done: it is `adapter.Upstream`) and
add a Google provider later, **aggregation before synchronisation**.

Aggregation answers "what do I have tomorrow" across a work calendar and
this one, and can be wrong only by omission. Bidirectional replication
can be wrong by duplicating, deleting, or overwriting somebody's
appointment, and needs its own design review before anyone writes it.
They are different problems and conflating them is how the second one
arrives without having been decided.

### C4. Not doing, for v1

A proprietary calendar UI; a Google Calendar clone; bidirectional
synchronisation; direct agent access to the store; iCalendar generated by
a model; scheduling optimisation; cross-household federation. Each is
addable to the capability later, and none of them is on the path to a
household having a calendar that works.
