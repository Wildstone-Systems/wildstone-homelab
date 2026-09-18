# Bot routing and per-Butler chat connections

- **Issue:** #153
- **Status:** complete
- **Audited:** 2026-09-10, on `787ad6d` (`butlers-on-the-live-path`)
- **Acceptance criteria:** AC08

Written by the audit slice for
[#153](https://github.com/chickenrat17/wildstone-homelab/issues/153).
**This file belongs to that slice alone** — no other session writes
here, which is what keeps parallel audits from colliding. See
[`../README.md`](../README.md) for how to run one and
[`../gap-report.md`](../gap-report.md) for the register.

`testing/e2e` lints this file's shape. Run
`cd testing/e2e && go test -run TestAuditRegister ./...` before you
commit.

## How this was audited

Code read on `787ad6d`. Tests run: `go test ./butlerlive/... ./runtimedb/...`
in `keystone/` — both packages pass.

**Runtime evidence is from a throwaway local Keystone, not from the
household deployment.** The live node described in
[`runtime-access.md`](../runtime-access.md) is not reachable from the
session this audit ran in: there is no systemd (`Failed to connect to
bus`), no `/var/lib/wildstone/keystone`, no `/etc/wildstone`, and
`127.0.0.1:9441` refuses the connection. So the runbook's token,
`journalctl` and `wild` steps could not run at all.

Instead the audited binary was built from this commit and run against a
scratch data directory on port 19441, with a stub model endpoint, two
synthetic Butlers (`alfie`, `sandy`), one synthetic household member
(`sam`) and two synthetic concierge credentials standing in for chat
adapters. That is real runtime evidence about **this code**; it is not
evidence about the household's configuration. Where a claim depends on
how the live deployment is set up rather than on what the code does, it
is marked `unverified` and says so.

The scratch instance, its database and its tokens were deleted
afterwards; nothing was written to the household deployment, which this
session could not reach in the first place.

## Requirements

| # | Baseline | Requirement | Status | Evidence |
|---|---|---|---|---|
| 1 | §7 | Dedicated bot identity per enabled external platform | partial | F-153-01 — one bot per *surface* per transport, not per Butler |
| 2 | §7 | Explicit account-to-Butler binding | missing | F-153-05 — a binding names a surface; no column, endpoint or screen binds an account to a Butler |
| 3 | §7 | Three Telegram-enabled Butlers require three Telegram bots | missing | F-153-01, F-153-02 — no package, provisioning or registration for a second bot; a hand-built one works on the inbound path and misroutes on the outbound |
| 4 | §7 | The same Butler reachable through web, Telegram and Slack | partial | F-153-12 — many addresses per Butler is supported; only telegram and matrix exist, and Console reach is by name not address |
| 5 | §7 | Authenticated enrollment; display names establish nothing | implemented | F-153-13 |
| 6 | §7 | Histories stay separate by default | partial | F-153-06 — separate, but a second Butler is refused rather than given its own thread |
| 7 | §16 | Stop delivery through removed or misconfigured connections | missing | F-153-07 |
| 8 | §16 | Never silently switch to another Butler's bot identity | missing | F-153-03 |
| 9 | §16 | Preserve pending delivery state and show the failure | partial | F-153-08 — the state is kept for seven days; the failure is never shown |
| 10 | §16 | Unknown senders need pairing or an allowlist | implemented | F-153-13 |
| 11 | ADR-W031 A7 | The bot token is the address; `Turn.Address` cannot come from a request body | implemented | F-153-11 |
| 12 | §7 | Per-Butler Connections and Chat configuration in the Console | missing | F-153-09, F-153-15 |
| 13 | §16 | A reply, a routine notification and an error each use the originating Butler's identity | missing | F-153-03, F-153-04 |

## Acceptance criteria

| AC | Result | Evidence |
|---|---|---|
| AC08 — two Butlers with different bot accounts | **fails** | No supported way to run a second bot on a surface: no package, no provisioning, no registration, no invite link. F-153-01, F-153-02 |
| AC08 — direct replies use the correct identity | **fails** | Sandy's reply was collected and acked by Alfie's bot. F-153-03 |
| AC08 — routine notifications use the correct identity | **fails** | The push path carries no Butler at all. F-153-04 |
| AC08 — errors use the correct identity | **fails** | Error replies are queued on the same Butler-less queue; a routing failure is reported to the person as a transient outage. F-153-03, F-153-14 |
| AC08 — reconnect behaviour uses the correct identity | **fails** | The adapter polls with cursor 0 on reconnect and drains every Butler's queued replies for its surface. F-153-03 |
| AC08 — switching Butlers preserves permitted shared knowledge | **fails** | Memory is siloed per Butler; established in #151 finding 1, belongs to #156. Cross-slice |
| AC08 — without merging private conversations | partial | Conversations are not merged, but the second Butler is refused rather than given its own thread. F-153-06 |

## Findings

### F-153-01 — A second bot on a surface is unsupported, so a household gets one Telegram-addressable Butler per surface

Baseline §7 wants a bot per Butler. The deployment's chat identity is a
*capability*: `chat-<transport>-<surface>`, one fixed string per package
(`packages/chat-telegram-family/package.yaml:70`,
`packages/chat-telegram-admin/package.yaml:56`). An adapter's address is
that capability, recovered from its principal name
(`keystone/api/handlers_butler_named.go:44`), and one live address
resolves to exactly one Butler
(`keystone/runtimedb/migrations/0034_butler_assignment.sql:73`).

So the supported set of chat identities is four — telegram/matrix ×
admin/family — and the household surface has exactly one. A second
household Telegram bot has no package to install, so nothing builds or
runs its container; it cannot register itself, because `handlePatterns`
is keyed by transport name and knows only `telegram` and `matrix`
(`keystone/api/handlers_chat_transports.go:175`) and a registration
whose transport disagrees with its capability is refused
(`handlers_chat_transports.go:213`); and without a registration there is
no handle, no invite link and no command menu
(`handlers_chat_transports.go:277`).

**Unsupported is not the same as prevented, and the difference matters
for remediation.** See F-153-02: the message path itself does not stop a
second adapter, and that is what makes F-153-03 urgent rather than
theoretical.

- **Status:** missing
- **Evidence:** code as cited on `787ad6d`; runtime on the scratch
  instance — `GET /api/v1/chat/transports` returned exactly four rows
  (`chat-matrix-admin`, `chat-matrix-family`, `chat-telegram-admin`,
  `chat-telegram-family`) from a catalog of 55 packages, and
  `POST /api/v1/chat/adapters/self` with transport `telegram2` returned
  `transport "telegram2" has no known handle format`.

### F-153-02 — A hand-provisioned second bot does reach the second Butler, and only its supporting furniture is missing

*Revised after review on this PR: the first version of this finding said
the second address was one no bot could ever arrive at. That is wrong,
and the correction matters, because remediation planned against a
structural limit that does not exist would be planned against nothing.*

Giving two Butlers the same address is refused, which is correct. Giving
the second Butler a different address succeeds, and a credential can be
minted for it by hand: `mintChatAdapter` validates the capability's
*name* — that it ends in `-family` or `-admin` — and never checks it
against the catalog or the installed set
(`keystone/api/handlers_chat_adapter.go:74`). An adapter run with that
token relays and delivers regardless of registration: `run` starts the
receive and deliver loops independently of `registerLoop`, and a
registration failure is explicitly non-fatal
(`cli/cmd/chat-adapter/adapter.go:165`, `:189`).

Nothing on the message path re-checks the reported transport against the
credential's capability either — only registration does
(`handlers_chat_transports.go:213`) — so such an adapter reports
transport `telegram`, is addressed as its own capability, and reaches
its own Butler.

What is actually missing is everything around it: the package, the
Console and CLI provisioning, the registration, the handle and the
tap-to-connect invite link. So a second bot is buildable by hand and
supported nowhere — and F-153-03 means its replies would come out of the
first bot.

- **Status:** missing
- **Evidence:** runtime on the scratch instance —
  `PUT /api/v1/butlers/alfie/address {"transport":"telegram","externalId":"chat-telegram-family","allow":true}`
  succeeded; the same call for `sandy` returned
  `telegram/chat-telegram-family already answers for another butler`;
  `{"externalId":"chat-telegram2-family"}` for `sandy` succeeded, and a
  hand-minted concierge credential for `chat-telegram2-family` then
  relayed a message reporting transport `telegram` that was answered by
  `sandy` (session `bs-2`, `butler: sandy`).

### F-153-03 — The outbound queue has no Butler, so one Butler's reply leaves through another Butler's bot

`chat_outbound` carries surface, transport, target and binding, and no
Butler (`keystone/runtimedb/migrations/0009_chat_outbound.sql:13`).
`queueChatReply` writes the surface and transport and nothing else
(`keystone/api/handlers_chat_message.go:413`), and `ChatOutboundAfter`
filters on exactly those two columns
(`keystone/runtimedb/chatoutbound.go:167`). Any adapter on a surface
therefore drains every Butler's replies for that transport, and acks
them. The transport a reply is filed under is the one the *inbound*
message reported, and the message path never re-checks that against the
credential's capability (only registration does,
`handlers_chat_transports.go:213`) — so two bots on one surface land in
one queue whatever they call themselves.

This is §16's "never silently switch to another Butler's bot identity",
and it is not a latent risk: it is the delivery path as written. It also
makes reconnect wrong for the same reason — the adapter always polls
from cursor 0 (`cli/cmd/chat-adapter/keystone.go:101`), so a bot coming
back up takes whatever is queued for its surface.

- **Status:** missing
- **Evidence:** runtime on the scratch instance — a message relayed by
  the `chat-telegram2-family` credential resolved to `sandy` (session
  `bs-2`, `butler: sandy`), and its reply, seq 4, was returned to and
  acked by the `chat-telegram-family` credential:
  `GET /api/v1/chat/outbound?after=3&transport=telegram` →
  `[(4, 'family', 'telegram', '9002', 'ok')]`, then
  `POST /api/v1/chat/outbound/4/ack` → `{"acked":4}`.

### F-153-04 — Routine notifications carry no Butler at all

A routine's chat delivery goes through `ChatSender`, wired per *chat
capability* (`keystone/cmd/keystone/main.go:844`). The recipient is
looked up from the family bindings by member
(`keystone/api/handlers_chat_message.go:504`) and the push is enqueued
with surface and transport (`handlers_chat_message.go:565`). Nothing in
that path names the Butler that owns the routine, even though
`household_routines` records one
(`keystone/runtimedb/migrations/0034_butler_assignment.sql:29`).

- **Status:** missing
- **Evidence:** code as cited on `787ad6d`; `grep -rn
  "QueueChatPush\|AskOverChat\|ChatRecipientFor"` finds no caller that
  supplies a Butler, and no `butler` column exists in `chat_outbound`.

### F-153-05 — A chat account is bound to a surface, not to a Butler

`chat_bindings` has transport, account, surface and identity
(`keystone/runtimedb/chatbindings.go:20`), and a claim code names a
surface and an identity (`keystone/api/handlers_chat.go:36`). Which
Butler a bound person reaches is decided entirely by the address the
message arrived at, so on a surface with one bot every bound account
reaches the same Butler, regardless of which Butlers that person has
been granted.

Baseline §7 asks for "explicit account-to-Butler binding". This is
per-surface binding plus per-bot addressing, which is a different thing:
an administrator cannot say "Sam reaches Sandy on Telegram, Ana reaches
Alfie".

- **Status:** missing
- **Evidence:** runtime on the scratch instance — `sam` was granted both
  `alfie` and `sandy`, and a message on the household bot was answered
  by `alfie` (session `bs-1`, `butler: alfie`) with no way to express
  the other choice.

### F-153-06 — The chat thread pointer is not keyed by Butler, so a second Butler cannot answer the same person on the same transport

`chat_threads` is keyed by (transport, conversation, surface, identity)
(`keystone/runtimedb/chatthreads.go:41`), and a turn that resumes a
session belonging to another Butler is refused
(`keystone/butlerlive/serve.go`, the `TaskButler` check in `task`). On
Telegram a private chat's id is the person's own id, identical across
every bot they talk to, so two Butlers on one transport collide on the
key by construction.

Histories do stay separate, which §7 asks for. What happens instead of
merging is a hard refusal for up to the 30-minute idle window.

- **Status:** partial
- **Evidence:** runtime on the scratch instance — the second bot's
  message on conversation `1001` returned
  `butlerlive: conversation bs-1 belongs to alfie and cannot be
  continued by sandy (ADR-W031 A4)`; the same message on conversation
  `9002` was answered normally by `sandy`.

### F-153-07 — A queued message is still delivered after its binding is revoked

`EnqueueChatOutbound` records `binding_id` and `ChatOutboundAfter` never
joins back to it (`keystone/runtimedb/chatoutbound.go:139`), so
revoking a binding stops the *next inbound message* and does nothing
about anything already queued for that account.

The fix shape is already in the same function for a different reason:
quarantine is re-checked on the read, not only on the write
(`chatoutbound.go:143`), precisely so that already-queued pushes cannot
escape through a restore.

- **Status:** missing
- **Evidence:** runtime on the scratch instance — after
  `POST /api/v1/chat/bindings/1/revoke` → `{"revoked":1}`, the queue
  still offered the pending reply for that binding:
  `GET /api/v1/chat/outbound?after=2&transport=telegram` →
  `[(3, bindingId 1, '1001', 'ok')]`.

### F-153-08 — An undelivered message stops being deliverable after six hours, and nothing ever says so

*Revised after review on this PR: the first version said the row was
swept at six hours. It is not, and the distinction changes the fix.*

Two separate clocks, and only the first is about delivery.

| | |
|---|---|
| `DefaultChatOutboundTTL`, six hours (`keystone/runtimedb/chatoutbound.go:53`) | after this `ChatOutboundAfter` stops returning the row, so no adapter can collect it |
| `ChatOutboundRetention`, seven days (line 47) | only after this does `pruneChatOutbound` delete it, and only on a later enqueue (line 266) |

So the pending state §16 asks to be preserved *is* preserved: the body,
the target, the binding and the timestamps sit in `chat_outbound` for at
least seven days after a message quietly became undeliverable. What is
missing is the second half of the requirement — showing the failure. The
only "not delivered yet" view is `ChatOutboundHeld`, which reports
quiet-hours holds only (line 216). Nothing surfaces an expired
undelivered message, so an adapter that was removed or misconfigured
produces silence, and the record that would explain it is reachable only
by opening the database.

That makes the remediation smaller than "keep the state": the state is
already there for a week, and what it needs is a read.

- **Status:** missing
- **Evidence:** code as cited on `787ad6d` — `pruneChatOutbound` deletes
  on `created_at < now-7d AND (delivered_at != '' OR expires_at < now)`,
  so expiry alone deletes nothing. `grep -rn "expired\|undelivered"`
  across `keystone/` and `cli/` finds no endpoint, CLI command or
  Console view that lists expired outbound messages; `wild chat
  outbound` reads the same undelivered-unexpired cursor an adapter does
  (`cli/cmd/wild/chat.go:156`).

### F-153-09 — Butler addresses cannot be set from the Console or the CLI

`PUT /api/v1/butlers/{name}/address` exists and works
(`keystone/api/api.go:433`, `keystone/api/handlers_butlers.go:768`), and
the Console client has `api.setButlerAddress`
(`console/src/api.ts:1024`). Nothing calls it: `grep -rn
"setButlerAddress" console/src/` returns the definition and no use, and
`wild` has no address subcommand (`cli/cmd/wild/butler.go` offers `ask`,
`sessions`, `status`).

This is the "exists but nothing calls it" shape. The mechanism is
reachable by a hand-written HTTP call with a manage token, and by no
shipped surface — so in practice which Butler answers on chat cannot be
configured by the person who owns the deployment.

- **Status:** missing
- **Evidence:** `grep -rn "setButlerAddress" console/src/` →
  `console/src/api.ts:1024` only; `grep -rn "externalId" console/src/views/`
  → one read-only render, `TabReach.tsx:114`. Runtime: the endpoint
  itself answered correctly when called directly (F-153-02).

### F-153-10 — The Reach tab matches an address label against a person's name, so every row reads "no address linked"

`TabReach.tsx:92` looks up a person's address with
`addresses.find((a) => a.label === m)`, where `m` is a household member
name and `label` is the free-text note for a human ("the gym bot"). The
schema says the label is never matched on
(`0034_butler_assignment.sql:57`). The lookup can only succeed if
somebody happens to have labelled an address with a member's name.

- **Status:** missing
- **Evidence:** `console/src/views/butlers/TabReach.tsx:92` and `:114` on
  `787ad6d`; the address created in the runtime check carried label
  `the household bot`, which no member name can match.

### F-153-11 — The address comes from the credential and cannot come from the request body

`inboundMessage.Butler` is refused outright for an adapter rather than
ignored (`keystone/api/handlers_chat_message.go:296` →
`handlers_butler_named.go:63`), the surface likewise comes from the role
(`handlers_chat_message.go:100`), and `Turn.Address` is the capability
recovered from the principal name (`handlers_butler_named.go:44`).
ADR-W031 A7 holds in the code.

- **Status:** implemented
- **Evidence:** runtime on the scratch instance — a bound message naming
  `sandy` from a concierge credential returned `a chat adapter cannot
  name a butler; the bot a message arrives at is the address (ADR-W031
  A7)`, and one naming surface `admin` returned `this credential serves
  the family surface`. Tests `TestTheAddressDecidesNotTheMessage` and
  `TestChatSurfaceComesFromTheCredential` pass.

### F-153-12 — One Butler may hold many addresses

The unique index is on the address, not on the Butler
(`0034_butler_assignment.sql:73`), so a Butler can answer on Telegram
and Matrix at once, which is §7's "the same Butler may be reachable
through web, Telegram and Slack". Web is a different mechanism — the
Console holder names a Butler rather than arriving at an address
(`handlers_butler_named.go:74`) — and no Slack transport exists.

- **Status:** partial
- **Evidence:** schema and code as cited on `787ad6d`; `GET
  /api/v1/chat/transports` on the scratch instance listed telegram and
  matrix only.

### F-153-13 — Pairing is required, and a display name establishes nothing

A binding is only ever written by redeeming a single-use, expiring code
issued by a manage-holder (`keystone/runtimedb/chatbindings.go:86`), an
unbound account gets one fixed reply that confirms nothing
(`keystone/api/handlers_chat_message.go:46`), refusals are
indistinguishable from each other (`handlers_chat.go:252`), and the
display name is bounded and used for display only
(`handlers_chat.go:270`). §16's unknown-sender rule holds.

- **Status:** implemented
- **Evidence:** runtime on the scratch instance — an unbound account
  received exactly the fixed guidance reply and produced no session row.
  Tests `TestChatUnboundAccountGetsOneReply`,
  `TestChatClaimRefusalsAreIndistinguishable` and
  `TestClaimOnTheWrongBotIsRefusedNotBurned` pass.

### F-153-14 — A permanent routing failure is reported to the person as a transient outage

When no Butler answers at an address — never configured, or revoked —
the API returns 404 `no butler answers there for you`
(`keystone/api/handlers_butler_named.go:141`). The adapter turns every
inbound failure into `I could not reach the house just now. Try again in
a moment.` (`cli/cmd/chat-adapter/adapter.go:281`). Retrying will never
help, and the household has no way to see why the bot went quiet.

- **Status:** missing
- **Evidence:** runtime on the scratch instance — after revoking
  `alfie`'s address, a bound account's message returned `{"error":"no
  butler answers there for you"}`, which is the case the adapter renders
  as a temporary failure.

### F-153-15 — The Console has no per-Butler Connections or Chat surface

The Butler drawer's tabs are Identity, Model, Reach, Tools, Memory,
Sessions (`console/src/views/butlers/ButlerDrawer.tsx:15`). `Chat` and
`Connect chat` are separate top-level pages
(`console/src/App.tsx:146`, `:200`). `Chat` offers issue-a-code and
revoke and no edit (`console/src/views/Chat.tsx:145`, `:295`);
`ConnectChat` installs a package, mints a credential and sets a bot
token. Neither mentions a Butler by name.

The `Chat` bindings table has a column headed `butler` that renders the
*surface* as "Household" or "Operator" (`Chat.tsx:182`, `:268`), which
reads as a per-Butler binding and is not one.

- **Status:** missing
- **Evidence:** files and lines as cited on `787ad6d`; `grep -in butler
  console/src/views/ConnectChat.tsx` finds only the surface type and
  prose.

## Design gaps

**The two-bot-per-transport design is coherent and is not the
baseline's.** ADR-W026 A3 splits chat by *surface* — a household bot and
an operator bot, in two packages, with two roles — and everything
downstream is built on it: the role decides the surface, the capability
name encodes the transport, the queue is per surface, the credential
ceiling is per surface. It is a good boundary and it works. It just
produces one bot identity per surface, and the baseline wants one per
Butler.

Closing that gap is a decision, not a fix, and it has at least two
shapes:

- **Instantiable chat capabilities.** A package installed more than
  once, with a name per instance, so `chat-telegram-family-sandy` is a
  real capability with its own principal, token, bot and address. It
  fits the existing address model exactly — nothing about routing
  changes — and it is a change to how capabilities are installed and
  named, which reaches well beyond chat.
- **Sub-addressing within one bot.** Telegram forum topics, the option
  ADR-W031 records as considered and left open. `butler_addresses`
  already anticipates it — the migration says the external id may be a
  topic id — but no code produces one: `Turn.Address` is always the
  capability, and neither the conversation nor the group ever reaches
  it. Cheaper, and it ties the household's Butler identities to one
  vendor's feature.

Either way, F-153-03 and F-153-04 must be fixed first or the second bot
makes things worse rather than better: today's queue would hand one
Butler's replies to the other's bot the moment a second bot exists.

**Who chooses which Butler answers a person.** Even with one bot per
Butler, a household member with three Butlers has three contacts, and
the baseline's "explicit account-to-Butler binding" could mean either
that or a binding that names a Butler. That choice determines whether
`chat_bindings` gains a Butler column or stays as it is. It is worth
settling before either is built.

## Cross-slice

- **AC08's second half — shared knowledge when switching Butlers —
  fails on memory, not on routing.** Memory is siloed per Butler
  (#151 finding 1). Belongs to [#156](https://github.com/chickenrat17/wildstone-homelab/issues/156).
- **Access lists and admission** decide whether a bound person may talk
  to the Butler at their address (`ResolveButlerFor`); their correctness
  is [#155](https://github.com/chickenrat17/wildstone-homelab/issues/155)'s.
- **Routine ownership.** `household_routines` records a Butler that the
  notification path ignores (F-153-04); whether routines are correctly
  owned in the first place is
  [#154](https://github.com/chickenrat17/wildstone-homelab/issues/154)'s.
- **Quiet hours and quarantine** gate the same queue this slice audited
  and were not exercised here; durable delivery belongs to
  [#158](https://github.com/chickenrat17/wildstone-homelab/issues/158).

## ADR amendments required

- **ADR-W026 A3 — one bot per surface.** Superseded by baseline §7:
  a bot identity belongs to a Butler, not to a surface. The surface
  ceiling it enforces (concierge cannot reach the admin surface) is
  independently right and should survive the amendment; what changes is
  that a surface may carry more than one bot.
- **ADR-W031 A7 — "Settled: how Telegram addresses more than one
  Butler".** The ADR leaves one-bot-per-Butler and one-bot-with-topics
  open as a deployment choice. §7 settles it: three Telegram-enabled
  Butlers require three Telegram bots. A7's binding half — the bot token
  is the address — is unaffected and holds in the code (F-153-11).
