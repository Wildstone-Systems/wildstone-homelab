# Routine authoring, execution and approval integrity

- **Issue:** #154
- **Status:** complete
- **Audited:** 2026-09-10, against `787ad6d` on `butlers-on-the-live-path`
- **Acceptance criteria:** AC02, AC10, AC16

Written by the audit slice for
[#154](https://github.com/chickenrat17/wildstone-homelab/issues/154).
**This file belongs to that slice alone** — no other session writes
here, which is what keeps parallel audits from colliding. See
[`../README.md`](../README.md) for how to run one and
[`../gap-report.md`](../gap-report.md) for the register.

`testing/e2e` lints this file's shape. Run
`cd testing/e2e && go test -run TestAuditRegister ./...` before you
commit.

## How this slice was evidenced

The live keystone was **not reachable from this session**: there is no
`keystone.service` on this host (`systemctl` reports the machine was
not booted with systemd), `/var/lib/wildstone/keystone` does not exist,
and `http://127.0.0.1:9441/api/v1/butlers` returns no response
(`curl` exit code 000). The runbook in
[`../runtime-access.md`](../runtime-access.md) describes a host this
audit session is not on.

So runtime evidence was taken from a **throwaway local control plane
built from the audited commit** — `go build ./cmd/keystone`, run on a
temp data directory with `-household-timezone UTC` and, for the
approval work, `-household-ingress 127.0.0.1` — which exercises the
real handlers, validators and schema. It is genuine runtime evidence
about the code; it is **not** evidence about the household's
deployment, its Butlers, or its installed capabilities. Anything that
depends on Alfred, Sandy, the live model, or the deployment's installed
packages is recorded `unverified` below and says what would settle it.

The trusted-ingress path **is** reachable from loopback: the flag takes
exact addresses and `127.0.0.1` is one, which is how
`handlers_household_review_test.go:29` configures it. A first pass of
this audit recorded the review redemption as unverified on the mistaken
belief that it could not be exercised locally; it was then driven end
to end and the result is under F-154-19. Doing so is what surfaced
F-154-24.

Both local instances and their data directories were deleted
afterwards. Nothing was written to the household's deployment, because
nothing could be.

## Requirements

§8's contract table, field by field, plus the §10 and §16 clauses the
issue cites.

| # | Baseline | Requirement | Status | Evidence |
|---|---|---|---|---|
| R01 | §8 | Both household and operator Butlers can help create routines | missing | `keystone/butler/household.go:236` registers four household tools; `get_automation_schema`, `dry_run_automation` and `propose_change` exist only on the operator gateway (`keystone/butler/gateway.go:158-170`). F-154-03 |
| R02 | §8 | A routine may be deterministic steps, an agent task, or both | partial | `resource.AutomationSpec` is one trigger and one action (`keystone/resource/automation.go:30-52`); an agent objective exists in `butler_tasks` but nothing creates one. F-154-10, F-154-16 |
| R03 | §8 | Conversational authoring saves the same definition the engine consumes | partial | The Butler authors `AutomationSpec` through `propose_change`, validated by `resource.Validate` and read back by the engine from the Statefile (`keystone/automation/automation.go:363-368`) — one shape. Two other routine records exist that nothing executes. F-154-04, F-154-05 |
| R04 | §8 | Sufficiently specified + within permissions → persist and activate without redundant confirmation | missing | Every Automation is a plan a human approves; `RoleButler` holds read+propose only (`keystone/principal/rbac.go:59-61`), and a butler token got `403` on `POST /api/v1/plans/plan-1/approve`. F-154-11 |
| R05 | §8 | Return routine ID, next run or trigger, responsible Butler, actions, recipients | partial | Proposal response carries plan id, steps, risk and blast radius (runtime, below); no routine id, no next run, no Butler. F-154-01, F-154-09 |
| R06 | §8 Ownership | Stable ID and version | partial | Automation: name plus Statefile git revision. `household_routines` has `seq`+`revision` (`migrations/0028`). Two identity schemes, neither shared. F-154-04 |
| R07 | §8 Ownership | Owner | partial | `metadata.owner` is validated as `household`/`household/<member>`/`capability/<name>` (`keystone/resource/automation.go:221-236`) and never read again at run time — `grep Metadata.Owner keystone/automation/*.go` matches nothing. F-154-08 |
| R08 | §8 Ownership | Responsible Butler | missing | The mechanism exists and is unwired: `household_routines.butler` (`migrations/0034`) with `AssignRoutineButler`/`ResolveRoutineButler` (`keystone/runtimedb/butlerassign.go:37-104`), no caller outside its own tests. `AutomationSpec` — the registry that actually executes — has no such field, and escalation always goes to the legacy single-provider host (`keystone/cmd/keystone/main.go:1208-1232`). F-154-09 |
| R09 | §8 Ownership | Participants | missing | No field in `AutomationSpec` or `household_routines`. F-154-06 |
| R10 | §8 Ownership | Bounded authorization grant | missing | `GrantScopeRoutine` is resolvable (`keystone/runtimedb/butlertools.go:38,380`) and every caller passes an empty routine name. F-154-07 |
| R11 | §8 Trigger | Schedule | missing | `AutomationTrigger` is `eventType`+`subject` only; a `schedule:` key is refused at runtime. F-154-01 |
| R12 | §8 Trigger | Event subscription | implemented | `keystone/automation/automation.go:99-140`, engine loop 420-530; fired/suppressed recorded on the household stream. |
| R13 | §8 Trigger | Conditions | implemented | Five closed kinds evaluated in code (`keystone/automation/automation.go:141-212`). |
| R14 | §8 Trigger | Explicit timezone | partial | One deployment-wide `-household-timezone` (`keystone/cmd/keystone/main.go:77,757`); nothing per routine. F-154-12 |
| R15 | §8 Trigger | Household-time vs traveler-time | missing | No such concept anywhere in the schema or the engine. F-154-12 |
| R16 | §8 Trigger | Catch-up policy | partial | One global `MaxTriggerAge` (1h) plus adopt-on-first-tick (`keystone/automation/automation.go:52-57`); not per routine, not configurable. F-154-13 |
| R17 | §8 Work | Deterministic steps | partial | Exactly one action per rule (`resource.AutomationAction`); no step list. F-154-16 |
| R18 | §8 Work | Agent objective | missing | Escalation replaces the text of one action param and nothing else (`keystone/automation/escalate.go:1-16`, `act.go:145-155`). F-154-16 |
| R19 | §8 Work | Permitted context sources | implemented | `AutomationContext`, bounded to 20 events of one declared type (`keystone/resource/automation.go:87-97`). |
| R20 | §8 Work | Tools | implemented | Action must be a package-declared `agentTool` at the declared tier with a registered executor (`keystone/domestic/domestic.go:154-177`). |
| R21 | §8 Work | Completion criteria | missing | `butler_tasks.completion_criteria` exists and no routine ever creates a task. F-154-10 |
| R22 | §8 Work | Execution limits | partial | Per-call timeout and byte bounds; no per-routine step, runtime or spend budget outside the unused task ledger. F-154-10 |
| R23 | §8 Delivery | Specific Butler bot connection and destination | partial | `ChatSender` is one executor per installed chat capability and resolves the member's own binding (`keystone/domestic/chat.go:24-64`) — a specific connection, bound to no Butler. F-154-09, F-154-14 |
| R24 | §8 Delivery | Audience policy | missing | No audience field on a routine or its action. F-154-14 |
| R25 | §8 Delivery | Behaviour when the person does not respond | partial | Global 4h `ChatQuestionWindow` (`keystone/runtimedb/chatquestions.go:22-25`) and a 12h ntfy reply-token TTL; no per-routine policy and no follow-up. F-154-14 |
| R26 | §8 Reliability | Deduplication | implemented | Deterministic follow-up event ids `<rule>@<seq>:<suffix>`, claim written before dispatch (`keystone/automation/act.go:157-190`). |
| R27 | §8 Reliability | Retries | missing | Deliberate: at-most-once, executors "must not retry on its own" (`keystone/domestic/domestic.go:80-84`). Design note below. |
| R28 | §8 Reliability | Timeout handling | implemented | `Gateway.Timeout`/`DefaultTimeout` bounds every call (`keystone/domestic/domestic.go:91-95`). |
| R29 | §8 Reliability | Partial completion records | missing | Requires steps; there are none. F-154-16 |
| R30 | §8 Reliability | Cancellation | partial | A rule can be disabled between evaluations; there is no cancellation of an action already dispatched. F-154-15 |
| R31 | §8 Reliability | Failure reporting | implemented | `automation.action_failed` with stage and error (`keystone/automation/act.go:137-155`). |
| R32 | §8 Lifecycle | Enabled or paused | implemented | `spec.disabled` skips the rule at load and at evaluation (`keystone/automation/automation.go:99,365`). |
| R33 | §8 Lifecycle | Current permissions checked at each run | missing | The engine consults no authority; `domestic.Call` carries no person and no grant (`keystone/domestic/domestic.go:43-60`). F-154-08 |
| R34 | §8 Lifecycle | Revision history | implemented | Statefile git commit per approved plan (`keystone/api/handlers_plans.go:327-333`); `household_routine_revisions` for the other registry. |
| R35 | §8 Lifecycle | Reassignment | missing | Written and unwired: `AssignRoutineButler` reassigns, refuses a retired Butler, and `ResolveRoutineButler` makes a routine wait rather than substitute a model (`keystone/runtimedb/butlerassign.go:37-104`). Test callers only, and it hangs off the registry with no runner. F-154-09 |
| R36 | §8 | Asking a question must not require a home-lab mutation | partial | A question is `chat-*.send` at tier domestic and mutates nothing; but *creating* the rule that asks it is a Statefile commit gated on human approval. F-154-11, design gap 1 |
| R37 | §10 | A routine runs under its owner grant | missing | Same evidence as R33. F-154-08 |
| R38 | §10 | Approval binds exact action, target, parameters, recipient, approver, validity | partial | Plans pin the exact documents and supersede on a moved Statefile head; household review tokens add member, expiry, single use and revision, verified at runtime including refusal after a material change. No validity period on a Console plan approval, no approval path at all for a Butler-initiated household action, and the review path cannot be entered. F-154-17, F-154-18, F-154-19, F-154-24 |
| R39 | §10 | Narrow standing permissions, never self-granted | missing | No standing-grant mechanism; `grep -r standing --include=*.go` finds only prose and unrelated policy invariants. F-154-20 |
| R40 | §16 | Learned procedures composed of already authorized tools, versioned, tested before replacing | partial | `MemoryProcedure` is a text note injected into the preamble (`keystone/runtimedb/butlermemory.go:44`, `keystone/butlerstore/memory.go:34-37`); the versioned-and-tested half is real for Automations (`dry_run_automation`, revisions). F-154-21 |
| R41 | §16 | Executable code, plugins, credentials, wider grants → administrative review | implemented | Everything structural is a plan; `RoleButler` cannot approve one (runtime `403`), and the household review path refuses any plan that is not purely Automations (`keystone/api/handlers_household_review.go:160-186`). F-154-22 |

## Acceptance criteria

| AC | Result | Evidence |
|---|---|---|
| AC02 Routines on both profiles | missing | Three independent blockers: no scheduled trigger exists at all (F-154-01), no capability on this repository declares a mailbox read or an email send — the only `agentTools` in `packages/` are `notifications.notify`, `chat-telegram-family.send` and `chat-matrix-family.send` (F-154-02), and the household surface has no authoring tool (F-154-03). A briefing routine cannot be persisted on either profile, so "neither acquires ungranted send or delete privileges" is true only because neither can acquire anything. |
| AC10 Approval integrity | partial | Where an approval exists, its integrity holds and is now verified at runtime: a token bound to one member, single use, and refused after the routine it pinned moved to a new revision (F-154-19). Three gaps against the criterion. The household review path cannot issue a token for any real plan (F-154-24). Nothing binds a Butler-initiated household action — `do_household_action` dispatches immediately with no approval record, so there is nothing to invalidate (F-154-17). And there are no narrow standing grants, so the criterion's "avoids redundant confirmation" half has no mechanism (F-154-20). |
| AC16 Package integrity and learned skills | partial | The package half holds: a service-package rewrite is a plan, and a butler-role principal is structurally unable to approve one — runtime `403` on `POST /api/v1/plans/plan-1/approve` (F-154-22). The learned-procedure half does not: a "procedure" is a remembered sentence, not a saved reusable procedure over granted tools (F-154-21). |

## Findings

### F-154-01 — A routine cannot be scheduled; the house has no clock

- **Status:** missing
- **Evidence:** `resource.AutomationTrigger` is `eventType` + `subject` only (`keystone/resource/automation.go:54-61`, `787ad6d`). Runtime, local instance: proposing an Automation carrying `spec.schedule: "0 7 * * *"` is refused — `put[0]: resource: decoding spec for kind "Automation": yaml: unmarshal errors: line 2: field schedule not found in type resource.AutomationSpec`. The three producers of household events are the intake endpoint `POST /api/v1/household/events` and the engine's own decision records (`grep -rn AppendHouseholdEvent`), so no time-based event exists for a rule to subscribe to either.

The only recurring thing in the control plane is a hardcoded 07:00
goroutine that appends one `report.morning` event
(`keystone/cmd/keystone/main.go:1246-1262`, `nextMorning`). It is not a
routine, is not configurable, and no rule can be attached to it.

Impact: every "every morning", "each Friday", "in an hour" routine a
household would ask for is unrepresentable. This is the single largest
gap in the slice and blocks AC02 on its own.

### F-154-02 — Nothing on this deployment can read a mailbox or send email

- **Status:** missing
- **Evidence:** `grep -rn agentTools packages/*/package.yaml` declares exactly three: `notifications.notify`, `chat-telegram-family.send`, `chat-matrix-family.send`. `packages/mail-roundcube/package.yaml` declares none — it is a webmail UI. No IMAP, SMTP or mailbox code exists in `keystone/` (`grep -rni "imap|mailbox|smtp"` matches only LDAP user email addresses and a git commit identity).

Impact: AC02's email briefing has no read source and no delivery
channel. AC02's "verify neither acquires ungranted send or delete
privileges" is vacuously satisfied.

### F-154-03 — The household Butler cannot author a routine

- **Status:** missing
- **Evidence:** `NewHouseholdGateway` registers `get_household_events`, `list_household_actions`, `do_household_action`, `describe_capabilities`, plus `recall`/`remember` when wired (`keystone/butler/household.go:234-266`). `get_automation_schema`, `dry_run_automation` and `propose_change` are registered only on the operator gateway (`keystone/butler/gateway.go:158-170`), and `HouseholdGateway.wired` has no case for any of them, so they cannot be added by configuration.

Impact: §8's "both household and operator Butlers can help users create
and run routines" fails for the family surface by construction. On the
live deployment that is Sandy. Whether Sandy would in practice succeed
at authoring is `unverified` — the deployment was unreachable — but the
tool is absent from the map, which settles the structural question.

### F-154-04 — Three unrelated routine records exist, and only one is executed

- **Status:** partial
- **Evidence:** (a) `resource.Automation` in the Statefile, executed by `keystone/automation`. (b) `household_routines` (`migrations/0028`), a free-text `document` with native/Wildstone ownership, revisions and review tokens. (c) `butler_tasks` with `kind='routine'` (`migrations/0017`), a full durable-work ledger. Only (a) has an executor; `grep -rn ButlerTaskRoutine --include=*.go` matches only its own definition and one validation line, and no caller outside `runtimedb` reads `household_routines.document`.

Impact: "which record is the routine" has three answers. The Console's
Routines page lists (b) (`console/src/views/Routines.tsx:51`,
`api.householdRoutines()`), which is the one that does nothing — so the
page a household looks at to see "what does this house do on its own"
shows the registry with no runner behind it. This is the most likely
proximate cause of "routines haven't turned out like I wanted".

### F-154-05 — A `household_routines` document is never parsed, never run, and reported as acting

- **Status:** missing
- **Evidence:** `CreateHouseholdRoutine` checks only that the document is non-empty (`keystone/runtimedb/householdroutines.go:110`); nothing parses it. Runtime, local instance:

  ```
  POST /api/v1/household/routines
    {"name":"morning-briefing","ownership":"wildstone",
     "document":"send a briefing every morning at 07:00 to mika"}
  → {"seq":1,"note":"a new routine starts as a draft; …"}

  POST /api/v1/household/routines/morning-briefing/state {"state":"enabled"}
  → {"performsActions":true,"routine":{…"state":"enabled"…}}
  ```

  An English sentence was accepted as a routine, enabled, and reported
  as performing actions. `HouseholdRoutine.PerformsActions` is a
  property of the state column (`householdroutines.go:88-91`), not of
  anything that runs.

Impact: the API asserts to the Console that a routine acts when no code
path can act on it. A household reading that page is told the house
does something it does not do.

### F-154-06 — A routine has no participants and no delivery record of its own

- **Status:** missing
- **Evidence:** `AutomationSpec` (`keystone/resource/automation.go:30-52`) has no participants field; recipients exist only as rendered action params, so who a routine is for is knowable only by reading a template string. `household_routines` has `author`/`updated_by` and nothing else.

Impact: §8 Ownership's "any participants" and §8 Delivery's audience
have no place to live. It also means a routine cannot be listed by
"routines that involve me".

### F-154-07 — Routine-scoped authorization grants are resolvable and never populated

- **Status:** missing
- **Evidence:** The resolver builds a routine-scoped grant candidate when a routine name is supplied (`keystone/runtimedb/butlertools.go:380-381`, `GrantScopeRoutine` at `:38`), and the seam carries the field the whole way (`keystone/butler/authority.go:44-55`, `keystone/butlerstore/authority.go:44`). Every call site passes the empty string: `keystone/butler/gateway.go:253`, `keystone/butler/household.go:444`, `keystone/butler/manifest.go:232` and `:306` — all `…, gw.SessionTask, "")`. `grep -rn RoutineName --include=*.go` outside tests returns only the plumbing.

This is the trap `../README.md` names: the mechanism is complete and
unwired. Marked `missing` for that reason.

Impact: §8's "bounded authorization grant" per routine cannot be
exercised even though the storage and resolution for it exist.

### F-154-08 — A routine's owner is recorded once and never checked again

- **Status:** missing
- **Evidence:** `metadata.owner` is validated at authoring (`keystone/resource/automation.go:221-236`) and never read by the engine — `grep -n "Metadata.Owner" keystone/automation/*.go` (excluding tests) matches nothing. `domestic.Call` carries tool, tier, params, rule, subject, correlation id and trigger type, and no person, grant or authority (`keystone/domestic/domestic.go:43-60`); `Gateway.dispatch` checks tier, package declaration and executor registration only (`:154-177`).

Impact: §10's "a routine runs under its owner grant" and §16's "recheck
current permissions before queued and scheduled actions" are both
unmet. A rule authored by a member who has since been offboarded keeps
firing with full domestic reach, and the household stream records the
rule as the actor rather than a person.

### F-154-09 — The routine-to-Butler binding is written, complete, and attached to the registry that does not run

- **Status:** missing
- **Evidence:** The mechanism exists. `migrations/0034_butler_assignment.sql` adds `household_routines.butler` under the stated principle that "a routine names its Butler … checked when the routine is written rather than when it fires", and `keystone/runtimedb/butlerassign.go:37-104` implements `AssignRoutineButler` (refusing a retired or disabled Butler), `RoutineButler`, and `ResolveRoutineButler` — which makes a routine **wait** rather than substitute another model when its Butler is unreachable, and refuses to pick one when none is assigned. `grep -rn "AssignRoutineButler\|ResolveRoutineButler" --include=*.go .` finds callers only in `keystone/runtimedb/butlerassign_test.go`; no API route exposes assignment and no runner calls resolution.
- The registry that **is** executed has no such field: `AutomationSpec` names no Butler, and the escalator is built once at startup from `apiSrv.ButlerHost` and `butlerCfg.Model` — the ADR-W017 legacy single-provider Butler (`keystone/cmd/keystone/main.go:1207-1233`) — while named Butlers answer conversations through `butlerlive.Butlers` (`:1086-1145`). The `Phraser` gets no tools and records its session as `ActingFor: automation.Source`.

Marked `missing` under the unwired-code rule, and the distinction
matters for remediation: **do not design a new assignment model.** The
one that exists already encodes the two decisions that are hard to get
right — no run-time dispatcher choosing a model, and waiting rather
than substituting when a Butler is unreachable. What it lacks is a
caller and a runner, and it is bolted to `household_routines` rather
than to the Automations the engine reads (F-154-04). Wiring or
consolidating it is the work; rebuilding it would be waste and would
probably lose those two properties.

Impact: §8 requires a routine to name a responsible Butler, and §8's
return value includes it. Today a routine's one model call is answered
by whatever the deployment-wide legacy provider is set to, which on
this deployment is neither of the named Butlers. Reassignment (R35) is
the same story: written, unreachable.

The privacy consequence — household escalation facts going to a
provider chosen outside the per-Butler cloud/local split — belongs to
#157; noted under Cross-slice, not chased.

### F-154-10 — The durable routine-run ledger has no producer

- **Status:** missing
- **Evidence:** `migrations/0017` gives routine runs steps, attempts, transitions, waits (including `schedule` and `approval`), evidence, budgets, grants, `deadline_at` and `next_wakeup_at`, with triggers enforcing evidence-before-completion. The only caller of `InsertButlerTask` outside `runtimedb` and tests is `keystone/butlerlive/serve.go:296`, and it hardcodes `Kind: runtimedb.ButlerTaskConversation`. `grep -rn ButlerTaskRoutine --include=*.go` finds no producer at all.

Impact: every §8 Reliability field the schema already supports —
partial completion, retries, budgets, waiting for an approval or a
schedule — is unreachable for routines. It also means the "routine"
half of `butler_tasks` is dead weight that reads as implemented.

Durable execution as a whole is #158's slice; this finding is recorded
here only because it is the routine execution path.

### F-154-11 — A routine cannot be activated without a human, ever

- **Status:** missing
- **Evidence:** An Automation reaches the engine only by being committed to the Statefile, which happens in `approvePlan` (`keystone/api/handlers_plans.go:310-340`) behind `principal.VerbApprove`. `RoleButler` holds `{read, propose}` (`keystone/principal/rbac.go:59-61`). Runtime, local instance: a butler-role token proposing is accepted, and `POST /api/v1/plans/plan-1/approve` returns `403`.

Impact: §8's "when a request is sufficiently specified and within
existing permissions, persist and activate it without redundant
confirmation" is not implemented and, as designed, cannot be — the
structural claim in `keystone/principal/rbac.go:42-45` is that a
fully-obeyed prompt injection ends at a proposed plan. That is a
deliberate safety property in direct tension with §8. See design gap 1;
it needs a decision, not a fix.

### F-154-12 — Timezone is deployment-wide, and traveler-time does not exist

- **Status:** partial
- **Evidence:** One `-household-timezone` flag loaded once into `Engine.Location` (`keystone/cmd/keystone/main.go:77,757-767`), used for `timeOfDay` and `weekday` and for `{{ .Time }}`/`{{ .Date }}` rendering (`keystone/automation/automation.go:164-186`, `act.go:88-100`). No per-routine zone; no notion of where the person is.
- Daylight saving is handled correctly *for what exists*: conditions convert the trigger's `occurred_at` with `In(loc)`, so a wall-clock window follows the offset change. The §16 hazard it warns about — duplicate or skipped execution across a DST boundary — cannot arise today because nothing is scheduled (F-154-01), so that specific check is `unverified` and will stay so until a scheduler exists.

Impact: §8 requires an explicit timezone per routine and a stated
household-time versus traveler-time behaviour. One global zone is a
reasonable default and is not the contract.

### F-154-13 — Catch-up is one global staleness bound, not a per-routine policy

- **Status:** partial
- **Evidence:** `DefaultMaxTriggerAge` is 1h for the whole engine (`keystone/automation/automation.go:52-57`), and a first tick over an existing stream adopts the cursor instead of evaluating history (`TickOutcome.Adopted`). Both are sensible; neither is per routine, and neither is configurable per rule — `AutomationSpec` has no catch-up field.

Impact: §16's "collapse missed briefings into one current briefing;
skip stale physical actions by default" cannot be expressed. The
current behaviour is the second half applied to everything.

### F-154-14 — Delivery names a connection but no audience and no follow-up

- **Status:** partial
- **Evidence:** `ChatSender` is registered one per installed chat capability and resolves the member's own bound conversation, refusing when the member has no binding (`keystone/domestic/chat.go:24-90`) — so the destination is specific and per transport. What is absent: any audience field, and any per-routine no-response behaviour. Unanswered chat questions expire on a fixed 4h window (`keystone/runtimedb/chatquestions.go:22-25`) and ntfy reply tokens on a fixed 12h TTL (`-household-reply-ttl`); neither escalates, re-asks, or records a "nobody answered" outcome the routine can act on.

Impact: §8 Delivery is half met. A routine cannot say "ask again in an
hour" or "if nobody answers, tell the other adult".

Whether a chat connection can be bound to a *specific Butler* is #153's
slice; noted under Cross-slice.

### F-154-15 — Cancellation stops the next evaluation, not the current run

- **Status:** partial
- **Evidence:** Disabling a rule removes it at load and at evaluation (`keystone/automation/automation.go:99,365`), and `HouseholdRoutine.Active()` is checked per state change. Once `act` has written its claim and called `Dispatch`, nothing can stop it; the gateway has a timeout and no cancellation seam (`keystone/domestic/domestic.go:146-152`).

Impact: acceptable for one-shot notifications, and short of §16's
"cancellation stops further actions where possible" for anything
multi-step — which does not exist yet (F-154-16). Recorded so the two
are prioritized together.

### F-154-16 — A routine is one tool call; there are no steps and no agent objective

- **Status:** missing
- **Evidence:** `AutomationAction` is a single `tool`, `tier` and `params` map (`keystone/resource/automation.go:121-136`), dispatched once (`keystone/automation/act.go:126-155`). Escalation replaces the text of one named param and explicitly never decides whether or what (`keystone/automation/escalate.go:1-16`, `keystone/resource/automation.go:99-119`).

Impact: §8's "deterministic actions, an agent task, or a combination"
is one third implemented. A daily briefing — read several sources,
reason, then deliver — has no shape to be written in, independently of
the missing schedule and the missing mailbox. Completion criteria
(R21), partial completion (R29) and execution limits (R22) all follow
from this.

### F-154-17 — A Butler-initiated household action has no approval binding

- **Status:** missing
- **Evidence:** `do_household_action` "runs immediately" by its own description and dispatches straight through the gateway (`keystone/butler/household.go:321-342`, `:454-460`). The only checks are the read-only session flag, the tool-authority resolver, and the gateway's tier/declaration/registration checks. No approval record is written, nothing is pinned, and `butler_task_waits` reason `approval` has no producer (F-154-10).

Impact: this is the core of AC10. "Approve a concrete consequential
action, then change its target" has no mechanism to test on the
household surface — there is nothing to invalidate, because there was
never an approval. Containment today comes from tier restriction
(`allowedTiers` refuses anything above domestic) rather than from
approval.

### F-154-18 — Plan approval binds target and parameters, with no validity period

- **Status:** partial
- **Evidence:** A plan stores the exact documents it would commit and is superseded on approval if the Statefile head moved since it was computed (`keystone/api/handlers_plans.go:297-308`) — so a material change to the world invalidates it. The plan record carries `resolved_by`/`resolved_at` (`keystone/runtimedb/plans.go:221-258`) and the closed transition table prevents re-approval. What is absent is any expiry: a plan proposed today can be approved indefinitely later, provided nothing else moved.

Impact: §10's "validity period" and "unanswered or expired approvals do
not authorize execution" are unmet for the Console path. The risk is
bounded by the head check, not eliminated: a proposal reviewed weeks
ago against unchanged state approves without anyone re-reading it.

### F-154-19 — The household review token is the one place approval binding is fully right

- **Status:** partial
- **Evidence:** `IssueHouseholdReviewToken`/`RedeemHouseholdReviewToken` (`keystone/runtimedb/householdroutines.go:303-390`) store only a hash, bind one plan id, one member, one routine revision and one expiry, and are single-use — enforced by the schema trigger `household_review_tokens_single_use` as well as by the redeem path. Issuance refuses a member who is not currently admitted, and refuses any plan that is not purely Automations (`keystone/api/handlers_household_review.go:160-186`, `plan.ClassHouseholdRoutine`). Redemption requires the trusted ingress header from an exact configured peer and resolves the asserted name through the identity-link table (`:230-280`).
- **Redemption exercised end to end**, on a local instance started with `-household-ingress 127.0.0.1`, an admitted member `mika`, and a `trusted-ingress` identity link to `mika@household`:

  | Request | Result |
  |---|---|
  | `Remote-User: someone-else` | `404 not found` |
  | no `Remote-User` header | `404 not found` |
  | `Remote-User: mika@household` | `{"planId":"plan-2","state":"approved","by":"mika"}` |
  | the same token, again | `this review link does not work` |

  And the material-change case, which is what AC10 asks for: a token
  issued against routine `mail-check` at revision 1, the routine then
  edited (`→ revision 2`), the token redeemed → `this review link does
  not work`, with the plan still `proposed` in the database. Approval
  binding, member binding, single use and invalidation on material
  change all hold.

Two defects inside an otherwise sound mechanism. The first is that
**nothing can ever reach it through the real proposal path** — see
F-154-24, which is why the two plan records above had to be written
into the sandbox database by hand in the shape the handler expects.
The second is that the revision it pins belongs to a
`household_routines` row, while the plan it approves contains
`Automation` resources from the Statefile — two registries with no
relationship (F-154-04). `routine` is an optional caller-supplied
field, so a token can be pinned to a revision that has nothing to do
with what it approves, or to nothing at all.

Status moved from `implemented` to `partial` on that first defect. The
binding logic is verified and correct; the door in front of it does not
open.

### F-154-20 — There are no narrow standing grants

- **Status:** missing
- **Evidence:** The only grant machinery is `butler_tool_grants` scoped to a task or a routine (`keystone/runtimedb/butlertools.go:36-42`), and the routine scope is never populated (F-154-07). Nothing expresses "this recipient, this action, until this date" as a standing permission.

Impact: §10's escape hatch from redundant confirmation is absent, which
matters more given F-154-11: the system has neither activation without
confirmation nor a standing grant that would make confirmation
unnecessary.

### F-154-21 — A learned procedure is a remembered sentence

- **Status:** partial
- **Evidence:** `MemoryProcedure` is one of four memory kinds (`keystone/runtimedb/butlermemory.go:44,114`), injected into the session preamble alongside preferences (`keystone/butlerstore/memory.go:34-37`). It names no tools, has no parameters, is not versioned as a procedure, and cannot be executed or tested. The versioned-and-tested half of §16 *is* real for Automations: `dry_run_automation` replays a draft against recorded history and writes nothing (`keystone/butler/tools_automation.go:57-72`), and every edit is a new Statefile revision.

Impact: §16's "agents may save reusable procedures composed of already
authorized tools" is met only in the weak sense that the model may be
reminded how to do something. Note also that memory is stamped per
Butler, so a procedure Alfred learns is invisible to Sandy — that is
finding 1 on #151 and belongs to #156.

### F-154-22 — Package and plugin changes do route to administrative review

- **Status:** implemented
- **Evidence:** Every structural change is a plan validated against `resource.KnownKind` and the standing policy engine before it becomes a record (`keystone/api/handlers_plans.go:149-225`), and approval needs `VerbApprove`, which `RoleButler` does not hold — runtime `403` from a butler-role token on `POST /api/v1/plans/plan-1/approve`. The household review path additionally refuses any plan that is not purely Automations, with the refusal reasoned explicitly as anti-escalation (`keystone/api/handlers_household_review.go:160-172`). The domestic gateway independently caps rule actions at read/domestic tiers (`keystone/domestic/domestic.go:155-158`).
- Not verified: that a real package rewrite proposed by the live operator Butler ends in the Console queue rather than anywhere else. That is a deployment check and stays **unverified**; driving `propose_change` on the live Alfred against a Container or ConfigFile would settle it.

### F-154-23 — A butler-role principal can create *and enable* a household routine

- **Status:** partial
- **Evidence:** `createHouseholdRoutine` refuses a non-draft state and says why: the route needs only propose, "so accepting a state verbatim let a compromised Butler credential create an action-capable routine and skip the review its activation is supposed to require" (`keystone/api/handlers_household_review.go:66-82`). But `POST /api/v1/household/routines/{name}/state` is registered under the same `principal.VerbPropose` (`keystone/api/api.go:360-362`). Runtime, local instance, with a freshly minted `role: butler` token:

  ```
  POST /api/v1/household/routines            → {"seq":2}            (draft)
  POST /api/v1/household/routines/butler-made/state {"state":"enabled"}
      → {"performsActions":true,"routine":{…"state":"enabled",
         "updatedBy":"audit-butler"…}}
  ```

  Two calls, no human, no plan.
- Recorded `partial` rather than `missing`: the guard on the create path exists and works, and the practical blast radius today is nil because nothing executes these documents (F-154-05). If a runner is ever built for that registry, this becomes a live authorization hole on the same commit.

Impact: the stated control — "activation goes through
SetHouseholdRoutineState and the Plan boundary, which is where a human
is" — is not true of the endpoint it names. Whether `VerbPropose` is
the right verb for a state change is an authorization question; pointer
left for #155 under Cross-slice.

### F-154-24 — The household review path cannot issue a token for any real plan

- **Status:** missing
- **Evidence:** `issueHouseholdReviewToken` decodes `plans.plan_json` straight into a `plan.Plan` and classifies its `Steps` (`keystone/api/handlers_household_review.go:171-186`). The proposal path stores a `plan.StoredProposal` in that column — `basedOnHead`, `summary`, `put`, `delete`, and the real plan nested under `"plan"` (`keystone/plan/stored.go:15-29`, written at `keystone/api/handlers_plans.go:251-258`), which `approvePlan` reads back correctly at `:290`. A `plan.Plan` decoded from that JSON has no steps, so `plan.Classify` returns `ClassEmpty` for every plan that exists.
- Runtime, local instance: proposing a plan whose only step is `Create Automation/mail-check`, then asking for a review token, returns

  ```
  plan plan-1 is empty, not a pure household-routine change, so it is
  reviewed in the Console — a plan that also touches infrastructure or
  identity is an infrastructure change with a routine in it
  ```

  The stored row confirms the shape:
  `{"basedOnHead":"","summary":"mail check rule","put":["apiVersion: …` — no top-level `steps`.
- The tests do not catch it because `storePlan` marshals a bare `plan.Plan` into `plan_json` (`keystone/api/handlers_household_review_test.go:44-58`) — the handler's shape, not the proposer's. Every test in that file passes against a fixture production never writes.

Impact: the entire household review page is unreachable. An admitted
adult cannot approve a routine without Console credentials, which is
the one thing ADR-W030 A8.1 built it for, and the refusal blames the
plan's contents rather than a decode mismatch — so the failure reads as
policy working correctly. This is the only finding in the slice that is
a plain defect with an obvious fix rather than a gap against the
baseline, and per the audit's scope rule it has not been fixed here.

## Design gaps

Decisions for the owner. Each is the code doing something coherent that
is not what §8 asks for, so none of them is a defect to file.

**1. A routine is desired state a human signs, not a job an agent
starts.** ADR-W024/W030 make every routine a resource that reaches the
engine only through a plan committed to the Statefile, and make
`RoleButler` structurally incapable of approving one — the explicit
safety claim is that a fully-obeyed prompt injection still ends at a
proposal (`keystone/principal/rbac.go:42-45`). §8 says the opposite for
the sufficiently-specified case: persist and activate without redundant
confirmation, and return the routine id and next run. Both positions
are defensible and they cannot both hold. **This is the likeliest
answer to "it hasn't turned out like I wanted."** The middle path §10
already names is a narrow standing grant (F-154-20): a bounded
authorization under which a Butler may activate routines of a named
shape — read-only, or notification-only, or one recipient — without a
per-routine signature. That is a design decision about where the human
sits, and it should be made before anything else in this slice is
scheduled, because most of the remaining findings inherit from it.

**2. The model may phrase, never decide.** Escalation replaces the
wording of one already-approved parameter and nothing else, so a rule
still works when the model is off. It is a genuinely good property. §8
nonetheless wants "an agent task" as a legitimate kind of routine work,
with an objective and completion criteria — the daily-briefing shape.
Deciding to allow that means deciding what a routine may do when the
model is unavailable, which the current design answers by never
depending on it.

**3. Three registries, one runner.** `Automation` resources,
`household_routines` documents and `butler_tasks` routine rows all
claim to be routines; only the first executes, and the Console shows
the second (F-154-04, F-154-05). This is not a design *choice* so much
as a design *drift*, but the resolution is a decision: which one is the
routine, and what happens to the other two. Consolidating on
`Automation` costs the review-token flow its home, since that flow is
built around `household_routines` revisions.

**4. The house has no clock (F-154-01).** Adding one is a choice
between two shapes: a scheduler that emits ordinary household events on
a cadence, so the existing event-triggered engine needs no new concept;
or a second trigger kind with its own catch-up, timezone and
traveler-time semantics. The first is much smaller and gets §16's
missed-schedule policy nearly free through `MaxTriggerAge`; the second
is what §8's contract table literally describes.

## Remediation ranking

§17 asks the audit to rank remediation, so this is one slice's ordered
view. It is not a schedule: #151 sequences remediation across all six
slices at once, and §13 puts identity, authorization, memory visibility
and privacy ahead of anything that increases tool reach. Where that
ordering and this one disagree, §13 wins.

**Blocked on a decision — nothing below rank 3 should be scheduled
until it is made.**

0. **Where the human sits** (design gap 1, F-154-11, F-154-20). Whether a Butler may activate a bounded class of routine under a standing grant, or every routine keeps needing a signature. Most of this list is shaped differently depending on the answer, and the standing-grant work at rank 7 exists only under one of them.

**Then, in this order.**

1. **F-154-24 — the review path cannot issue a token.** A decode mismatch, an afternoon's work, and it restores the only way an admitted adult approves a routine without Console credentials. Nothing else here is this cheap. It is also a live authorization surprise: the refusal blames the plan, so the mechanism looks like it is working.
2. **F-154-23 — routine activation is gated on `VerbPropose`.** Small, and it closes a hole that becomes real the moment rank 5 lands. §13 puts authorization first; this is the authorization item in this slice. Coordinate with #155, which owns the verb table.
3. **F-154-04 / F-154-05 — decide which registry is *the* routine, and stop the other from claiming it acts.** Everything below builds on whichever record wins, so doing it later means doing part of it twice. The Console reporting `performsActions: true` for an unexecutable document is the user-visible half and should not wait for the rest.
4. **F-154-01 — give the house a clock.** The largest single gain against §8, and the prerequisite for AC02. Design gap 4 says the cheap shape is a scheduled event source feeding the existing engine.
5. **F-154-08 / F-154-07 — carry the owner and their grant into the run, and recheck at each run.** §10 and §13 both put this before widening reach, and F-154-07's storage already exists. Do it before rank 6, not after: rank 6 increases what a routine can touch.
6. **F-154-09 — wire the routine-to-Butler assignment that already exists.** Consolidate it onto whichever registry won rank 3 rather than rebuilding it. Carries the privacy consequence for #157 with it.
7. **F-154-17 / F-154-20 — approval binding for Butler-initiated actions, and narrow standing grants.** Shape depends entirely on rank 0. This is the rest of AC10.
8. **F-154-02 — a mailbox capability.** Real work, and AC02 cannot pass without it, but it increases tool reach and §13 puts it after ranks 5 and 7.
9. **F-154-03 — routine authoring on the household surface.** Cheap once ranks 3 and 5 exist; premature before them, because it would let the family surface author into whichever shape is about to change.
10. **F-154-16 / F-154-10 — multi-step work and agent objectives, on the ledger that already exists.** The largest piece of work in the slice, gated on rank 0 and on #158.

Not ranked: F-154-06, F-154-12, F-154-13, F-154-14, F-154-15, F-154-21.
Each is a real gap and none blocks another; they are cheapest to take
alongside whichever rank above touches the same code.

## Cross-slice

Noted and not chased.

- **#153 — bot routing.** Delivery resolves a per-capability, per-member chat binding (`keystone/domestic/chat.go`) but nothing ties a routine or its delivery to a named Butler (F-154-09, F-154-14). Whether a chat connection can be bound to a specific Butler at all is that slice's question.
- **#155 — identity and authorization.** `POST /api/v1/household/routines/{name}/state` is gated on `VerbPropose`, which lets a butler-role credential activate a routine (F-154-23). The verb table itself is that slice's.
- **#156 — memory.** `MemoryProcedure` is per-Butler like every other record, so a procedure one Butler learns is invisible to another (F-154-21). This is finding 1 on #151.
- **#157 — privacy.** Household escalation facts go to the legacy single-provider host rather than to the per-Butler cloud/local assignment (F-154-09), which crosses the split the privacy slice cares about.
- **#158 — durable execution.** The entire `butler_tasks` routine path — steps, budgets, waits, evidence, catch-up — has no producer (F-154-10).

## ADR amendments required

- **ADR-W030 A6** — "a routine is created as a draft and activated separately" by a human. §8 requires activation without redundant confirmation for a sufficiently specified request within existing permissions. The baseline supersedes; the ADR needs amending, or §8's exception has to be written as a bounded standing grant (design gap 1).
- **ADR-W024 A6** — escalation may phrase one action param and never decide. §8 admits an agent objective as routine work. The ADR needs to say which kinds of routine it constrains rather than constraining all of them (design gap 2).
- **ADR-W024 H7** — "there is no second execution path" is true of the engine and false of the repository: `household_routines` is a second routine *definition* shape with no execution path at all (F-154-04, F-154-05). The ADR should either claim the narrower thing or the registry should go.
