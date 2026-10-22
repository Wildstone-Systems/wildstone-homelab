# Slice 18: household execution fencing (#219)

Household effects require authority held by this DB handle, not the presence of
somebody else's coordinator row. The shared store checks the acquired holder and
epoch, then the existing dispatch quarantine. Promotion stops the old process at
its next boundary; no timeout elects a replacement. Permitted reads still work.
An OS lock prevents two live processes with the same stable deployment ID from
both acquiring authority. It is released on close/crash; ordinary restarts retain
the epoch. Lock files remain beside the database to prevent unlink/recreate races.

## Effect inventory

| Caller | Final boundary |
| --- | --- |
| Legacy and named household Butler, deterministic automation | Domestic Gateway checks before confirmation and dispatch; concrete executors also check |
| ntfy notification | Notifier checks again immediately before HTTP send |
| Calendar create/update/delete | CalDAV write hook checks after any lookup and before HTTP mutation; shared Homepage CalendarBook uses the same hook |
| Chat send/ask, replies, invitations and stalled-task notices | Store EnqueueChatOutbound checks every kind; ChatOutboundAfter checks before handing queued content to an adapter |
| Homepage and Butler file upload/rename/delete | FileStore requires a write-authority callback; streamed upload rechecks before publication |
| Integration adapter action work | ClaimIntegrationWork checks before handing work to the adapter |
| Durable Butler attempts | Existing StartButlerAttempt lease/dispatch checks remain |

An already dispatched HTTP request cannot be recalled. Adapter work already handed
off follows its existing claim/receipt protocol; this slice prevents further claims
or queue delivery after a stop. It does not claim distributed cancellation of an
external service's in-flight request. Infrastructure reconciliation remains under
its existing Plan authority, outside household domestic execution.

The coordinator heartbeat now runs every 30 seconds and updates only the exact
holder/epoch it acquired. Promotion ends the old heartbeat. This fixes F-158-18
without automatic takeover.

## Validation

Regression tests first reproduced missing store authority and duplicate live
same-holder acquisition. Positive controls retain normal coordinator effects and
read access without a lease. Negative cases cover unclaimed handles, another
holder, quarantine, promotion, queued delivery, stopped streaming uploads, and
calendar authority changing during lookup. Existing effectful test fixtures now
explicitly acquire authority, preserving their original admission/disclosure tests.

Mutation snapshots remove the store guard, FileStore guard, and CalDAV final-write
hook separately. Each compiles and fails its negative test; the store mutation
also permits real legacy HTTP notification and chat executor calls in all three
forbidden states. Full Linux vet/race validation and PR review follow integration.
