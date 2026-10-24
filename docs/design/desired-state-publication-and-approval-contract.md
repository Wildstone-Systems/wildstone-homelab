# Desired-state publication and bounded-approval contract

- **Issue:** old private issue 521
- **Coordinator:** [#130](https://github.com/Wildstone-Systems/wildstone-homelab/issues/130)
- **Evidence base:** `e33a88036231e4c94ef95e6a2dbaffe985fcfdd4` (`main`, 2026-10-16)
- **Status:** accepted 2026-10-24 by the maintainer (#157), with the §8 answers recorded in ADR-W007 A8
- **Amends:** ADR-W007 A4/A6 (A8) and ADR-W008 A1/A2 (A2a)
- **Delivery:** 0a immutable reads (done), 0c bounded approval (done; risk recorded but not compared, see ADR-W008 A2a), then 0b shared publisher

This proposal defines the selected foundation boundary authorized by #580:
the immutable desired-state revision Keystone accepts, how an approved change
is published across Git and SQLite, how interruption is recovered, and the
exact effects an infrastructure Plan approval permits. It is deliberately
narrower than #521's full architecture tracker and #566's wider audit.

No runtime code, migration, deployment, optional Ansible/OpenTofu backend, or
new resource kind is authorized by this document. Existing accepted ADRs
continue to govern until the maintainer accepts and amends them.

## 1. Current evidence and the unresolved boundary

Current main has the intended architectural pieces, but their consistency
boundary is not explicit:

1. `keystone/proposals` stores `BasedOnHead`, the exact desired documents and
   the review-time `Plan` in one `plan.StoredProposal`.
2. `api.approvePlan` compares `State.Head()` with `BasedOnHead`, mutates the
   Statefile working tree with `Put`/`Delete`, commits Git, writes
   `statefile_head`, and only then transitions the Plan to `approved`.
3. `api.applyAuthored` is a second intentional API writer for Meter and
   Commitment tracker edits. It preserves their existing no-Owner,
   non-host-executing semantics with an `AuditOnly` Plan, but performs a
   separate `Put`/`Delete`, Git commit, `statefile_head`, and Plan transition
   sequence.
4. Those filesystem, Git and SQLite steps are not one transaction. A failure
   between them can leave a dirty tree, a Git commit without a matching DB
   marker, or accepted desired state whose Plan record is still `proposed`.
   The audit-only writer can likewise leave a committed tracker edit whose
   record is still non-terminal.
5. `statefile.Load()` and the reconcile loop read mutable working files. A
   valid but uncommitted edit can therefore affect a pass even though no
   immutable revision or approval names it.
6. The out-of-band ingestor detects committed HEAD changes, but its first
   adoption validates schema only, not standing policy. It and the approval
   and audit-only handlers are separate Statefile writers.
7. `reconcile/loop.Tick` treats every `approved` or resumed `applying` Plan as
   a single Boolean authorization for Medium/High work. It converges the
   current whole tree, then gives all queued Plans one uniform verdict.

These are verified code paths. Concurrent-approval, crash and unrelated-drift
failures are inferred risks from those paths, not reproduced incidents.

## 2. Proposed decisions

### D1. The accepted revision is the only infrastructure intent

Keystone records one `AcceptedRevision` containing an immutable Statefile Git
commit SHA and its canonical tree digest. Infrastructure reconciliation,
proposal comparison and desired-resource reads use that commit through
`LoadAt(revision)`. Neither the checked-out branch, index nor working files are
authority.

```text
AcceptedRevision {
    revision        git commit SHA
    treeDigest      digest of canonical resource paths and bytes
    parentRevision  previously accepted SHA
    publicationID   durable publication journal identity
    acceptedAt      database timestamp
}
```

The runtime database is the selection authority for which immutable Git object
is accepted. Git remains the durable, human-readable resource history and
escape hatch. The database does not become a second copy of resource YAML.

An accepted revision must resolve locally, match its recorded tree digest, and
pass schema and standing-policy validation. Failure of any check places
infrastructure reconciliation in explicit quarantine; Keystone must not fall
back to mutable files, a nearby branch head, or an older convenient commit.
Every prepared or accepted candidate also has a durable internal Git ref under
a reserved Keystone namespace. The human-facing branch is not the only
reachability root for authoritative objects: internal refs keep candidates
reachable through normal Git garbage collection and inclusion in
`git bundle create --all`.

Runtime-DB facts projected into a reconcile pass remain DB-owned. A mutation
that can change projected host effects (including a service entitlement,
Person-group membership or calendar grant) records a durable projection
decision and advances a projection generation in the same SQLite transaction.
Before dispatch, Keystone resolves that decision against one accepted revision
and one DB checkpoint into an immutable, server-derived projection envelope.
The pass records both selectors in its result so an operator can tell which
immutable intent and runtime facts were used. This proposal does not move
grants, household data or status into Git or force those existing governed
actions through an infrastructure Plan.

### D2. One coordinator serializes every desired-state publication

API approval, the authorized household audit-only writer, and accepted
out-of-band commits use one `DesiredStatePublisher`. They do not call
`State.Put`, `State.Delete`, `State.Commit`, `SetStatefileHead`, and
`TransitionPlan` independently.

The coordinator accepts:

```text
PublishRequest {
    publicationID
    source              api-plan | household-audit | out-of-band
    planID
    actor
    expectedRevision
    proposed documents  exact puts and deletes, or an exact candidate commit
    approvalEnvelope
}
```

It produces a candidate commit from the accepted tree without first exposing
candidate files as live intent. Publication uses compare-and-swap semantics:
`expectedRevision` must still be accepted when the SQLite acceptance
transaction commits. A stale or concurrent request is superseded and must be
reviewed again; it never rebases approval onto newer intent.

Before a request can become durably `prepared`, the publisher creates its
candidate object and compare-and-swap creates a per-publication internal ref
pointing to that exact SHA. A candidate object created before its ref is not
authority and may be rebuilt after a crash. The acceptance transaction verifies
that the internal ref still resolves to the recorded SHA and digest. This makes
the Git object reachable before SQLite can select it without pretending Git and
SQLite share one transaction.

Only one publication may be in the acceptance transition at a time. The
serialization mechanism must work across HTTP handlers, ingestion ticks and
process restart. An in-memory mutex alone is insufficient; the durable journal
and SQLite transaction provide the cross-restart fence.

`household-audit` retains the existing immediate Meter/Commitment editing
contract and produces no host-execution authority. It shares publication
serialization and recovery because it moves the same Git history, not because
it has been converted into an infrastructure approval workflow.

### D3. Acceptance is a durable journal, not an assumed call sequence

The publication journal has these states:

```text
prepared -> accepted -> mirrored
       \-> abandoned
```

- `prepared`: the request, expected revision, candidate SHA/tree digest and
  approval-envelope digest are durable, and the candidate is reachable through
  its per-publication internal Git ref. It grants no execution authority.
- `accepted`: one SQLite transaction compared the current accepted revision
  and selected the candidate revision. For infrastructure publication it also
  persisted the approval envelope and moved the Plan to `approved`; for an
  authorized household audit-only publication it moved that record to
  `applied` without creating host-execution authority. This is the publication
  point and, only for the bounded infrastructure envelope, the authorization
  point.
- `mirrored`: the human-facing Statefile branch/worktree and deduplicated
  notification/event have caught up. Mirroring is repairable presentation,
  not a second acceptance point.
- `abandoned`: a prepared candidate lost compare-and-swap, failed validation,
  or was explicitly superseded. It never becomes reconcile input.

Creating a Git commit object may precede `accepted`; merely creating that
object or moving a non-authoritative worktree does not authorize it. The
acceptance transaction must not point at an absent or unverified commit.

Internal refs are lifecycle records, not an alternate desired-state selector.
The per-publication ref remains while its journal, Plan, approval/projection
envelope, execution receipt or recovery retention can name the revision. An
`abandoned` ref may be deleted only after the durable state proves that no
accepted revision or live authority references it. Superseding an accepted
revision does not immediately make its ref collectible: applying work, audit
history and the configured recovery retention still fence deletion. Ref
creation/deletion uses compare-and-swap and is reconciled idempotently at
startup; it never moves an existing publication ID to a different SHA.

`statefile_head` is replaced or redefined as the accepted revision record. It
must not ambiguously mean both “branch HEAD last observed” and “revision safe
to execute.” Branch observation belongs in separate ingestion metadata.

### D4. Approval is an exact effect envelope

An infrastructure Plan approval names all of:

- the proposal's base accepted revision;
- the candidate accepted revision and tree digest;
- the exact changed resource identities;
- each allowed desired-state action (`create`, `update`, or `delete`);
- the reviewed before/after resource digests;
- the maximum reviewed risk per entry; and
- the Plan/publication identities that supplied the authority.

```text
ApprovalEntry {
    kind
    name
    action
    beforeDigest
    afterDigest
    maxRisk
}
```

The envelope is derived from trusted parsed resources, not accepted from
caller-authored digests. Its canonical digest is stored with the publication
and approval record.

At reconciliation, every Medium/High step must match one live infrastructure
approval entry or one live projection-authority entry: same resource identity,
action, desired digest and risk no greater than the governing decision. A
missing resource that turns a reviewed `update` into a `create`, a newly
observed dependency effect, a changed target, or a higher risk is not silently
covered. It becomes a fresh pending preview unless the exact effect is derived
from a current authorized projection decision as described in D5.

Low-risk drift may retain ADR-W004's existing independently authorized repair
policy, but its result is attributed to drift repair, not to an unrelated
human Plan. Revocation, restore quarantine, ownership checks and Wardstone's
typed operation boundary remain additional live denials; an approval envelope
never overrides them.

### D5. Level-triggered convergence stays; blanket tick authority does not

The reconciler still observes current reality and computes a fresh diff toward
the accepted revision. Stored Plan steps remain review evidence rather than an
imperative script. The change is the authorization predicate:

```text
allowed(step) = lowRiskDriftPolicy(step)
             OR matchingLiveApproval(acceptedRevision, step)
             OR matchingProjectionAuthority(acceptedRevision, dbCheckpoint, step)
```

Several applying Plans may contribute entries to the same pass. Their union
does not authorize any step outside those entries. Each executed or blocked
step records the Plan entry that authorized it, and each Plan receives its own
result. One failed target must not give every queued Plan the same verdict.

DB-owned projections use the third predicate, not an infrastructure Plan. The
existing public API authorization and step-up rules govern the household
decision; entitlement seeding from an accepted package manifest is bound to
that accepted revision instead. The SQLite transaction records the actor or
seed provenance, decision kind, narrowly affected
Person/group/capability/calendar identities and a monotonic projection
generation. The reconciler deterministically resolves only the fields owned by
that projector, then durably records exact resource/action/before-and-after
digests and maximum risk as a `ProjectionAuthority` before any dispatch. That
envelope is bound to the accepted revision and DB checkpoint used to derive it.

A projection envelope cannot authorize a base Statefile field, another
projector's field or unrelated drift. A changed accepted revision or projection
generation fences dispatch and requires recomputation from the still-current
governing decisions. Superseded grants authorize nothing; revocations and
offboarding decisions produce their exact removal effects and remain eligible
without a second Owner approval. Crash recovery may resume only an envelope
whose accepted revision, checkpoint and source decisions are still current.
Each projected effect records its projection decision/envelope instead of a
Plan ID as the authority receipt.

An applying Plan survives restart only with its persisted approval envelope
and candidate accepted revision intact. A Plan tied to an older accepted
revision may continue to authorize its still-current exact resource outcome;
if a later accepted revision changes that resource or action, the older entry
is superseded for that target rather than widened.

### D6. Execution failure does not rewrite accepted intent

ADR-W008's level-triggered rule remains: a host-operation failure does not
rewind accepted desired state. Recovery or rollback of an individual typed
effect is not desired-state publication rollback. Abandoning or reversing an
accepted intent is a new Plan against the current accepted revision.

Publication recovery may finish a missing branch mirror or event, but it may
not manufacture a new accepted revision, widen an approval envelope, or mark
host effects successful without observation.

## 3. Interruption and recovery contract

Startup recovery runs before infrastructure reconciliation or new publication.

| Interruption point | Durable evidence | Required recovery |
| --- | --- | --- |
| Before a candidate commit/ref exists | no accepted journal state | Leave current accepted revision unchanged; abandon or retry the prepared request idempotently. |
| Candidate ref exists, no journal row | orphan per-publication ref, no authority | Verify that no accepted revision or live record names it, then remove the orphan ref with compare-and-swap or recreate the same idempotent request. Never reconcile it. |
| Candidate object/ref exists, DB still `prepared` | candidate SHA/digest and per-publication internal ref, no authority | Verify ref, object and digest; resume the same publication ID or abandon it. Never reconcile it. |
| During the SQLite acceptance transaction | transaction either commits or rolls back | Re-read journal and accepted revision. There is no partially accepted DB state. |
| DB is `accepted`, branch mirror is old | accepted SHA and envelope are authoritative | Verify the object, reconcile only from it, then repair the mirror with compare-and-swap. Do not overwrite a divergent human branch. |
| Branch mirror moved, response/event missing | accepted journal plus branch state | Return the already accepted result and emit the event/delivery once using publication ID deduplication. |
| Crash while Plan is `applying` | accepted revision, envelope and per-step receipts | Re-observe, recompute and resume only matching effects. Never replay stored steps blindly. |
| Crash with projected effects pending | projection decision/generation, derived envelope and accepted revision | Revalidate all three selectors; resume only exact still-current effects, otherwise supersede and recompute. A revoked grant never regains authority from an older envelope. |
| Accepted internal ref, SHA/object or digest is missing after restore | inconsistent recovery set | Keep restore/infrastructure quarantine active and require operator recovery; no fallback revision. |

The whole-site recovery set must contain the Git objects for the recorded
accepted revisions, their reserved internal refs and the SQLite record that
selects them. The Git bundle explicitly includes the reserved ref namespace;
restore verification checks ref-to-SHA-to-digest-to-DB linkage before releasing
reconcile quarantine. Capturing a coherent whole-site snapshot remains
coordinated recovery work; this proposal only defines the consistency assertion
it must satisfy.

## 4. Out-of-band Git editing

The Statefile remains user-editable, with these boundaries:

1. Dirty or uncommitted working files are never reconcile input.
2. The ingestor compares committed branch history with separate observation
   metadata, then validates the exact candidate tree against schema and policy.
   Initial adoption receives the same policy validation as later adoption.
3. A valid fast-forward candidate is published through the same journal and
   bounded envelope. Existing ADR-W007 authority for a holder of repository
   access may remain auto-approval policy, but it is no longer an alternate
   write path.
4. Invalid candidates leave the accepted revision unchanged. A restoring
   commit may repair the editable branch without pretending the invalid tree
   was ever accepted.
5. Divergent or non-fast-forward history is explicit conflict state. Keystone
   does not merge, force-update, or infer which history the person intended.

## 5. Plans outside infrastructure desired state

Research configuration, exact routine versions, household settings,
subscriptions and `AuditOnly` tracker edits already have distinct owning
stores and approval/apply paths. Service entitlements, Person-group membership
and calendar grants likewise retain their existing public API authorization;
their narrowly derived projection authority is not an infrastructure approval.
These paths do not gain blanket infrastructure authority and must not create
empty approval envelopes merely to pass through the reconcile loop. An
`AuditOnly` edit that intentionally publishes Meter or Commitment resources
uses the shared publication coordinator only to serialize and recover the
Git/SQLite revision change; it remains immediately applied and cannot authorize
a host operation. This contract does not force all household actions into an
infrastructure Plan.

Self-driven backup and secrets records likewise keep their owning state
machines. The reconcile loop continues to exclude them rather than treating
their transient `approved` state as infrastructure authority.

## 6. Required interface changes if accepted

This table is a review boundary, not permission to implement all rows in one
PR.

| Area | Current interface/evidence | Required contract seam |
| --- | --- | --- |
| `statefile` | `Put`, `Delete`, `Commit`, mutable `Load`, immutable `LoadAt`; recovery bundles refs through `--all` | Prepare/verify an immutable candidate from an expected revision; maintain compare-and-swap internal refs for prepared/accepted candidates; load accepted resources only by revision; mirror accepted history without making the worktree authoritative. |
| `runtimedb` | separate Plan transitions and `statefile_head` update; DB-owned entitlement/calendar decisions are projected during reconcile | One transactional compare-and-swap that records accepted revision, journal state, envelope and Plan approval; idempotent lookup by publication ID. Separately, persist projection decisions/generation and exact derived projection envelopes without granting general Plan authority. |
| `plan.StoredProposal` | `BasedOnHead`, exact puts/deletes and review Plan | Rename/interpret base as accepted revision; persist canonical candidate/envelope identities derived server-side. |
| `api.approvePlan` | direct filesystem, Git and DB sequence | Call the shared publisher; return an idempotent accepted/superseded/recovery result. |
| `api.applyAuthored` | separate optimistic filesystem, Git and DB sequence for Meter/Commitment `AuditOnly` edits | Call the same publisher with non-authorizing `household-audit` provenance; retain immediate household semantics and a terminal audit record. |
| `ingest.Ingestor` | separate HEAD marker and auto-approved Plan sequence | Observe committed candidates, then call the same publisher and conflict rules as API approval. |
| `reconcile/loop` | mutable `Load`; DB-owned entitlement/calendar projection; tick-wide `allowMediumHigh`; uniform Plan verdict | Load the accepted SHA; resolve projection-owned fields at a recorded DB checkpoint; match each Medium/High step to an infrastructure or exact projection envelope; attribute receipts and outcomes to the governing Plan or projection decision. |
| restore | Git bundle and DB restored under quarantine | Verify accepted SHA/tree digest/envelope linkage before reconcile release. |

The first implementation should split at least into immutable reads,
serialized publication/recovery, and bounded execution authorization. It must
not combine this foundation with optional provisioning backends or a platform
layering rewrite.

## 7. Testable acceptance for later implementation

### Immutable intent

- A valid uncommitted YAML edit cannot change proposal comparison,
  reconciliation input or observed desired revision.
- A committed but unaccepted branch edit cannot change runtime behavior.
- Every reconcile result exposes the accepted revision it used; the loaded
  tree digest matches the accepted record.

### Serialized publication

- Two approvals based on the same revision race deterministically: exactly
  one compare-and-swap accepts, and the other is superseded without publishing
  any of its files or authority.
- API approval, an authorized household audit-only publication, and
  out-of-band ingestion racing from the same base have the same property.
- Fault injection at every row in the recovery table restarts to either the
  old or candidate accepted revision, never a mixed tree or silent widening.
- Repeating a request with the same publication ID returns the same result and
  does not create a second commit, Plan transition, event or delivery.
- Running Git garbage collection after branch divergence cannot prune a
  prepared or accepted candidate. A recovery bundle/restore preserves the
  reserved internal ref and verifies the same SHA and tree digest selected by
  SQLite.
- Meter and Commitment edits retain their authorized immediate behavior, but
  interruption cannot leave an accepted revision with a non-terminal
  `AuditOnly` record, and their publication grants no reconcile authority.

### Bounded approval

- An approved container update on one node cannot authorize unrelated
  Medium/High drift on that node or another node.
- A reviewed update that recomputes as create/delete or exceeds reviewed risk
  waits for a new preview; no host operation is dispatched.
- Multiple approved Plans in one pass authorize only the union of their exact
  entries and receive independent outcomes.
- An authorized entitlement or calendar grant/revocation produces an exact
  projection envelope and can apply its Medium-risk directory/config effect
  without an infrastructure Plan; the same envelope cannot authorize any
  unprojected Statefile field or unrelated drift.
- A projection generation/checkpoint change before dispatch fences the old
  envelope. Restart resumes only exact effects whose source decisions are still
  current, and an older grant cannot outrank a newer revocation or offboarding.
- Restart during `applying` resumes only matching entries. Stale, superseded,
  revoked and quarantined authority dispatches nothing.
- Low-risk drift that existing policy independently permits remains possible
  and is reported separately from Plan execution.

### Recovery and compatibility

- Restoring a DB whose accepted SHA is absent from the Git bundle keeps
  quarantine active with a specific diagnostic.
- A complete matching Git/DB recovery resumes from the accepted revision and
  does not adopt worktree or branch differences.
- Existing Plan history and Statefile commits remain readable. Migration does
  not mark old `proposed` Plans approved or invent envelopes for unresolved
  work.
- Console, CLI and Butler continue through the public API; only a human Owner
  can approve an infrastructure Plan; Wardstone receives only typed ops.

## 8. Review decisions still required

1. Confirm SQLite as the selector of the accepted Git commit, with a
   human-facing branch as a recoverable mirror rather than authority.
2. Confirm that out-of-band repository access retains ADR-W007's auto-approval
   status after it passes the shared publisher, schema and policy gates.
3. Confirm the strict action rule: an approved `update` does not cover a
   recomputed `create` or `delete`, even when the final desired spec is the
   same.
4. Confirm per-Plan outcomes when several approval envelopes participate in
   one level-triggered pass, replacing the current uniform verdict.
5. Select the companion ADR edit shape: amend ADR-W007/W008 directly or add a
   new accepted ADR that explicitly supersedes their publication and
   tick-authority clauses.

**Answered 2026-10-24 (#157):** 1 yes; 2 yes; 3 yes; 4 yes; 5 amend
ADR-W007/W008 in place (ADR-W007 A8, ADR-W008 A2a).
