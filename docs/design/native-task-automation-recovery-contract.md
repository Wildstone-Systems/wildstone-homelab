# Native task and automation recovery contract

Status: code-backed contract and repository evidence for #272, under #166.
This is not management-independent recovery, off-host recovery, or HA
acceptance.

## Scope and distinct owners

Three product concepts use the word “task” but do not share one lifecycle:

| Concept | Owner and durable state | Execution semantics |
|---|---|---|
| Household commitments/tasks | Household commitment resources plus `commitment_*` runtime tables | Human-facing intent, bookings, completions, and announcement state. They are not executable Agent jobs and have no provider attempt lease. |
| Native Agent work | `butler_tasks`, step/attempt/wait/evidence/receipt/transition tables, and `conversation_work` in the runtime SQLite database; `agentgateway` and `butlerstore` enforce it | The native runtime is a per-run Node subprocess. Keystone owns handles, authority, checkpoints, receipts, budgets, and resume decisions. |
| Deterministic automation | Exact Owner-approved `routine_plan_versions`, `routine_runs`, transitions/effect receipts, household event stream, and `routine_execution_state` cursor in runtime SQLite | Keystone evaluates and dispatches reviewed occurrences. Kubernetes/Flux do not schedule individual rules. Scheduled Agent subscriptions use their separate `agent_subscription_*` leases, runs, and compare-and-swap checkpoints. |

The management runtime claim contains these SQLite records and the Statefile.
Uploads and private workspace bytes remain separate recovery groups. Flux owns
deployment reconciliation, not task execution.

## Recovery matrix

| State | Owner and persistent records | Restart/recovery behavior | Authorization and duplicate protection | Existing/new evidence |
|---|---|---|---|---|
| Task not started | Keystone; `butler_tasks.state=pending` and opening transition | Remains pending and appears in the durable task listing. Work starts only through a coordinator-held gateway. | Task authority is re-evaluated before retrieval/dispatch; `StartButlerAttempt` checks the live dispatch gate. | `TestButlerTaskRoundTrip`, `TestButlerTasksInStateIsTheResumeQueue`; restore coverage in `TestNativeTaskHistoryAndReceiptsSurviveRestoreAlongside`. |
| Task executing | Keystone/Agent gateway; running task, ordered steps, and attempt checkpoint written before dispatch | A verified step remains reusable. On startup, abandoned model attempts are fenced and may retry at the next task epoch. Abandoned tool/delivery attempts are fenced and parked as `uncertain-outcome`; they are not blindly repeated. | Current Person, Agent/runtime, connection, task, tool, restore, household-stop, and coordinator authority are checked at their production boundaries. Attempt epoch rejects late results. | `TestAConversationSurvivesARestartWithoutRepeatingADispatch`; new `TestInterruptedButlerAttemptsRecoverByExecutionBoundary`. |
| Task completed | Keystone; terminal task, evidence, successful effect receipt where required, transition history | Remains terminal. Verified reads are served from the ledger; identical effect receipt retries resolve to the existing immutable receipt. | Effectful completion requires a successful receipt linked through completion evidence. Terminal state cannot return to running. | `TestButlerTaskCannotCompleteWithoutEvidence`, completion schema tests, gateway restart dedup test; new restore-alongside receipt/history test. |
| Task attempt failed and retryable | Keystone; running task with failed/stalled/fenced attempt and unchanged durable step | Retry is a new attempt. A stall or recovery fence increments the task epoch; an old result is refused. A task transitioned to `failed` is terminal and is not itself retried. | Live authority and coordinator fence are checked again before the new attempt. | `TestButlerAttemptFencedByNewerEpoch` and new model-interruption recovery case. |
| Automation waiting for next occurrence | Reviewed routine executor; exact applied Plan snapshot plus monotonic `routine_execution_state.cursor` | Cursor and approved version survive restart. Only events after the cursor are considered; the first enablement adopts existing history. | `ApprovedRoutineVersions` requires the live routine document, revision, audience, Agent assignment, resource signature, and applied Owner-approved Plan to match exactly. | Existing reviewed-routine tests; new `TestReviewedRoutineRecoveryContract`. |
| Automation interrupted during execution | Reviewed routine executor; unique `(plan, trigger_event)` run and pre-dispatch `effect_state=claimed` | Startup changes a still-claimed running effect to `uncertain`, writes its receipt, and requires Owner resolution. It never treats a missing response as permission to dispatch again. Capability-blocked work may retry the same occurrence only before an effect claim. | Run boundary rechecks household execution and the exact approved snapshot. The unique run and one-way effect claim provide at-most-once dispatch within Keystone; no exactly-once external-side-effect claim is made. | `TestRoutineEffectClaimDoesNotRepeatAfterAStop`, capability recovery tests; new restart/uncertain/restore case. |
| Revoked user, Agent, capability, or routine resumes | Product authority records in SQLite and deletion/revocation journal where applicable | Revocation remains durable. Gateway/routine boundaries re-read current authority; restore starts quarantined and reconciliation replays later revocations before release. | Disabled principals cannot authenticate or mint; runtime revocation fences open tasks/subscription runs; disabled/revoked/edited routines no longer produce an approved execution version. | Principal/runtime/subscription/background-authorization restore tests; new revoked Owner promotion and disabled-routine restart/restore assertions. |
| Stale coordinator lease after restart | `coordinator_lease` row plus OS process lock and process-local acquired lease | Same stable deployment identity re-acquires the same epoch. A different identity serves recovery/read paths but cannot dispatch. There is no timeout takeover. An authenticated management Owner explicitly promotes, then Keystone restarts and acquires the promoted epoch. | Every household effect checks the process-local lease against the durable holder/epoch. Promotion increments the epoch; stale workers fail their next fence and late attempt results cannot commit. | `TestCoordinatorLeaseSurvivesRestart`, `TestTwoLiveProcessesCannotReacquireTheSameHolder`; new authenticated route/fencing test. |

## Synthetic fixture and exact boundaries

The reviewed automation fixture is a valid `Automation` document parsed by
`automation.ParseDraft`. It is created as a Wildstone-owned draft, submitted
as an exact routine Plan snapshot, and approved by a recorded Owner through
the supported product store workflow. Its deterministic `chat-test.send`
provider records calls only in memory. It performs no network call, model
inference, shell command, privileged action, or household effect.

Two interruptions are seeded through supported checkpoint methods:

1. `StartButlerAttempt` is committed and the fixture omits
   `FinishButlerAttempt`, modelling a stop after the native task's
   checkpoint-before-dispatch. No provider is invoked.
2. `ClaimRoutineEffect` is committed and the fixture omits dispatch and
   `RecordRoutineEffect`, modelling a stop at the reviewed routine's exact
   pre-dispatch boundary.

No test writes runtime tables directly or bypasses routine approval. Test
clocks are fixed and recovery is driven synchronously; no timing sleeps are
used.

## Guarantees and limits

- SQLite checkpoints, transitions, approved snapshots, cursors, waits, and
  receipts survive close/reopen and supported `BackupTo`/`RestoreBackup`.
- Completed verified native reads are not re-dispatched on resume. Effectful
  completion requires receipt evidence. Reviewed routine occurrences are
  unique by approved Plan and trigger, and a claimed unknown effect becomes
  uncertain instead of being retried.
- “At most one Keystone dispatch” is not “exactly once in an external
  system.” A crash after an external system accepts a request but before its
  receipt is recorded remains uncertain unless that integration supplies an
  idempotency/reconciliation contract.
- Restore-alongside opens on separate writable storage and in restore
  quarantine with no process-local execution lease. The tests inspect the
  preserved history without allowing the original and restored databases to
  drive the same provider.
- Coordinator promotion is deliberate and authenticated. It is not automatic
  failover or HA; the promoted process must restart to acquire its new durable
  epoch and run startup recovery.
- Household commitments remain durable human-facing records; this evidence
  does not claim they use the Agent attempt/receipt lifecycle.
