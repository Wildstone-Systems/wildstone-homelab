# Workspace resource authority and privacy foundation

Issue #475 (S03), under [ADR-W047](../decisions/ADR-W047-native-agent-research-and-workspaces.md)
and the [binding v1 contract](native-agent-capability-contract-v1.md).

This foundation does not register tools, expose routes, grant capabilities,
publish bytes or implement sharing. Storage/publication and tool/API slices
must use these checks and repeat them at their execution fences.

## Resource authority

The existing `butlerstore.Authority` recognizes resource kind `workspace`.
Canonical gateways provide a kind-only reference. `ResolvePrivateWorkspaceTask`
resolves its private workspace ID from trusted Person/Agent/task identity, then
checks current authority. Explicit IDs remain selectors, not ownership claims.
Resolution never creates a workspace or grants access.
`CheckWorkspaceTask` requires a live Person, the owning Agent, current Agent
access, a private conversation task belonging to that same Person and Agent,
and matching current class, surface and originating Connection. Missing and
hidden resources receive the same bounded refusal. Owner administration is
not resource access. Demotion, offboarding, Agent retirement, access revocation
and Connection revocation take effect on the next check. Unattended routines
cannot use their proposer's identity; shared workspaces remain refused until
explicit sharing is implemented. Tool assignments remain an independent gate.

Only trusted handle/ingress code binds `WorkspaceAccess`. Tool inputs must never
supply its Person, Agent, task, Connection, model or privacy context. Runtime
callers require a `PrivacyTurn` bound to the same task and database; missing,
failed, group or mismatched context refuses access. Direct metadata primitives
in `workspace.go` remain internal persistence/recovery APIs, not runtime APIs.

## Read and revision seams

`ReadWorkspaceRevision` authorizes the exact workspace/artifact/revision,
requires committed retained metadata, checks current source audiences and
processing/delivery policy, then durably records the read before returning.
A failed transaction returns no metadata. Callers must use it before revealing
filenames, snippets or bytes, including listing/search candidates and retries.
The storage layer must still verify immutable bytes and enforce confinement.

`CreateWorkspaceRevisionForTask` requires current authority and a full, ready
route; QA-only reads remain possible but revision creation refuses. It captures
in-flight memory inputs, inherits the task/session and expected base revision's
policy, records an explicit base as a task/session input even without a prior
read, pins source identity and inserts the revision in one transaction.
Refusals and database failures roll back the revision and its new evidence.
The publication coordinator must independently recheck expected revision,
privacy, quota and execution authority immediately before effects/commit.
This seam neither reserves quota nor publishes a revision.

The first implementation admits only same-Person, same-Agent private workspace
sources and currently authorized memory sources. Unrecognized external source
references or audience forms fail closed; later provider/sharing adapters must
supply reviewed, typed source rules rather than interpret arbitrary source IDs
as public. No model-controlled source label can remove inherited restrictions.

## Durable provenance

Migration 0099 adds exact task/session-to-revision reads and immutable-revision
input snapshots. Foreign keys pin referenced revisions, sessions and memory
records so deletion cannot silently drop a known restriction. Future lifecycle
work must coordinate these references; this change does not implement purge.
The new workspace graph joins the existing memory graph at policy evaluation.
`wr:` nodes name revisions, `wa:` artifact policy, and `t:` task provenance;
existing `m:` memory and `s:` session nodes retain their meaning. Traversal uses
`UNION`, so cycles terminate while preserving the strictest reachable labels.

Known source identities are captured when a revision is created. Live lineage
also conservatively includes later task/session sources, so resumed work and
later source tightening cannot silently widen the result. Revision and source
labels are combined with current `content_privacy` marks. No human policy-edit
endpoint is introduced here. Trusted-only restrictions remain independent of
local model processing; local inference does not authorize external disclosure.

Task-backed delivery checks provenance even before the first session exists.
Gateway Manifest and Invoke restore originating task/session provenance. Invoke
checks it before dispatch or ledger reuse. Workspace/export calls must re-enter
their resource/receipt seam instead of returning generic cached tool output.
No content bounds, tool grants or runtime protocol fields are expanded.

## Migration sequence

The parent reserved 0098 for #490 / PR #519 and 0099 for this slice. The runner
sorts migration filenames and tracks each full name; it allows gaps and would
apply a subsequently-added lower-numbered migration. Therefore ordering is a
merge prerequisite, not a runner-enforced safeguard: **0099 must not land before
0098**. No placeholder migration is supplied. Candidate-chain checks against
#490's real migration are integration evidence only; repeat final ordered-chain
verification against the merged feature tip before declaring readiness.
