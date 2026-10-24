# W050 slice 0: accepted revision and bounded approval (#128 G1/G2)

> Draft issue body. Not created on GitHub. Labels when approved: `W050`, `POST Alpha`, `agent:blocked`.
> Plan: [`../w050-migration-plan.md`](../w050-migration-plan.md) §10.

## Goal

Make every apply run against one immutable accepted revision, and make an approval cover only the steps it reviewed. Under W050 the Wardstone leader will act on approvals across machines, so these guarantees must exist first.

## User-visible outcome

- A Plan shows the exact revision and the exact steps being approved.
- Approving one Plan never lets an unrelated Medium/High change through in the same tick.

## Scope

- Implements the proposed [desired-state publication and bounded-approval contract](../desired-state-publication-and-approval-contract.md); accept or amend it first.

- **G1:** `keystone/statefile/statefile.go:54-168`, `keystone/api/handlers_plans.go:230-300`, `keystone/ingest/ingest.go:85-142`. Apply reads a committed revision (Git commit ID), not the working tree; approval, commit and DB transition become one ordered transaction.
- **G2:** `keystone/reconcile/loop/loop.go:134-164`, `:249-274`, `:392-413`. Replace the tick-wide `allowMediumHigh` with a per-approval set of `(kind, name, action, spec digest)`; `Reconcile` applies a Medium/High step only when its tuple is in the set.
- Plans record the revision they were approved against.

## Non-goals

- Remote GitOps ingestion (G10).
- Any Kubernetes work.

## Dependencies

None. Can land on the Docker backend right after alpha.

## Acceptance criteria

- [ ] Test: two concurrent approvals of different Plans apply only their own steps.
- [ ] Test: drift raised during an approved tick waits for its own approval.
- [ ] Test: a crash between commit and DB transition recovers to a consistent state on restart.
- [ ] Test: editing the working tree after approval does not change what is applied.
- [ ] `go test -race ./...` in `keystone/`; e2e and exit drill pass unchanged.

## Risks

- Behaviour change for households with queued Plans: document in release notes.
- Static-analysis findings; reproductions may show more than the audit found.

## Size

**L.** Touches Plans, approvals, the loop and the Statefile, and needs concurrency and crash tests.

## ADR amendment

No. Implements ADR-W004/W008 intent; update `566-iac-architecture.md` disposition.
