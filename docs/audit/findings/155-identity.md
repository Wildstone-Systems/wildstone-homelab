# Identity and authorization

- **Issue:** #155
- **Status:** complete
- **Audited:** 2026-09-10 against `787ad6d` (`butlers-on-the-live-path`); runtime rerun on the homelab host 2026-09-10 against `d062979`, whose `keystone/` is identical (#165)
- **Acceptance criteria:** AC03, AC06, AC09

Written by the audit slice for
[#155](https://github.com/chickenrat17/wildstone-homelab/issues/155).
**This file belongs to that slice alone** — no other session writes
here, which is what keeps parallel audits from colliding. See
[`../README.md`](../README.md) for how to run one and
[`../gap-report.md`](../gap-report.md) for the register.

`testing/e2e` lints this file's shape. Run
`cd testing/e2e && go test -run TestAuditRegister ./...` before you
commit.

## How this slice was audited

Code evidence is on `787ad6d`; line numbers are that commit's.

Test evidence: `go test ./runtimedb/ ./principal/ ./butlerlive/ ./api/`
was run in this session and passes (19.2s / 2.0s / 2.9s / 65.1s). Named
tests cited below are from that run.

Runtime evidence: obtained from a **scratch instance** — the audited
build run against a throwaway data directory on `127.0.0.1:19441` per
[`../runtime-access.md`](../runtime-access.md), started with
`-household-ingress 127.0.0.1` so the forwarded-identity path could be
exercised. F-155-05, F-155-07 and F-155-09 carry executed evidence, and
the first two came back **stronger** than the code reading had them. The
instance, its data directory and the one scratch harness were deleted
afterwards; the repository is unchanged by them.

In the first two rounds the **household deployment** was not reachable and was
never touched: `systemctl` reports the system was not booted with
systemd, `/var/lib/wildstone` and `/etc/wildstone` do not exist, and
`curl http://127.0.0.1:9441/api/v1/butlers` returns `000`. Nothing was
created on it, so there is no test data there to clean up. What that
costs the audit is in F-155-16, and it is now narrow: the household's
own configuration, not the behaviour of the code.

**Host round (#165), 2026-09-10.** Run on the homelab host against
`d062979` on `audit/155-identity`; its `keystone/` is identical to
`787ad6d`, so every line number in this file still holds. Two runtimes,
kept apart:

- **A scratch instance for everything that writes** — the audited build
  on its own data directory and `127.0.0.1:29441`, pointed at the **real
  Ollama endpoint** (`http://<ollama-host>:11434`, address redacted for
  publication; `qwen3.5:4b`), with the
  real catalog so a family chat adapter could be registered and
  credentialed. That removed both blocks the previous round recorded:
  turns ran on a real model, and the chat path ran end to end — claim,
  redeem, bound messages in direct and group conversations, the outbound
  queue. The directory roster was satisfied by approving `LdapUser` plans
  on the scratch Statefile; no node was joined and nothing was
  materialized (those plans failed at reconcile for want of a node, as
  expected). People, tokens and memory content were synthetic — `jude`,
  `robin`, `mira`, `kit`; canaries `PARTY-CANARY` and `PREF-CANARY`. The
  endpoint was shared with another audit session and the small model
  often hit the 120s client timeout, so turns were repeated and results
  are stated with their counts. To fit that timeout, four of Sandy's
  household tools were narrowed on the scratch instance with the
  product's own control; `recall` was narrowed only where a finding says
  so, to isolate a channel.
- **The household deployment, read-only** — `GET` requests only, for the
  configuration questions in F-155-16. Nothing was written to it: no
  admissions, grants, memory, sessions or restarts.

This round moved F-155-16 from `unverified` to `missing`, corrected
F-155-11 (the preamble carries preferences and procedures, not facts)
while adding a second observed disclosure channel to it, and added
F-155-18 to F-155-20. Everything else stands; findings that gained
executed evidence carry a line marked *Also, at runtime*. The scratch
instance, its data directory, its tokens, the copy of the household
database and the harness scripts were deleted afterwards.

## Requirements

| # | Baseline | Requirement | Status | Evidence |
|---|---|---|---|---|
| R1 | §2 | Invocation carries a trusted initiating-person identifier, assigned by the runtime | partial | Derived from a binding or principal and never from model output (`api/handlers_chat_message.go:209`, `butlerlive/butlerlive.go:239-252`); but the account it is derived from is asserted in the adapter's request body (F-155-10) and two routes derive it two different ways (F-155-15) |
| R2 | §2 | Trusted Butler identifier, not accepted from the message | implemented | The adapter's own credential is the address; naming a Butler in the body is refused (`api/handlers_butler_named.go:33-79`); `TestTheAddressDecidesNotTheMessage` |
| R3 | §2 | Trusted conversation-audience identifier | missing | Audience is `msg.Group`, a boolean in the adapter's request body, and it decides both the gateway and the task audience (F-155-10) |
| R4 | §2 | Trusted task identifier | implemented | `butlerlive/serve.go:65-80`; a conversation cannot change Butler mid-thread (`butlerlive/serve.go:250-268`) |
| R5 | §2 | Trusted delivery-connection identifier | missing | No connection identifier reaches a dispatch at all (F-155-02) |
| R6 | §2 | Model output cannot supply an authority-carrying identifier | implemented | A remembered note's `Owner`/`Author` come from the session, never the tool input (`butlerstore/memory.go:135,159`); `subject` is a topic label and is not read as authority; `"me"` is resolved from the session (`butler/household.go:474-478`) |
| R7 | §10 | Effective authority is the intersection of person ∩ Butler ∩ connection ∩ task/routine | missing | Only the Butler leg is computed. The person is not in the query (F-155-01), the connection check has no callers (F-155-02), and the task/routine leg is present in the query but never reached, because no tool requires a grant (F-155-17) |
| R8 | §10 | The Butler's own grants and narrowings bound a dispatch | partial | Narrowings and administrator disable-controls do, and are wired: `runtimedb/butlerscope.go:245` reached from `butlerstore/authority.go:47` from `butlerlive/serve.go:79`; `TestTheSessionIsScopedToItsButlerBeforeItRuns`. Work-bound **grants** bound nothing — no tool requires one, so the grant branch is never taken (F-155-17) |
| R9 | §10 | A source event cannot supply missing authority | missing | The rule engine dispatches household actions through a gateway with no person and no authorizer (F-155-13) |
| R10 | §10 | Approval binds action, target, parameters, approver and validity period; material change invalidates it | partial | Statefile head binds the plan and a move supersedes it (`api/handlers_plans.go:295-308`); no validity period, and no re-check of the proposer's authority (F-155-14) |
| R11 | §16 | Person→Butler access is decided per person and rechecked at use | implemented | `runtimedb/butlers.go:463-497` — access list, then admission, on every turn; `TestAnUngrantedMemberIsRefusedAtTheAddress` |
| R12 | §16 | Children and guests: the requesting person's authority applies even on an operator Butler | missing | Nothing about the requesting person shapes the toolkit on the admin surface (F-155-03); the child profile cannot be admitted (F-155-04, observed). Observed: a `household`-profile member got `propose_change` on the operator Butler exactly as an operator did (AC03), and this household has two such members on its operator Butler (F-155-16) |
| R13 | §16 | Ambiguous speakers get only configured basic household actions; voice alone establishes nothing | missing | No voice path and no ambiguous-speaker class exists (F-155-12) |
| R14 | §16 | Group audiences retrieve only what is appropriate to all recipients | partial | The household timeline is narrowed (`butler/household.go:490-495`, code); memory is not — observed, `recall` and the memory preamble each carry the asker's private records into a group room (F-155-11), and a thread's history crosses a group/direct change (F-155-18) |
| R15 | §16 | Re-check the destination before sending if audience or membership changed | missing | The reply is rendered and queued at answer time and delivered with no re-check (F-155-09, observed end to end); a thread keeps its history when its audience changes (F-155-18) |
| R16 | §16 | Revocation: pause affected work, end its grants | implemented | `runtimedb/offboarding.go:96-140` — personal work pauses, shared transfers keeping its author, task grants revoked; `TestOffboardingReportsPerStore` |
| R17 | §16 | Revocation: invalidate cached private context and outstanding approvals | partial | Private memory is quarantined and notices stopped (`runtimedb/offboarding.go:176-197`); bearer tokens of linked principals are revoked (F-155-07); queued replies (F-155-09) and proposed plans (F-155-14) survive, and a model round in flight retries after the pause (F-155-20) |
| R18 | §16 | Revocation lands at the next check rather than at a cache expiry | partial | True on the chat path at each slice and each round (binding revoked *and* admission rechecked); not between a model round's retries (F-155-20); the link-generation mechanism designed for this is unwired (F-155-08) and the Console path checks neither (F-155-05) |
| R19 | §18 | Person-specific enforcement for adults, children, guests and operators reaching the same Butler | missing | Enforcement is per Butler and per surface. The only per-person decision is whether the person may open a session at all (F-155-01, F-155-03) |

## Acceptance criteria

| AC | Result | Evidence |
|---|---|---|
| AC03 — person authority follows the request | **fails** | **Observed on the scratch instance, real model.** `mira` — admitted on the `household` profile and on Alfred's access list — asked the operator Butler for an infrastructure change: `propose_change` was dispatched and not rejected (work trace, task 36, `tool.result rejected:false`) and `plan-5` was proposed acting for `mira`. `robin`, on the `operator` profile, got the same dispatch and `plan-6` (task 42). Both are proposals that still need an approver, and both were produced for the same reason: nothing in the decision names the person (F-155-01, F-155-03). The scenario's own subject cannot be built — a `child` record is accepted and its admission refused (F-155-04, observed). The authorized-operator half is not demonstrated as a *separate* authorization: there is no grant to hold (F-155-17), so an operator succeeds for the same reason a household member does. **Live:** two `household`-profile people are on this household's operator Butler's list (F-155-16). |
| AC06 — revocation and cached context | **partial** | **Observed end to end through a registered adapter, real model.** Holds: after `POST /api/v1/butler/members/jude/offboarding`, a new message from `jude` was answered `bound:false` with a fixed introduction and no session; `POST /api/v1/butlers/Sandy/ask` with his token was refused `not admitted`; his work went to `paused-offboarded` (9 tasks) and none was resurrected. **Fails:** a reply rendered from his private note before revocation (outbound seq 24, `bindingId 1`, naming `PARTY-CANARY`) was still offered to the adapter after it (F-155-09, now on the real path); his bearer token still reads `/butlers`, `/plans` and `/butler/members` (F-155-07); and a turn in flight when he was offboarded made another provider call 56 seconds later, which the task's own recheck says must not happen (F-155-20). Not executed: a plan he proposed stays approvable (F-155-14, code). "Scheduled runs" remain untestable — there is no scheduled Butler execution (F-155-13, #154). |
| AC09 — group and voice boundaries | **fails** | **Observed, real model, 3 of 3 each.** In a group conversation `jude`'s own private records reach the room by **two channels**: `recall` returns his private *facts* (tasks 23, 29, 30: `rejected:false`, 463 bytes; replies queued to `target: room-kitchen` quoting `PARTY-CANARY`), and with `recall` narrowed the **memory preamble** still carries his private *preferences* with no tool call (tasks 32–34, `toolCalls: []`, *"You prefer oat milk."* into three fresh rooms). `robin`, in the same rooms, got neither — the reader scope holds; the audience does not. So **F-156-08's `recall` is part of AC09's failure, not the whole of it**: the preamble needs no tool call, and a conversation's history crosses a group/direct change (F-155-18). The group audience is whatever the adapter says on each message (F-155-10, observed). An unrecognized account is refused with a fixed introduction and no capability (F-155-12, observed) — the fail-closed half holds; there is no voice path and no ambiguous-speaker class. |

## Findings

### F-155-01 — the §10 intersection never names the initiating person

- **Current status:** implemented
- **Current evidence:** `keystone/api/handlers_chat_message.go` and `keystone/api/handlers_butler_named.go`; 1332d75; `TestARevokedPersonsMessageNeverReachesTheProvider`.
- **Audit status:** missing
- **Audit evidence:** `keystone/butler/authority.go:44-59` (`ToolAuthorityQuery`: Profile, Tool, Structural, GrantRequired, TaskID, RoutineName) and `keystone/runtimedb/butlertools.go:282-296` (`ToolQuery`, identical) — neither carries a person. `keystone/butlerlive/serve.go:78-80` binds the session's authorizer from the Butler's name and the task id only. The person does reach the session, as `Runner.Actor` (`butlerlive/serve.go:101`), and `keystone/butler/runner.go:272-273` is its only consumer: it stamps trace rows.
- **Also, at runtime:** 2026-09-10, scratch instance, real model — `POST /api/v1/butlers/Alfred/ask` as `mira` (`household` profile) and as `robin` (`operator`), each asking for the same infrastructure change. Both dispatched `propose_change` with `rejected:false` (work trace, tasks 36 and 42) and each produced a proposed plan (`plan-5` acting for `mira`, `plan-6` for `robin`). The one difference between the two requests was the person, and it changed nothing.

The person's identity is captured correctly at the edge and is used for
three things — which Butlers they may open a session with, which memory
records they may read, and who the work trace is attributed to. It is
not used for the fourth: what the session may *do*. Once a session is
open, every dispatch resolves against the Butler and the task and
nothing else, so two people talking to the same Butler have identical
authority by construction.

This is the finding §13 puts ahead of everything that increases tool
reach, and AC03 fails on it.

### F-155-02 — the connection-scope leg is complete and has no callers

- **Current status:** implemented
- **Current evidence:** `keystone/api/handlers_chat_message.go` and `keystone/api/handlers_butler_named.go`; 1332d75; `TestARevokedPersonsMessageNeverReachesTheProvider`.
- **Audit status:** missing
- **Audit evidence:** `keystone/runtimedb/butlerscope.go:139-169` — `ButlerMayReadConnection`, whose own comment calls it "the check every integration read goes through". `grep -rn "ButlerMayReadConnection" --include=*.go .` returns the definition and five assertions in `runtimedb/butlerscope_test.go:293-316`. No caller outside the defining package.

This is the same shape as the Butler memory subsystem in #151: a correct
mechanism, tested, wired to nothing. `GrantButlerConnection` and
`ButlerConnections` *are* called — from the Console handlers and from
`ButlerScope` — so the Console shows and edits a scope that no dispatch
consults. §10 names the service connection's scope as one of the four
legs of effective authority; it is absent from the answer.

### F-155-03 — the admin surface builds one toolset for everybody

- **Current status:** open
- **Current evidence:** `keystone/api/handlers_butlers.go:listButlers` still computes inventory and tool counts without the acting Person; the Console view needs a Person-scoped toolset contract.
- **Current owner:** chickenrat17
- **Tracking:** #316
- **Audit status:** partial
- **Audit evidence:** `keystone/cmd/keystone/main.go:1102-1136`. The family branch builds a gateway *per session, from the member*: `butlerstore.MemorySeams(db, b.Name, t.Member, log)`, then `NewHouseholdGroupGateway(t.Member, mine)` or `NewHouseholdGateway(t.Member, mine)`. The admin branch (`:1134-1135`) is `perSession := *bgw` — a copy of one shared gateway, with no member in it. `butler/gateway.go:253` then authorizes each dispatch against the constant `ProfileOperator`.
- **Also, at runtime:** the AC03 pair in F-155-01 — a `household`-profile member and an `operator`-profile member on the admin surface received the same toolkit and the same dispatch.

So the family surface does narrow by person (memory scope) and by
audience (the group gateway), and the operator surface narrows by
neither. The `partial` is that per-person narrowing exists on one of the
two surfaces; AC03 is about the other one.

### F-155-04 — the child classification is a record that cannot be used

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/butleridentity.go` still needs the complete child-as-subject and no-authentication migration gate tracked in #284.
- **Current owner:** chickenrat17
- **Tracking:** #284
- **Audit status:** missing
- **Audit evidence:** `keystone/runtimedb/butleridentity.go:37-40` (`ProfileChild` "is stored so guardian relationships can be recorded, and cannot be admitted"), `:96-97` (a child record requires a guardian), `:136-137` (`AdmitButlerMember` refuses a child outright). `runtimedb/butlers.go:171-177` accepts the same three profiles for a *Butler*, and pairs `operator` with the admin surface.
- **Also, at runtime:** 2026-09-10, scratch instance on `d062979`, whose `keystone/` is identical to `787ad6d` — `POST /api/v1/butler/members` with `{"member":"kit","profile":"child","guardian":"robin"}` returned `{"recorded":"kit"}`, and `POST /api/v1/butler/members/kit/admit` returned `runtimedb: the child surface is not enabled, so "kit" cannot be admitted (ADR-W030 Part C)`. The same two calls admitted `jude` on `household` and `robin` on `operator`.

A child can be recorded with a guardian and can never be admitted, so
they cannot talk to a Butler at all. That is fail-closed and is the
right refusal for a surface that does not exist. What is missing is the
capability the owner described — a person scoped to less shared memory
and fewer tool calls — and, more immediately, that a member's profile
(`household` vs `operator`) is read in exactly one place in the whole
codebase: `runtimedb/butlerhandoff.go:171`, which refuses a handoff to
the admin surface for a non-operator. It bounds no dispatch.

An unscoped person today gets: every Butler they were granted, and the
full ceiling of each.

**Live in this household** (host round, #165): two `household`-profile
members are on the operator Butler's access list — see F-155-16.

### F-155-05 — Console forward-auth trusts the header the review route validates

- **Current status:** implemented
- **Current evidence:** `keystone/api/auth_console.go`; 2ed0c0f; `TestTheConsoleForwardAuthRefusesEverythingElse`.
- **Audit status:** partial
- **Audit evidence:** runtime, 2026-09-10, scratch instance on `ad29662` — started the audited build with `-household-ingress 127.0.0.1`, offboarded member `sam` through `POST /api/v1/butler/members/sam/offboarding` (reported per store; `admitted` went to `false`, `offboardedAt` set, generation bumped to 3), then presented **no bearer token** and only headers. `Remote-User: sam` + `Remote-Groups: console_users` → `200` on `/api/v1/butlers` and on `/api/v1/butler/members`; `GET /api/v1/whoami` returned `{"name":"sam","role":"admin","actingFor":"sam"}`. **`Remote-User: ghost` — a name with no principal row, no member record and no link — also returned `role: admin`.** Controls behaved: no `Remote-Groups` → `401`, `console_users_ro` → `401`.
- **Also, in code:** `keystone/api/auth_console.go:65-80` — peer is the trusted ingress, `Remote-User` is non-empty, `Remote-Groups` contains `console_users`, then `principal.Identity{Name: user, Role: RoleAdmin, ActingFor: user}`. Compare `keystone/api/handlers_household_review.go:278-291`, which takes the *same* header from the *same* proxy and additionally resolves it: `s.DB.ResolveSurfaceIdentity(runtimedb.LinkTrustedIngress, asserted)`, with the comment "equal names are not proof of identity (ADR-W030 A2.1)". Tests: `TestTheConsoleGroupIsWhatGrantsTheConsole`, `TestABadTokenDoesNotFallThroughToTheHeaders`.

Two paths behind one proxy disagree about whether an asserted name is an
identity. The consequence for AC06 is concrete: `OffboardMember` sets
`admitted = 0`, bumps the generation and revokes every identity link
(`runtimedb/offboarding.go:56-77`), and `consoleForwardAuth` consults
none of those. An offboarded person still holding `console_users`
membership still authenticates as `RoleAdmin`.

**Revised after runtime evidence.** The first version of this finding
said an *offboarded person* stays an administrator, which is true and is
not the strongest form. The `ghost` result above is: the asserted name is
never checked against anything at all, so on this path the household's
person model is not weakened by a stale record — it is not consulted. What
authenticates is the group claim, and the name beside it is carried
through to `Identity.Name` and `ActingFor` unexamined, which is what every
audit row, Butler grant and plan approval then records.

That is exactly what ADR-W032 designed — membership IS the permission, the
identity is ephemeral, no principal row is created — and within the
owner's trust model (Console access means admin) it grants nobody anything
they could not already take. It matters here because it is the point at
which "the requesting person's authority" stops being a property of a
person: combined with F-155-06, a header naming anyone reaches every
Butler without passing an access list or an admission check.

ADR-W032 makes this deliberate — "membership IS the permission", and the
identity is ephemeral so there is only one place to revoke. That is
defensible; what is not is that the household's own offboarding action
reports success across eight stores while leaving this one open, and
says nothing about it. Whether the fix is to consult the link table or
to make offboarding drive the directory is a decision, not a defect —
see *Design gaps*.

### F-155-06 — the owner path skips the access list and the admission recheck

- **Current status:** open
- **Current evidence:** `keystone/api/handlers_butler_named.go:actingAsOwner` still exempts a manage-verb credential from Agent access checks; review this against Person Owner admission.
- **Current owner:** chickenrat17
- **Tracking:** #316
- **Audit status:** partial
- **Audit evidence:** `keystone/api/handlers_butler_named.go:94-96` — `actingAsOwner` is `principal.Allowed(id.Role, principal.VerbManage)`, true for every `RoleAdmin`, which is every Console caller (F-155-05). It sets `Turn.AsOwner`, and `keystone/butlerlive/butlerlive.go:246-247,290-300` then takes `ownedButler`, which resolves the Butler by name and checks only that it is live and reachable — deliberately not `ResolveButlerFor`, so neither the access list nor `GetButlerMember(...).Active()` runs. Tests: `TestTheOwnerReachesAButlerWithoutBeingOnItsAccessList`, `TestOwnerIsStillRefusedByTheThingsThatAreNotPermissions`.

The reasoning in the code is sound on its own terms: a manage-holder can
add themselves to any access list in one call, so the exemption removes
a step rather than granting anything. It is recorded because it means
"the requesting person's authority applies" is enforced for chat members
and, for the caller class the household actually uses day to day, not
computed at all. `TestAnAddressedMessageNeverTakesTheOwnerPath` confirms
the exemption cannot be reached from chat, which is the half that
matters most.

### F-155-07 — offboarding leaves the person's API tokens live

- **Current status:** implemented
- **Current evidence:** Merged in `494f5b8d14038552bd26f78b2803b098f9e695b5` (#556). `keystone/runtimedb/offboarding.go` collects the person's live `api-principal` links before the denial and calls `RevokeTokensFor` for each afterwards (reported as the `api-tokens` store); tokens of unlinked principals and of links revoked earlier are left alone. `TestOffboardingRevokesLinkedAPITokens`, `TestOffboardingRevokesLinkedPrincipalToken`.
- **Current owner:** chickenrat17
- **Tracking:** #316
- **Audit status:** partial
- **Audit evidence:** runtime, 2026-09-10, scratch instance on `ad29662` — created principal `sam` (role `operator`), minted it a token, admitted `sam` as a Butler member, then offboarded them through `POST /api/v1/butler/members/sam/offboarding`. The endpoint reported success per store and the member record went to `admitted: false` with `offboardedAt` set. Re-presenting **the same bearer token** afterwards: `GET /api/v1/butlers` → `200`, `GET /api/v1/plans` → `200`, `GET /api/v1/butler/members` → `200`. On the `approve` verb, a three-way control — offboarded `sam` → `400`, live `admin` → `400` (identical, both past RBAC and failing only on the absent plan), a `butler`-role principal which does not hold `approve` → `403`. An offboarded operator is indistinguishable from a live administrator at the RBAC gate.
- **Also, in code:** `keystone/runtimedb/offboarding.go:42-232` — eight stores: admission, links, chat bindings, tasks, grants, connections, notices, private memory, credentials. Principals and tokens are not among them. `RevokeTokensFor` exists (`runtimedb/householdevents.go:165-169`) and its only caller is node join-token cleanup (`api/handlers_household.go:206,217,221`). `principal.Authenticate` (`principal/principal.go:83-108`) checks the token's revocation and expiry and the principal's `Disabled` flag — never `butler_members`.
- **Also, at runtime (host round):** reproduced after offboarding through the chat path — `jude`'s bearer token (a `butler`-role principal) still returned `200` on `GET /api/v1/butlers`, `GET /api/v1/plans` and `GET /api/v1/butler/members`, while `POST /api/v1/butlers/Sandy/ask` with it was refused `runtimedb: this identity is not admitted to the Butler`.

An offboarded person holding a bearer token keeps authenticating. What
they can then do depends on their role: at `operator` they retain
`read`, `propose` and **`approve`** across the public plane — the
403/400 contrast above is that retention demonstrated rather than
inferred — and are denied at the Butler, because `ResolveButlerFor`
rechecks admission. At `admin` they retain everything including the
owner path in F-155-06.

`approve` is the one that matters. §16 requires revocation to invalidate
outstanding approvals; here the revoked person can still *grant* them.

The store-by-store report this endpoint returns is what makes the gap
worth flagging as more than a missing line: it tells the operator
exactly which stores were settled, and a reader has no reason to suspect
a ninth.

### F-155-08 — the A2.1 link-generation check is wired for one of its three link types

- **Current status:** open
- **Current evidence:** The live link-generation paths need a three-type authorization regression, beyond the original single-link check.
- **Current owner:** chickenrat17
- **Tracking:** #316
- **Audit status:** missing
- **Audit evidence:** `keystone/runtimedb/butleridentity.go:354-386` — `ResolveSurfaceIdentity`, "the call A2.1 means by *check current membership and link generation at every retrieval and dispatch*… meant to be made every time, not cached". Its only caller outside `runtimedb` is `api/handlers_household_review.go:286`, with `LinkTrustedIngress`. `ResolveChatBinding` (`:388-391`), the convenience wrapper for the chat case, has **no** callers outside `runtimedb` — only `runtimedb/butleridentity_test.go:74,98,110`. `LinkAPIPrincipal` is never resolved anywhere.

The chat path instead re-derives the person from `chat_bindings.identity`
(`api/handlers_chat_message.go:209`), and revocation happens to land
because `LookupChatBinding` filters `revoked_at = ''`
(`runtimedb/chatbindings.go:181`) and `ResolveButlerFor` rechecks
`Active()`. So the *outcome* is right on that path and the designed
mechanism is not the reason for it. The generation counter — the part
that makes re-admission a new decision rather than an undo
(`butleridentity.go:110-114`) — is never consulted on any live path.

Marked `missing` rather than `implemented` per the README: the mechanism
exists, its callers are tests, and one live route uses a third of it.

### F-155-09 — a queued reply is delivered after the person is revoked

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/offboarding.go`; 71df923; `TestStoppedDeliveryReportsFailureWithoutDisclosingThePayload`.
- **Audit status:** missing
- **Audit evidence:** runtime, 2026-09-10 — a scratch harness against a real `runtimedb` built by the repo's own migrations, driving the shipped calls in the order the live path uses them — `InsertChatClaimCode` → `RedeemChatClaimCode` (binding id 1, identity `jude`, family surface) → `EnqueueChatOutbound` of a reply carrying a canary body and `BindingID: 1`. Before revocation `ChatOutboundAfter` offered 1 message. Then `RevokeChatBinding(1)` — the same call `OffboardMember` makes. After it: `LookupChatBinding` → `runtimedb: not found`, so a *new* inbound message is refused; and `ChatOutboundAfter` → **still 1 message**, `kind=reply target=tg-jude-1 bindingId=1 body="PRIVATE-CONTEXT-CANARY"`. The inbound door closes and the outbound queue does not. Harness deleted after the run.
- **Also, in code:** `keystone/api/handlers_chat_message.go:413-424` — `queueChatReply` enqueues the rendered body with a `BindingID`. `keystone/runtimedb/chatoutbound.go:139-172` — the read filters on surface, sequence, delivery, expiry, hold, and the restore quarantine. It does not join `chat_bindings` and does not look at `revoked_at`. `RevokeChatBinding` (called by offboarding, `runtimedb/offboarding.go:79-95`) marks the binding and nothing else.
- **Also, at runtime (host round), end to end:** scratch instance with a registered, credentialed family adapter and a real model. `jude` asked over `POST /api/v1/chat/messages`; the answer, rendered from his private note, was queued as outbound seq 24 (`bindingId 1`, naming `PARTY-CANARY`). Then `POST /api/v1/butler/members/jude/offboarding` (`chat-bindings: 1`). Polling `GET /api/v1/chat/outbound` as the adapter afterwards: seq 24 still offered, undelivered. A new message from the same account was answered `bound:false`. Same result as the store-level harness above, on the path a real bot uses.

The comment at `chatoutbound.go:143-153` shows the shape was reasoned
about for one case — restore quarantine is checked on the read precisely
because gating the write leaves already-queued rows deliverable — and
revocation is the same case with a different trigger. §16's "check the
destination again before sending if audience or membership has changed"
has no implementation on this path; the destination is fixed when the
answer is rendered.

The window is small but it is exactly AC06's: private context is
retrieved, then the grant is revoked, then the disclosure happens.

### F-155-10 — the conversation audience is a field the adapter fills in

- **Current status:** implemented
- **Current evidence:** `keystone/api/handlers_chat_message.go`; 956cdc1; `TestGroupDestinationIsRefusedForARecognisedPerson`.
- **Audit status:** partial
- **Audit evidence:** `keystone/api/handlers_chat_message.go:222` (`if msg.Group`) and `:301` (`Group: msg.Group` into the turn). It decides the gateway (`cmd/keystone/main.go:1123-1126`), the system-prompt note (`handlers_chat_message.go:230`), the task audience (`butlerlive/serve.go:292-295`) and a memory record's source kind (`butler/memory.go:451-454`). `msg` is the decoded request body.
- **Also, at runtime:** 2026-09-10, scratch instance — a registered family adapter posting `POST /api/v1/chat/messages` for bound member `jude`, `conversation: room-kitchen` throughout. With `group: true` the tasks were recorded with audience `shared` (tasks 23–27); with only `group: false` changed, task 28 was recorded `private` and its reply queued to the same room (`target: room-kitchen`). Nothing corroborated either claim.

Set against the care taken next to it, this stands out: the same handler
refuses to let an adapter name a Butler (`handlers_butler_named.go:64-69`,
"a field an adapter fills in is a field a compromised adapter can fill
in") and pins its surface to its credential
(`handlers_chat_message.go:101-105`). The audience is the third
authority-carrying identifier in the same request and it is taken as
given. An adapter reporting `group: false` for a room gets the direct
gateway and the private audience.

`partial` because the transport genuinely knows this and Keystone
genuinely does not — the fix is corroboration or a per-conversation
record, not a check. It is the same trust the account field rests on
(R1), and the account at least has a stored binding behind it.

### F-155-11 — group narrowing covers the timeline and not memory

- **Current status:** implemented
- **Current evidence:** `keystone/api/handlers_chat_message.go`; 956cdc1; `TestGroupDestinationIsRefusedForARecognisedPerson`.
- **Audit status:** partial
- **Audit evidence:** narrowed: `keystone/butler/household.go:490-495` passes `GroupSafe: gw.group` into the events seam, honoured at `api/handlers_butler_household.go:352,365`. Not narrowed: `keystone/butlerstore/memory.go:62-121` — `MemorySeams(db, butlerName, member, log)` takes no group parameter, and both `Preamble` and `Recall` query with `Reader: member`. `keystone/butler/memory.go:297-306` renders that preamble into the system prompt unconditionally, via `butlerlive/serve.go:198-203`. The group flag reaches memory only at `butler/memory.go:451-454`, where it sets a record's *source* label on write.
- **Also, at runtime (host round):** scratch instance, real model, registered family adapter, `jude` bound. **`recall` in a room:** asked in `room-kitchen` with `group: true`, 3 of 3 turns dispatched `recall` (`rejected:false`, 463 bytes — work trace, tasks 23 and the two repeats) and queued replies to `target: room-kitchen` quoting his private `PARTY-CANARY` fact. **The preamble in a room:** with `recall` narrowed and a fresh conversation id each time, 3 of 3 group turns answered *"You prefer oat milk."* — his private `PREF-CANARY` preference — with **no tool call** (tasks 32–34, `model.answer {"toolCalls":[]}`). **Control:** `robin` in the same rooms got neither (tasks 24, 35). With `recall` narrowed, the fact did not surface in three group turns (session 6, ordinals 6–11).

So in a room, the asker's private preferences and procedures are injected
into the system prompt before the model speaks, and `recall` will return
more of the same on request. Meanwhile `butler/household.go:666-686`
tells the model: *"Your lookups are already narrowed for this: you can
see the household's own facts, its devices, and the person asking —
another member's activity is not available to you here at all."* That
sentence is true of the timeline and not of memory, and it is the
material that reaches the model that decides the disclosure, not the
sentence about it.

**Revised after runtime evidence.** The first version said the preamble
carries the asker's preferences *and facts*. It carries preferences and
procedures only (`keystone/butlerstore/memory.go:37`, `preambleKinds`);
facts reach a room through `recall`. Both channels were then observed,
and they differ in kind. `recall` needs the model to ask for it — it is
the channel F-156-08 demonstrated. The preamble does not: it is in the
system prompt before the model reads the message, so no tool narrowing,
and no model choosing not to call a tool, closes it.

The first version also called the disclosure bounded because it is the
asker's own material in a room they are in. That still bounds it — the
`robin` control shows another member's records do not reach the room —
and it stays `partial` for that reason and because the timeline
narrowing exists in code (`api/handlers_butler_household.go:352,365`;
not exercised here, since this round narrowed that tool to fit the
model's timeout). But the replies were addressed to the room, and the
other people in the room are exactly who §16 is about. A person who told
the Butler privately what they are planning for someone can have it
repeated in the chat that person is in.

Cross-slice: the audience vocabulary this would need is #156's
finding 2.

### F-155-12 — no ambiguous-speaker class, and no voice path to need one

- **Current status:** superseded
- **Current evidence:** No voice/ambiguous-speaker transport is shipped; any later voice ingress must establish a Person before admission.
- **Superseded by:** docs/agent-system-design.md §4 Person identity and §8 supported surfaces
- **Audit status:** missing
- **Audit evidence:** `grep -rn "voice\|Voice" --include=*.go keystone/` returns nothing outside tests. Every person identifier on a live path arrives from a chat binding (`api/handlers_chat_message.go:209`), a bearer principal (`principal/principal.go:83`), or the ingress header (`api/auth_console.go:71`). There is no representation of a speaker who is *probably* someone.
- **Also, at runtime (host round):** the fail-closed half, observed — a message to the family bot from an account with no live binding (`jude`'s, after revocation) was answered `{"bound":false,"reason":"no binding for this account on this surface"}` with a fixed introduction (outbound seq 25), and no session or tool call ran.

§16 requires that ambiguous speakers get only configured basic household
actions. There is no such category: a person is bound or they are
refused, which is fail-closed and satisfies AC09's "private retrieval
requires authenticated routing" today. The requirement is unmet and the
risk is latent rather than live — it becomes real the day a voice or
shared-device surface lands, and that surface will need the category to
exist first.

### F-155-13 — a fired rule dispatches household actions with no person and no authorizer

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/routineprincipal.go`; 573fe1c; `TestRoutineCannotUseASecretGrantedOnlyToItsProposer`.
- **Audit status:** missing
- **Audit evidence:** `keystone/domestic/domestic.go:43-60` — `Call` carries Tool, Tier, Params, Rule, Subject, CorrelationID, TriggerType. No actor, no owner, no grant. `:154-186` — `dispatch` checks the tier is `read` or `domestic`, that the capability declares the tool, that the declared tier matches, and that an executor is registered; then it runs. `keystone/automation/act.go` renders the rule's params, claims the action, and calls `Dispatch`. There is no `Authorizer` in the package (`grep -n "Authorizer" automation/*.go` is empty).

§10: "A routine runs under its owner grant; a source event, model
instruction, bot identity, or delegated agent cannot supply missing
authority." A fired rule supplies its own. The tier ceiling is a real
and well-placed containment — an infrastructure change cannot be reached
this way at all — but it is a ceiling on the *action class*, not an
intersection with anybody's authority.

Note that the Butler's own `do_household_action` reaches the same
gateway *through* the tool authorizer (`butler/household.go:444`), so
the two entry paths onto identical executors enforce different things.

Cross-slice: routine authoring and execution is #154. Recorded here
because the missing leg is authorization, not scheduling.

### F-155-14 — outstanding approvals survive the revocation of who asked for them

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/ownerreview.go`; 89fccf6; `TestRevokingAPersonInvalidatesPendingApprovalsButNotApprovedRoutines`.
- **Audit status:** partial
- **Audit evidence:** `keystone/api/handlers_plans.go:295-308` — a plan is approvable only against the exact Statefile head it was computed from, and a move supersedes it. That is §10's "material changes invalidate it", and it works. There is no expiry on a proposed plan, and no check that the proposer still holds authority. `runtimedb/offboarding.go:42-232` touches no plans; `grep` for a plan-state transition in that file returns nothing.
- **Also, at runtime:** 2026-09-10, scratch instance — two plans proposed back to back; after `plan-1` was approved, `POST /api/v1/plans/plan-2/approve` returned `statefile changed since proposal; plan superseded`. The material-change half is observed working. The revocation half was not exercised here.

§10 requires approval to bind a validity period, and §16 requires
revocation to invalidate outstanding approvals. A plan proposed by a
person — including one proposed by a Butler session *acting for* them —
stays `proposed` and approvable after they are offboarded. An
administrator approving it is doing so consciously, which is why this is
`partial` rather than `missing`, but nothing marks the plan as belonging
to someone who no longer has authority.

### F-155-15 — two derivations of "who is asking" in the same server

- **Current status:** open
- **Current evidence:** `keystone/api/api.go:authenticate` and chat Person claim paths still need a cross-surface canonical-identity regression.
- **Current owner:** chickenrat17
- **Tracking:** #316
- **Audit status:** partial
- **Audit evidence:** `keystone/api/handlers_butler_tasks.go:44-49` — `actingMember(id)` prefers `id.ActingFor`, falling back to `id.Name`. It is what the identity, authority, task and offboarding handlers use. But `keystone/api/handlers_butler.go:199` passes `id.Name` into `askNamedButler` as the member, and `handlers_butler_named.go` `askOneButler` sets `Member: id.Name` directly.

For a Console caller the two agree — `consoleForwardAuth` sets `Name`
and `ActingFor` to the same string (`api/auth_console.go:79`). They
diverge for a bearer token minted with an acting identity
(`principal/principal.go:57-79`), where the Butler session runs as the
token's principal while the task and grant handlers attribute the same
person's work to whoever it acts for. `ActingFor` is described as "the
acting-identity field (ADR-W001 foundation)" and is the value stamped on
every audit row (`api/api.go:539`).

Low severity today and cheap to get wrong later: it is the identifier
§2 says the runtime assigns, and the runtime assigns it two ways.

### F-155-16 — the household's own configuration: the latent conditions are live

- **Current status:** open
- **Current evidence:** The 2026-09-10 deployment observation cannot establish the current live household state; a read-only rollout check is required.
- **Current owner:** chickenrat17
- **Tracking:** #316
- **Audit status:** missing
- **Audit evidence:** runtime, 2026-09-10, **read-only against the household deployment** from the homelab host (`keystone.service`, `127.0.0.1:9441`), `GET` requests only. `GET /api/v1/butler/members` → `admin` (`operator`, active), `<member-a>` (`household`, active), `<owner>` (`household`, active), `Admin` (`household`, offboarded). `GET /api/v1/butlers/Alfred/scope` → `people: ["admin","<member-a>","<owner>"]`, `modelKind: cloud`; `GET /api/v1/butlers` → Alfred on the `admin` surface, `operator` profile, `toolsAllowed 11/11`, `leavesTheHouse: true`; `GET /api/v1/butlers/Alfred/tools` → the eleven operator tools, `propose_change` among them. `GET /api/v1/butlers/Sandy/scope` → the same three people. `GET /api/v1/resources` → `LdapUser/<member-a>` and `LdapUser/<owner>` both declare `groups: [console_users, media_users]`. `GET /api/v1/chat/bindings` → `<owner>` bound on `family`/telegram and `admin` on `admin`/telegram, neither revoked; `GET /api/v1/chat/transports` → both Telegram adapters installed, credentialed and online. `GET /api/v1/butler/tools?profile=household` and `?profile=operator` → one control row each, no narrowings.

Previously titled *"what is left unverified is the household's own
configuration"* and marked `unverified`. **Changed by the host round
(#165):** the four checks this finding listed were run, read-only, and
each one came back as the live form of a code finding.

- **F-155-01, F-155-03 and F-155-04 are live.** Two `household`-profile
  people are on Alfred's access list. Alfred is the operator Butler —
  eleven tools including `propose_change`, on a model whose context
  leaves the house. No leg of a dispatch names the person (F-155-01),
  the admin surface builds one gateway for everybody (F-155-03), and the
  profile bounds no dispatch (F-155-04), so either of them in a session
  with Alfred holds the full operator ceiling. The scratch round shows
  the same thing as behaviour (AC03, above).
- **F-155-05 and F-155-06 reach the same two people.** Both are in
  `console_users`, which is the whole of Console authority: each signs
  in as `RoleAdmin` (F-155-05) and through the owner path reaches every
  live Butler without an access-list or admission check (F-155-06).
- **The chat surface is in use.** A `household`-profile member has a
  live family binding on a live bot, so the group disclosure
  demonstrated on the scratch instance (F-155-11) has a real path here
  as soon as that person talks to the bot in a group. Whether any group
  conversation exists was not read; the bindings endpoint does not say.

**How urgent that is.** Under the owner's stated trust model — Console
access means admin, and the people who are not admins are children —
both of these people are adults the household already treats as
administrators, and both hold the Console anyway. So the live condition
is not that someone untrusted holds operator power. It is that the
`household`/`operator` distinction the household *recorded* is enforced
nowhere, and will not start to mean anything when a less-trusted person
is added. That makes F-155-01/F-155-03 a remediation to do before
anyone is added on a narrower footing, not an emergency today. The
item with a live path to harm is F-155-11 (with F-156-08), through the
bound family bot.

**Still unverified, and narrow.** Console forward-auth was not driven on
the household deployment: its trusted ingress is the identity proxy's
container address, not `127.0.0.1`, so a header sent from the host
would test nothing. F-155-05's behaviour is established on the scratch
instance; what the live proxy forwards for these two people would need
a sign-in through the real Console. Nothing was written to the
household deployment in establishing any of this.

### F-155-17 — no tool requires a grant, so the grant branch is never taken

- **Current status:** implemented
- **Current evidence:** `keystone/api/handlers_chat_message.go` and `keystone/api/handlers_butler_named.go`; 1332d75; `TestARevokedPersonsMessageNeverReachesTheProvider`.
- **Audit status:** missing
- **Audit evidence:** `grep -rn "GrantRequired: true" --include=*.go keystone/` matches only test files (`butlerstore/authority_test.go:125,163`, `runtimedb/reviewfixes_test.go:167`). Both gateways say so in their own field comments: `keystone/butler/gateway.go:56-62` ("No v1 tool sets it; the field exists so the resolver has something to read when P12's and P13's write paths arrive") and `keystone/butler/household.go:171-172`. `keystone/runtimedb/butlertools.go:342` gates the whole grant step on `q.GrantRequired`, so `liveGrant` (`:365-`) is unreachable from any dispatch.
- **Also, at runtime (host round):** `propose_change` was dispatched for a `household`-profile member and an `operator`-profile member with no grant issued to either (F-155-01's AC03 pair); offboarding `jude` later reported `grants-revoked: 0`.

Raised by `chatgpt-codex-connector[bot]` on
[#161](https://github.com/chickenrat17/wildstone-homelab/pull/161) and
verified. It corrects an overstatement in the first version of this
report, which credited AC03's authorized-operator half to work-bound
grants.

The grant machinery is complete and correct — issuing, scoping to a task
or routine, expiry, revocation, and the neat property that a grant dies
with its task's state rather than needing a sweeper. Nothing consumes
it, because the flag that would route a dispatch into it is `false` on
every registered tool. The Console can issue a grant
(`api/handlers_butler_authority.go:99-`) that cannot change any
dispatch's answer.

This is the third instance of #151's trap in this slice, after F-155-02
and F-155-08, and the field comments make it an intentional one: the
field was added early so that P12/P13 would not have to be retrofitted
through every call site. That is a reasonable thing to have done. It
still means the fourth leg of §10's intersection — the task or routine's
bounded authorization — is inert today, and that AC03's second half has
no mechanism behind it to test.

### F-155-18 — a chat thread is keyed without its audience, so one conversation carries its history across a group/direct change

- **Current status:** superseded
- **Current evidence:** The final design refuses group chat destinations for an admitted Person, so a group/direct shared thread is not a supported continuation path.
- **Superseded by:** docs/agent-system-design.md §8.3 group destinations
- **Audit status:** missing
- **Audit evidence:** `keystone/runtimedb/migrations/0015_chat_threads.sql` — `PRIMARY KEY (transport, conversation, surface, identity)`, no audience; `keystone/runtimedb/chatthreads.go:44-46`; `keystone/api/handlers_chat_message.go:357-384` (`chatThread` continues the session the pointer names and replays its turns as history, checking only `acting_for` and `surface`). Runtime, 2026-09-10, scratch instance: `jude`'s turns in `room-kitchen` with `group: true` and then `group: false` all landed in one session (`butler_session_turns`, session 6, ordinals 0–13). The last — `recall` available, **no tool call** (work trace, task 28: `model.answer {"toolCalls":[]}`) — answered *"Based on what you've told me, there is a surprise party planned for Robin on the 14th arranged by PARTY-CANARY"*, from the replayed history of the group turns before it.

The migration's comment reasons carefully about why surface and identity
are in the key, and both reasons are good. Audience is not in it, and
audience is the thing §16 names: *check the destination again before
sending if audience or membership has changed.* Here the audience changes
mid-thread, nothing resets, and the history — which can already hold
private material recalled earlier (F-155-11) — is replayed to the model
under the new audience.

What was **not** shown: in three group turns with `recall` narrowed and
that same history present, the model did not repeat the note (session 6,
ordinals 6–11). So replayed history is shown here as a channel that group
narrowing does not cover, carrying private content across an audience
change; the model declining to use it three times is the model, not a
control. F-155-10 makes the change free — the audience is whatever the
adapter says on each message.

### F-155-19 — the per-Butler tools screen is computed for nobody, and omits the tools a session actually has

- **Current status:** open
- **Current evidence:** `keystone/api/handlers_butlers.go:listButlers` computes deployment-wide tool counts, not the effective tools for the acting Person's session.
- **Current owner:** chickenrat17
- **Tracking:** #316
- **Audit status:** partial
- **Audit evidence:** `keystone/api/handlers_butlers.go:729` — `butler.NewHouseholdGateway("", s.ButlerHost.householdSeams())`: no member, and the host's seams, which carry no per-session memory. The session gateway is built per member instead (`keystone/cmd/keystone/main.go:1102-1136`, `MemorySeams(db, b.Name, t.Member, log)`). Runtime, 2026-09-10, scratch instance: `GET /api/v1/butlers/Sandy/tools` listed four tools — `describe_capabilities`, `do_household_action`, `get_household_events`, `list_household_actions` — while Sandy's sessions dispatched `recall` (work trace, tasks 23, 29, 30) and wrote three records through `remember`.

The screen answers "what can this Butler do" for no one in particular,
and on the family surface the answer is short by exactly the two tools
that read and write a person's private memory. It is F-155-01 seen from
the Console: there is no person in the question, so there cannot be a
person-specific answer. `partial` because the four it lists are right;
the omission is what makes it wrong.

### F-155-20 — a model round's retries skip the authority recheck, so offboarded work makes another provider call

- **Current status:** implemented
- **Current evidence:** `keystone/api/handlers_chat_message.go` and `keystone/api/handlers_butler_named.go`; 1332d75; `TestARevokedPersonsMessageNeverReachesTheProvider`.
- **Audit status:** partial
- **Audit evidence:** `keystone/butler/runner.go:358` and `:394` recheck authority before each slice and each round; `keystone/butler/runner.go:477-482` — `modelRound` re-calls `modelAttempt` up to `RetryLimit` times with no `Authorize` between tries. `keystone/butlerstore/butlerstore.go:62-94` is `Authorize`, whose comment at `:77-80` says offboarded work "performs no retries, notices, or model calls until an administrator reviews it". Runtime, 2026-09-10, scratch instance: `jude`'s task 39 started at 23:28:45.88; `POST /api/v1/butler/members/jude/offboarding` at 23:28:49.88 moved it to `paused-offboarded`; the work trace then shows `attempt.fenced` and a **second `model.call` at 23:29:45.88** — 56 seconds after the pause — then `wait.opened {"reason":"model-unavailable"}`. The task stayed `paused-offboarded`.

The recheck is where the design puts it, twice, and the retry loop sits
between the two. A stalled provider is exactly when retries happen, and
a revocation is exactly when a person's work should stop taking provider
rounds. On the scratch instance the second call went to a local model;
on a cloud Butler it is another round of an offboarded person's
conversation leaving the house after they were removed.

What is **not** established: both attempts timed out, so what the runner
does with a *successful* late answer after the pause — whether it would
complete the task and queue a reply — was not observed, and neither was
whether the retried system prompt still carried the person's preamble
once their memory was quarantined. `partial` because the slice and round
rechecks hold: no further round ran, no notice was raised (`notices`
empty), and the state was not resurrected.

Cross-slice: #158 owns retries and fencing; the gap is authorization.

## Design gaps

**The trust model and the code's threat model disagree, and the code is
stricter.** The owner's position is that Console access and chat access
both mean admin, and that the people who are not admins are children —
a capability that does not exist yet. Several mechanisms audited here
defend instead against mutually-untrusting adults: per-Butler memory
silos (`runtimedb/butlerscope.go:1-24`), the group gateway withholding
other members' timeline events (`butler/household.go:481-495`),
`ErrNotAdmitted` collapsing five distinct states so a prober learns
nothing (`runtimedb/butleridentity.go:52-58`), and the access list being
a boundary rather than a filter so a person does not learn which other
Butlers exist (`runtimedb/butlers.go:427-433`).

None of that is wrong and none of it should be removed reflexively —
some of it is what makes the *child* capability implementable later. But
it is worth deciding deliberately, because the cost is paid now in
places the household notices (a fact told to Sandy is invisible to
Alfred) for a property nobody has asked for yet.

The findings above are a different matter. Person-authority-follows-the-request
(F-155-01, F-155-03) is required under either threat model: it is what
makes a child scoping mean anything when it is built, and today there is
nowhere to put it.

**Whether offboarding should reach the directory.** F-155-05 and
F-155-07 are the same decision seen twice. `OffboardMember` denies
across eight stores and reports per store; Console access and bearer
tokens are outside all eight. Three coherent answers: consult the link
table in `consoleForwardAuth` and revoke tokens in `OffboardMember`
(makes the runtime authoritative, adds a second place to revoke, which
ADR-W032 A4 deliberately avoided); drive the LDAP group from the
offboarding path (makes the directory authoritative, and makes
offboarding depend on a write that can fail — which
`runtimedb/offboarding.go:34-40` is explicitly built not to do); or
leave it and have the endpoint *say* what it does not cover. The third
is the smallest and the most honest, and it is still a decision.

**The child profile's guardian field.** `RecordButlerMember` requires a
guardian for a child (`runtimedb/butleridentity.go:96-97`) and nothing
reads it. When child scoping is designed, whether the guardian is an
approver, an audience for the child's activity, or only a record is
unsettled — the schema has taken a position and the code has not.

## Cross-slice

- **#154 (routines).** F-155-13's authorization gap sits on the rule
  engine's dispatch path. There is also no scheduled Butler execution at
  all — no `DueRoutines`, no runner — so AC06's "scheduled runs"
  requirement has nothing to test against. `runtimedb/householdroutines.go`
  stores an `author` per routine; nothing rechecks it, because nothing
  runs it.
- **#156 (memory).** F-155-11 needs an audience vocabulary that can say
  "appropriate for this room", which is #156's finding 2 (the
  `private|shared` CHECK constraint). The memory *reader* filter itself
  — whether `RetrieveMemory`'s permission clause is correct — was not
  audited here; only whether a group narrows it, which it does not.
- **#157 (secrets/privacy).** `writeJSON` redacts every response body
  except three carve-outs (`api/api.go:598-618`), and a remembered note
  is redacted on the way in (`butler/memory.go:447`). Not audited here
  beyond noting that neither is person-scoped.
- **#153 (bot routing).** F-155-10's group boolean and the account field
  are both adapter assertions on the same request. Whether an adapter's
  credential can be bound tightly enough to make them trustworthy is a
  bot-routing question.
- **#158 (execution).** F-155-20 sits in `modelRound`'s retry loop: the
  fence moves the attempt epoch before a retry, and authority is not
  rechecked at the same point.

## ADR amendments required

- **ADR-W030 A7/A8** describes the effective-authority resolver as the
  seam every dispatch goes through, and it is — but its query has no
  person and no connection in it (F-155-01, F-155-02). Baseline §10
  names four legs; the ADR's resolver was designed for two. The ADR
  needs to say so, or be amended to the baseline's shape.
- **ADR-W032 A3** — "there is no narrower thing to be on this surface
  yet, and inventing one would ship a screen whose every control answers
  403" — is a reasonable ADR-time judgement that baseline §16 and §18
  now contradict: person-specific enforcement is required for people who
  reach the same surface. The Console is where a child scoping would
  have to be visible.
- **ADR-W030 A2.1** requires membership and link generation to be
  checked "at every retrieval and dispatch". One live route does
  (F-155-08). Either the other routes adopt it or the ADR should say
  which surfaces it governs.
