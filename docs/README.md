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

The original design brief (`00-design-brief-v0.1.md`) and architecture proposal (`01-architecture-proposal.md`) remain in the maintainer's private reference deployment repo and are not published.

## Agent / Butler architecture

Read these in this order:

1. [**Agent system design**](agent-system-design.md) — the current clean design contract. This is the best description of how the Wildstone Agent/Butler system is intended to work now, including detailed end-to-end acceptance criteria.
2. [**ADR-W033: The Wildstone agent system**](decisions/ADR-W033-the-agent-design.md) — the current architectural decision record: rationale, tradeoffs, explicit supersessions of older decisions, and the short dependency-oriented implementation order.
3. [**ADR-W033 review**](decisions/ADR-W033-review.md) — repository/schema evidence gathered while checking W033 against the code.
4. [**Agent architecture baseline**](agent-architecture-baseline.md) — background and still-valid foundations (events, integrations, handler engine, executors), trimmed to agree with ADR-W033. Its section numbers are cited by the audit.
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

older ADRs (edited to agree with W033)
        ↓
decisions outside W033's scope

agent-architecture-baseline.md
        ↓
background; defers to the above
```

Older ADRs have been edited to remove clauses that conflicted with ADR-W033, so no ADR states a superseded rule. ADR-W033 Part G lists what changed; git history holds the earlier wording.

The detailed implementation sequence belongs in GitHub issues/planning. ADR-W033 keeps only the architectural dependency order, while `agent-system-design.md` carries the behavioral acceptance gates.

## Supporting agent documents

- [ADR-W030](decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md) — continuity, durable-work, and scoped-integration decisions, aligned with W033.
- [ADR-W031](decisions/ADR-W031-butlers-are-user-created-specialists.md) — specialist-Butler design (no router, one Butler per job), aligned with W033's Agent classes, model chains, Connections and memory scopes.
- [ADR-W032](decisions/ADR-W032-who-may-open-the-console.md) — how the Console authenticates. Only Owners may enter it (W033 A4/A4.1).
- [ADR-W034](decisions/ADR-W034-the-household-keeps-its-own-calendar.md) — the `calendar` capability. Mostly a capability decision, listed here for the part that is not: what an agent may do to a calendar, and why writing is gated twice.
- [ADR-W035](decisions/ADR-W035-commitments-and-meters.md) — `Commitment` and `Meter`: the household keeps track of what it owes itself, due against a number that only counts up, of which the calendar is one.
- [ADR-W039](decisions/ADR-W039-canonical-agent-api-and-pluggable-runtimes.md) — one canonical Agent API that the Keystone-hosted runtime and external runtimes (Hermes, OpenClaw) consume identically; implemented except push delivery, an out-of-process Keystone-hosted runtime, H8 and H10. Amends W033 B6 for external runtimes and adds subscriptions as a routine-like principal.
- [ADR-W040](decisions/ADR-W040-agent-api-transport.md) — accepted and implemented (#342): the Agent API transport. REST plus long-poll over the existing Keystone HTTP API, authenticated by the runtime principal's bearer, bodies the `agentapi` types 1:1; MCP as a second transport, hand-rolled in the `agent-bridge` sidecar over the same handlers with no new authority.
- [ADR-W042](decisions/ADR-W042-voice-pipeline-speech-to-text.md) — implemented: voice part one. Voice messages on Telegram, Discord, Slack and Matrix, and dictation on the dashboard and Console, become text on the house's own Speaches engine (or OpenAI, only for an Agent that already sends out of the house); a stranger's audio is never transcribed, and a transcript cannot run a command or store a secret.
- [ADR-W043](decisions/ADR-W043-voice-pipeline-spoken-replies.md) — implemented: voice part two. Replies read aloud with Kokoro under the same privacy rule: a voice message is answered with a voice message, and the dashboard and Console have a listen button; spoken turns carry a 🎙️.
- [ADR-W044](decisions/ADR-W044-pantryos-integration.md) — proposed: the PantryOS integration. PantryOS may run on its own machine (connection-only); a thin HTTP adapter over PantryOS's `pantry-1` contract; PantryOS owns the grocery list when connected, with no silent fallback; `pantry.*` events and agent tools with risk tiers, including a step-by-step cook-along.
- [ADR-W045](decisions/ADR-W045-off-site-backup.md) — accepted, not yet implemented: off-site backup to S3-compatible storage. Opt-in only, ciphertext only; a `BackupTarget` kind; a per-plan route (`offsite-only`, or `local-and-offsite` via `restic copy`) with a separate password; a disk-space guard so a local copy never fills its disk; user-selectable plans, host folders and NAS shares; a whole-site recovery set (Statefile, runtime DB, sealed secrets) and a recovery kit.
- [ADR-W046](decisions/ADR-W046-markdown-knowledge.md) — proposed: the household keeps its knowledge as a folder of Markdown files. `knowledge-wildstone` owns the folder, its backup and six bounded agent tools (list, search, read, and write/append/update that never overwrite blindly and refuse executable content). `notes-silverbullet` is a removable browser UI over the same folder.
- [Hermes capability package and execution profiles](design/hermes-capability-package-and-execution-profiles.md) — #334, built on ADR-W039; H1–H7 and H9 implemented, H8 and H10 open: packaging Hermes as an optional capability bound to a Wildstone Agent, one container per Agent, three execution profiles (platform-only, sandboxed, host administration via Wildstone executors), threat analysis, spike plan and draft follow-up issues.
- [Agent API and Hermes runtime QA runbook](design/agent-api-qa-runbook.md) — a step-by-step manual and automated QA pass for the ADR-W039/ADR-W040/#334 work on `main`: CI-equivalent checks, and scenarios for runtime credentials, the HTTP Agent API, conformance, proposals, subscriptions, execution profiles, processing class, and the Hermes package.
- [Capability and design brief](butler-capability-and-design-brief.md) — non-normative rationale and deployment examples.
- `butler-memory.md` — notes generated from existing code, retained for repository inventory rather than as a normative source.
