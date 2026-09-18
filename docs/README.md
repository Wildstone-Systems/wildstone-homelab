# docs/

Wildstone product documentation and ADRs.

- `02-implementation-scope.md` — implementation scope (epics E0–E13, milestones M0–M5, sizing, cut line).
- `decisions/` — Wildstone ADRs (W-series).

User and operator docs (E13, ADR-W018):

- `install.md` — installing Keystone, joining nodes, `wild`, Console, Butler.
- `concepts.md` — Statefile, plans, reconcile, principals, capabilities, secrets, backups, and the Butler ceiling.
- `recovery.md` — recovery contract and total-host-loss runbook.
- `threat-model.md` — threat model refreshed against as-built behavior.
- `vision.md` — product direction and standing principles.
- `migration.md` — dogfood migration runbook and cutover rules.
- `friction-log.md` — dogfood friction and alpha-exit cleanup.

The original design brief (`00-design-brief-v0.1.md`) and architecture proposal (`01-architecture-proposal.md`) remain in the reference deployment repo, `chickenrat17/homelab-config`, under `docs/architecture/wildstone/`.

## Agent / Butler architecture

Read these in this order:

1. [**Agent system design**](agent-system-design.md) — the current clean design contract. This is the best description of how the Wildstone Agent/Butler system is intended to work now, including detailed end-to-end acceptance criteria.
2. [**ADR-W033: The Wildstone agent system**](decisions/ADR-W033-the-agent-design.md) — the current architectural decision record: rationale, tradeoffs, explicit supersessions of older decisions, and the short dependency-oriented implementation order.
3. [**ADR-W033 review**](decisions/ADR-W033-review.md) — historical repository/schema review of the first W033 draft. Its repository findings remain useful evidence, but design conclusions that conflict with the final W033 are superseded.
4. [**Agent architecture baseline**](agent-architecture-baseline.md) — the 10 September 2026 baseline that triggered the repository audit. It is retained as historical input, not as the current source of truth where it differs from the final W033/design contract.
5. [`audit/`](audit/) — repository audit evidence and the [gap register](audit/gap-report.md). Individual finding files remain the detailed evidence base for implementation planning.

`Agent` and `Butler` mean the same architectural object. Product language may say Butler; technical language may say Agent.

### Current authority

Where these documents disagree, use this precedence for the agent-system work:

```text
agent-system-design.md
        +
ADR-W033-the-agent-design.md
        ↓
current design

older ADRs / 2026-09-10 baseline
        ↓
historical decision and audit context
```

Older ADRs are intentionally preserved as historical records. ADR-W033 Part G states the current amendments and supersessions instead of rewriting those records to make them appear to have always contained the final design.

The detailed implementation sequence belongs in GitHub issues/planning. ADR-W033 keeps only the architectural dependency order, while `agent-system-design.md` carries the behavioral acceptance gates.

## Supporting agent documents

- [ADR-W030](decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md) — historical continuity, durable-work, and scoped-integration decisions. See W033 Part G for current supersessions.
- [ADR-W031](decisions/ADR-W031-butlers-are-user-created-specialists.md) — historical specialist-Butler design. See W033 Part G for the current Agent-class, model-chain, Connection, and memory model.
- [ADR-W032](decisions/ADR-W032-who-may-open-the-console.md) — historical Console-access design. Final W033 makes Console strictly Owner-only and treats Child as a constrained Member profile rather than a third fundamental role.
- [Capability and design brief](butler-capability-and-design-brief.md) — non-normative rationale and deployment examples.
- `butler-memory.md` — notes generated from existing code, retained for repository inventory rather than as a normative source.
