# Slice 24: audit remediation and rollout (#225)

Base: `main` after slice 23 (`71df923`). Current authority: `docs/agent-system-design.md`, ADR-W033 and the shipped code. The 2026-09-10 finding text remains historical evidence; current dispositions must be new, checkable conclusions.

## Work sequence

1. Extend the audit-register test first so every F-153–F-158 finding needs exactly one current disposition: `implemented`, `open` with a named owner and tracking issue, or `superseded` with a design section. Preserve the original audit status and evidence under explicitly historical labels. A current `implemented` claim needs a named commit, production call path and test or runtime result. Add a negative fixture that proves stale findings fail CI.
2. Re-evaluate all 121 findings against merged slices 1–23. Record code and test evidence at the current main commit, and keep genuinely unresolved work open instead of converting it to a pass. Recompute the gap-register totals from the new dispositions.
3. Reconcile ADR-W030 A4’s conversation budget, uncertain result and partial-completion language, verify ADR-W031 A4’s slice-13 amendment, and correct ADR-W032 A3’s obsolete surface claim. Keep ADR amendments in commits separate from the finding sweep, as issue #225 requests.
4. Resolve residue: document why `RoleOperator` remains or rename it with a migration; remove unused 0038 settings by a new forward migration unless a current production reader is found; create focused tracking issues for semantic recall, Butler-to-Butler delegation and clone credential inheritance.
5. Test the migration chain from an empty database and a representative pre-epic snapshot. Use a sanitized fixture for CI; record separately whether a real production snapshot was available and what additional evidence it provides. Run §18 acceptance tests against a scratch runtime first.
6. Update branch guidance in the epic, audit README and docs README. Prepare a concrete deployment change and validation record. Live household deployment is a separate final action after its exact artifact, commands and rollback path can be reviewed; no scratch test mutates the household database.

## Decisions inherited from earlier slices

- Slice 21 chose and implemented a durable parent task, so the budget is conversation-wide.
- Partial completion remains represented by verified evidence and a final task state unless the §18 recheck proves that representation loses a required distinction.
- The unused `index_conversations` and `transcript_retention_days` columns should be dropped forward; indiscriminate transcript ingestion is not implied by their defaults.
- `RoleOperator` is a machine/API role distinct from a Person’s Owner/Member role; keep the name if changing it would create migration risk without removing ambiguity at the boundary.

## Verification

Run the audit-register and migration tests before and after the sweep. Run the full Go and Console CI suite, then the §18 acceptance matrix. Every current disposition must cite a shipped entry point and a negative test where authority or privacy is involved. Record real-runtime limits plainly.
