# Wildstone HomeLab — Implementation Scope

**Version:** 1.0
**Status:** Accepted — D1–D4 resolved with platform owner; Phase 0 complete
(thesis gate GO, PR #1)
**Date:** 2026-07-30 (migrated into this repository 2026-08-19)
**Inputs:** `00-design-brief-v0.1.md`, `01-architecture-proposal.md` (merged)

> Provenance: authored 2026-07-30 alongside the architecture proposal in the
> reference repo (`chickenrat17/homelab-config`, session archive committed to
> this repo at `ac88281`); it never landed as a file in either repo's tree.
> Migrated here verbatim (status header updated) because it defines the
> E-milestone numbering (E0–E13) the ADRs and commit history reference.

---

## Purpose

The architecture proposal says *what* to build and *why*. This document says
*what the work actually is*: the repositories to create, the epics and tasks
inside each, how they depend on each other, what "done" means at each
milestone, honest effort sizing, and what to cut first if reality disagrees
with the plan.

## 0. Assumptions (challenge these first — they drive everything)

1. **Team = one person (Mika) plus AI agents**, part-time to
   nearly-full-time. All sizing below is in **focused solo-weeks (fw)** — a
   week of real engineering attention, agent-accelerated. If a second
   engineer joins, the critical path shortens less than you'd hope (it's
   sequential through the resource schema and plan artifact) but the package
   and Console tracks parallelize almost perfectly.
2. **Agent-heavy development is a first-class strategy, not a garnish.** The
   architecture is unusually agent-friendly: typed protocols, closed
   operation surfaces, package-as-data, and a reference deployment that acts
   as a fixture library. Tasks below are annotated ◆ where they are
   near-fully delegable to coding agents with review, vs ▲ where they need
   sustained human judgment.
3. **The reference deployment (this repo) is the test oracle.** Every epic's
   acceptance test is ultimately "does it reproduce something HeadOverHalls
   Home already does by hand?"
4. **Product code lives in a new repo** (`wildstone`, monorepo — see §1).
   This repo stays what it is: the reference deployment, and progressively
   the dogfood target.
5. No external funding pressure; correctness of the five locked decisions
   (proposal §10, Phase 0) outranks calendar speed.

## 0.5 Decisions log (resolved 2026-07-30 with platform owner)

| # | Decision | Call | Scope impact |
|---|---|---|---|
| D1 | MVP target | **Full MVP as scoped** (E1–E13 complete: Photos+Media+Documents, Butler write tools, public ingress). The §3 cut line remains the pre-agreed descope order if slipping — it is a contingency, not the plan. | Calendar: ~72 fw ≈ 9–16 months depending on pace |
| D2 | Audience year one | **You-first.** Dogfood target is HeadOverHalls; distribution = GitHub releases only. No `get.wildstone.dev` domain, hosted install script, or public docs site until a second user exists. | E0.1 shrinks ~0.5 fw; E9 bootstrap installs from a GitHub release URL; signing kept (cheap now, painful to retrofit) |
| D3 | Repo & name | **`chickenrat17/wildstone-homelab` (created 2026-07-30), "Wildstone" name locked.** Accepted rename risk; module paths (`github.com/chickenrat17/wildstone-homelab/...`) and binary names (`wardstoned`, `keystone`, `wild`) may harden immediately. | Unblocks E0.1 today |
| D4 | Butler dev LLM | **One cloud (Anthropic API) + one local (Ollama) from day one.** Tool-reliability probe and degraded modes developed against both in parallel. | S4/E12 as scoped |

## 1. Repository and delivery infrastructure (Epic 0)

One monorepo, `wildstone`:

```
wildstone/
├── proto/            # gRPC + resource schemas — THE contract, versioned first
├── wardstone/        # host agent (Go)
├── keystone/         # control plane (Go): api/, reconciler/, capability/,
│                     #   policy/, plan/, statefile/, secrets/, butler/
├── console/          # web UI (TypeScript/React)
├── cli/              # `wild` (Go, shares client lib with butler)
├── packages/         # capability packages (data) + package toolchain
├── installer/        # get.wildstone.dev bootstrap + preflight
├── testing/          # e2e harness, fixture VMs, restore-drill runner
└── docs/
```

Tasks:
- **E0.1** ◆ Repo scaffold, Go workspace, CI (lint/test/build matrix
  amd64+arm64), release signing keys + artifact pipeline (goreleaser or
  equivalent, cosign signatures) publishing to GitHub releases only (D2 —
  no hosted install endpoint yet). *1 fw*
- **E0.2** ▲ **e2e test harness**: scripted disposable Ubuntu 24.04 VMs
  (Proxmox API against the existing homelab — it's right there — plus
  cloud-image fallback for CI), snapshot/restore between tests. This is the
  highest-leverage infrastructure in the whole plan: every subsequent epic's
  exit criterion runs on it. Build it *before* the spikes, not after. *2 fw*
- **E0.3** ◆ Fixture extraction: transcribe this repo's working configs
  (Authelia, Caddy, LLDAP, Immich OIDC wiring, Gluetun) into
  `testing/fixtures/` as golden files. **Explicit scrub step:** working
  configs carry semi-secrets inline (domains, internal hostnames, OIDC
  client IDs) even where `.env` files were untracked — every fixture is
  scrubbed to placeholder values, and the wildstone repo runs a secret
  scanner in CI from commit one (belongs in E0.1's scaffold). *1 fw*

**Epic 0 total: ~4.5 fw.** Nothing else starts until E0.2 exists in rough
form.

## 2. Phase 0 — Spikes (retire the risky bets)

Per proposal §10, four spikes; each produces throwaway-quality code but a
**keep-quality decision record** (ADR in `wildstone/docs/`).

| Spike | Content | Proves / locks | Size |
|---|---|---|---|
| **S1 Wardstone protocol** ▲ | Dial-out gRPC agent, mTLS bootstrap w/ join token, 8 ops (EnsureContainer, StopContainer, ContainerLogs, PullImage, WriteManagedFile, ReadManagedFile, HostFacts, Health), reconnect storm test, A/B self-update happy path | Locked decision #1: typed no-exec protocol; ADR on proto layout & versioning | 2.5 fw |
| **S2 Reconciler + Plan** ▲ | Container/Route/Volume kinds; spec→diff→Plan→approve→apply→health-gate→rollback, end to end on a fixture VM. **Plus one external-resource kind end to end** (a Cloudflare DNS record: create, observe drift, handle partial failure, tolerate rate limits) — external control planes are the most likely 2× overrun in E3/E6 and must be priced here, not discovered in month six | Locked decisions #2 (resource schema + Statefile format) and #3 (plan artifact shape); external-resource controller pattern | 3.5 fw |
| **S3 Vertical slice: packages** ▲ | Hand-write Identity + Photos(Immich) packages; from blank VM to working OIDC-authenticated Immich behind Caddy, entirely via S1+S2 machinery | Locked decision #4 (package format); validates the entire product thesis in one demo | 3 fw |
| **S4 Butler loop** ◆ | Provider abstraction (OpenAI-compat + Ollama + Anthropic), read tools over S2's API, one plan-mediated write tool, tool-reliability probe, injection red-team afternoon | AI feasibility across model tiers; tool-surface shape | 2 fw |

Sequencing: S1 → S2 → S3 strictly serial (each consumes the last). S4
overlaps S3. **Phase 0 total: ~10 fw elapsed (~2.5 months part-time).**

**Phase-0 exit gate (go/no-go):** S3 demo passes on a clean VM in under 30
minutes unattended, and the five locked decisions each have an ADR. If S3
fails ugly — package-generated identity wiring turns out to need per-app
hand-tuning that resists templating — the product thesis itself needs
revisiting *before* MVP investment. That's the point of doing it third.

## 3. Phase 1 — MVP epics

Everything below assumes spike learnings are folded in (spike code rewritten
to production quality where needed — budgeted inside each epic).

### E1 — Wardstone, production grade — *4 fw*
- ◆ Full operation surface (proposal §4.2 table: ~25 ops incl. storage
  probes, mounts, usage metrics, systemd status).
- ▲ Internal allowlist enforcement + ownership-label guards + tests that
  *prove* the no-exec / managed-prefix invariants (these tests are security
  documentation).
- ◆ Signed A/B self-update with revert; journald audit stream.
- ◆ Cert rotation; protocol N−1 compatibility test in CI.

### E2 — Keystone core: API, Statefile, runtime DB — *5 fw*
- ▲ Resource schema v1 (from S2 ADR) with codegen for Go + TS clients.
- ◆ API gateway: sessions, tokens, RBAC roles (admin, operator, butler),
  rate limits, audit middleware, **redaction layer** (secrets never serialize
  — property-tested).
- ◆ Statefile engine: git-backed store, structured commits, out-of-band
  commit ingestion (validate → plan → accept/revert).
- ◆ SQLite runtime DB: observed state, events, plan records, audit,
  migrations framework.
- ▲ Principal model carries an acting-identity (on-behalf-of) field on every
  session/token from day one — schema-level only at MVP (ADR-W001
  foundation).

### E3 — Reconciler + Plan service — *5 fw*
- ▲ Controllers per kind: Container, Volume, BindMount, Route, DnsRecord,
  OidcClient, LdapGroup, BackupPlan, HealthCheck, Secret-ref, TunnelIngress,
  NetworkAttachment.
- ▲ Drift classifier (repair / report / adopt-or-exempt), bounded
  remediation + circuit breaker.
- ▲ Plan compiler: spec-diff → ordered ops + risk grades + blast-radius
  summary; health-gated executor with per-step reverse ops.
- ◆ Event stream (feeds Console live view, ntfy, Butler).

### E4 — Policy Engine — *2.5 fw*
- ▲ Standing invariants from proposal §9.2 as validation rules on every plan.
- ◆ Classification vocabulary (adopted unchanged from this repo), risk-tier
  policy table, per-principal scoping, exemption objects (explicit, logged).
- ◆ Risk-tier vocabulary includes `domestic` — defined and validated, unused
  until v2 (ADR-W001 foundation).

### E5 — Capability Engine + package toolchain — *4 fw*
- ▲ Package format v1: templates → resources, dependency/trait resolution
  (`postgres(pgvector)` etc.), lifecycle hooks as containers with declared
  mounts.
- ◆ Package build/lint/sign toolchain + local registry serving; version
  resolution and upgrade-plan generation.
- ▲ `agentTools` declaration block in format v1: parsed, linted, stored — not
  executed until v2. Must land before the format freezes at the Phase-0 exit
  gate (ADR-W001 foundation).

### E6 — Infrastructure packages — *4 fw*
Identity (LLDAP+Authelia), Ingress (caddy-public/-private + Cloudflare
Tunnel + Tailscale join), DNS, Monitoring (Uptime Kuma + Wardstone metrics),
Notifications (ntfy), **Backups (restic engine + BackupPlan controller +
restore plans + monthly restore-drill duty)**, Dashboard.
- Mostly ◆ transcription from fixtures, but ▲ for backups/restore-drill logic
  (~1.5 fw of the 4 is the backup engine — it's the MVP headline feature).
- ▲ Known sharp edge: Authelia's OIDC clients are static config, so every
  capability install bounces the identity provider and briefly breaks every
  login in the house. The Identity package must batch OIDC-client changes
  within plans and schedule the restart as an explicit, user-visible plan
  step (and we track Authelia's dynamic-client work upstream).

### E7 — Launch app packages — *2.5 fw*
Photos (Immich), Media (Jellyfin incl. LDAP plugin config from fixtures),
Documents (Paperless). Each: package + upgrade path + backup hooks + e2e test
(install → OIDC login probe → backup → restore-alongside). ◆ heavy — this is
the most delegable epic; fixtures make it transcription + verification.

### E8 — Secrets path — *2 fw*
- ▲ Encrypted store (age-style), key custody in `/etc/wildstone`, Wardstone
  materializer, generation-at-install, rotation plan type. Small epic,
  security-critical, mostly design-review time.

### E9 — Installer + onboarding — *3.5 fw*
- ◆ Bootstrap script (fetched from GitHub releases per D2; a
  `get.wildstone.dev` alias is a post-you-first nicety): preflight, Docker
  install, wardstoned install, Keystone pull-up, pairing code.
- ▲ Onboarding flow in Console (proposal §7.2, steps 1–7) including live
  storage/DNS/tunnel validation probes and **the Restore path** (same flow,
  branches at step 3 — do not bolt this on later; it shares every probe).

### E10 — Console — *6 fw*
- ▲ Information architecture: capability-centric home, plan review/approval
  screen (the product's signature interaction — budget real design time),
  drift/incident view, storage view, settings, family-member management
  (LLDAP CRUD via API).
- ◆ Implementation against generated TS client; live updates from event
  stream. Deliberately boring visual design at MVP.

### E11 — `wild` CLI — *1.5 fw*
◆ Thin mapping over the API: status, plan list/show/approve, capability
enable/disable, logs, audit replay, backup run/restore. Shares client lib.

### E12 — Butler — *4 fw*
- ◆ Productionize S4: session store, transcript audit, provider config +
  validation probe UX, degraded modes (Q&A-only, reduced tool surface).
- ▲ Diagnostics quality pass: dependency-chain walking over real fixture
  failures (broken OIDC secret, dead DB, full disk, detached NFS — build the
  failure-fixture library, ~8 scenarios); morning-report duty.
- ▲ Injection hardening per proposal §6.3 + red-team day.
- ◆ Session store binds every session to a principal (do not "optimize" to a
  singleton admin session — ADR-W001 foundation).

### E13 — Hardening, docs, alpha release — *3 fw*
- ▲ **The exit drill:** total-host-loss restore, unattended, on the e2e
  harness — run weekly in CI from the moment E6-backups lands.
- ◆ User docs (install, concepts, recovery), operator runbook, threat-model
  doc refresh against as-built.
- ▲ Dogfood migration: HeadOverHalls infra VM capabilities recreated by
  Wildstone on a parallel VM; each friction filed and burned down.
  **Cutover rule (two-masters problem):** hand-managed and
  Wildstone-managed resources may coexist for workloads (the drift
  classifier's never-delete-foreign rule protects them), but ingress and
  identity are singular — two Caddys cannot both own :443 and two
  directories cannot both be the identity source. Apps cut over
  per-capability via DNS; **identity migrates exactly once**
  (LLDAP export/import, then the old directory is retired) and ingress
  ownership transfers atomically per hostname, never incrementally.

### MVP sizing summary

| | fw |
|---|---|
| E0 delivery infra | 4 |
| Phase 0 spikes | 10 |
| E1–E13 MVP | 47.5 |
| Concierge foundation (ADR-W001, folded into E2/E4/E5/E12) | 1.5 |
| Integration/slack (15%) | ~9 |
| **Total to MVP exit** | **~72 fw** |

At 3 focused days/week that is **≈ 14–16 calendar months solo**; at
full-time-equivalent with strong agent leverage, **≈ 9–11 months**. The
proposal's "4–5 months after spikes" holds only for the cut-down MVP below —
worth being honest about now rather than in month six.

### The cut line (pre-agreed descope order if slipping)

Cut in this order, never a different one:
1. E7 shrinks to Photos only (Media/Documents become post-MVP packages).
2. E12 Butler ships read/diagnose-only (no write tools) — the plan/approval
   UX doesn't depend on it.
3. E10 Console family-management screen → CLI-only for alpha.
4. Cloudflare Tunnel → Tailscale-only alpha (private-first homelab still
   fully works; public ingress is v1).

Never cut: backups/restore drill, secrets path, policy invariants, the plan
artifact. Those are the product.

## 4. Sequencing and critical path

```
E0.1 ─ E0.2 ─ E0.3
        │
        S1 ── S2 ── S3 ──────────────┐   (S4 overlaps S3)
        │                            ▼
        E1 ═══ E2 ═══ E3 ═══ E5 ═══ E6 ═══ E9 ═══ E13   ← critical path
                │        │     │      │
                E4       │     E7 (parallel, agent track)
                E8       │     E12 (parallel)
                E10 (starts at E2-API-freeze, parallel)
                E11 (anytime after E2)
```

The critical path is **contract-shaped**: proto/resource schema (E2) and plan
artifact (E3) gate everything. Two practical rules fall out:
1. **Freeze the API in layers.** Publish the generated TS/Go clients the day
   E2's schema lands, even while server internals churn — that unblocks the
   Console, CLI, and Butler tracks (the parallelizable ~40% of the work, and
   the most agent-delegable 40%).
2. **Run a package track continuously.** From S3 onward, someone/some agent
   is always converting a fixture into a package. Packages are the product's
   content; they should never wait for a "packages phase."

## 5. Milestones with exit criteria

| | Name | Exit criterion (demoable, binary) | Target (FTE-ish pace) |
|---|---|---|---|
| **M0** | Harness | e2e harness spins a clean VM, runs a test, restores snapshot, in CI | +1 month |
| **M1** | Spike gate | S3 demo: blank VM → OIDC-authenticated Immich in <30 min unattended; 5 locked-decision ADRs merged | +3 months |
| **M2** | Skeleton | `wild` + Console can: install identity+ingress capabilities on a fresh VM via plans, show drift, auto-repair a killed container | +5.5 months |
| **M3** | Backbone | Backups land: nightly restic + restore-alongside + first automated restore drill passes; secrets path live; Photos capability e2e green | +7.5 months |
| **M4** | **MVP / private alpha** | Onboarding §7.2 end-to-end incl. validation; **total-host-loss drill passes unattended**; Butler diagnoses the 8 failure fixtures correctly; HeadOverHalls infra-VM equivalent recreated by Wildstone | +9–11 months |
| **M5** | Dogfood cutover | This repo's homelab runs *on* Wildstone (capability-by-capability migration complete for infra + 3 apps); v1 scoping doc written from friction log | +MVP+2 months |

## 6. Top risks to the plan (not the product)

| Risk | Signal | Mitigation |
|---|---|---|
| Solo-founder attrition (the real #1) | fw/week trending down | Milestones are demo-shaped on purpose; M1's demo is the motivation engine. Keep a "ship something visible monthly" rule |
| Package abstraction leaks (every app needs bespoke glue) | S3/E7 templates sprouting per-app conditionals | Caught at M1 gate by design; fallback: narrower trait set + more code-in-engine, fewer generic templates |
| Plan/rollback engine complexity spiral | E3 exceeding budget by >50% | Reduce reverse-op coverage to snapshot-restore-only rollback for MVP (coarser but sound) |
| External control planes (Cloudflare, Tailscale) harder than container-shaped kinds: partial failures, token custody, rate limits, expensive drift observation | S2's external-resource slice overruns | Priced in S2 by design; fallback: reconcile external resources on-demand + scheduled (not continuously), and narrow MVP to the tunnel-ingress happy path |
| Console eats the schedule | E10 > 6 fw | It's off the critical path by design — hold the line on boring UI; the plan screen is the only screen that must be good |
| Agent-generated code quality debt in security-critical paths | — | Standing rule: Wardstone internals, policy engine, secrets path get human-written tests and line-by-line review (▲); delegation lives in packages/Console/CLI (◆) |

## 7. Butler and concierge mode — phased scope (ADR-W001, ADR-W030)

ADR-W030 supersedes the rule that the family-facing product ships only as one
v2 block after M5. Its v1 pulls forward a narrow text-based path across the
existing Household and Operator bots/Console: explicit identity linkage and
handoff, durable tasks, scoped tools and grants, permission-aware continuity
and selected-source memory, adapter connections, model routing, and restore
quarantine. Each slice ships only with its own structural and recovery evidence;
accepting W030 is not implementation authorization.

The C1–C5 table remains a capability inventory and historical size estimate,
not a requirement to deliver one 11.5-fw epic. ADR-W030 Part C and the
non-normative design brief define the new sequence. Child, document/image
intake, email-to-calendar writes, and voice remain later independently reviewed
slices.

| Epic | Content | Size |
|---|---|---|
| **C1 Tool gateway extension** ▲ | `domestic` tier enforcement (gateway-mediated, plan pipeline bypassed), MCP client wrapped behind the Wildstone tool abstraction with the MCP protocol version pinned per package declaration (MCP is a transport detail, never the package format), REST-manifest fallback, per-call group×tool×classification checks, per-principal rate limits | 2.5 fw |
| **C2 Identity propagation** ▲ | OIDC token exchange where supported; per-user API credential minting at capability install as fallback; `shared-identity` tool marking + policy handling; per-app mapping (HA person mapping). Expect negotiation-per-app: packages declare *best achievable* attribution per app rather than the epic fighting for uniform delegation | 2.5 fw |
| **C3 Family chat surface** ▲ | Family-facing Butler UI (not the admin Console): authenticated via Authelia, mobile-first, per-member sessions; admin kill-switch global/per-group | 3 fw |
| **C4 Tool-exporting packages** ◆ | Home Automation package exports HA conversation/intent + MCP tools; PantryOS (or second connected app) exports list/inventory tools; cross-service orchestration recipes | 2 fw |
| **C5 Safety + audit** ▲ | Cross-principal red-team (kid-vs-policy escalation attempts), per-member audit views, concierge-specific policy UX (group tool grants). Includes *knowledge* scoping, not just action scoping: non-admin principals' read tools redact policy configuration, so the Butler cannot helpfully explain to a kid how the parental controls work | 1.5 fw |
| **Total** | | **~11.5 fw** |

Voice remains post-v1. Local transcription and Kokoro text-to-speech are the
current preferences, using the same identity/conversation boundary; the exact
transport and processing pipeline remain unscoped.

## 8. Immediate next actions (this month)

1. Decide the two challengeable assumptions: solo pace (§0.1) and
   MVP-vs-cut-down target (§3 cut line) — this picks the calendar.
2. Create the `wildstone` monorepo; land E0.1 scaffold + CI.
3. Stand up E0.2 harness against the existing Proxmox host.
4. Start E0.3 fixture extraction from this repo (fully delegable today).
5. Begin S1.
