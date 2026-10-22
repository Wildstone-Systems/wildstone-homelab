# ADR-W048: Native Agent skill lifecycle, dependencies, and rollout consistency

- **Status:** proposed; design only; not implemented by this document.
- **Date:** 2026-10-08
- **Project:** Wildstone-Systems/wildstone-homelab
- **ADR identifier:** W048
- **Issue:** old private issue 561; tracker [#126](https://github.com/Wildstone-Systems/wildstone-homelab/issues/126).
- **Decision owner:** Wildstone maintainer.
- **Scope:** lifecycle states, dependency validation, bulk rollout and rollback, concurrency, revocation, side-effect honesty, audit, and implementation slicing for governed native Agent skills.
- **Related:** ADR-W033 (Agent design, Owner/Member separation), ADR-W039 (canonical Agent API), ADR-W041 (native AgentRuntime contract), ADR-W047 (native research and workspaces).

## 1. Context

Tracker #559 settles the v1 skill model: an installation-scoped registry of immutable versions, exact-version Agent assignments, proposal-only drafting, and Console/Assistant surfaces. This ADR specifies the remaining edge cases so that implementation slices S02–S11 can be finalised without re-deciding them. It changes no code, schema, privilege, or host state.

A skill declares requirements and composes tools that Keystone already authorizes. A skill never grants a tool, credential, memory scope, network destination, approval authority, or access to another Agent. Every rule below preserves that property.

## 2. Decision summary

| Area | Decision |
| --- | --- |
| Identity | Every skill has an immutable stable ID. Names are unique labels per scope and may change without detaching assignments. |
| Versions | Published versions are immutable and content-hashed. Editing creates a new draft. |
| Lifecycle | Version states: `draft`, `in_review`, `published`, `retired`, `revoked`. Assignment states: `pending`, `active`, `unavailable`, `paused`, `unassigned`, `revoked`. |
| Retire / revoke / unassign | Three distinct operations (section 3.3). Retired and revoked versions stay addressable in history and cannot be newly activated. |
| Dependencies | Declared on the version, resolved to exact versions at assignment, rechecked at task start and continuation. Failures use a closed set of reason codes (section 5). |
| Bulk operations | Preview first; partial success by default, optional all-or-nothing per operation; idempotency key per operation (section 6). |
| Concurrency | Compare-and-set on an assignment revision counter. A stale writer is rejected, never merged (section 7). |
| Emergency revocation | Separate from rollback; fail-closed; checked at tool-call and continuation boundaries including cached manifests (section 8). |
| Rollback | A pointer change only. It is never described as reversing external side effects (section 9). |
| Audit | One parent record per operation plus one record per affected assignment (section 11). |

## 3. Lifecycle states and transitions

### 3.1 Skill version

```text
draft ──submit──▶ in_review ──approve/publish──▶ published ──retire──▶ retired
  ▲                  │                              │
  └──── return ──────┘                              └──revoke──▶ revoked
```

| From | To | Actor | Notes |
| --- | --- | --- | --- |
| (none) | `draft` | Person, Owner, or Agent via `skill.propose` | Agent drafts never leave `draft` or `in_review` by Agent action. |
| `draft` | `in_review` | author | Requires passing schema, scope and compatibility checks. |
| `in_review` | `draft` | reviewer or author | Returned for changes; the review record is kept. |
| `in_review` | `published` | Owner, or author for a safe personal overlay (#559) | Content hash is frozen. |
| `published` | `retired` | Owner, or author for own personal skill | No new assignments. Existing assignments keep working until unassigned or replaced. |
| `published` or `retired` | `revoked` | Owner (emergency) | Fail-closed. Applies to every assignment of the version. |

`revoked` is terminal. `retired` may become `revoked` but never returns to `published`. A `draft` that is abandoned is withdrawn (kept, marked withdrawn), never silently deleted when referenced by a proposal or evaluation. A new revision of a retired or revoked skill is a new draft under the same stable ID with the retired/revoked version as its parent.

### 3.2 Assignment (Agent to exact version)

| State | Meaning |
| --- | --- |
| `pending` | Awaiting an approval the change needs (section 6.4). Not effective. |
| `active` | Effective for future task starts and continuation boundaries. |
| `unavailable` | Assigned, but a dependency, tool, or compatibility check currently fails. Not effective; reason code recorded. Returns to `active` when the cause clears. |
| `paused` | Stopped for safety by an Owner, or by the assignment's Person. Not effective. Resumes only by an explicit audited action, never automatically (section 3.3). |
| `unassigned` | Stopped for future runs. History retained. |
| `revoked` | Fail-closed. Re-enablement needs an explicit authorized action and an audit record (section 8). |

### 3.3 Operation meanings

| Operation | Target | Effect on running tasks | Effect on history | Reversible |
| --- | --- | --- | --- | --- |
| Unassign | one assignment | continue under the version recorded at start; stop at the next start | kept | yes, by assigning again |
| Pause | one assignment | interrupted at the next tool-call or continuation boundary, as for revocation | kept | yes: an Owner resumes it, or the Person resumes a pause they set themselves; resume is a compare-and-set write with an audit record and revalidation (section 5.4) |
| Retire | a version | none directly | kept; version stays addressable | no new assignments; existing ones persist until unassigned |
| Revoke | a version or an assignment | interrupted fail-closed at the next tool-call or continuation boundary | kept | version revocation is terminal; assignment re-enablement is a new audited action |
| Delete | a referenced version | not allowed in v1 | n/a | n/a |

An Owner may pause, revoke, or unassign a personal skill assignment for safety without gaining the right to read its private content (ADR-W033 separation of administrative and disclosure authority). Pause is the reversible safety stop; revoke is the fail-closed one (section 8). Revocation overrides a pause: a paused assignment that is revoked stays revoked when the pause is lifted.

## 4. Retirement, replacement, and migration guidance

- Retiring a version may record a `replaced_by` reference to a published version under the same stable ID, or to a different skill ID with a stated reason. The reference is guidance, not an automatic reassignment.
- The Console shows, for a retired version, every assignment still pinned to it and offers a bulk migration (section 6) to the replacement.
- If no replacement exists, assignments remain on the retired version, flagged `retired` in the UI, until an Owner unassigns them. Retirement never silently changes behavior.
- Migration is always an explicit reassignment to an exact published version. If the replacement declares more requirements than the old version (section 6.4), approval is required before the assignment becomes `active`.

## 5. Dependency declarations and validation

### 5.1 Declarations

A skill version declares, as data only:

- `requires_tools`: Keystone tool/capability names with a minimum contract version.
- `requires_skills`: stable skill IDs with a version constraint (exact version or a compatible range of published versions).
- `requires_scopes`: memory scopes, processing class, output audience, surface, and whether it supports unattended execution.

Declarations grant nothing. They are inputs to validation.

### 5.2 Resolution and pinning

At assignment time, each skill-to-skill dependency is resolved to an exact published version and the resolved set is stored with the assignment. The runtime manifest contains the resolved versions and hashes. A later publication of a dependency never changes a resolved set. Dependency cycles are rejected at submit time. Maximum dependency depth is bounded by platform policy and is checked during validation.

### 5.3 Deterministic failure reasons

Validation returns every failing check, ordered by a fixed code order, so identical input always yields an identical result.

| Code | Condition |
| --- | --- |
| `dependency_missing` | A required skill or tool does not exist in the installation. |
| `dependency_incompatible` | Version constraint, tool contract version, or Agent class does not match. |
| `dependency_retired` | A required skill version is retired (blocks new assignment; an existing assignment becomes `unavailable` only if the policy for the dependency is `strict`). |
| `dependency_revoked` | A required skill version or tool assignment is revoked. Always blocks. |
| `dependency_cycle` | The dependency graph contains a cycle. |
| `tool_unassigned` | The Agent lacks the assigned tool; the skill does not grant it. |
| `scope_ceiling` | Member skill on an incompatible surface, or Owner skill on a non-Owner Agent. |
| `approval_required` | The change expands requirements and no valid approval exists. In a rollout or migration this stages the change for approval (section 6.4) rather than blocking it. |
| `rollback_target_retired` | A rollback item's prior version has been retired since the rollout (section 6.6). |
| `rollback_target_revoked` | A rollback item's prior version has been revoked since the rollout (section 6.6). |
| `stale_assignment` | The assignment revision no longer matches (section 7). |

Codes are a closed set. Adding one is an ADR amendment. Reasons are shown to the user and recorded in audit; they contain no private skill content.

### 5.4 When validation runs

At assignment, at task start, and at every continuation boundary. A check that newly fails at task start or continuation moves the assignment to `unavailable` for future starts and never alters a running execution except under revocation (section 8).

## 6. Bulk rollout and rollback

### 6.1 Targets

A rollout may target one Agent, selected Agents, an Agent class, a household scope, or all compatible assignments, and moves each selected assignment from one exact version to another.

### 6.2 Preview

Before any change, the preview classifies every candidate assignment as `eligible`, `needs_approval` (passes every check except `approval_required`), `blocked` (with section 5.3 codes), `incompatible`, `revoked`, or `already_current`. The preview is computed from a snapshot that records each assignment revision (section 7). Execution revalidates against live state; the preview is advisory.

### 6.3 Modes and partial success

| Mode | Behavior |
| --- | --- |
| `partial` (default) | Apply every eligible assignment independently and stage every `needs_approval` one (section 6.4). Report each success, staged item, and failure. Blocked ones are untouched. |
| `all_or_nothing` | Selected at operation creation. Validate all, then apply the eligible items and stage the `needs_approval` items in one transaction. Any blocked or stale item fails the whole operation with no change. |

The result always lists every assignment with its outcome and code. Neither mode ever reports a blocked assignment as done.

### 6.4 Capability expansion and approval

If the new version requires additional tools, connections, memory scope, processing class, output audience, or unattended behavior compared with the version it replaces, the diff is shown in review and the change waits for approval. A new assignment is created `pending`. An existing `active` assignment stays `active` on its current version and records the target as a staged change; the operation reports the item `pending`, never applied. The change takes effect only after the applicable approval (Owner for anything beyond a safe personal overlay). Approval applies the staged change with a compare-and-set (section 7); rejection, expiry, or cancellation discards it and the assignment stays on its current version. An approval is bound to the exact version, hash, and requirement diff; a changed element invalidates it and a new approval is required. Approvals are auditable and support expiration and cancellation.

### 6.5 Idempotency and retry

- Every bulk operation carries a client-supplied idempotency key, unique per operator and operation kind, stored with the parent record.
- Replaying the same key with the same parameters returns the stored result and makes no further change. Replaying a key with different parameters is rejected.
- Retrying a `partial` operation is a new operation that targets only previously blocked or failed items, optionally referencing the parent. Items already at the target version are `already_current` and are not re-applied or re-audited as changes.
- Each per-assignment change is itself a compare-and-set, so a crash mid-operation leaves every assignment either fully old or fully new. A resumed operation completes the remaining items from the stored parent record.

### 6.6 Rollback

Bulk rollback reverses the assignment changes recorded by a specific rollout, using the prior pinned version stored per assignment. It is itself a bulk operation with its own idempotency key and audit parent. Items that changed since the rollout (their revision no longer matches the rollout's resulting revision) are reported `stale_assignment` and left alone, so a later deliberate change is never overwritten. If an item's prior version has been retired or revoked since the rollout, the item is not re-pointed: it is reported `rollback_target_retired` or `rollback_target_revoked`, left on its current version, and listed for an explicit migration to a published version (section 4). Retired and revoked versions are never newly activated, by rollback or any other path, so a revocation stays fail-closed. Individual assignments remain independently recoverable. A failed candidate never overwrites the last known-good version; while that version is still published, rollback re-points to it and keeps the candidate in history.

### 6.7 Agent offline during rollout

Assignments are server-side state, not pushed content. An offline or sleeping runtime changes nothing about the rollout outcome. The Agent picks up the effective assignment at its next task start or continuation boundary, after revalidation. The operation result reports assignments as applied. Each manifest fetch reports the revision of every assignment the runtime loaded, and Keystone records it per assignment as `observed_revision`; the UI shows, per Agent, every assignment whose `observed_revision` is behind its current `revision`, so an operator can see exactly which changes an Agent has not yet loaded. A rollout never waits for, blocks on, or times out because of an offline Agent.

## 7. Concurrency

- Every assignment has a monotonically increasing `revision`. Every write (assign, pin, promote, pause, resume, unassign, revoke, rollout item, rollback item) states the revision it expects and succeeds only if it matches. A mismatch returns `stale_assignment` and the caller must re-read.
- Publishing, approving, and retiring a version are guarded by a `version_state` check on the version row; a transition from an unexpected state is rejected.
- Approvals bind to a specific version, hash, and requirement diff. If any of them changed since the approval was granted, the approval no longer applies.
- Simultaneous edits to a draft use a draft revision counter; the second writer receives a conflict and keeps its text for manual merge. Last-writer-wins is never used.
- A rollout and a rollback touching the same assignment are serialized by the assignment revision. The loser is reported, not retried automatically.
- Revocation takes precedence over any in-flight change: a revoked version or assignment cannot be moved to `active` by a write that was prepared before the revocation.

## 8. Emergency revocation (decided constraints)

Confirmed in #561 and #559, recorded here as binding:

- Revocation is separate from rollback and fails closed.
- Scopes: one assignment, one skill version, one Agent, one capability, or the whole installation.
- Revocation is checked at every tool-call boundary and every continuation boundary, including against a cached manifest. A manifest is valid only while its recorded assignment revision and revocation epoch still match; otherwise it is refetched or the task is interrupted.
- The Console exposes a prominent safety control and shows affected tasks, assignments, and Agents before and after.
- Re-enablement requires an explicit action by an authorized Owner and an audit record. Nothing re-enables automatically.
- Revoking does not disclose private skill content to the revoking Owner.

## 9. Rollback after durable external side effects

A rollback or revocation changes which version future runs use. It does not undo anything a prior run did.

- Documentation, UI text, audit records, and APIs must not describe a pointer rollback as reverting messages sent, events created, files exported, purchases, or other external effects.
- Task and run records keep their effect evidence (ADR-W041 recovery outcomes, completion evidence). Where a prior run's effect state was uncertain, it stays uncertain after rollback.
- The rollback result lists, for each affected Agent, tasks that were running or recently completed under the rolled-back version, with a link to their evidence, so the operator can decide on any manual remediation.
- Compensating actions, if ever offered, are separate operations with their own approval, not a property of rollback.

## 10. Decided constraints carried from #559 and #561

1. **Approval invalidation.** An approval is bound to the specific proposed action plan: material inputs and intended side effect. If the Agent changes a recipient, amount, item, quantity, destination, permission, or capability, the prior approval is invalid and a new one is requested. Approval records are auditable and support expiration or cancellation.
2. **External content is untrusted data.** Email, web pages, documents, and provider responses cannot modify skills, grant capabilities, change policy, bypass approval, or alter a task's security boundary. Skills and adapters preserve this distinction when importing external text into context, consistent with ADR-W047.
3. **Exact pins and promotion.** Publishing never changes an assignment. Promotion is an explicit reassignment.
4. **Running executions.** A rollback applies to future starts and continuation boundaries after revalidation. A running task continues under the version recorded at its start unless a revocation requires interruption. Traces keep the skill ID, version, and content hash.
5. **Immutable identity.** Names are unique within the applicable scope; the stable ID is the technical identity.
6. **Personal skills.** Reach is per Person and Agent. Owner administrative actions on a personal skill are limited to pause, revoke, and unassign.

## 11. Audit and metrics

### 11.1 Audit records

Each state change writes an append-only record containing: actor and surface, operation kind, target IDs and versions, previous and new assignment revision, outcome, section 5.3 reason code where applicable, idempotency key, approval reference, and timestamp.

- A bulk operation writes one parent record (targets, mode, key, preview snapshot reference, counts) and one child record per assignment change, including blocked and stale items.
- Records never include personal skill content, secrets, or task payloads. They reference IDs and hashes.
- Task traces reference the exact skill ID, version, hash, and resolved dependency set used.

### 11.2 Metrics

Counters and gauges, without Person-identifying labels: rollouts started, completed, partially completed; assignments applied, blocked by code, stale; rollbacks by outcome; revocations by scope; assignments `unavailable` by code; assignments whose observed revision is behind their current revision; approvals requested, granted, expired, invalidated.

## 12. Boundaries and non-goals

- No change to Member/Owner boundaries, approval gates, or tool authorization. Nothing here may be implemented in a way that weakens them or a pinned structural test.
- No hard purge of referenced versions in v1. A purge, if ever added, is a separate consequential operation that never rewrites task history.
- No automatic canary logic here; canary and outcome comparison remain in S05/S06.
- No change to Hermes or other runtime-local skill stores; they remain non-authoritative.

## 13. Implementation-ready follow-up slices

These refine the S02–S11 slices in #559. Each is one bounded PR and carries negative-path tests.

| Slice | Content | Depends on |
| --- | --- | --- |
| L1 | Registry schema: stable ID, version states, hash, parent, `replaced_by`, lifecycle transition guards, no hard delete | S02 |
| L2 | Assignment table with `revision`, states including `paused`, compare-and-set writes (pause and resume included), `stale_assignment` | L1 |
| L3 | Dependency declaration schema, resolver, cycle check, closed reason-code set, validation at assign/start/continuation | L1, L2, S03 |
| L4 | Revocation epoch, manifest validity check at tool-call and continuation boundaries, fail-closed interruption, re-enable audit | L2, S03 |
| L5 | Bulk operation engine: preview (including `needs_approval`), `partial` and `all_or_nothing`, staged changes for approval, idempotency keys, retry-of-blocked, parent/child audit, per-assignment `observed_revision` | L2, L3 |
| L6 | Rollout rollback with stale-item and retired/revoked-target reporting, and the side-effect disclosure list | L5, S06 |
| L7 | Capability-expansion diff and approval binding (version, hash, diff), expiry and cancellation | L3, L5 |
| L8 | Retirement flow with `replaced_by` and bulk migration UI hooks | L1, L5, S08/S09 |
| L9 | Metrics and audit exports | L5 |
| L10 | Conformance tests: stale version, revoked assignment, restart/resume, rollback during active work, offline Agent, uncertain external effect, cross-person isolation | all |

## 14. Acceptance mapping (#561)

| Acceptance criterion | Section |
| --- | --- |
| Lifecycle states and transitions documented | 3, 4 |
| Dependency validation has deterministic failure reasons | 5 |
| Bulk operations have explicit partial-success and retry semantics | 6 |
| Concurrent operations cannot silently overwrite newer assignments | 7 |
| Retired and revoked versions stay addressable, cannot be newly activated | 3, 4 |
| External side effects not represented as reversible by a pointer rollback | 9 |
| Links back to #559 and produces follow-up slices | header, 13 |
