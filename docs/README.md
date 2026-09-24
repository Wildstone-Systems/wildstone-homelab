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
5. [`audit/`](audit/) — repository audit evidence and the [gap register](audit/gap-report.md). Each of the 121 findings now has a separate current disposition beside its historical audit status; use that and its follow-up issue for implementation planning.

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
- [ADR-W034](decisions/ADR-W034-the-household-keeps-its-own-calendar.md) — the `calendar` capability. Mostly a capability decision, listed here for the part that is not: what an agent may do to a calendar, and why writing is gated twice.
- [ADR-W035](decisions/ADR-W035-commitments-and-meters.md) — `Commitment` and `Meter`: the household keeps track of what it owes itself, due against a number that only counts up, of which the calendar is one.
- [ADR-W039](decisions/ADR-W039-canonical-agent-api-and-pluggable-runtimes.md) — exploration output for #333, proposed: one canonical Agent API that the native Butler and external runtimes (Hermes, OpenClaw) consume identically; current-state map from code, gap analysis, runtime contract, and an incremental migration plan. Authorises no implementation.
- [ADR-W040](decisions/ADR-W040-agent-api-transport.md) — accepted (#342): the transport ADR-W039 §3.8 deferred. REST plus long-poll over the existing Keystone HTTP API first, authenticated by the runtime principal's bearer, bodies the `agentapi` types 1:1; MCP as a later second transport wrapping the same handlers with no new authority.
- [Hermes capability package and execution profiles](design/hermes-capability-package-and-execution-profiles.md) — exploration output for #334, built on ADR-W039: packaging Hermes as an optional capability bound to a Wildstone Agent, one container per Agent, three execution profiles (platform-only, sandboxed, host administration via Wildstone executors), threat analysis, spike plan and draft follow-up issues.
- [Capability and design brief](butler-capability-and-design-brief.md) — non-normative rationale and deployment examples.
- `butler-memory.md` — notes generated from existing code, retained for repository inventory rather than as a normative source.
