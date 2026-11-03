# Restored coordinator promotion contract

Status: implemented repository contract for #286. Journal reconciliation and
quarantine release are #284/#285; controlled restart and execution recovery
remain #287.

## Authority and preconditions

`POST /api/v1/coordinator/promote` remains the sole promotion API. For a
database carrying a restore generation, it accepts only a fresh Console
session that resolves on the current request to one unambiguous, admitted,
active Owner. Administrator bearer credentials, Operators, Members, Agents,
stale sessions, disabled Owners and revoked credentials are not promotion
authority.

Promotion revalidates, inside the same immediate SQLite write transaction:

1. the nonempty current restore generation and exact restored snapshot;
2. completed conservative deletion/revocation replay;
3. explicit quarantine release for that generation;
4. the matching immutable `recovery_journal_reconciliations` row;
5. the canonical terminal evidence signature under the currently installed
   `recovery-journal.pub` and its signer fingerprint;
6. the snapshot prefix, terminal head, replay suffix provenance, independent
   custody, coverage and nonempty original-writer fence; and
7. the preserved current coordinator holder and epoch.

An Owner decision cannot replace missing or stale recovery evidence, and a
release decision from another restore generation cannot be reused.

## Durable transition and fencing

The transaction inserts one immutable `recovery_coordinator_promotions` row,
then advances the existing `coordinator_lease` from `(previous holder,
previous epoch)` to `(replacement holder, previous epoch + 1)`, and finally
records the matching projection in `restore_state`. The audit retains the
Owner, restore generation, journal evidence identifier, timestamp, and both
sides of the holder/epoch transition.

Schema triggers reject direct use of the ordinary lease promotion path for a
restored database, reject a promotion without the released generation-bound
evidence and original-writer fence, and make the decision immutable. Repeating
promotion is a conflict rather than another epoch increment. Existing
coordinator checks fence the old holder and epoch from committing further
work.

## Restart boundary

Promotion changes durable authority only. It does not populate the running
process's in-memory execution lease, start a task runner, reconcile a mailbox,
dispatch a tool, or resume an automation. The database handle that records the
decision cannot acquire it. Only a newly opened handle after the controlled
Keystone restart may call the existing `AcquireCoordinator` path for the exact
promoted holder and epoch.

Repository tests prove that the decision and epoch survive database reopen,
but #286 does not perform a Kubernetes restart or run native startup recovery.
#287 owns the controlled singleton restart, acquisition and end-to-end stale
execution/duplicate-dispatch proof. Completed receipts and conservatively held
uncertain effects remain unchanged by promotion.

No repository test in #286 authorizes a live Kubernetes change, release or
promotion of the retained #277 installation, or real Agent/automation
execution.
