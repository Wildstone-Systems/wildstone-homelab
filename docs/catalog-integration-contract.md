# The catalog integration contract — what a package tells Butler it can do

- **Status:** design note. Not a decision record. Feeds ADR-W025 Part B
  (the unbuilt half of the catalog integration layer) and supplies the
  catalog-side half of ADR-W030 A5, which defers "exact schemas" to the
  adapter implementation slice.
- **Scope boundary:** this note owns the *declaration*: the vocabulary a
  package writes against, the `integrates:` block, and what an adapter
  author must implement. It does not own the queue tables, the claim/
  lease endpoints, the task ledger, or any Keystone runtime. Those are
  the implementation plan's, and this note deliberately stops at their
  edge.
- **Baseline:** main at `35d4565`.
- **Evidence:** the Davis findings in §7 were taken from the running
  container on `wildstone-homelab`, not from the package or upstream
  docs.

---

## 1. The problem, in one sentence

A package can already tell the household **what it emits**; it has no way
to say **what it can be asked** or **what it can be told to do**, so
Butler's entire action vocabulary across a 54-package catalog is `notify`
and `send`.

That is not a Butler limitation. It is a catalog one, and it is fixable
with data.

## 2. Three verbs, one of which already exists

W030 A5 says an integration declares "events it reports, queries it
answers, and actions it performs." Wildstone already has the first:

| Verb | Direction | Today |
|---|---|---|
| **reports** | app → household | `publishes:` (W024 A10), five packages use it |
| **answers** | Butler → app → Butler | nothing |
| **performs** | Butler → app | `agentTools:`, three packages, both tools are "send a message" |

**Only one verb is actually missing.** Both existing keys are already
scheduled to grow into their roles by conditional amendments that become
binding when W030 is accepted:

- ADR-W005's amendment keeps `agentTools` as the declaration and expands
  its validated metadata to "purpose, input schema, data/action scope,
  mutating behavior, and approval requirements" — which is precisely what
  a `performs:` block would have carried.
- The same amendment gives `publishes` a validated audience policy, with
  household-shared as the default and provenance stored at intake.

So this note should not invent `reports:` and `performs:` as new keys.
Doing that would fork two contracts that already have a written future.
What is missing is **`answers:`**, the query verb, which no key covers
and no amendment mentions — plus the adapter runtime that makes pull
possible at all, and the closed vocabulary in §3 that makes queries
substitutable.

One inconsistency worth fixing while nearby: ADR-W005 still describes
`agentTools` as spike-only, "parsed + validated only" (lines 75, 204,
212), but `keystone/cmd/keystone/main.go:801` builds the household action
vocabulary from the registry and gates it on the install, and the
domestic gateway dispatches it. The declaration's language never caught
up with the runtime, and the amendment above is the natural place to
correct it.

## 3. The vocabulary must be closed, for the same reason traits are

W025 A1 closed the trait vocabulary into `keystone/trait` — a leaf Go
package importing nothing — because a free-string seam cannot tell a rule
from a typo, and `requires: {trait: identiy}` was a valid package that
nothing satisfied.

Queries and actions have that problem and a worse one. Traits are matched
*within* the catalog: a misspelling fails to resolve and somebody notices
at install. A query name is matched against **an adapter written by
somebody else, for an app Wildstone may not run**. If the Davis adapter
answers `calendar.agenda` and a Nextcloud adapter answers
`cal.get_events`, both are individually correct and Butler's tool surface
becomes deployment-specific — which means every routine, every fixture,
and every regression test written against one household is worthless in
the next.

Substitutability is the whole point of having a contract. So:

> **Propose `keystone/integration`**: a leaf package, closed by review,
> declaring domains and the queries and actions each domain carries.
> Adding one is a change to one file, exactly like `keystone/trait` and
> `policy.Classifications`.

```go
// keystone/integration/integration.go
type Domain struct {
    Name        string   // "calendar"
    Description string
    Queries     []Query
    Actions     []Action
    Events      []string // well-known types; see below
}

type Query struct {
    Name        string   // "calendar.agenda"
    Description string   // what an author reads to pick the right one
    Params      []Param  // the FULL param set; implementations may support a subset
    Returns     string
}

type Action struct {
    Name        string
    Description string
    Params      []Param
    // ProposedClass is metadata, not authority. W030 A8: "Adapter
    // metadata proposes a class, but platform policy validates it and an
    // administrator may make it stricter, never silently weaker."
    ProposedClass string // low_impact | consequential
}
```

**Events stay open, with a well-known subset.** W024 deliberately made
event types open (regexp-shaped, any package may invent one) and
automations already name them; closing that now would break the five
packages that publish and every rule written against them. The compatible
move is a well-known list per domain that Butler and shipped routines
rely on, with lint *warning* — not failing — when a package invents a new
type inside a namespace that has a well-known vocabulary. That catches
`calendar.event_updated` next to the well-known `calendar.event_changed`
without making the vocabulary a gate.

## 4. The `integrates:` block

```yaml
integrates:
  domain: calendar          # closed, from keystone/integration
  shape: adapter            # native | sidecar | config | adapter | connection-only

  adapter:
    image: "ghcr.io/wildstone/caldav-adapter@sha256:..."
    # What the adapter NEEDS. Not where it runs: placement, fallback
    # nodes and grants belong to the connection and its Plan, not to the
    # package. A package that named a node would stop being data.
    needs:
      # An adapter reaches its app over a network endpoint, a read-only
      # mount of the app's own store, or both. Declaring the store as a
      # need is what lets an integration exist with no credential at all
      # (§7.3).
      mounts:
        - { source: davis-data, target: /davis, readOnly: true }
      endpoint:
        description: "The CalDAV base URL, e.g. https://calendar.example.com/dav/"
        validate: url
      secrets:
        - name: butler-caldav
          scope: member        # household | member
          purpose: "Writes events to calendars this member shared with Butler."

  # The missing verb. reports stay in `publishes:`, performs stay in
  # `agentTools:` — see §2.
  answers:
    - query: calendar.agenda   # closed vocabulary
      deadline: 5s             # W030 A5.1 default, within caller's budget
      # Capabilities can be partial (W030 A5). The closed query defines
      # every param; this says which ones THIS implementation honours, so
      # one query name spans a server with free/busy and one without.
      supports: [member, from, to]

# Existing block, two proposed additions (see below).
publishes:
  shape: adapter
  events:
    - type: calendar.event_changed
      subject: member
      production: polled       # NEW: pushed | polled
      freshness: 300s          # NEW: staleness this implementation guarantees
      fields:
        - { name: calendar,  type: string, required: true }
        - { name: uid,       type: string, required: true }
        - { name: starts_at, type: timestamp }

agentTools: []                 # read-only. An empty list is an answer.
```

Four things in there are load-bearing and worth defending:

**`production:` and `freshness:` on a published event.** W030 A5 requires
responses to "distinguish valid results, including empty lists, from
unavailable/stale information and carry source and observation time." A
polled event carries the adapter's *observation* time, not the moment the
thing happened, and the timeline must not present the two as the same
kind of fact. If only the adapter knows which it is, Keystone takes its
word for it at intake. Declared in the package, the label is reviewed
once and applied by the intake — the same posture as W024 A3, where the
intake stamps `source` from the publisher's token because "a publisher
can only ever speak for itself." These are two fields on `PublishedEvent`
in `keystone/pkgspec`, not a new block.

**`supports:`** is what keeps the vocabulary closed without making it a
lie. Without it, either every adapter must implement every param of a
query, or the query set fragments per-app — and the second is the failure
this whole design exists to prevent.

**`scope: household | member`** on a secret is the catalog-side
expression of W030 A5's connection defaults. A package that needs a
member-scoped credential is declaring, in data, that its connections
default to personal, read-only, live-query-only. Nothing else has to
carry that decision.

**No placement in the package.** Primary node, ordered fallbacks and
grants are connection state established by an approved Plan. The package
says what the adapter needs; the operator says where it runs. This is
W025's thesis applied — the moment a package names a node, adding a
service stops being data.

## 5. Where `adapter` sits among the existing shapes

`publishes.shape` already has three values, and they are all *push*: the
app calls the intake (`native`), a sidecar does it for the app
(`sidecar`), or a rendered config makes the app do it (`config`).

An adapter is two things at once, and they belong in two places:

- **As a publisher** it is a genuine fourth shim shape. When a CalDAV
  adapter manufactures `calendar.event_changed` from a poll, the thing
  pushing at the intake *is* the adapter — so `adapter` joins `native`,
  `sidecar` and `config` in `publishShapes`. One new entry in a closed
  list, which is the cheapest possible change.
- **As a runtime** it is something the existing shapes have no word for:
  it also *pulls* work. That is what `integrates.shape` names, along with
  the package's overall posture — including `connection-only` for an app
  Wildstone does not run at all.

The two do not collide because they answer different questions:
`publishes.shape` says how events reach the intake, `integrates.shape`
says what this package's relationship to the application is. W025's Part
B item 1 already wants the second field for exactly this reason ("the
three shim shapes are declared in `publishes` while the package's overall
posture is not declared anywhere"). This is that field, with the query
verb and the adapter runtime attached.

## 6. The adapter shim contract

Small on purpose. An adapter author writes app-specific code and nothing
else; everything below is the same for every adapter.

1. **Claim.** Long-poll a work endpoint, scoped to the adapter's named
   connections. The poll wakes when work arrives. This mirrors the chat
   outbound queue (`keystone/runtimedb/chatoutbound.go`) rather than
   reusing it — W030 A5.1 is explicit that `chat_outbound` must not be
   overloaded.
2. **Work envelope.** `{correlationId, connection, verb, name, params,
   deadline, epoch}`. It never carries a secret value. Where an adapter
   needs a credential at all, it reaches the container through the node
   secret path into its environment — the same mechanism
   `ROUNDCUBEMAIL_DES_KEY: "secret/roundcube-des-key"` already uses. An
   adapter that reads a read-only mount of the app's own store needs no
   credential and should declare none (§7.3).
3. **Do the app-specific thing.** The only part an author writes.
4. **Report.** `{correlationId, epoch, status, observedAt, source,
   payload}` where status is `ok | empty | unavailable | stale`. `empty`
   and `unavailable` are different answers and conflating them is how
   Butler tells a household there are no events when it simply could not
   look.
5. **Stay fenced.** Results carrying a superseded epoch are rejected. The
   adapter does not need to know it lost a lease; it needs to not be
   trusted when it has.
6. **Discovery reports, it does not authorize.** An adapter may report
   capabilities it found against the live app's version and credential
   scope. Found is not enabled — W030 A5 and W025's amendment both say
   so, and the package's declaration remains the ceiling.

Catalog-side follow-ons, both already on W025's Part B list:
`wild pkg new --adapter` (extends A5's scaffold) and the adapter chapter
of `packages/AUTHORING.md` (Part B item 2).

## 7. Worked example: `mail-roundcube`

The package runs two apps and the honest answer is that they integrate
very differently.

### 7.1 Roundcube is not an integration target

Roundcube is a webmail UI over the provider's IMAP. It holds the
household's settings, identities and address book, and nothing else — the
mail lives at Fastmail or Google or wherever. There is no API here worth
adapting: an adapter that wanted mail would speak **IMAP to the provider
directly**, and Wildstone does not own that mailbox.

So mail is a `connection-only` integration against the provider, not an
integration with the Roundcube container. One package, two integrations,
different shapes. Worth naming because the instinct is to integrate with
the thing Wildstone runs, and here that thing is the wrong end.

IMAP also has a genuine push mechanism (IDLE), so `mail.received` could
be `production: pushed`. That is follow-up work regardless — W030 line
307 puts "selected read-only IMAP monitoring with review-first calendar
writes" after v1.

**No mail is stored locally, and none should be.** From the running
container's `roundcube-db`: `cache_messages`, `cache_index` and
`cache_thread` all hold **0 rows** — Roundcube's message cache is off by
default and this package does not enable it. What is local is small:
`identities` (1 row), `contacts` (0, the address book) and `session` (38,
each holding an IMAP password encrypted with `des_key`).

This is the asymmetry that shapes everything below: **calendars are
local, mail is not.** The calendar path can avoid credentials entirely.
The mail path cannot — reading mail means IMAP to the provider, which
means the full-power credential.

It is worth writing down that Roundcube's message cache must stay off.
`roundcube-db` is in the daily BackupPlan, so enabling the cache would
put message bodies into restic snapshots — and the package's header
already reasons carefully about keeping sensitive material out of exactly
those volumes. The moment §7.3's approach works well for calendars, "do
the same for mail" is the obvious next thought, and it is a trap.

### 7.2 Davis has a REST API the package never turns on

Taken from the running container:

- `php bin/console debug:router` lists a full `/api/v1/` surface:
  `health`, `users`, `users/{userId}`, `calendars/{userId}`,
  `calendars/{userId}/{calendar_id}`, plus create/edit/delete/share.
- Authentication is `App\Security\ApiKeyAuthenticator` with
  `$apiKey: "%env(API_KEY)%"` (`config/services.yaml:83`).
- `packages/mail-roundcube/package.yaml` sets no `API_KEY`. Every API
  endpoint, `/api/v1/health` included, returns **500** in this
  deployment — Symfony cannot resolve the env var, so the authenticator
  cannot be constructed.

That last point is a live catalog defect, not a design question: the
package ships an app with an admin API wired to an environment variable
it never provides. Turning it on is a generated `Secret` and one env
line, in exactly the shape the package's three existing secrets already
use.

**But the API does not solve the interesting problem.** There are no
event endpoints. It manages *calendar collections and users*, not
calendar *entries*. So:

| Want | Path | Credential |
|---|---|---|
| Which members exist, which calendars they have | `/api/v1/` | `API_KEY`, household-scoped |
| The actual agenda | CalDAV `/dav/` | a member principal |

Enumeration is free. Reading an event is not.

### 7.3 The credential problem, and why v1 does not have one

Davis runs `AUTH_METHOD=IMAP` with `IMAP_AUTH_USER_AUTOCREATE`, so every
principal *is* an IMAP identity and Wildstone generates no credential for
any person. The naive read — give the adapter each member's CalDAV
password — means holding a credential that, for Gmail and Outlook, is an
app-password with full mailbox access. W030 A5.1 anticipates this and
requires disclosing "the credential's wider upstream power", but
disclosure is a consolation prize.

Where the credential sits is not the interesting question. The adapter
holds it and the model never sees it either way; that much W030 already
settles. What matters is what the credential can *do*, and a mail
password fails on three counts: the blast radius of a compromised adapter
is every member's whole mailbox, revoking Butler means changing a
password that also breaks webmail and phones, and the provider cannot
tell Butler's session from the member's.

**So v1 does not use one.** The calendars are on the node, in SQLite, and
an adapter can mount that volume read-only — `resource.Mount` already
carries `ReadOnly` (`keystone/resource/resource.go:122`). No credential
exists to steal, revoke, or onboard. The household keeps one password.

The schema is unusually well suited to this. From the running container:

| Table / column | What it gives the adapter |
|---|---|
| `calendarobjects.calendardata` | the raw iCalendar text — the *same* payload CalDAV returns |
| `firstoccurence`, `lastoccurence` | indexed timestamps, so "what is on Tuesday" is a range query, not a recurrence expansion |
| `calendarchanges` | sabre/dav's sync log (RFC 6578), so polling reports real deltas rather than re-reporting the calendar every tick |
| `calendarinstances` | share relationships and access levels |

That last row corrects an assumption worth stating plainly, because it is
the one that would otherwise sink this option: **reading the database
does not mean bypassing Davis's sharing rules.** The share relationships
and their access levels are in `calendarinstances`, so an adapter can
read them and honour them. Per-calendar consent survives the
credential-free path, which is the property it looked like it would cost.

Two honest limits. The adapter is trusted to apply those rules — a bug
reads everything, where a real CalDAV login would have been refused by
the server. And this path is read-only, permanently: see §7.4.

### 7.4 Writes are a second tool, and they will need a credential

The tempting next step is to widen the same mechanism to writes. It does
not work. sabre/dav maintains `etag`, `lastmodified`, the
`calendarchanges` sync log and `schedulingobjects`; writing rows behind
the application's back desynchronises every subscribed phone, which
either misses the change or starts failing its sync. A write path has to
go through CalDAV, and CalDAV needs a principal.

So the sequence is: **read from the database now, write over CalDAV
later, as a separately declared and separately approved tool.** This is a
deferral of the credential decision, not an escape from it — and it is
the right shape anyway:

- the read half ships with no credential, no onboarding surface, and a
  blast radius bounded by one read-only mount;
- the credential question is answered later, when the household knows
  whether it wants Butler writing to calendars at all;
- W030 already sequences it this way — read-only CalDAV in v1, "review-first
  calendar writes" in the follow-up at line 307;
- and the two halves share an implementation. `calendardata` is raw
  iCalendar, so this is one data model with two transports, not two
  integrations.

When the write tool is wanted, these are the options, and none is free:

| | Credentials held | Logins per person | Per-calendar consent | New dependency |
|---|---|---|---|---|
| Member mail passwords | everyone's, full mailbox power | 1 | yes | none |
| `AUTH_METHOD=LDAP` via LLDAP | one, scoped | 2 | yes | `identity` |
| Provider scoped app-password | one per member, calendar-only | 1 | yes | provider must support it |

The LDAP row deserves detail because the pieces are already running.
`config/services.yaml:62` passes `$authMethod: "%env(AUTH_METHOD)%"` to a
single service — one method for every DAV principal, no per-principal
mix — and Davis ships `src/Services/LDAPAuth.php` beside its IMAP and
Basic backends. `packages/identity` (profile `authelia-lldap`) already
deploys the LLDAP that `lldap` and `authelia` are running against, and
`media-jellyfin` already authenticates there. Switching Davis to LDAP
would let Wildstone provision a Butler principal in a directory it
already owns, rather than asking the mail provider for a mailbox.

Its cost is the package's headline design, quoted from its own header:
"ONE LOGIN, TWO SURFACES… a person has exactly one credential — their
mail password." Roundcube must stay on IMAP, so members would end up with
a mail password for webmail and a household identity for calendars —
two credentials, which is what the package was written to avoid. It also
re-authenticates every synced phone and adds `requires: identity`.

The third row is worth ten minutes before designing around either of the
others: some providers issue app-passwords scoped to a single protocol,
which would make a calendar-only credential possible with one login and
no directory change. Gmail and Outlook do not. Whether this household's
provider does is a question of fact, not architecture, and it changes
which row is cheapest.

### 7.5 What the declaration would say

V1, reading the volume. No `secrets:` block at all — that absence is the
whole point of §7.3, and it is the one line a reviewer should check
first.

```yaml
integrates:
  domain: calendar
  shape: adapter
  adapter:
    image: "ghcr.io/wildstone/caldav-adapter@sha256:..."
    needs:
      # Reading the store, not the protocol. No principal, no password,
      # nothing to onboard or revoke.
      mounts:
        - { source: davis-data, target: /davis, readOnly: true }
  answers:
    - { query: calendar.agenda,  deadline: 5s, supports: [member, from, to] }
    - { query: contacts.lookup,  deadline: 5s, supports: [name] }

publishes:
  shape: adapter
  events:
    - { type: calendar.event_changed, subject: member, production: polled, freshness: 300s }

agentTools: []          # read-only. An empty list is an answer.
```

The follow-up write tool is a separate declaration against the same
package, and it is where the credential finally appears:

```yaml
  adapter:
    needs:
      endpoint: { description: "Davis CalDAV base URL.", validate: url }
      secrets:
        - { name: butler-caldav, scope: member, purpose: "Writes events to calendars this member shared with Butler." }

agentTools:
  - { name: create_event, risk: domestic }
```

Two notes for whoever writes the adapter:

**Read deltas, not calendars.** The `calendarchanges` table is sabre/dav's
`sync-collection` log (RFC 6578); hold a sync token per collection and
report what changed. An adapter that re-reads whole calendars re-reports
every event on every tick, and `production: polled` is a declaration the
intake dedupes against — not a licence to emit the calendar repeatedly.

**Apply the share rules yourself.** Reading the volume means the server is
not there to refuse anything. `calendarinstances` carries the share
relationships and their access levels, and honouring them is the
adapter's job. This is the one place the credential-free path is weaker
than a real login, and it should be tested as its own case rather than
assumed.

## 8. Open questions

1. **Which credential backs the write tool?** Deferred by design, not
   unresolved — §7.4 lists the three options and none is needed until
   writes are wanted. The cheapest answer depends on a fact nobody has
   checked yet: whether this household's mail provider issues
   app-passwords scoped to a single protocol. Worth establishing before
   the write slice is designed, and irrelevant until then.

2. **Should the read adapter also declare a `scope: household`
   `davis-api-key`?** The REST API enumerates users and calendars without
   a member credential, which is genuinely useful for discovery — but
   §7.3's adapter can read `principals` and `calendars` straight from the
   volume, so the API buys nothing the mount does not already give. It
   may be that fixing the `API_KEY` defect in §7.2 is worth doing purely
   because a shipped app with a broken admin API is a defect, and not
   because this integration needs it.
3. **Should `answers:` live inside `integrates:` while its two siblings
   stay top-level?** Coherent-looking would be all three under
   `integrates:`; compatible is what §2 chose. The split is a little ugly
   and the alternative is migrating two keys that five packages already
   use, for cosmetics.
4. **Who owns the well-known event list per domain** — `keystone/integration`
   alongside the queries, or `keystone/household` with the envelope? The
   second is closer to where event semantics already live.
5. **Does `integrates.domain` allow more than one per package?**
   `mail-roundcube` wants `calendar` *and* `mail` at different shapes,
   which argues for a list. That in turn argues `integrates:` is a list
   of blocks, not one block — cheap now, expensive later.
6. **Lint's reach.** A3 makes the shipped catalog lint-clean in CI. Lint
   can check a query name against the closed vocabulary and a `supports:`
   subset against its params, but it cannot know whether the adapter
   image actually implements them. W029's "a package is checked against
   the app it configures" is the relevant precedent for how far to push.
