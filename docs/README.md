# docs/

Wildstone product documentation and ADRs.

- `02-implementation-scope.md` — the implementation scope (epics E0–E13,
  milestones M0–M5, sizing, cut line). Authoritative here.
- `decisions/` — Wildstone ADRs (W-series).

User and operator docs (E13, ADR-W018):

- `install.md` — installing Keystone, joining nodes, `wild`, Console,
  Butler.
- `concepts.md` — the mental model: Statefile, plans, reconcile,
  principals, capabilities, secrets, backups, the Butler's ceiling.
- `recovery.md` — the recovery contract and total-host-loss runbook
  (executed weekly by `scripts/exit-drill.sh`).
- `threat-model.md` — the threat model refreshed against as-built.
- `vision.md` — why Wildstone exists: the home lab as household
  infrastructure, the Butler as the counter-image of Alexa, the
  two-butler arc, and the standing principles.
- `migration.md` — the dogfood migration runbook and the two-masters
  cutover rule.
- `friction-log.md` — friction found while dogfooding, filed and
  burned down before alpha exit.

The design brief (`00-design-brief-v0.1.md`) and architecture proposal
(`01-architecture-proposal.md`) remain in the reference deployment repo:
`chickenrat17/homelab-config` → `docs/architecture/wildstone/`.

Agent / Butler architecture. **Start with the baseline** — it is the
accepted statement of intent and supersedes conflicting ADR language:

- [Agent architecture baseline](agent-architecture-baseline.md) — accepted
  2026-09-10. What a Butler is, shared memory with audiences, secrets and
  the privacy gateway, routines, and the AC01–AC16 acceptance criteria.
  Generated from a `.docx` original; regenerate with
  `scripts/docx-to-md.py`. Tracked by
  [#151](https://github.com/chickenrat17/wildstone-homelab/issues/151).

Supporting, and older than the baseline:

- [ADR-W030](decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md) — continuity, durable work, and scoped integrations. A3's deferral of vector retrieval and W031 A4's per-Butler memory scope are both superseded by the baseline.
- [ADR-W031](decisions/ADR-W031-butlers-are-user-created-specialists.md) — Butlers as user-created specialists.
- [Capability and design brief](butler-capability-and-design-brief.md) — non-normative rationale, deployment examples, and delivery sequencing behind the ADR.
- `butler-memory.md` — **notes, not a source.** Generated on 2026-09-10 by
  reading existing code; its step ordering was invented and is what
  deferred semantic recall. Kept for its accurate inventory of what
  `runtimedb` already contains.
