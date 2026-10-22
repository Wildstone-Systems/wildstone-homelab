# Slice 19: objective-aware completion evidence (#220)

The store defaults missing and unknown objective classifications to effectful.
Trusted task creation can explicitly mark informational work. Informational
completion still needs existing delivery/observation evidence; effectful
completion needs an independently recorded successful tool, scheduler or executor
receipt. The store checks inside the evidence/state transaction, and a schema
trigger refuses bypasses. Historical completed tasks remain historical records.

Receipts are immutable and bound to a task, source, outcome, permission label and
audience. Completion evidence references them without widening their disclosure.
Tool receipts additionally require an actual verified tool step and successful
attempt. A model step or response/delivery evidence cannot act as a receipt.
Failed and uncertain outcomes do not complete work. A receipt from another task
cannot be borrowed. Scheduler/executor writers are trusted runtime seams, not
model tools or public request fields.

## Chat classification

The household accepted a separate verification call to the already assigned
model, examining the original objective, current request and answer for required
or claimed effects. Verification is tool-free, checkpointed through the same
runner/provider path, and rechecks current authority. Unknown, malformed,
truncated or tool-calling verdicts remain effectful. No alternate model or cloud
fallback is introduced. This adds one verification round and its model usage to
a normally completed chat turn.

The verifier classifies meaning; it cannot certify that an effect happened.
Only structured executor confirmation from a successful mutation supplies a tool
receipt. Reads, proposal-only tools, rejection, pending confirmation, and text
that imitates a receipt cannot supply one. Notification HTTP acknowledgements,
calendar/file mutations, stored memories and meter readings carry confirmation.
Explicit proposal-only objectives are still effectful, but require a distinct
proposal receipt. The API supplies it only after the shared proposal pipeline
persists the Plan; a proposal receipt cannot certify execution of the change.
The same distinction is carried by proposal-producing domestic tools.

Chat enqueue alone remains unconfirmed until an actual delivery receipt exists;
this slice does not invent delivery confirmation from queue acceptance.

The real serving path verifies classification before completion. When an action
has no successful receipt, the task waits and the person receives an explicit
unverified-action message instead of the model's unsupported success claim.

> **Amendment (2026-09-25).** "The task waits" is right only when something
> *might* have happened. Waiting records an uncertain outcome, and that holds
> the whole conversation (`conversation_work.uncertain_outcome`) until a person
> reviews it, so every later tool call in the conversation is refused. When no
> tool that can change anything ran in the task — every tool step, if any,
> names a tool the manifest marks read-only (the capability's own `Mutating`
> declaration) and reported no effect — the outcome is not uncertain: the
> request was not done. The task then ends `failed` ("not done"), the
> conversation is not held, and the person sees the model's own answer when
> the verifier says it only declines, asks or explains
> (`answerClaimsEffect: false`), or a fixed "I wasn't able to do that — nothing
> was changed." when it claims the effect or the claim cannot be judged. The
> receipt rule is unchanged: an effectful objective still never *completes*
> without one.

The Console distinguishes action confirmation receipts from delivery/observation
evidence. Existing trusted task factories may explicitly classify informational
work; omitted classifications remain effectful.

Remaining: integrate prior slices, finish full validation and mutation checks,
and complete PR review rounds.

## Tests so far

The initial regression completed a restart objective from a runner delivery row;
it failed before the store fix and passes afterward. Focused tests cover ordinary
informational completion, raw SQL refusal, scheduler/executor success vs failure
and uncertainty, unknown classification, tool vs model steps, unrelated-task
receipts, disclosure metadata, atomic rollback, and existing interrupted-completion
recovery. The complete store and Person-reference tests pass, as do the focused Linux
race tests. Separate compiling mutations removing the Go check and SQL trigger
are both caught. This is not a claim that live integration or the full Keystone
suite passes yet.

## Required-effect coverage

The verifier enumerates each requested or claimed effect and binds it to an
actual operation's step identity, tool, parameters, and confirmed result. An
uncovered effect retains an empty binding. The store and schema require a
successful same-task receipt for every binding, not merely one arbitrary
success somewhere in the task. Unknown/empty coverage remains incomplete.
Trusted scheduler/executor callers must also register the effects and source
identities they can confirm. A proposal receipt is tied to its originating
verified proposal step; it cannot certify applying the proposed change.

Natural-language coverage remains a judgment by the assigned tool-free model,
not a formal proof of intent. Structured operation metadata and receipt checks
prevent invented, unrelatedly keyed, missing, or partial evidence from passing
the completion boundary. No user-facing approval step is added.

Chat sends carry the runner-assigned task and operation identity through the
outbound queue. Enqueue is pending; the adapter acknowledgement atomically
creates a delivery receipt. If privacy replaces the queued body, that receipt
cannot certify the original send. A continuation can complete against the
acknowledgement without sending again. Partial proposal submission similarly
retains every successful Plan/operation pair while leaving missing effects open.

## Mixed effects and acknowledged continuation

Each required effect carries its own proposal purpose, so a request to remember
something and submit a Plan can finish with both kinds of proof. Neither receipt
can substitute for the other. Successful partial proposal persistence is retained
for later completion. Missing siblings keep the task waiting.

Verification input includes persisted executor acknowledgment for the exact tool
step. A resumed pending chat send can therefore use the existing delivery receipt
without sending again. The adapter acknowledgment and receipt are transactional;
a privacy replacement does not certify the original message.
