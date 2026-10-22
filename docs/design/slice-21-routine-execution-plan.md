# Slice 21 — reviewed routine execution (#222)

The current Automation document already has an event trigger, deterministic
conditions, an optional inference escalation, and a domestic action. The
Statefile engine deliberately suppresses review-managed routine identities;
this slice gives those exact approved Plan versions their own executor. It
does not infer authority from the routine name, proposer, or approving Owner.

## Execution path

1. Inside the coordinator process, scan household events with a separate
   durable cursor. On first start adopt the stream head, as the existing
   Automation engine does, so old events do not unexpectedly fire.
2. Load only approved, current Wildstone routine versions. Parse and validate
   the approved snapshot. A malformed or mismatched document blocks visibly;
   it never falls back to the Statefile copy.
3. Match its reviewed trigger and evaluate deterministic conditions. Record
   one run per `(Plan version, trigger event)` before any external effect. Each
   run carries its version, trigger, status, steps, and effect outcome. The
   household, not the proposer or approver, owns the occurrence. The existing
   `butler_tasks` `kind='routine'` cannot currently serve as that ledger:
   `author_person_id` and `owner_person_id` are both mandatory Person foreign
   keys, and offboarding acts on them. Reusing either human would violate this
   slice's survival tests. Use a household-owned occurrence ledger, with an
   explicit link to the approved Plan, rather than inserting a borrowed Person
   into the old table.
4. Check execution lease, household stop, current version/state, output
   recipient admission, and the required capability at every effect boundary.
   Render fixed parameters and execute deterministic work even if the assigned
   Agent's model is unavailable. An escalation requires that exact Agent and
   records a visible blocked/failed step if unavailable; it never substitutes
   another Agent or silently uses fallback wording.
   Reviewed external delivery currently requires a direct chat recipient with
   a live Connection. Opaque broadcast topics and tools without a resolvable
   output destination block. Granted memory is filtered for that Connection
   before model use and checked again before the effect leaves.
5. The domestic gateway retains the final coordinator/effect fence. A claimed
   effect is at most once; if dispatch returns an uncertain outcome, record it
   for Owner resolution rather than blindly replaying it. A confirmed effect
   remains recorded if a later stop halts the run.

## Lifecycle and controls

- Disable is an immediate Owner action and preserves the approved version;
  re-enable checks that it is still the same approved revision. Revoke ends
  that version's authority and requires a fresh Plan approval. Soft deletion
  ends execution while the object remains recoverable.
- Missing required capability marks the routine blocked without removing its
  definition. A later poll checks for the same compatible tool and records
  restoration before allowing further runs.
- Revocation of a delivery recipient pauses the routine for Owner resolution;
  it is not automatically restored by a capability probe.
- The household chose a dedicated emergency stop with its own Owner release
  path. Restore quarantine retains its deletion-journal replay requirement.
  Both must fence dispatch.

## Household decisions

- F-158-06: a durable conversation parent sits above the per-turn ledgers.
  New turns keep fresh evidence, while the parent carries the 64-call budget
  and unresolved outcome through later turns and restarts. An Owner can record
  outcome resolution or extend the budget in the Console API; resolution
  closes the ambiguous turn without replaying its effect.
- F-158-13: the emergency household stop has its own Owner release path.
  Releasing it never clears restore quarantine or skips journal replay.

This slice is delivered in one PR so the executor and its stop controls are
reviewed together. The existing event trigger is the reviewed trigger;
adding separate cron grammar would require a reviewed schema extension.
