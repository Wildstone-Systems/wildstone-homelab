# ADR-W033 repository review

- **Reviewed:** 16 September 2026
- **Repository/branch reviewed:** `claude/ecstatic-wright-czl9v4`
- **Purpose:** repository/schema review of the first W033 draft before implementation
- **Current authority:** [`ADR-W033`](ADR-W033-the-agent-design.md) and [`../agent-system-design.md`](../agent-system-design.md)

> Repository and schema evidence gathered while checking W033 against the code. For decisions, [ADR-W033](ADR-W033-the-agent-design.md) and [`agent-system-design.md`](../agent-system-design.md) govern.

The detailed finding evidence remains in [`../audit/`](../audit/) and the per-slice finding files. This review is a synthesis of the important cross-cutting repository discoveries, not a replacement for those finding records.

---

## Why this review was done

The first W033 draft was coherent enough that it was easy to mistake internal consistency for repository compatibility. The review therefore checked the decisions against the actual schema and code rather than only against the issue discussions that produced them.

What it found are repository facts: forward-only migrations, where authorization seams actually sit, duplicated audience constraints, and provider construction chokepoints.

---

# Repository/schema findings

## 1. The audit counts in the first draft were wrong

The first draft cited an obsolete total. The findings files actually contained:

| State | Count |
|---|---:|
| missing | 69 |
| partial | 41 |
| implemented | 8 |
| unverified | 3 |
| **total** | **121** |

The audit register remains the authoritative source for these historical conformance counts.

## 2. Migration 0038 cannot simply be deleted or "reverted"

ADR-W007's migration model is forward-only and records applied migrations by filename. If a migration has already been applied, deleting it makes upgraded and fresh databases disagree about their schema.

**Implementation consequence:** any withdrawal of `0038_index_conversations.sql` behavior must happen through a new forward migration. The old migration stays as applied history.

## 3. Audience constraints occur across multiple tables

The first draft treated the old two-value audience constraint as if it belonged only to memory. The repository showed audience-like constraints across multiple task/memory/index/integration tables.

**Implementation consequence:** any audience or scope migration must be designed across every place that serializes or constrains the value. A richer memory classification cannot be silently collapsed at task/delivery boundaries.

The final W033 changes the memory model substantially, so the exact target schema must be derived from that final model rather than copied from the first draft's four-audience migration plan.

## 4. Person identity cannot rely on the optional ToolAuthorizer seam

The repository's authorization seam deliberately has a permissive "no administrative controls configured" behavior. Person identity is not optional administrative configuration; it must be present on every interactive request.

**Implementation consequence:** fail-closed Person establishment/checking must occur unconditionally before any optional tool-control seam. A request that cannot name the acting Person does not reach inference or dispatch.

This repository observation remains directly compatible with final W033.

## 5. Provider construction has a useful chokepoint

The first review found that `provider.Provider` is an interface and that the Butler provider is constructed through a narrow construction path rather than independently at every model call site.

**Implementation consequence:** the privacy/egress gateway should wrap provider construction centrally, and tests should prevent concrete provider construction outside the approved gateway path. This is stronger and easier to audit than relying on scattered redactor calls.

## 6. Current role/profile fields are inconsistent across records

The repository had profile constraints in some tool-control/grant tables while task records were less constrained.

**Implementation consequence:** when final W033's two fundamental human roles and two Agent classes are implemented, migrations must define the axes clearly and consistently. Do not reuse a historical `surface`, `profile`, principal role, human role, and Agent class as if they were one concept.

## 7. A Butler-role principal could create/enable household routine state more broadly than the first safety claim implied

The audit found an existing routine path whose authority was broader than the design prose suggested.

**Implementation consequence:** final W033's rule is now simpler and stricter: Agents may propose Plans, but a human Owner activates the exact executable version. Remediation should close any path where an Agent principal can independently activate durable routine behavior.

## 8. Deletion journaling exists but ordinary memory-forget paths did not consistently produce it

The repository already contains deletion-journal/restore-quarantine machinery intended to prevent backups from resurrecting deleted state.

**Implementation consequence:** final memory implementation should use durable deletion journaling for all relevant memory scopes and derived representations. Forgetting must survive restore.

## 9. Task/step/attempt storage already supports the task-versus-turn direction

The repository's durable work tables are compatible with a design where a Task spans multiple turns while individual turns/attempts carry fresh evidence.

**Implementation consequence:** final W033's task/turn split can build on existing durable-work concepts rather than inventing an unrelated execution engine.

## 10. Existing node-secret storage is genuinely write-only by design

The node/infrastructure secret path enforces its no-read property at the operation surface, not merely by convention.

**Implementation consequence:** final W033 is correct to keep household/user secrets as a separate domain rather than weakening the infrastructure secret store with a general read path.

---

# Cross-cutting findings that still matter

The first review identified several behaviors that remain important under the final architecture.

## Revocation must affect in-flight/cached state, not only future dispatch

The audit found examples of queued delivery, approvals, sessions/tokens, and cached context outliving the authority that created them.

Final W033 resolves the product behavior as:

- active authority is removed immediately
- sessions/admission/approvals are invalidated
- cached/private prepared context is invalidated where needed
- durable household resources are not blindly destroyed; they are resolved explicitly

Routines are Household-owned, so a departing Person's routines are not paused wholesale (F1).

## Completion evidence and coordinator gating require verification

Two findings remain particularly important before implementation assumes those guarantees:

- **F-158-19** — verify that completion evidence is sufficiently independent and is not merely a runner writing its own receipt.
- **F-158-21** — verify that every relevant durable-action dispatch is actually gated by the coordinator/lease design.

Neither is solved by prose in W033. They require repository/runtime verification.

## Group/destination disclosure remains a live safety concern

**F-156-08** demonstrated a private-memory-to-group-room disclosure path during the audit. Final W033 changes the memory and Connection models, but it does not make this finding irrelevant: retrieval/delivery must still enforce the destination audience before content reaches the model or leaves the system.

---

# What to use during implementation

For implementation/remediation work, use:

1. [`../agent-system-design.md`](../agent-system-design.md) for current behavior and acceptance criteria.
2. [`ADR-W033`](ADR-W033-the-agent-design.md) for current architectural decisions, rationale, supersessions, and dependency order.
3. [`../audit/`](../audit/) and the per-slice findings for repository evidence.
4. This document for repository/schema lessons from the first design review.
5. Older ADRs, which have been edited to agree with W033.

Nothing in this review itself establishes that the repository implements the final architecture.
