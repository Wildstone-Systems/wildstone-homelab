# Journaled workspace publication

ADR-W047 / #478 is dormant Keystone machinery. It registers no tools, HTTP
routes, grants, provider configuration or configurable quota policy. Callers
supply trusted workspace/task identity, inherited privacy/provenance, a current
settings loader, and a mandatory current resource-authority callback. The
callback must check originating Person, Agent, task, surface, grants and privacy;
this package does not replace #475's resolver. Missing authority fails closed.

`Begin` binds an immutable session to those inputs, the logical path, expected
base revision and configuration revision. Begin keys are scoped to task and
operation; changed arguments conflict. Chunks are consecutive, at most 1,024
UTF-8 bytes each, no NUL, at most 10,240 chunks and the snapshotted file ceiling.
A SQLite transaction reserves scratch bytes and journals each chunk's random
object identity/hash/size before `StageObject` writes it. Exact retries verify
the original object. Pending chunks cannot be skipped or committed. Session
and chunk rows contain metadata, not file contents.

`Commit` assembles only durable verified chunks. One database transaction
checks the exact chunk count/size and expected base, creates immutable revision
facts, stores provenance, reserves assembly capacity and seals the session.
The existing unique pending-publication index serializes competing writes to
one artifact across coordinator instances; a stale contender conflicts.
Bounded local striped locks serialize individual calls, never a model wait.

The coordinator then persists and verifies assembly bytes, advances
`staging -> prepared`, exclusively publishes bytes, records `bytes_published`,
and commits revision visibility, reservation consumption, receipt and session
state in one SQLite transaction. That last transaction covers **only metadata**.
SQLite uses explicit FULL synchronous WAL connections; filesystem objects and
directories are fsynced independently. There is no cross-store transaction.

Random object identity is durable before every byte write. A collision or
uncertain publication always retains that identity; retry rehashes and fsyncs
the exact object before advancing. Missing/corrupt/unavailable storage parks the
operation without success. Current settings, resource authority and process
execution fences are checked again at effects and before receipt reuse. Metadata
transitions compare the acquired coordinator epoch under SQLite's write lock.
The original receipt is returned after reauthorization, including a committed
session retried with another unused Invoke key. Reusing a key for a different
session conflicts. Public metadata reads still expose only committed revisions.

## Recovery and accounting

An authorized `Commit` retry can finish any durable journal boundary. Startup
has no fresh resource-authority decision for each task, so before dispatch the
active Keystone coordinator calls `RecoverWorkspaceWrites` to roll back
unsettled S05 metadata. It never claims recovered publication success, emits a
notification, or creates a receipt. Already committed writes and original
receipts remain settled. Incomplete open sessions remain resumable scratch.
Quarantine/coordinator fences refuse recovery just as they refuse execution.

Rollback marks unsettled publication/session rows aborted. It preserves prior
committed revisions, all byte objects and held reservations, including an
uncertain published object. A fresh authorized create can reuse an aborted
pending artifact at the same path without deleting its journal/history. The
aborted session itself cannot resume. Retained bytes remain charged until the
later lifecycle slice explicitly reconciles and removes them. Chunk copies are
separately charged scratch; assembly staging/revision hard links share one
reserved object. There is no automatic deletion or quota-policy expansion.

## Migration and verification

Migration **0100** is allocated to #478. Required feature merge order is
0098 (#490/PR519), 0099 (#475/PR518), then 0100 (#478/PR523). Tests against a
checkout lacking the first two are isolated S05 evidence only, not a deployable
migration-chain claim. No placeholders stand in for missing migrations.

Tests cover concurrent same-base edits, original receipts, key/argument
conflicts, chunk restart/retry and scratch pins, size bounds, empty files,
crashes around staging/publication/database commit, conservative repeated
startup rollback, corrupt/missing bytes, cancellation, authority revocation,
configuration changes and process promotion. Storage tests separately exercise
actual fsync faults and descriptor/mount substitutions. Fixture results do not
claim live runtime conformance or close any epic release gate.
