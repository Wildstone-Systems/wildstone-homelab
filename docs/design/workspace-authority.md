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
sources and currently authorized memory sources. Every reachable revision must
retain its originating task class/surface ceiling; changing an Agent class does
not make old Owner/admin content available to a new Member/family task. Existing
0097 edges naming the same owning stable Person ID remain valid private
audiences; foreign Person selectors confer no access. Unrecognized external source
references or audience forms fail closed; later provider/sharing adapters must
supply reviewed, typed source rules rather than interpret arbitrary source IDs
as public. No model-controlled source label can remove inherited restrictions.

Memory sources use the same content-authority predicate as canonical recall:
active Person/Agent access, audience and inherited audience constraints, scope,
forgetting, and external Connection disclosure. The destination is the bound
privacy turn's destination, not the originating workspace Connection. Every
reachable memory is checked, including transitive derivatives. The existing
verified one-to-one chat rule remains intact; an unverified destination requires
compatible recorded and live reach, encryption, retention and surface profiles.
Recall-only selection rules (supersession and promotion deduplication) do not
remove historical sources or replace their authority checks.

Task/session resume and delivery repeat this authority check whenever their
history contains workspace revisions. They also repeat the original revision
class/surface ceiling and live Person/Agent admission checks. Finished tasks
retain provenance for delivery; finishing a task is not itself a content grant.
The Console memory-query exemption does not waive workspace Agent grants.

| Source dimension | Enforced at read/edit and workspace-derived resume/delivery |
| --- | --- |
| Person/Agent ownership and admission | Same private owner/Agent; live Person, role, Agent and explicit access |
| Class/surface | Original revision task ceiling and current Agent ceiling |
| Audience/scope | Current memory audience, inherited constraints and Agent scope restrictions |
| External disclosure | Canonical memory disclosure predicate using the actual destination |
| Revocation | Forgotten memory, revoked access and Connection checks on every evaluation |
| Processing/privacy marks | Recursive local-only and trusted-only policy across all sources |


## Durable provenance

Migration 0099 adds exact task/session-to-revision reads and immutable-revision
input snapshots. Foreign keys pin referenced revisions, sessions and memory
records so deletion cannot silently drop a known restriction. Future lifecycle
work must coordinate these references; this change does not implement purge.
The new workspace graph joins the existing memory graph at policy evaluation.
A writer task also retains its own revisions, so creating a revision without
subsequently reading it cannot bypass source authority on resume or delivery.
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
Queued external task deliveries also recheck the task owner's live Person
binding independently of memory provenance, including workspace-only sources.
The task pins the trusted ingress conversation ID separately from the Connection
account address. Delivery checks that exact Connection/Person/conversation tuple,
including a matching live binding and room discovery, in the source-authority
transaction. Unknown legacy conversation identities fail closed; mutable thread
pointers and an account address cannot supply a replacement identity.
No content bounds, tool grants or runtime protocol fields are expanded.

## Migration sequence

Migration 0098 belongs to #490 / PR #519 and 0099 to this slice. PR #519
merged into the feature branch before 0099; the integration test upgrades from
0097 and asserts the real 0097 → 0098 → 0099 application order and idempotence.
The runner sorts filenames and tracks each full name; it permits gaps and can
apply a subsequently-added lower number. Ordered landing is therefore an
integration prerequisite, not a safeguard enforced by the runner. No placeholder
migration is supplied.
