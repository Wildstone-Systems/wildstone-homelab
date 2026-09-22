# Durable execution, recovery, delegation and lifecycle

- **Issue:** #158
- **Status:** complete
- **Audited:** 2026-09-10 on `787ad6d` (`butlers-on-the-live-path`)
- **Acceptance criteria:** AC05, AC13, AC14, AC15

Written by the audit slice for
[#158](https://github.com/chickenrat17/wildstone-homelab/issues/158).
**This file belongs to that slice alone** — no other session writes
here, which is what keeps parallel audits from colliding. See
[`../README.md`](../README.md) for how to run one and
[`../gap-report.md`](../gap-report.md) for the register.

`testing/e2e` lints this file's shape. Run
`cd testing/e2e && go test -run TestAuditRegister ./...` before you
commit.

## Method, and one limit worth stating first

Evidence here is **code** (file and line on `787ad6d`) and **test**
(named, and run: `go test ./butlerstore/... ./butlerlive/...
./runtimedb/...` — all three packages pass on this commit).

**There is no runtime evidence in this report, and that is a fact about
the session rather than about the deployment.** This slice ran in an
ephemeral container, not on the homelab host.
[`../runtime-access.md`](../runtime-access.md)'s starting points all
came back empty:

```
$ systemctl status keystone
System has not been booted with systemd as init system (PID 1).
$ curl -sS -m5 http://127.0.0.1:9441/api/v1/butlers
curl: (7) Failed to connect to 127.0.0.1 port 9441
$ sudo ls /var/lib/wildstone       → No such file or directory
$ sudo ls -l /etc/wildstone/keystone.env → No such file or directory
$ ls /usr/local/bin/keystone /usr/local/bin/wild → No such file or directory
```

That limit changes almost nothing below. Every finding marked `missing`
is missing because **nothing in the repository writes the value, calls
the function, or reaches the code path** — a `grep` over the whole tree
settles those, and a live box could not overturn one. The two places
where a live box would have added something are called out in
`F-158-11` and `F-158-13`, and neither is recorded as `unverified` to
be polite: both are established from code and the runtime check would
only have confirmed the operator experience.

Nothing was created on the live deployment, so there is no test data to
clean up.

## Requirements

| # | Baseline | Requirement | Status | Evidence |
|---|---|---|---|---|
| 1 | §7 | Expose working / waiting for you / blocked / partially complete / finished / cancelled | partial | `keystone/runtimedb/butlertasks.go:54-65`; `console/src/views/Work.tsx:33-54` — F-158-01 |
| 2 | §7 | Preserve pending questions and unfinished work across restarts | partial | `keystone/butlerlive/serve.go:240-300`; `TestAConversationSurvivesARestartWithoutRepeatingADispatch` — F-158-04, F-158-05 |
| 3 | §7 | Show meaningful progress and the reason for a block | implemented | `keystone/runtimedb/butlersteps.go:408-420`; `keystone/api/handlers_butler_tasks.go:106-130`; `console/src/views/Work.tsx:159-186` |
| 4 | §7 | No completion without a successful tool or scheduler receipt | partial | evidence is mandatory and atomic (`keystone/runtimedb/butlersteps.go:680-731`); the receipt is not (`keystone/butler/runner.go:697-702`) — F-158-19 |
| 5 | §7 | Context compaction retains task state without widening visibility or restoring secrets | missing | no compaction exists: `keystone/api/handlers_butler.go:290-299` replays every stored turn — F-158-16 |
| 6 | §7 | Model execution budgets | partial | `keystone/butlerlive/serve.go:19-31`; `keystone/butler/runner.go:425-447` — F-158-06, F-158-07 |
| 7 | §16 | Deduplication keys where supported | partial | in-task fingerprint `keystone/butler/runner.go:277-296`; no external idempotency key anywhere — F-158-03 |
| 8 | §16 | After a timeout, inspect the external outcome before retrying | missing | `keystone/butler/runner.go:589-670` — F-158-03 |
| 9 | §16 | Record an **uncertain** result rather than repeating a consequential effect | missing | `uncertainty` is read in three places and written in none — F-158-02 |
| 10 | §16 | Persist completed steps; resume only unfinished work | implemented | `keystone/butler/runner.go:298-320, 596-612`; `TestAConversationSurvivesARestartWithoutRepeatingADispatch` — F-158-20 |
| 11 | §16 | Distinguish partial success from full completion | missing | no state and no flag; a task with verified steps that stops is `failed` — F-158-01 |
| 12 | §16 | Per-routine catch-up policy; collapse missed briefings; skip stale physical actions | missing | routine documents are opaque `TEXT` (`keystone/runtimedb/householdroutines.go:97-155`); nothing schedules them — F-158-14 |
| 13 | §16 | Explicit timezone, DST without duplicate execution | missing | no `LoadLocation`, no cron, no per-routine zone in `keystone/` — F-158-14 |
| 14 | §16 | Coordinate writes to the same resource; detect recurring conflicts | missing | no conflict detection between agents, handlers and application rules — F-158-15 |
| 15 | §16 | Delegation carries initiating person, scope, allowed data, bounded authority | missing | no delegation exists — F-158-09 |
| 16 | §16 | A delegate may narrow, never widen, bypass a denial, or combine grants | missing | vacuous: nothing to delegate to — F-158-09 |
| 17 | §16 | Bound delegation depth, event recursion, tool steps, runtime and cost | partial | tool steps bounded per task; depth, recursion, runtime and cost unbounded — F-158-07, F-158-09 |
| 18 | §16 | Clone copies configuration without bot credentials, transcripts or running routines | partial | `keystone/runtimedb/butlers.go:556-620` — F-158-12 |
| 19 | §16 | Pause stops new runs; cancellation of active work is separate | missing | no agent-level pause; retire is the only lever — F-158-11 |
| 20 | §16 | Deleting an agent preserves person/household memory and pauses its routines | partial | memory kept (`keystone/runtimedb/butlers.go:306-326`); routines not paused — F-158-10 |
| 21 | §16 | Hiding in the UI is not pausing | implemented | retire is a stored `retired_at`, enforced in `ResolveButlerFor` (`keystone/runtimedb/butlers.go:463-512`), not a client filter |
| 22 | §16 | Household-wide stop control | partial | restore quarantine gates dispatch/retrieval/ingestion/delivery (`keystone/runtimedb/restore.go:124-162`) — F-158-13 |
| 23 | §16 | Targeted connection revocation | implemented | `keystone/runtimedb/butlerscope.go:87-110`; routes at `keystone/api/api.go:376, 389, 503`; `PUT /api/v1/butlers/{name}/connection` |
| 24 | §16 | Unfinished work preserved when a dependency is unavailable | implemented | `keystone/butler/runner.go:498-513`; `TestProviderStallFencesRetriesAndWaits` |
| 25 | §16 | The owner is told when work stalls | missing | the notifier is constructed with a nil `Binding` and returns early — F-158-08 |
| 26 | ADR-W030 A11 | One coordinator; a superseded one stops at its next checkpoint | partial | holds for durable work (`keystone/runtimedb/coordinator.go:137-153`; `keystone/butler/runner.go:339-345, 508-520, 673-680`); the legacy path escapes it entirely — F-158-17, F-158-18, F-158-21 |

## Acceptance criteria

| AC | Result | Evidence |
|---|---|---|
| AC05 — crash after a completed action | **missing** | Nothing checks or deduplicates an external effect after a timeout, and no code path ever writes the `uncertain` state the criterion requires. An ordinary restart does not even pause a task whose attempt is still `dispatched`; only the *restore-from-backup* path does (`keystone/runtimedb/restore.go:356-368`). F-158-02, F-158-03 |
| AC13 — scheduling and conflict handling | **missing** | There is no scheduler. Nothing reads `butler_tasks.next_wakeup_at`, no routine ever becomes a task (`ButlerTaskRoutine` has no producer), routine documents carry no trigger/timezone/catch-up fields, and there is no same-resource conflict detection. F-158-14, F-158-15 |
| AC14 — delegation and execution budgets | **missing** | No Butler-to-Butler delegation exists, so the privilege-expansion and recursive-handoff halves cannot be exercised at all. Of the budgets, tool steps are bounded per **task** — which on this path means per turn, never per conversation (F-158-06) — and runtime, depth, recursion and cost are not. F-158-06, F-158-07, F-158-09 |
| AC15 — lifecycle and partial work | **partial** | Clone is close to right (F-158-12) and deleting preserves memory rows. But there is no pause distinct from retire, retiring does not pause the routines that call the Butler, and "accurate partial status" is not representable — a task that established real work and stopped reports `failed`. F-158-01, F-158-10, F-158-11 |

## Findings

### F-158-01 — the state machine has no "partially complete", and "blocked" is a wait reason rather than a state

- **Current status:** superseded
- **Current evidence:** Current design treats partial success as completed step/effect evidence plus remaining work, not a separate terminal task state.
- **Superseded by:** docs/agent-system-design.md §16 durable work and E10 completion evidence
- **Audit status:** partial
- **Audit evidence:** `keystone/runtimedb/butlertasks.go:54-65` defines seven states — `pending`, `running`, `waiting`, `completed`, `failed`, `cancelled`, `paused-offboarded`. `console/src/views/Work.tsx:33-54` renders exactly those. §7's six map as: working→`running`, waiting for you→`waiting`, finished→`completed`, cancelled→`cancelled`.

Two do not map.

**Blocked** is carried by the wait reason rather than the state:
`butler_task_waits.reason` is a closed set of eight
(`keystone/runtimedb/butlersteps.go:408-420`) and separates "waiting for
you to say continue" from "could not reach the model" from "a source it
needs is unavailable". `console/src/views/Work.tsx:177-186` renders each
in words. That is arguably a better shape than a state — but a caller
reading `state` alone cannot tell a person-block from a system-block,
and the API returns `state` on the list endpoint
(`keystone/api/handlers_butler_tasks.go:78-88`) with the waits only on
the detail endpoint.

**Partially complete** is not representable anywhere. A task that
verified three tool steps and then failed is `failed`; the evidence rows
survive and say what was established, but nothing in the state, the
summary, or the list view distinguishes it from a task that achieved
nothing. §16's "Distinguish partial success from full completion" has no
carrier.

### F-158-02 — the uncertain result is readable in three places and written in none

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/conversationwork.go`; 0baaa8d; `TestConversationParentPreservesUncertainOutcome`.
- **Audit status:** missing
- **Audit evidence:** `butler_tasks.uncertainty` is declared in migration `0017_butler_tasks.sql:92-94`, scanned in `keystone/runtimedb/butlertasks.go:352`, queried by the restore replay at `keystone/runtimedb/restore.go:363`, typed in `console/src/api.ts:502` and rendered in `console/src/views/Work.tsx:138-142`. `grep -rn uncertainty` over the whole tree returns no writer: there is no `SetButlerTaskUncertainty`, no `UPDATE ... SET uncertainty`, and no caller that would use one.

The wait reason `uncertain-outcome`
(`keystone/runtimedb/butlersteps.go:419`) is the same shape: defined,
validated in `OpenButlerTaskWait`, given a Console sentence at
`console/src/views/Work.tsx:187`, and passed by nothing. The runner's
mirrored wait constants (`keystone/butler/runner.go:66-70`) are `reply`,
`budget-exhausted` and `model-unavailable` only.

This is the trap [`../README.md`](../README.md) names. Reading the
schema, the API type and the Console component in sequence would tell
you uncertain outcomes are handled. The Console message "One outcome is
unknown, so it will not retry that step on its own" can never appear.

**This is the AC05 gate.** "If confirmation is unavailable, record an
uncertain result" has a column, a wait reason, a UI string and no
mechanism.

### F-158-03 — nothing inspects an external outcome after a timeout, and a resume re-dispatches an unverified action

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/conversationwork.go` holds an uncertain outcome, but no adapter-specific destination verification after timeout is implemented.
- **Current owner:** chickenrat17
- **Tracking:** #318
- **Audit status:** missing
- **Audit evidence:** `keystone/butler/runner.go:589-670` (`toolCall`). A tool call is checkpointed before dispatch (`StartAttempt`, `keystone/runtimedb/butlersteps.go:214-270`) and again after its result. Between those two points the process can die, leaving the attempt `dispatched` — an outcome the store's own comment calls "unknown, which the resume path must treat as uncertain rather than retry".

No resume path does that. `verifiedResults`
(`keystone/butler/runner.go:299-320`) builds its skip-set from steps in
state `verified` only; a step left `running` with a `dispatched` attempt
contributes nothing, so when the model asks for the same call again the
fingerprint misses and `toolCall` dispatches it a second time. Nothing
between reads `butler_task_attempts` looking for a dispatched row.

Two related absences:

- **No external verification.** `ButlerEvidenceExternalVerification`
  exists in the evidence vocabulary
  (`keystone/runtimedb/butlersteps.go:600-608`) and is written by
  nothing; the runner emits only `tool-result` and `delivery`
  (`keystone/butler/runner.go:684-703`).
- **No external deduplication key.** The fingerprint at
  `keystone/butler/runner.go:277-296` is a local skip-set; it is not
  sent to the application performing the effect. `DedupeKey` exists on
  the chat queue (`keystone/butlerstore/notify.go:64`,
  `keystone/api/handlers_chat_message.go:413`) and nowhere in the action
  path (`keystone/butler/tools_write.go`, `keystone/domestic`).

The one place the requirement *is* honoured is the restore replay:
`keystone/runtimedb/restore.go:356-368` moves every `running`/`waiting`
task with a dispatched attempt or a non-empty `uncertainty` to
`paused-offboarded`, with the comment "A restored coordinator that
resumes it is doing the one thing A4 forbids". That runs on
restore-from-backup and on no other restart, which is the narrower half
of the case AC05 describes.

### F-158-04 — nothing resumes a task on its own; the resume queue and the wakeup index have no readers

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/butlertasks.go:ButlerTasksInState` stores a resume queue; no autonomous wakeup reader is proven.
- **Current owner:** chickenrat17
- **Tracking:** #318
- **Audit status:** missing
- **Audit evidence:** `ButlerTasksInState` (`keystone/runtimedb/butlertasks.go:399-407`), documented as "the resume queue after a restart", has **zero callers outside its own package**. Its only exercise is `TestButlerTasksInStateIsTheResumeQueue` (`keystone/runtimedb/butlertasks_test.go:528`).

`butler_tasks.next_wakeup_at`, documented as "the scheduler's index"
(`keystone/runtimedb/butlersteps.go:463-467`), is written by
`OpenButlerTaskWait` and `ResolveButlerTaskWait` and **read by nothing**
— `grep -rn next_wakeup_at keystone/` outside `butlersteps.go` and
`butlertasks.go` returns nothing. `ButlerWaitSchedule` ("waiting for a
scheduled time") therefore never comes due.

The only thing that advances a task is an inbound turn:
`keystone/butlerlive/serve.go:246-278` resumes the session's in-flight
task when the next message arrives. So a task waiting on
`model-unavailable` resumes if and only if a person types again, on the
same conversation. That is exactly the accumulation
`keystone/api/handlers_butler_tasks.go:384-391` records from the live
deployment: *"nothing reaps a wait whose cause has gone… Three of them
accumulated on a live deployment in one morning."*

The mechanism exists and is unwired. §7's "Preserve pending questions
and unfinished work across restarts" holds for the storage and not for
the resumption.

### F-158-05 — a first turn that fails leaves a task nothing can ever reach

- **Current status:** implemented
- **Current evidence:** `keystone/api/handlers_butler_named.go`; 0baaa8d; `TestAFailedTurnKeepsItsDurableTask`.
- **Audit status:** partial
- **Audit evidence:** `keystone/butlerlive/serve.go:243-262` resumes a task by looking it up from the **session** id. The session is created after the answer, in `keystone/api/handlers_butler_named.go:176-215`, and the task is pointed at it there (`SetButlerTaskSession`, line 213).

`runNamedButlerSession`
(`keystone/api/handlers_butler_named.go:131-152`) returns on **every**
error from `Serve` — before `recordTurn`, and therefore before the link
is written. But a failing turn is precisely the one that leaves a task
in `waiting` with verified steps in its ledger
(`keystone/butler/runner.go:498-513`).

So on the first message of a conversation, a model outage produces a
durable task with no session, no wakeup reader (F-158-04) and no
handle. It is preserved and unreachable. A second message opens a new
conversation and a new task, re-dispatching whatever the first had
already established.

Resume works for the case the test covers —
`TestAConversationSurvivesARestartWithoutRepeatingADispatch`
(`keystone/butlerlive/butlerlive_test.go:456`) links the session by hand
at line 494 before restarting, which is the *second*-turn shape — and
fails for the first-turn one. `wild butler ask` opens a new conversation
every time ([`../runtime-access.md`](../runtime-access.md)), so on the
CLI every turn is a first turn.

### F-158-06 — the aggregate tool budget resets every turn, which is not what it says it does

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/conversationwork.go`; 0baaa8d; `TestConversationParentCarriesBudgetAcrossTurnLedgers`.
- **Audit status:** partial
- **Audit evidence:** `keystone/butlerlive/serve.go:24-31`: *"conversationToolCalls bounds a conversation's tool calls across every turn and every restart. Unlike the slice budget it does not reset, because a budget that resets on retry is not a budget."* The same claim is in `keystone/butler/runner.go:184-190`.

It resets every turn. `Runner.Run` seeds the counter from
`countToolSteps(steps)` (`keystone/butler/runner.go:322-329`), and
`steps` is `Ledger.Steps(taskID)` — the **task's** ledger. The live path
opens a new task per turn: `keystone/butlerlive/serve.go:266-276` resumes
only a task that is still in flight and falls through to
`InsertButlerTask` for a completed or failed one, and every successful
turn completes its task (`keystone/butler/runner.go:378-381`). Proved by
`TestAConversationCanHaveASecondMessage`
(`keystone/butlerlive/butlerlive_test.go:656-700`), which asserts the
follow-up gets a *different* task id.

So the enforced budget is 64 tool calls **per turn**, on top of 8 model
rounds per turn. A twenty-message conversation is bounded at 1280 tool
calls by nothing that had to agree to that number.

The per-task accounting itself is correct and carefully done — a result
served from the ledger spends nothing
(`keystone/butler/runner.go:432-440`, `TestRereadingAnEstablishedFactDoesNotSpendBudget`).
The defect is that "the task" and "the conversation" are the same word
in the comment and different objects in the code.

### F-158-07 — no runtime, deadline or cost budget exists; the two columns for them are written by nothing

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/butlersettings.go` bounds model rounds and tool calls, but runtime/deadline/cost budgets are not enforced together.
- **Current owner:** chickenrat17
- **Tracking:** #318
- **Audit status:** missing
- **Requires:** §16 requires "finite retry, duration, step, and spending limits; exact values must be explicit configuration rather than unlimited defaults", and §7 lists "execution budgets" as per-Butler configuration.
- **Audit evidence:** code as cited below — `keystone/butlerlive/serve.go:96-107`, `keystone/butlerstore/butlerstore.go:88-92`, `keystone/butlerstore/retry_test.go:138`; test `TestWithdrawnAuthorityBlocksContinuation`.

What exists: rounds per turn (8), tool calls per task (64), one model
call bounded at 2 minutes, one retry
(`keystone/butlerlive/serve.go:96-107`). All four are **compile-time
constants in one file**, not configuration, and none is per-Butler.

What does not exist:

- **Duration.** `ButlerTask.DeadlineAt` is enforced —
  `keystone/butlerstore/butlerstore.go:88-92` refuses a task past its
  deadline, and `TestWithdrawnAuthorityBlocksContinuation` covers it —
  but `DeadlineAt:` appears in exactly one place in the tree,
  `keystone/butlerstore/retry_test.go:138`. Production never sets it.
  `keystone/butlerlive/serve.go:283-300` builds a task with no deadline.
- **Cost.** `butler_tasks.budget_json` is validated on insert
  (`keystone/runtimedb/butlertasks.go:207-210`) and scanned back
  (line 351). No caller ever supplies it and no code reads it. There is
  no token accounting, no spend counter, and no cost concept anywhere in
  `keystone/butler*` — `keystone/runtimedb/butlertrace.go:65` says a
  trace records "neither cost nor remaining account allowance".
- **Per-Butler configuration.** `runtimedb.Butler` carries no budget
  fields; `PUT /api/v1/butlers/{name}/model` sets the route only.

Another enforced-but-unset field, same shape as F-158-02.

### F-158-08 — the stalled-task notice is wired with no way to reach anyone

- **Current status:** implemented
- **Current evidence:** `keystone/butlerstore/notify.go`; 0baaa8d; `TestStallNoticeDeduplicatesAndWaitsForQuietHours`.
- **Audit status:** missing
- **Audit evidence:** `keystone/butlerstore/notify.go:44-56`: `Notifier` needs a `Binding func(taskID string) (runtimedb.ChatOutbound, bool, error)` to resolve the surface a notice goes to, and `NotifyStalled` returns `nil` immediately when `n.Binding == nil` (lines 60-62).

`keystone/cmd/keystone/main.go:1143` is the only construction in the
tree:

```go
butlers.Notifier = &butlerstore.Notifier{Ledger: ledger}
```

`Binding` is not set, and nothing sets it afterwards — `grep -rn
"\.Binding" keystone/` outside `notify.go` and its tests returns
nothing. So `keystone/butler/runner.go:507-511` calls a notifier that
returns nil without queuing anything. No message is ever sent.

Everything behind the seam is complete and tested — dedupe key per task,
quiet-hours hold, silence when there is no direct surface
(`TestStallNoticeDeduplicatesAndWaitsForQuietHours`,
`TestStallNoticeIsSilentWithNoDirectSurface`,
`TestQuietHoursWindowWrapsMidnight`). The tests supply their own
`Binding`; production does not.

Combined with F-158-04 and F-158-05: a task stalls, nobody is told, and
nothing resumes it.

### F-158-09 — delegation between Butlers does not exist

- **Current status:** open
- **Current evidence:** `keystone/butler/runner.go` does not expose a bounded Agent-to-Agent delegation contract.
- **Current owner:** chickenrat17
- **Tracking:** #312
- **Audit status:** missing
- **Audit evidence:** `grep -rin "delegat" keystone/` returns four hits, all unrelated: `gateway.go:519`, `butler/policy.go:23`, `butler/redact.go:5` and `reconcile/origin.go:3` all use the word for "this function calls that package".

There is no delegate concept, no sub-task, no initiating-identity
propagation across agents, and no depth counter. The only cross-Butler
movement in the system is `butlerhandoff.go` — a **person** moving their
own conversation between surfaces, which explicitly carries no privilege
(`keystone/runtimedb/butlerhandoff.go:17-23`: "A handoff continues
context; it does not carry privilege"). The live path goes further and
refuses a conversation to change Butler at all
(`keystone/butlerlive/serve.go:248-262`).

The §16 loop bounds are correspondingly absent. Delegation depth: no
concept. Event recursion: the handler engine has a one-hop guard —
`automation.Matches` refuses to fire on an event the engine itself
recorded (`keystone/automation/automation.go:93-110`) — which bounds
rule-on-rule and says nothing about a Butler acting, that action
becoming an event, and a rule escalating back to a Butler. Runtime and
cost: F-158-07.

AC14 cannot be exercised. Both its halves — attempt privilege expansion,
attempt recursive handoffs — need a delegate to attempt them through.

The absence looks deliberate rather than overlooked: A4's isolation
("one Butler's material does not reach another Butler's model",
`keystone/runtimedb/butlerscope.go:1-25`) is built structurally
everywhere, and delegation is the feature that has to breach it under
control. See **Design gaps**.

### F-158-10 — retiring a Butler does not pause the routines that call it; it breaks them

- **Current status:** open
- **Current evidence:** `keystone/automation/routine_execution.go` needs a retirement-to-routine pause/owner-review regression test.
- **Current owner:** chickenrat17
- **Tracking:** #318
- **Audit status:** missing
- **Requires:** §16: "Deleting an agent preserves person- and household-owned memory and pauses its routines pending reassignment."
- **Audit evidence:** code as cited below — `keystone/runtimedb/butlers.go:312-326`, `keystone/runtimedb/butlerassign.go:52`, `keystone/runtimedb/butlers.go:632`.

`RetireButler` (`keystone/runtimedb/butlers.go:312-326`) executes one
statement, against the `butlers` row. It does not touch
`household_routines`, which carries a `butler` column
(`keystone/runtimedb/butlerassign.go:52`, `keystone/runtimedb/butlers.go:632`).
`keystone/api/handlers_butlers.go:382-391` adds nothing.

The consequence is counted and shown but not acted on:
`ButlerImpactOf` (`keystone/runtimedb/butlers.go:622-660`) lists the
routines, and the confirm dialog tells the operator they *"will fail"*
(`console/src/views/butlers/ButlerDrawer.tsx:257-264`) rather than that
they will be paused. Memory and sessions are correctly preserved and
correctly described in the same dialog.

"Pending reassignment" has a lever — `AssignRoutineButler`
(`keystone/runtimedb/butlerassign.go:52`) — and nothing pauses the
routine while it waits for one.

### F-158-11 — there is no pause; retire is the only agent-level lever, and it does not stop active work either

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/butlers.go` exposes retirement, but no independent pause/resume control for an active Agent is evidenced.
- **Current owner:** chickenrat17
- **Tracking:** #318
- **Audit status:** partial
- **Requires:** §16 asks for two separate controls: "Pause stops new runs and notifications; expose separate cancellation for active work."
- **Audit evidence:** code as cited below — `keystone/api/api.go:434`, `keystone/runtimedb/butlers.go:463-512`, `keystone/butlerstore/butlerstore.go:64-96`; test `TestWithdrawnAuthorityBlocksContinuation`.

The Butler has one control, `DELETE /api/v1/butlers/{name}` →
`RetireButler` (`keystone/api/api.go:434`). It does stop new
conversations — `ResolveButlerFor` refuses a retired Butler
(`keystone/runtimedb/butlers.go:463-512`) — and it is reversible
(`RestoreButler`, line 513), so it functions as a pause. But it is
presented as deletion, it is the *same* act as delete, and it does
nothing to work already in flight: no task is cancelled, no wait is
resolved, `Ledger.Authorize`
(`keystone/butlerstore/butlerstore.go:64-96`) does not consult the
Butler's retirement at all.

What does exist, and is properly wired:

- **Per-person pause.** `pausePersonalWork`
  (`keystone/api/handlers_butler_identity.go:117-136`) moves an
  offboarded member's pending/running/waiting tasks to
  `paused-offboarded`, and `Ledger.Authorize` refuses to continue them
  (`keystone/butlerstore/butlerstore.go:78-83`).
- **Per-task cancel.** `POST /api/v1/butler/tasks/{id}/cancel`
  (`keystone/api/handlers_butler_tasks.go:398-419`), enforced mid-run —
  `Runner.Run` re-authorizes before every model round
  (`keystone/butler/runner.go:352-366`), covered by
  `TestWithdrawnAuthorityBlocksContinuation`.

Neither is reachable from the Console: `console/src/api.ts:1205-1222`
exposes list, detail, feedback and evaluation for tasks and no cancel,
and `Work.tsx` renders no control. So §7's "cancellation controls" is an
API-only capability. *(A live box would have shown what an operator can
actually reach; the client code settles it either way.)*

### F-158-12 — clone is close to right, but it copies integration connection grants

- **Current status:** open
- **Current evidence:** Current clone behavior needs a negative credential/grant-inheritance test against the post-Connection schema.
- **Current owner:** chickenrat17
- **Tracking:** #313
- **Audit status:** partial
- **Audit evidence:** `DuplicateButler` (`keystone/runtimedb/butlers.go:556-620`) copies directive, surface, profile, model route, connection grants and tool narrowings, in one transaction. It deliberately copies neither the access list, nor memory, nor sessions (lines 542-554), and it touches `household_routines` and `butler_addresses` not at all — so a clone has no bot identity, no transcripts and no running routines. That is §16's clone requirement, and the API says so honestly (`keystone/api/handlers_butlers.go:610-613`: "Nobody can talk to it, it remembers nothing").

The one thing to weigh is the connection grants
(`keystone/runtimedb/butlers.go:590-597`). Those are scoped credential
references — `butler_connection_scope` rows joining to
`integration_connections` — and §7 says "no automatic inheritance of
another agent's credentials". They are copied automatically. The
counter-argument is in `duplicateButler`'s own comment
(`keystone/api/handlers_butlers.go:575-586`): a copy that gets the route
but loses the narrowings would be *wider* than its original, and the
transaction exists to prevent that.

Both readings are defensible, which makes it a decision rather than a
defect. See **Design gaps**.

### F-158-13 — the household-wide stop control exists as restore quarantine, and its release path belongs to a different incident

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/routineexecution.go`; 0baaa8d; `TestHouseholdStopHaltsAllRoutineExecution`.
- **Audit status:** partial
- **Requires:** §16: "Provide a household-wide stop control and targeted connection revocation." The revocation half is implemented (requirement 23 above).
- **Audit evidence:** code as cited below — `keystone/api/api.go:402`, `keystone/runtimedb/restore.go:124-162`, `keystone/runtimedb/butlersteps.go:220-225`.

The stop half exists in substance. `POST /api/v1/restore/quarantine`
(`keystone/api/api.go:402`) sets a flag that `db.gate` checks at the
four chokepoints — `GateRetrieval`, `GateDispatch`, `GateIngestion`,
`GateDelivery` (`keystone/runtimedb/restore.go:124-162`). The gate is on
the **store**, not on HTTP middleware, deliberately so a background loop
or a new route cannot walk past it (lines 134-146), and
`StartButlerAttempt` calls it before every model and tool dispatch
(`keystone/runtimedb/butlersteps.go:220-225`). It fails closed on a read
error. That is a real stop, and a well-placed one.

What makes it partial:

- **Getting out is shaped for a restore, not an incident.** Release
  requires asserting the deletion journal is current *and* replaying it
  (`ReleaseRestoreQuarantine`, `keystone/runtimedb/restore.go:145-175`;
  `keystone/api/handlers_offboarding.go:92-131`), enforced by the schema
  as well. After a "stop everything, something is wrong" there is no
  journal to bring current, and the replay pauses every task with a
  dispatched attempt (`restore.go:356-368`) — collateral an operator who
  only wanted to stop the household did not ask for.
- **It is named and documented as something else.** Every string an
  operator sees says "restore" and "reconciled"
  (`keystone/api/handlers_offboarding.go:58-70`).
- **No Console control.** `grep -rn "quarantin" console/src/` returns
  nothing. It is a `curl`-only control, which is the wrong shape for the
  thing you reach for in an incident.

- **It does not reach the legacy session path.** The gate lives on
  `StartButlerAttempt`, which that path never calls, so a quarantined
  database still dispatches household actions there. See **F-158-21**.

*(A live box would have shown how the endpoint reads to an operator
mid-incident; the release precondition is settled from code.)*

### F-158-14 — nothing schedules anything: no routine becomes a task, and routine documents carry no trigger, timezone or catch-up policy

- **Current status:** open
- **Current evidence:** `keystone/automation/routine_execution.go` makes event-triggered reviewed runs; no clock schedule creates an occurrence.
- **Current owner:** chickenrat17
- **Tracking:** #318
- **Audit status:** missing
- **Audit evidence:** `ButlerTaskRoutine` (`keystone/runtimedb/butlertasks.go:48`) is the task kind for "one occurrence of a Wildstone-owned routine". `grep -rn ButlerTaskRoutine keystone/` outside the file that defines it and its own validation returns nothing: **no producer**. Every task created on the live path is a `conversation` (`keystone/butlerlive/serve.go:284`).

`household_routines.document` is opaque `TEXT`. `CreateHouseholdRoutine`
(`keystone/runtimedb/householdroutines.go:97-155`) validates ownership,
name, author, non-empty document and state, and parses nothing inside
the document. There is no trigger field, no `catch_up` column, no
per-routine timezone. `grep -rni "cron\|LoadLocation\|catch.up"
keystone/` finds no scheduler and no zone handling in the agent path —
the only timezone in the system is the deployment-wide
`-household-timezone` (`keystone/api/api.go:116-120`), used to answer
`source: timezone` params and to give handler rules a wall clock
(`keystone/automation/automation.go:306`).

So all four AC13 scenarios are unreachable: a missed briefing (nothing
fires briefings), a stale physical action (nothing to skip), a DST
boundary (no per-routine zone to cross), and two routines on one
resource (F-158-15).

Cross-slice: whether routines *should* execute, and how, is #154's. What
belongs here is that the durable-execution machinery has a routine
shape built into it with nothing on the other end.

### F-158-15 — no conflict detection between agents, handlers and application rules

- **Current status:** open
- **Current evidence:** `keystone/automation/routine_execution.go` fences one reviewed effect, but no cross-Agent/handler conflict detector arbitrates shared changes.
- **Current owner:** chickenrat17
- **Tracking:** #318
- **Audit status:** missing
- **Requires:** §16: "Coordinate writes to the same resource and detect recurring conflicts between agents, handlers, and application rules. Pause and surface conflicting routines rather than allowing repeated opposing commands."
- **Audit evidence:** code as cited below — `keystone/api/handlers_plans.go:307`, `keystone/automation/automation.go:93-110`.

`grep -rni conflict keystone/ | grep -v "ON CONFLICT"` returns HTTP 409
responses and one desired-state check. The one real mechanism is plan
supersession: `keystone/api/handlers_plans.go:307` refuses to apply a
plan when the statefile changed since it was proposed. That protects the
infrastructure desired state against a stale plan. It is not
same-resource coordination between two automations, and it does not
detect a recurring one.

Nothing correlates two routines by target, nothing counts opposing
commands over time, and nothing can pause a pair. The suppression that
exists (`automation.Matches`,
`keystone/automation/automation.go:93-110`) stops a rule firing on the
engine's own event — one hop, and only within the handler engine.

### F-158-16 — there is no context compaction, and history is replayed whole every turn

- **Current status:** open
- **Current evidence:** `keystone/butler/runner.go` assembles turn history without a durable context-compaction step for long conversations.
- **Current owner:** chickenrat17
- **Tracking:** #318
- **Audit status:** missing
- **Requires:** §7: "Context compaction must retain task state without widening memory visibility or restoring secrets into prompts."
- **Audit evidence:** code as cited below — `keystone/api/handlers_butler.go:290-299`, `keystone/butler/runner.go:331-334`, `keystone/api/handlers_butler.go:301-312`.

No compaction exists. `historyFrom`
(`keystone/api/handlers_butler.go:290-299`) turns every stored turn of a
session into a provider message, unconditionally and unbounded;
`Runner.Run` prepends all of it (`keystone/butler/runner.go:331-334`).
There is no summarization, no window, no token accounting (F-158-07).
A long conversation grows its prompt until the provider refuses it.

Recorded as `missing` rather than not-applicable because the underlying
need — bounded context that keeps task state — is the requirement, and
nothing meets it.

The two safety properties the requirement attaches to compaction do hold
today, by construction rather than by a compactor: history is per
session and never crosses conversations
(`keystone/api/handlers_butler.go:301-312`), and stored turn text goes
through `redact.String` on the way in
(`keystone/api/handlers_butler_named.go:174, 190-198`), so a secret is
not restored into a later prompt. Whatever compaction gets built has to
keep both.

### F-158-17 — the coordinator lease is the gate on durable work, and it holds

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/coordinator.go`; 569a284; `TestCoordinatorLeaseIsSingleAndFenced`.
- **Audit status:** implemented
- **Audit evidence:** `keystone/cmd/keystone/main.go:506-518` acquires the lease at startup and sets `coordinating`; `main.go:1079-1150` wires the ledger, `butlerlive.Butlers` and `apiSrv.Butlers` **only** inside `if coordinating`. `AcquireCoordinator` (`keystone/runtimedb/coordinator.go:49-88`) never steals: a different holder returns `ErrCoordinatorHeld` and changes nothing; re-acquiring your own lease keeps the epoch, which is what lets a restart resume its own tasks. Takeover is the explicit `PromoteCoordinator`, which bumps the epoch (lines 90-135).

Fencing is enforced at every checkpoint that matters:
`Runner.Run` fences at entry and each round
(`keystone/butler/runner.go:339-345, 352-355`), *after* the model answers
and before its result is accepted (lines 508-520 — the case a
tool-call-free answer would otherwise slip through), before each tool
dispatch (line 611) and again before completion (lines 673-680).
`FinishButlerAttempt` independently refuses a result whose attempt epoch
is behind the task's (`keystone/runtimedb/butlersteps.go:284-292`),
covered by `TestButlerAttemptFencedByNewerEpoch` and
`TestLateResultFromAFencedAttemptIsRejected`.

**What a non-coordinating process still does**, since the issue asks:
it serves every read path and the whole API, and it answers
conversations on the legacy `-butler-provider` path — `apiSrv.ButlerHost`
is wired unconditionally at `main.go:1053-1057`, outside the
`coordinating` branch. `apiSrv.Butlers` stays nil, so
`butlersConfigured()` returns false
(`keystone/api/handlers_butler_named.go:110-126`) and named Butlers are
bypassed rather than refused. It logs the situation loudly
(`main.go:512-517`).

That is honest about **durable** work — no task, step, attempt or trace
is written by a process that does not hold the lease, which is what the
ADR's comment claims and what this finding covers. It is not the same as
"performs no actions": the legacy path it falls back to can still
dispatch external effects, unfenced. That is **F-158-21**, and it is why
requirement 26 is `partial` while this finding is `implemented`.

### F-158-18 — the coordinator heartbeat is never written, so the evidence for promoting is stale by design

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/coordinator.go` and `keystone/cmd/keystone/main.go`; 569a284; `TestCoordinatorHeartbeatRecordsLivenessAndStopsOnPromotion`.
- **Audit status:** missing
- **Audit evidence:** `HeartbeatCoordinator` (`keystone/runtimedb/coordinator.go:157-168`) exists so that `heartbeat_at` is "evidence for a human deciding whether to promote". `grep -rn HeartbeatCoordinator keystone/` returns only its definition and `coordinator_test.go`. Nothing in `cmd/keystone` starts a heartbeat loop.

`heartbeat_at` is therefore only ever set by `AcquireCoordinator`
(`coordinator.go:60-80`) — at process start. `GET
/api/v1/coordinator` reports it, and an operator deciding whether the
other Keystone is gone reads a timestamp that says when it last booted,
not when it was last alive. A process that has been hung for six hours
and one that restarted six hours ago are indistinguishable.

Small, and it sits directly under the one manual decision
(`PromoteCoordinator`) that the whole fencing design depends on being
made correctly.

### F-158-19 — evidence is mandatory, but the runner writes its own receipt

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/butlersteps.go`; 5dd3ac8; `TestRunnerAuthoredDeliveryRowDoesNotSatisfyAnEffectfulObjective`.
- **Audit status:** partial
- **Requires:** §7: "The agent must not claim completion without a successful tool or scheduler receipt."
- **Audit evidence:** code as cited below — `keystone/runtimedb/butlertasks.go:432-437`, `keystone/runtimedb/butlersteps.go:680-731`, `keystone/runtimedb/butlertasks_test.go:209`; tests `TestButlerTaskCompletionIsRefusedBySchemaWithoutEvidence`, `TestButlerTaskInterruptedBetweenEvidenceAndCompletion`.

Three independent layers. `TransitionButlerTask` refuses to reach
`completed` at all (`keystone/runtimedb/butlertasks.go:432-437`).
`CompleteButlerTask` (`keystone/runtimedb/butlersteps.go:680-731`)
refuses an empty evidence set, validates every row, and writes evidence
and state in one transaction. A schema trigger refuses the transition
independently — asserted by
`TestButlerTaskCompletionIsRefusedBySchemaWithoutEvidence`
(`keystone/runtimedb/butlertasks_test.go:209`).

Wired end to end:
`Ledger.Complete` (`keystone/butlerstore/butlerstore.go:225-238`) is
called by `Runner.complete` (`keystone/butler/runner.go:665-703`), which
builds one evidence row per verified tool step plus a `delivery` row. A
crash between evidence and state leaves a running task with evidence
attached rather than a completed task that never delivered —
`TestButlerTaskInterruptedBetweenEvidenceAndCompletion`
(`butlertasks_test.go:233`).

Evidence rows carry a mandatory permission label and audience
(`keystone/runtimedb/butlersteps.go:625-643`,
`TestButlerEvidenceRequiresPermissionMetadata`), so the completion
record is filterable rather than a claim nobody can show safely.

**What is not enforced is the receipt.** §7 says "must not claim
completion without a successful tool or scheduler receipt". The schema
trigger (`0017_butler_tasks.sql:252-258`) requires only that *some*
evidence row exists, and `CompleteButlerTask` requires only a non-empty
list. `Runner.complete` (`keystone/butler/runner.go:697-702`) always
appends a `delivery` row — "an answer was produced and returned to the
person who asked" — written by the runner itself, before anything
confirms the answer reached anyone. So a turn in which the model called
no tool at all completes on a row the runner wrote about itself.

That is the right answer for a question needing no reads, and the runner
says so (`runner.go:665-670`). It is the wrong answer when a model
returns "I've restarted that service" without calling a tool: the task
completes, the Console shows `completed` with evidence, and no tool or
scheduler receipt exists. Nothing distinguishes the two cases, because
`delivery` is written unconditionally either way.

The schema's own comment already states the intent this misses —
migration `0017_butler_tasks.sql:22`: "evidence appropriate to the
objective, not merely a model saying". Closing it needs the evidence
kind to be constrained by what the objective claimed, or `delivery` to
become a real delivery receipt rather than a runner-authored row.
Recorded as `partial` rather than `implemented`: the structure around
completion is genuinely strong and the specific §7 guarantee is not
met.

Corrected after review on the PR (Codex, P1). It was first written as
`implemented`, which read the three enforcement layers as proving the
receipt when they prove only that a row exists.

### F-158-20 — a restart does not repeat an established read

- **Current status:** implemented
- **Current evidence:** `keystone/butlerstore/butlerstore.go`; 0baaa8d; `TestInterruptedRunResumesWithoutRepeatingReads`.
- **Audit status:** implemented
- **Audit evidence:** `Runner.Run` reads the step ledger before doing anything (`keystone/butler/runner.go:322-329`) and serves any tool call whose fingerprint is already verified from it rather than dispatching again (`keystone/butler/runner.go:596-612`). Fingerprints are JSON-normalized so argument order does not change the answer (lines 277-296), and travel *with* the stored result so a change to the scheme cannot match old rows to new questions (lines 322-329).

Freshness is bounded rather than assumed: `verifiedResults`
(`keystone/butler/runner.go:299-320`) drops a result older than
`ObservationTTL`, set to 15 minutes on the live path
(`keystone/butlerlive/serve.go:29-31`), so a conversation resumed
tomorrow re-reads rather than answering today's question from
yesterday's observation.
`TestStaleObservationsAreRefreshed`
(`keystone/butlerstore/retry_test.go:166`) covers the expiry;
`TestAConversationSurvivesARestartWithoutRepeatingADispatch`
(`keystone/butlerlive/butlerlive_test.go:456-534`) covers the whole
loop — model dies mid-turn, process restarts, the resumed slice re-asks,
and `get_health/n1` is asserted to have been dispatched exactly once
across both.

The mechanism is real and tested. What it lacks is anything that starts
a resume without a person typing (F-158-04, F-158-05), and any handling
of the dispatched-but-unknown case (F-158-03).

### F-158-21 — a process that does not hold the lease can still dispatch household actions

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/executionfence.go`; 569a284; `TestFenceIsOnTheStoreNotTheRoute`.
- **Audit status:** missing
- **Audit evidence:** `keystone/cmd/keystone/main.go` wires `household.Actions` at line 1036 and `apiSrv.ButlerHost` at line 1058 — both **before** and **outside** the `if coordinating` block that starts at line 1079.

The consequence follows from three facts that are individually
deliberate:

1. Without the lease, `apiSrv.Butlers` stays nil, so
   `butlersConfigured()` returns false
   (`keystone/api/handlers_butler_named.go:110-126`) and the request
   falls back to the legacy `-butler-provider` path — bypassed rather
   than refused, which is the upgrade path that check exists for.
2. That path is `runFamilySession`
   (`keystone/api/handlers_butler_household.go:204-230`), which builds a
   plain `butler.Session` — the **in-memory** loop. It holds no
   `TaskLedger`, so it never calls `Fence()`, never opens a task, and
   never reaches `StartButlerAttempt`.
3. `host.householdGateway(member)` carries the same `household.Actions`
   callback, so `domestic.Dispatch` is reachable from it
   (`keystone/cmd/keystone/main.go:1036-1051`).

So a second Keystone against the same database — the exact situation
`ErrCoordinatorHeld` exists to prevent — refuses to write a single
durable row and will still send a notification or a chat message on the
family surface. The lease bounds the ledger, not the effects.

The same gap swallows the stop control. `gate(GateDispatch)` is called
from `StartButlerAttempt` (`keystone/runtimedb/butlersteps.go:220-225`)
and `ClaimIntegrationWork` only. A path that opens no attempt passes no
gate, so restore quarantine — the household-wide stop of F-158-13 —
does not hold actions on the legacy path either. `db.gate`'s own comment
(`keystone/runtimedb/restore.go:134-146`) argues for putting gates on
the store rather than on HTTP routes precisely so a new caller is
covered the day it is written; the legacy session path predates the
store gate and was never brought under it.

Impact is bounded by how one reaches it: a second coordinator is an
operator error that logs loudly at startup (`main.go:512-517`), and the
family surface's action set is what `domestic.Gateway` was registered
with. It is still the one place where "one coordinator" and "stop
everything" are both true of the ledger and false of the world.

Raised by review on the PR (Codex, P1) and confirmed against the wiring;
F-158-17 originally claimed the opposite in its closing sentence.

## Design gaps

**The unit of durable work is the turn, not the task.** Every turn on
the live path opens its own task and completes it
(`keystone/butlerlive/serve.go:240-300`). That is defended in the code
and the defence is good: turn two must not serve turn one's readings out
of the ledger, because the house moved on between them. But three
things the baseline attaches to a *task* inherit turn granularity as a
side effect — the aggregate budget resets (F-158-06), an `uncertain`
outcome would be scoped to one turn, and "partially complete" would
describe a turn rather than the work someone asked for. The decision to
put to the owner is whether a longer-lived task object should sit above
the per-turn one, or whether the budget and status requirements should be
restated in terms of conversations. Fixing F-158-06 by making the budget
span turns without deciding this would just move the problem.

**Delegation is absent by construction, not by omission.** A4 isolation
is built structurally throughout `butlerscope.go` and `butlerlive`, and
delegation is the one feature that must breach it under control. That is
a design decision to take before anything is scheduled: whether the
household wants Butler-to-Butler delegation at all, given §16 also says
"shared knowledge retrieval does not require delegation" — which, once
the #156 memory-sharing finding is fixed, removes the most common reason
to want it.

**Clone and credential inheritance.** F-158-12: copying connection
grants makes a clone usable and matches "copy appropriate
configuration"; not copying them makes it safe and matches "no automatic
inheritance of another agent's credentials", at the cost of a copy that
does nothing until it is re-granted. The current code chose usable. A
third option — copy the grants but mark them pending an explicit
confirmation — is not implemented and would satisfy both readings.

**The stop control and the restore control are the same switch.**
F-158-13. Either the incident stop gets its own flag with its own
release path, or restore quarantine gets a second entry reason and a
release path that does not require a journal replay. Deciding that is
cheaper than discovering it mid-incident.

## Cross-slice

- **Memory is Butler-stamped, so "deleting an agent preserves memory"
  preserves rows nothing can read.** `RetireButler` correctly keeps
  `butler_memory` rows (`keystone/runtimedb/butlers.go:306-311`) and
  `ButlerImpactOf` counts them for the confirm dialog. But per #151
  finding 1, `RetrieveMemory` ANDs the Butler scope into its candidate
  SQL, so once the Butler is retired its notes are preserved and
  unreachable — the *letter* of §16 without the point of it. Belongs to
  **#156**; recorded here because it is the lifecycle path that makes it
  visible, and because #156's fix resolves it for free.
- **Routine execution, catch-up and approval integrity** — F-158-14
  establishes that nothing executes a routine and that the document
  carries no trigger. What the routine contract should look like is
  **#154**.
- **Revocation and cached context (AC06)** — `Ledger.Authorize`
  (`keystone/butlerstore/butlerstore.go:64-96`) rechecks task state,
  deadline and owner admission before every slice, and does **not**
  recheck the Butler's own connection grants or the tool authority the
  task was admitted under. Whether that is a gap is **#155**'s call.
- **Quarantine gates retrieval as well as dispatch**
  (`keystone/runtimedb/restore.go:124-132`), which is a privacy-boundary
  behaviour as much as an execution one — **#157** if it matters there.

## ADR amendments required

**ADR-W030 A4 — "a budget that resets on retry is not a budget".** The
statement is true of the store and false of the deployment: the live
path opens a task per turn, so the aggregate tool budget resets every
turn (F-158-06). The comments asserting the guarantee
(`keystone/butlerlive/serve.go:24-31`, `keystone/butler/runner.go:184-190`)
say "conversation" where the code means "task". Either the ADR is
amended to state the budget is per turn, or the code is changed to make
it per conversation — but the ADR must stop claiming a property nothing
holds.

**ADR-W030 A4 — the uncertain result.** A4 requires recording an
uncertain outcome rather than retrying a consequential effect. The
schema, the wait reason and the Console string are built; no code path
writes any of them (F-158-02, F-158-03). The ADR is not wrong, it is
unimplemented — but the *repository* currently reads as though it were
implemented, which is the specific failure #151 exists to stop. The ADR
should carry a note naming P-number for the unwired half until it lands.

**Baseline §7 supersedes ADR-W030 A4's state machine.** The implemented
set has seven states and no "partially complete" (F-158-01); the
baseline's six require it and treat "blocked" as a state rather than a
wait reason. Per the epic's ground rules the ADR is what changes: A4's
state table needs a `partial` state, or an explicit statement that
partial completion is carried by evidence rows and the ADR's mapping to
§7's six is documented rather than assumed.
