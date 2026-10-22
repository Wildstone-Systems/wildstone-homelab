# Gap register: repository against the architecture baseline

Tracked by old private issue 151.
See [`README.md`](README.md) for how to run a slice.

**One file per slice, and no session writes into another's.** The audit slices were run independently so detailed evidence stays in per-slice files rather than one shared document.

| Slice | Issue | Findings |
|---|---|---|
| Bot routing and per-Butler chat connections | old private issue 153 | [`findings/153-bot-routing.md`](findings/153-bot-routing.md) |
| Routine authoring, execution, approval integrity | old private issue 154 | [`findings/154-routines.md`](findings/154-routines.md) |
| Identity and authorization | old private issue 155 | [`findings/155-identity.md`](findings/155-identity.md) |
| Shared memory, audiences, semantic recall | old private issue 156 | [`findings/156-memory.md`](findings/156-memory.md) |
| Secrets and privacy gateway | old private issue 157 | [`findings/157-secrets.md`](findings/157-secrets.md) |
| Durable execution, delegation, lifecycle | old private issue 158 | [`findings/158-execution.md`](findings/158-execution.md) |

Each findings file carries its own audit status. This index holds no findings of its own.

---

## Finding identifiers

`F-<issue>-<nn>` — for example `F-153-01` or `F-156-07`.

**Never renumber findings.** Remediation issues may already cite them. Withdrawn findings should be struck through in place with the reason recorded.

## What the lint checks

`testing/e2e/auditregister_test.go`, which CI runs, checks that:

- every findings file declares the expected issue number and a valid slice status
- finding IDs are well formed, namespaced correctly, unique, and monotonically increasing
- every finding carries a valid status and non-empty evidence
- this index links every findings file and every findings file is represented here

Run it before committing audit-register changes:

```bash
cd testing/e2e && go test -run TestAuditRegister ./...
```

---

## Audit summary

The completed audit contains:

| State | Count |
|---|---:|
| missing | 69 |
| partial | 41 |
| implemented | 8 |
| unverified | 3 |
| **total** | **121** |

These numbers describe repository conformance at the time of the audit. They are evidence, not the architecture itself.

## Current disposition after slices 1–23

Slice 24 (#225) re-evaluated the same 121 stable finding IDs against the
merged implementation. Each findings file now places **Current status** and
**Current evidence** beside its original **Audit status** and **Audit evidence**.
The old requirement and acceptance tables above the findings remain the
September 10 audit snapshot, not a current pass/fail report.

| Current state | Count | Meaning |
|---|---:|---|
| implemented | 60 | A cited shipped path and named regression test address the finding. |
| open | 54 | The row names a current owner and follow-up issue; partial work is not counted as complete. |
| superseded | 7 | The accepted design changed the contract; the row points to its governing section. |
| **total** | **121** | Every original ID has exactly one current disposition. |

Open work is grouped in [chat delivery #57](https://github.com/Wildstone-Systems/wildstone-homelab/issues/57),
[routine execution #58](https://github.com/Wildstone-Systems/wildstone-homelab/issues/58),
[identity #59](https://github.com/Wildstone-Systems/wildstone-homelab/issues/59),
[memory and privacy #60](https://github.com/Wildstone-Systems/wildstone-homelab/issues/60),
and [durable work #61](https://github.com/Wildstone-Systems/wildstone-homelab/issues/61).
[Semantic recall #55](https://github.com/Wildstone-Systems/wildstone-homelab/issues/55),
[Agent delegation #56](https://github.com/Wildstone-Systems/wildstone-homelab/issues/56),
and clone credentials old private issue 313
have focused issues. Child identity remains tracked by [#54](https://github.com/Wildstone-Systems/wildstone-homelab/issues/54).

## Design resolution after the audit

The audit exposed several repeated architectural questions. Those questions were subsequently reviewed with the Owner and re-baselined in:

- [`docs/agent-system-design.md`](../agent-system-design.md) — current clean design contract and detailed acceptance criteria
- [`ADR-W033`](../decisions/ADR-W033-the-agent-design.md) — current architectural decisions, rationale, tradeoffs, and supersessions

The earlier [`ADR-W033 review`](../decisions/ADR-W033-review.md) remains useful for the repository/schema observations it captured while reviewing the first draft. Where its design conclusions conflict with the final W033, the final W033 and design contract govern.

The final W033 deliberately **does not duplicate the full 121-finding coverage matrix**. Detailed findings stay in this audit tree. Likewise, detailed implementation sequencing belongs in GitHub issues/planning rather than in the ADR.

## Architectural dependency order

The final W033 keeps only the high-level dependency order:

1. Human identity and surfaces
2. Service entitlements
3. Agent classes and tools
4. Connections and sessions
5. Memory scopes and retrieval policy
6. Secrets and privacy boundaries
7. Plans and Owner Review Queue
8. Routine execution
9. Lifecycle and soft deletion
10. Audit remediation, migrations, and rollout

When remediation issues are created or updated, they should cite both the relevant finding IDs and the final W033/design-contract requirement they implement.

## Important audit evidence that remains relevant

Several repository-level observations from the first review remain important even though the architecture was later refined:

- **F-156-08** demonstrated private-memory disclosure into a group-room path on the running deployment. Destination-aware filtering remains required by the final design.
- **F-158-19** questions whether the current completion receipt is sufficiently independent evidence.
- **F-158-21** questions whether the coordinator lease actually gates every durable-action dispatch path.
- Schema and migration findings remain implementation evidence even where the final target schema changed after the review.

These should be verified or remediated before implementation work relies on the affected guarantees.

## Historical note

This register was originally framed as an audit against the 10 September 2026 architecture baseline. That baseline remains the source that generated these findings, but it is no longer the current normative design where it conflicts with final ADR-W033. Do not rewrite historical findings merely because the target architecture evolved; instead, remediation should explain whether a finding is still applicable, superseded by the new design, or requires a differently shaped fix.


## Supplementary IaC assessment

[Infrastructure as Code architecture assessment](findings/566-iac-architecture.md), tracked by [#128](https://github.com/Wildstone-Systems/wildstone-homelab/issues/128), preserves the October 7, 2026 implementation snapshot at `b4836adda35c69ae8fde38199fdb92ab292807d8`. Its discussion and current-code revalidation remain in progress. The assessment uses document-local G/A identifiers and does not change the 121 historical finding IDs or disposition counts above. Its tool assignments and roadmap are recommendations pending review, not accepted architecture decisions.
