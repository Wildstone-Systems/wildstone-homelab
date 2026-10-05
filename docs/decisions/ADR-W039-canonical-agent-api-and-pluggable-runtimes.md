# ADR-W039: Canonical Agent API and pluggable agent runtimes

- **Status:** accepted (#333); implemented except the items in §2. Amends ADR-W033 B6 for external runtimes (§9).
- **Governed by:** ADR-W033 where they overlap, as amended by §9 of this ADR.
- **Scope:** the contract between Wildstone and whatever performs an Agent's reasoning: the resource boundaries of an Agent API, the minimum behaviour of a compatible runtime, and how the Keystone-hosted runtime and an external runtime (Hermes first, OpenClaw as a check) both sit behind it
- **Depends on / relates to:** #334 (Hermes capability package and execution profiles), [`ADR-W033`](ADR-W033-the-agent-design.md) and [`docs/agent-system-design.md`](../agent-system-design.md) (the design of record this ADR extends rather than reopens), [`ADR-W040`](ADR-W040-agent-api-transport.md) (transport), [`ADR-W041`](ADR-W041-agent-runtime-contract.md) (the `native` runtime's loop), ADR-W024 A5, ADR-W026 A10, ADR-W030 B3, ADR-W017 A2, ADR-W006 A1/A3, CONTRIBUTING ground rules 3 and 5
- **Implementation:** package map in §10.

`Agent` and `Butler` mean the same architectural object. Product-facing text may say Butler; technical text may say Agent. `Runtime` means the thing that turns a turn or an event into model calls and capability calls.

Runtime type `native` = Keystone-hosted; its loop is chosen per deployment by `-agent-runtime legacy|native`: `legacy` is `keystone/agentruntime/legacy.Runner`, `native` is the OpenAI Agents SDK sidecar (ADR-W041). Runtime types `hermes` and `openclaw` are external Agent API clients.

---

## 0. The decision in one paragraph

Wildstone owns the platform and the Agent contract; a runtime owns cognition. Every runtime — Keystone-hosted or external — is a **client of one Agent API**, holding its own runtime credential, discovering capabilities from Wildstone, invoking them through the same `authn → authz → policy/approval → adapter → execute → audit` chain, reading and writing durable memory through Wildstone's scope and audience rules, and receiving events only where the deterministic engine or an Owner-approved subscription escalates. The ceiling does not move: a runtime is at most a `butler`-role principal (read and propose), never approves, never widens an audience, never creates identity. The policy-enforcing pieces (`Toolset`, `ToolAuthorizer`, the ledger) stay on the Wildstone side as the gateway; the Keystone-hosted loop is just another client of it, in-process. Changing an Agent's runtime replaces one record and one credential; identity, permissions, Connections, routines and memory stay. This is not "replace Wildstone with Hermes" (ADR-W030 B3 stands); it is the separate compatibility decision B3 said would be needed.

---

## 1. The native loop's boundary

The Keystone-hosted loop reaches Wildstone only through `agentapi.Client`, pinned by `keystone/agentruntime/legacy/structural_test.go`.

---

## 2. Not yet built

- **C7 — optional out-of-process Keystone-hosted runtime.** The Keystone-hosted loop runs in-process and its provider keys stay in the root-only key file; packaging it as a container with its provider key as a package secret is optional and not built.
- **Push delivery.** A runtime's declared `delivery: push` is recorded; nothing sends. Every runtime pulls (§4.1, ADR-W040 §4).
- **H8** — split SSH-backed sandbox tier for Hermes (#354).
- **H10** — the conformance suite has not been run against a real, packaged Hermes (#356).

---

## 3. Agent API

### 3.1 Principles

1. **One contract, all runtimes.** The Keystone-hosted loop and a Hermes profile use the same Agent API. Nothing the Keystone-hosted loop can do is unavailable to an external runtime, and nothing an external runtime is denied reaches the Keystone-hosted loop by a side door. Ground rule 3 is a transport property, checked structurally (§7).
2. **The chain stays in Wildstone.** Every capability call runs `authn (runtime credential) → authz (three-layer resolver) → policy/approval (tier, Plan, confirm) → adapter/executor → audit`. The runtime sees a manifest and a result, never an integration credential, a Statefile handle or a DB.
3. **The ceiling is the ceiling.** A runtime credential carries at most the `butler` role's verbs: it cannot approve, manage, emit, converse, create identity, widen an audience or grant a tool. `TestStructuralCeilingCannotBeWidened` (`keystone/butlerstore/authority_test.go:58`) and the no-reconcile-import test (`keystone/butler/structural_test.go:19`) pin Wildstone-side code that does not move.
4. **Authority is derived at use time, never copied to the runtime.** A runtime names a Wildstone-issued task handle; Wildstone resolves Person, Connection, audience and grants from it at every call (W033 A0, DSN §10.1). A runtime's own claim about who is asking is ignored, as `MemoryDestination` already is (`memorydisclosure.go:5`).
5. **Deterministic first.** The automation engine consumes the household stream. A runtime is invoked only where an approved routine or subscription escalates, and a delivered event is untrusted input that confers nothing (ADR-W024 A4).

### 3.2 Resource families

The families below are conceptual boundaries, not endpoint definitions. For each: what Wildstone stays authoritative for, what the runtime owns, and what existing code backs it.

| Family | Wildstone is authoritative for | Runtime owns | Backed by |
|---|---|---|---|
| **Identity & session** | Agent identity; runtime credential issue/rotate/revoke; credential → Agent binding; heartbeat and health; which Person/Connection a turn belongs to | Its process identity and its own model-provider credential | `Butler` record; principal table (ADR-W007 A2); `butlersessions` |
| **Capabilities** | The manifest narrowed by effective authority; every invoke decision, tier, confirm/approval, execution, redaction, envelope, audit | Which capability to call and when; how to present results | `Toolset`; `ToolAuthorizer`; `domestic.Gateway`; `finishOutcome` (`gateway.go:405-409`) |
| **Memory** | The three durable scopes, audiences, provenance, corrections, forgetting, per-agent restrictions, disclosure by destination and processing class | Reasoning state, transcripts, profile files, embeddings, skills, scratch | `butlermemory.go`; `memoryscopes.go`; `memorydisclosure.go`; `MemorySeams` |
| **Events & subscriptions** | The household and operational streams; the deterministic engine; an Agent's Owner-configured subscriptions; per-subscription cursor, ack, replay, retention | How it reacts; wake a session or start fresh | `householdevents.go`; `automation.Engine`; escalation seam |
| **Work** | Task identity, derived authority, budgets, checkpoints, waits, evidence, fencing, stall notices | The reasoning between checkpoints | `TaskLedger`; `butlertasks.go`; `butlersteps.go`; `butlertrace.go` |
| **Proposals & approvals** | `propose_change` → Plan → policy → Owner approval; the outcome; who may approve (never a runtime) | Deciding what to propose | `tools_write.go:151`; `submitProposalOpts`; `ownerreview.go` |
| **Routines** | Records, versions, Owner activation, the routine principal, escalation to the assigned Agent | The wording or reasoning a step asks for | `householdroutines.go`; `routineprincipal.go`; `routine_execution.go` |
| **Notifications & Connections** | Every outbound message: Connection, admission, disclosure, quiet hours, dedupe; the chat credentials | Composing the text | `Connection`; `/chat/outbound`; `StallNotifier`; notices |
| **Audit** | Every capability call, memory access, proposal, delivery, credential event; the runtime's reported run lifecycle | Reporting its run lifecycle honestly | `audit.go`; `butlersteps.go`; `butlertrace.go`; `domestic.acted`; gateway JSONL |

Two rules hold across all families. A runtime never receives an integration credential: it holds its runtime credential and its own model-provider credential, nothing else (DSN §12.3; ADR-W012; threat model "a container is never handed the node's secrets"). And a runtime never sends a message directly: platform tokens stay in the chat-adapter packages and outbound text goes through Wildstone Connections, where admission and disclosure are evaluated.

### 3.3 The runtime record on the Agent

```yaml
agent: mika                      # identity, class, surface, members,
                                  # tools, connections, routines, memory restrictions
runtime:
  type: native | hermes | openclaw
  endpoint: https://hermes-mika.internal/   # absent for native; a Route host or
                                             # container/<name>:<port> for a packaged runtime
  credentialRef: runtime-mika-3             # a Keystone principal; rotated, revoked here
  health: unregistered | connected | degraded | offline | revoked
  lastHeartbeat: 2026-09-24T09:12:00Z
  negotiated:
    capabilities: [get_household_events, do_household_action, recall, remember, ...]
    delivery: push | pull                    # recorded; only pull is served (§2)
    processingClass: local_only | cloud      # declared by the Owner at registration
  executionProfileRef: platform-only         # platform-only | sandboxed | host-admin
```

`runtime` is one field of the Agent, stored in the `butler_runtimes` side table with an immutable `runtime_principals` binding. Replacing it replaces nothing else. For `type: native` the endpoint is absent, the credential is still a real minted principal (`runtime-<butler>-<n>`, which is also the session and audit principal), and the negotiated set is whatever the Toolset offers. For `native` the Agent's ordered model/provider chain (ADR-W033 B6) applies unchanged; code not yet conformant: today it is one assigned model. For an external runtime the model chain is runtime-owned and Wildstone records only the Owner-declared processing class (3.5); this is the ADR-W033 B6 amendment in §9.

Health is Wildstone's own bookkeeping: a runtime's heartbeats set `connected`, and a sweeper marks a non-native runtime `offline` when none arrives within `-agent-heartbeat-ttl`. Wildstone never probes a runtime.

### 3.4 The capability call

```text
runtime ──(runtime credential + task handle + capability name + args)──▶ Agent Gateway
   │                                                                          │
   │   1 authn: credential → principal → bound Agent; disabled/revoked ⇒ 401  │
   │   2 handle → Task → Person, Connection, audience, budget (derivedAuthority)│
   │   3 authz: ToolAuthorizer(ToolAuthorityQuery{...}) ∘ WithConnection ceiling│
   │   4 tier: read ⇒ run; domestic ⇒ domestic.Gateway; confirm ⇒ notice + wait │
   │           mutating infra ⇒ propose_change ⇒ Plan (never applied here)     │
   │   5 executor/adapter resolves any secret below inference, by reference    │
   │   6 result: redact → bound → envelope-as-untrusted                        │
   │   7 audit: ledger step/attempt + operational stream + API audit row       │
   ◀──(result | waiting(reason) | denied(reason))────────────────────────────────┘
```

Steps 2–7 run in `agentgateway.Invoke` over the existing `ToolGateway`/`HouseholdGateway` dispatch and ledger checkpoints; the runtime supplies a credential and a handle, never a closure or a context. `Invoke` holds a reservation lock across its budget check and step append, because a runtime may issue tool calls concurrently.

Contract details:

- `Invoke` folds an authorization refusal into `Outcome: denied` with HTTP 200, an ordinary outcome the runtime handles. Every other Agent API method returns HTTP 403 on an authorization failure.
- Invoke authorization reuses the `butler` role's `read`/`propose` verbs; there is no new RBAC verb.
- Two closed wait-reason sets exist: `WaitReason` (invoke-level: `approval_pending|confirm_pending|runtime_offline|budget_exhausted`) and `WorkWaitReason` (task-level, the ledger's own values).
- `Checkpoint` returns a `CheckpointReceipt{Seq,Attempt,Epoch}`; the epoch fences a superseded attempt.
- A grant that changes mid-session applies from the runtime's next run; a runtime refreshes its tool list by calling `Manifest` again (MCP `tools/list_changed` is not sent).

### 3.5 Memory boundary — decided

**Wildstone-authoritative:** the three durable scopes (`personal`, `household`, `agent_local`), people and relationships, permissions and grants, routines, home and application state (live applications remain authoritative for live state, DSN §11.8), and any cross-agent context an Owner has authorised. `agent_local` stays Wildstone-owned even though it is per-Agent: an Owner can inspect, export, transfer or promote it at Agent deletion (DSN §11.5), which a file inside a runtime container cannot offer.

**Runtime-local:** reasoning state, `SOUL.md`/`USER.md`/`MEMORY.md`-style profile files, session transcripts and their FTS indexes, embeddings, skills, scratch files, and whatever "learning" the runtime does about how to work. The Keystone-hosted loop's equivalent is its conversation history and nothing else.

Three rules:

1. **Promotion goes through `remember`.** A runtime-local fact becomes Wildstone-held knowledge only by calling the `remember` capability, which records it with provenance, kind and confidence in the scope the caller may write (personal for the Person on the handle; `agent_local` for the Agent; never household — as today, `remember` cannot create Household memory (`butlerstore/memory.go:140-141`), and promotion to Household stays a Person-approved product control, W033 C4), with the audience rules of `butlermemory.go:266` applied. Agents do not autonomously promote Personal into Household (DSN §11.4); a runtime is an Agent.
2. **Runtime-local memory never confers authority.** The authority resolver does not read Wildstone's memory table (`butlermemory.go:8-13`); it cannot read a runtime's files at all. A runtime that "remembers" it was allowed something is exactly as authorised as one that does not.
3. **Disclosure is filtered at the gateway, by destination and processing class.** `recall` binds `MemoryReadContext` from the task handle's Person, Agent and destination, as `BindMemoryRead` does today, and additionally withholds `cloud_processing: local_only` records from a runtime whose declared processing class is not `local_only`. Wildstone cannot verify which model an external runtime calls; it can refuse to hand protected content to a runtime the Owner did not declare local-only. This is the gateway-side replacement for wrapping the provider object (C10). Undeclared fails closed.

### 3.6 Events and subscriptions

```text
publisher ──emit──▶ household event log (seq, idempotent) ──cursor──▶ automation.Engine
                                                                          │
                                                   rule fires ────────────┤
                                                   deterministic action ──┤──▶ domestic.Gateway
                                                   escalation ────────────┤──▶ Agent runtime (wording only, tool-free)
                                                   subscription match ────┴──▶ Agent runtime (bounded Task)
```

A **subscription** is Agent configuration, versioned and approved by an Owner like a routine (DSN §15): match rule, output audience, permitted capability set, budget. A subscription version is an unattended principal treated exactly like an approved routine version: only a human Owner approves its exact version and any edit needs reapproval (ADR-W033 E2, E4); it runs with no active Person and never borrows its proposer's or approver's authority (E6, E6.0); what it may read is bounded by its approved output audience (C7); it may use Household secrets only through a grant to that exact version and never a Personal secret (D1, D3); and any Owner may stop it without a Plan (E6.1). The engine evaluates the match deterministically; on a match it creates a Task whose originating authority is the subscription version, not any Person — the same shape as a routine principal (`routineprincipal.go:10-16`). Delivery to the runtime is then a Work item plus an event envelope, which the runtime pulls and acknowledges (§4.1). A subscription is proposed as a Plan and takes effect only when an Owner approves it. The LLM is therefore invoked only where a routine escalates or an approved subscription matches, and what it may do afterwards is bounded by the subscription's approved capability set and audience. That set can only narrow the Agent's effective authority, never widen it: every invoke still runs the full resolver against the Agent's assignments, controls and grants, and a capability the Agent does not hold is refused however the subscription names it. Per-subscription cursor, ack, replay window and TTL are Wildstone-side state (the engine is already cursor-based; the log is already idempotent). Unacknowledged deliveries are redelivered; expired ones are recorded as `expired`, never silently dropped.

#### 3.6.1 Scheduled subscriptions and external-source checkpoints (amendment, epic #326 S03)

An external source such as a mailbox emits nothing onto the household event log, so a subscription may instead carry a **scheduled trigger**: due every cadence (default 15 minutes, bounded 5 minutes to 24 hours), evaluated by the same coordinator-held Keystone loop — never host cron (`automation/scheduled.go`, migration 0099). Everything above still applies; what changes:

- **Exact version.** Every edit bumps the subscription's version and stops dispatch at once (a run in flight is fenced). An Owner approves the exact new version through the ordinary Plan path; approving a version the row has since moved past enables nothing.
- **Person-bound, not Owner-attributed.** A subscription whose source is a Personal connection is bound to that connection's owning Person. Its work is authored by and owned by that Person, never by the household's first Owner. Owner approval remains necessary and never substitutes for the Person's own authorization of unattended use (ADR-W033 D2's narrow delegation; the evaluator is the `BackgroundAuthority` interface, deny-all until wired — wired since the epic #326 integration as `automation.PersonalBackgroundAuthority`, see below). This is the one exception to "never a Personal secret" above, and only under that intersection.
- **Dispatch gate, consulted before every run.** Coordinator lease, restore quarantine and the household emergency stop hold the whole family; per subscription, the approved version, the Person's admission (offboarding blocks), the source connection's ownership and state, and the `BackgroundAuthority` must all agree. A refusal is recorded as health (`blocked_reason`), not as a failure.
- **No overlap, bounded catch-up and retry.** At most one run per subscription is in flight (enforced by a partial unique index). A claim sets the next due time from the claim, so any downtime collapses into at most one catch-up run. `unavailable`, `stale`, Keystone errors and runs abandoned at lease expiry back off from one minute, doubling, capped at the cadence and one hour. `auth_failed` and `config_failed` hold the schedule until the Person resumes it. `empty` is a success.
- **Checkpoint contract.** Each subscription and source has an opaque JSON cursor. Only an `ok`/`empty` report commits one, only after the run's own results are durable, only if every fence the run was dispatched under still holds (including a live re-check of the authority), and only by compare-and-swap on the checkpoint version the run was handed. A crashed, fenced, restored or late run commits nothing, so the next run re-reads from the last committed position: committed work is never skipped, and uncommitted work is re-read at-least-once (consumers dedupe by fingerprint), never in a storm.
- **Evaluator wired (epic #326 integration).** `cmd/keystone` runs `SubscriptionEngine.RunScheduled` every 30 seconds in the coordinator-held block, on the Butler runner's ledger, with `automation.PersonalBackgroundAuthority`: `runtimedb.AuthorizeBackgroundUse` for the subscription's source connection and exact version, and the grant must name the run's Agent, its connection epoch and its canonical Person. A subscription with no Personal source is refused, as before. The evaluator also refuses a subscription edited past its approved version, so an edit that leaves the digested content alone still waits for the Owner's approval of the new version and needs nothing new from the Person. Scheduled match kinds register their dispatchers in `cmd/keystone/subscriptions.go` (`scheduledDispatchers`); `mail.changes` refuses until its consumer registers. The event family is not started yet: it must tick after the deterministic engine in the same loop.
- **Public API.** `GET /api/v1/agent-subscriptions[/{id}]`, `PUT /api/v1/agent-subscriptions/{id}` (edit; files the approval Plan) and `POST /api/v1/agent-subscriptions/{id}/control` (`pause`, `disable`, `resume`, `enable`, `cadence`) serve Console, CLI and Butler alike. A Person sees only subscriptions bound to their own source; Owners see all. An Agent may pause or disable, never resume, enable or change cadence.

### 3.7 Offline, revocation, runtime change

- **Offline** is a health state on the Agent (3.3), not a failure of anything else. Deterministic steps continue; an escalation falls back to the author's wording as it already does (`escalate.go:12-16`); a routine or subscription step that requires the runtime reports `waiting: runtime_offline`, in the spirit of ADR-W028 ("a capability waiting is not a node failing"). Queued deliveries wait behind the cursor within retention. On reconnect the runtime resumes from its cursor and from ledger-verified steps, not from a replayed transcript (ADR-W030 A4).
- **Immediate revocation** = disable the runtime principal (every call authenticates per request), drop the Agent's subscriptions, fence in-flight attempts (`FenceAttempts`) so late results are refused, and set `health: revoked`. All capability calls fail closed from the next request. No runtime cooperation is required; a revoked Hermes profile keeps reasoning into a wall.
- **Runtime change** replaces the `runtime` field: revoke the old credential, register the new runtime, negotiate, keep everything else. Identity, members, tools, Connections, routines, subscriptions and all three memory scopes are untouched. Runtime-local memory is the runtime's; exporting a Hermes profile into an OpenClaw workspace is between those two projects (OpenClaw documents importing Hermes memory files, [memory](https://docs.openclaw.ai/concepts/memory)), and is not Wildstone's problem.

### 3.8 Transport

ADR-W040: REST plus long-poll over the Keystone HTTP API; MCP as a translation layer in front of it.

---

## 4. Runtime contract

The minimum a Wildstone-compatible runtime must do. Everything else — reasoning strategy, skills, model choice, sandboxing of its own workspace — is its own business.

A compatible runtime **must**:

1. Authenticate every call with its runtime credential and accept rotation and revocation without restart.
2. Act only as the one Agent its credential is bound to; carry the Wildstone-issued task handle on every capability, memory and work call; never assert a Person, Connection or audience itself.
3. Discover capabilities from the manifest and treat it as the complete tool surface for Wildstone actions; never hold an integration credential or reach a household service directly.
4. Treat every capability result, memory record and delivered event as untrusted data (they arrive enveloped; Hermes and OpenClaw both already wrap external content).
5. Accept `denied`, `waiting(approval_pending | confirm_pending | runtime_offline | budget_exhausted)` and `expired` as ordinary outcomes and stop, rather than retrying a consequential effect blindly (DSN §16).
6. Long-poll deliveries and acknowledge each one; tolerate redelivery by idempotency key.
7. Report its run lifecycle for a task: started, ended with outcome, and each capability call it believes it made (Wildstone records the calls it actually served; the two are compared in audit).
8. Send outbound text only via the Notifications family; never operate a household messaging credential.
9. Write nothing into Wildstone memory except through `remember`; keep its own memory local and never treat it as authority.
10. Send heartbeats; a runtime that stops is marked `offline` after the heartbeat TTL.
11. Stay inside the platform network; expose no browser-facing model or tool endpoint.

A compatible runtime **may**: keep its own skills, transcripts, embeddings and profile files; use any model or provider it likes, subject to the Owner-declared processing class (the ADR-W033 B6 amendment, §9); run its own sandboxed workspace under an execution profile (#334); wake sessions on delivery rather than starting fresh ones; offer sampling or elicitation features that Wildstone ignores.

### 4.1 Lifecycle and who calls whom

```text
Owner at Console: create Agent (unchanged) ──▶ select runtime type
        │
        ▼
register runtime: Wildstone mints a runtime principal bound to this Agent,
                  shows the credential once (Console "consequential" freshness,
                  W033 A4.1: minting any bearer credential)
        │
        ▼
authenticate: runtime presents the credential; Wildstone records health: connected
        │
        ▼
negotiate: Wildstone offers the manifest narrowed by the Agent's effective authority;
           runtime declares delivery mode; the processing class is the one the
           Owner declared at registration, never a claim the runtime makes
        │
        ▼
assign permissions: unchanged Console flows — tools, members, Connections, routines,
                    subscriptions; every change is visible on the next manifest fetch
        │
        ▼
connect: deliveries begin from the subscription cursor; heartbeat starts
```

- **Who calls whom:** the runtime calls Wildstone for everything: capabilities, memory, work, proposals, notifications, heartbeats, and deliveries (a Connection turn, a subscription match, an escalation, a proposal outcome), which it long-polls against its cursor and acknowledges. Wildstone never dials a runtime (ADR-W040 §4). Push delivery is optional and not built (§2). Nothing delivered grants anything; everything asked for is checked.
- **Credential issue and rotation:** a runtime principal is a normal Keystone principal (`name, role, disabled, actingFor`), minted by an Owner at the Console under the consequential-freshness rule, rotated by minting a successor and disabling the predecessor. For a Wildstone-packaged runtime it is a `provided: true` package Secret reaching the container as env, like the chat-adapter token (`packages/chat-telegram/package.yaml:81`); for a user-managed runtime elsewhere it is pasted into that runtime's config. Same credential, same API.
- **Identity binding:** principal → Agent, stored by Wildstone, immutable for the principal's life. The Person on a call comes from the task handle Wildstone issued when it accepted the turn or matched the subscription; there is no runtime-side claim.
- **Offline, queuing, revocation:** section 3.7.

---

## 5. Hermes

Hermes Agent is a Python, MIT-licensed personal agent ([architecture](https://hermes-agent.nousresearch.com/docs/developer-guide/architecture)); it integrates by configuration, without a fork, either externally hosted or as the packaged `agent-hermes` capability. Detail lives in [`docs/design/hermes-capability-package-and-execution-profiles.md`](../design/hermes-capability-package-and-execution-profiles.md). Decisions:

- **One Hermes profile per Wildstone Agent.** Upstream says Hermes is single-tenant and session identifiers are not authorization boundaries; per-Person separation inside the profile is Wildstone's job at the gateway.
- **The Wildstone MCP server is trusted in Hermes, with `readOnlyHint` set correctly on every read capability.** Wildstone's gateway is the enforcement point; Hermes's own approval is reserved for runtime-local actions in its sandbox. Wildstone's `confirm` tier reaches Hermes as a `waiting` tool result, not a Hermes approval.
- **On the platform-only profile, Hermes's messaging adapters, Home Assistant tools, and terminal and file tools are off.** People are reached only through Wildstone Connections; Home Assistant is a Wildstone capability; host administration is only ever a Wildstone executor behind policy, never a Hermes shell.
- **Integrate over HTTP/MCP with pinned image tags**, never Python internals.

## 6. OpenClaw

Every contract item has an OpenClaw counterpart ([architecture](https://docs.openclaw.ai/concepts/architecture)); the abstraction is not Hermes-specific. Decision: an OpenClaw-backed Agent needs its own gateway cell (OpenClaw's gateway token is root for every agent on that gateway), and `tools.deny` must include `gateway`, `cron`, `sessions_spawn` and `sessions_send`. Wildstone hands out one runtime credential per Agent and never the gateway token; no runtime port is ever a Route with `auth: none`.

---

## 7. Structural tests

The reasoning loop (`Runner`, `Phraser`) lives in `keystone/agentruntime/legacy`, with the strict import-ban structural test; `ToolGateway`/`HouseholdGateway` stay in `keystone/butler`. A structural or red-team test may move to the package where its subject lives; it may never be weakened.

---

## 8. Phases

Phases 0–6 are implemented except the items in §2.

---

## 9. Amendments and what is kept

This ADR amends ADR-W033 B6, ADR-W024 A5 and ADR-W026 A10 as listed below and keeps everything else.

- **ADR-W024 A5, "Actions run through the `domestic` tier gateway", its "External agents" bullet (amended).** Retained for an external agent that merely holds a `butler` token: read and propose, no domestic gateway. Amended for an external *runtime bound to a Wildstone Agent*: it acts as that Agent, whose sessions are principal-bound, redacted and bounded by the gateway rather than by the runtime, so the Agent sits behind the domestic gateway on exactly the terms a `native` Agent does. A5's reason ("only its sessions are principal-bound, redacted, and bounded") is preserved by moving that property into the gateway.
- **ADR-W026 A10, "External agents are not surfaces" (amended).** Retained: a runtime never gets a surface, a chat binding or a chat credential, and "somebody else's agent, calling Wildstone" stays distinct from "Wildstone's Butler, reachable from Telegram". Added: a third thing, "Wildstone's Butler whose cognition runs in Hermes", which has the Butler's surface, bindings and Connections because the *Agent* has them, while the runtime holds only the runtime principal. The reasoning in ADR-W026 for keeping `converse` off the `butler` row stands.
- **ADR-W030 B3 (kept).** "Replace Wildstone with an external agent runtime" remains not selected. This ADR is the "separate compatibility decision" B3 anticipated, and it selects one contract with both native and external clients, not replacement.
- **ADR-W017 A2, ADR-W006 A1, CONTRIBUTING ground rule 3 (strengthened).** "There is no private Butler channel" becomes a transport-level property for every runtime, native included, checked by a structural import test.
- **ADR-W006 A3, "tools mirror the API 1:1" (kept and generalised).** Capabilities *are* the API; a runtime's tools are the manifest, nothing more.
- **ADR-W033 B6, per-Agent ordered model/provider chain (amended for external runtimes; accepted).** Unchanged for runtime type `native`, whichever loop drives it: the Agent's own chain applies, with no global fallback, no Agent substitution, and a local-only task failing rather than falling to cloud (code not yet conformant: one assigned model today). For an external runtime the model chain is runtime configuration; Wildstone records the Owner-declared processing class and applies it as a disclosure filter. Stated plainly: Wildstone cannot say by name which model an external runtime called, and only the Owner-declared processing class remains, which is why 3.5 rule 3 withholds `local_only` content from any runtime not declared local-only and undeclared fails closed. An Agent has exactly one runtime.
- **The rest of ADR-W033; DSN §10, §11, §14–§16 (kept).** Apart from B6 above, the runtime contract is expressed in ADR-W033's terms (A0, A4, B1–B5, the C-series memory scopes, D-series secrets, E-series Plans and routines) and changes none of them. The one new principal, the subscription version (3.6), is not a new rule: it is treated exactly like an approved routine version under E2, E6, E6.0, C7 and D3.
- **ADR-W012, ADR-W029 A10–A11, DSN §12 (kept).** Node secrets on nodes; provider keys in the root-only key file or as a package secret for a packaged runtime; household and personal secrets resolved by trusted executor code below inference, by reference.
- **ADR-W030 A5.1 (kept, extended in spirit).** A runtime is a second package-managed-container shape with a second narrow credential; "adapters on non-node machines need a later credential/identity design" is answered for runtimes by the runtime principal.

---

## 10. Package map

| Package / file | What it owns |
|---|---|
| `keystone/agentapi` | Manifest/Invoke/Memory/Work/Proposal/Delivery/Heartbeat types, `Client` interface, opaque `Handle`; structural test forbidding `runtimedb`/`statefile`/`domestic`/`butler` imports |
| `keystone/agentapi/httpclient` | The shared HTTP implementation of `agentapi.Client`; own structural test |
| `keystone/agentapi/conformance` | Conformance suite runnable against the in-process client and any HTTP runtime |
| `keystone/agentgateway` | `Gateway`; `Manifest`/`Invoke` (`invoke.go`), `memory.go`, `propose.go`, `work.go` (work and deliveries), `notify.go` (outbound through the task's own Connection), `disclosure.go` (processing-class filter on manifest, invoke, recall and deliveries), `secrets.go` (executor secret-use context derived from the task); `InProcess(*Gateway) agentapi.Client` |
| `keystone/agentruntime` | Shared ledger vocabulary (`ToolCallRecord`, `Audit`, `Transcript`, step/attempt/evidence types) and the `AgentRuntime` contract (ADR-W041) |
| `keystone/agentruntime/legacy` | `Runner`, `Phraser`, objective, `Slice`; the strict import-ban structural test |
| `keystone/agentruntime/native` | The OpenAI Agents SDK sidecar adapter (ADR-W041) |
| `keystone/proposals` | Plan-filing pipeline |
| `keystone/automation/subscriptions.go` | Deterministic subscription matching |
| `cmd/keystone/reviewed_escalator.go` | Routine escalation as a tool-free run request (an `escalation` delivery); in-process `Phraser` for `native`, author's wording as fallback |
| `runtimedb/migrations/0081`–`0087` | `butler_runtimes` + `runtime_principals`; task handles; task turn context; `agent_deliveries`; `runtime_offline` wait; agent subscriptions; delivery expiry |
| `api/handlers_butler_runtime.go` | `GET`/`PUT .../runtime`, `POST .../runtime/credential`, `POST .../runtime/revoke` |
| `api/handlers_agent.go` | `POST /api/v1/agent/{manifest,invoke,recall,remember,checkpoint,start,wait,complete,fail,propose,notify,heartbeat}`, `GET /api/v1/agent/deliveries` (long-poll, bounded by `maxDeliveryPoll` = 30s), `POST /api/v1/agent/deliveries/{id}/ack` |
| `api/handlers_agent_subscriptions.go` | Applying an Owner-approved subscription Plan |
| `api/handlers_runtimes.go` | `GET /api/v1/runtimes`, runtime inventory across Agents |
| `cli/cmd/agent-bridge` | Sidecar for a packaged runtime: long-polls deliveries, posts runs, heartbeats, and serves a hand-rolled MCP capability server translating to the REST Agent API (ADR-W040 §6) |
| `packages/agent-hermes` | The Hermes capability package |
| `domestic/hostops.go` + `butler/tools_hostops.go` | `host.*` executors behind `-hostops-enabled` (default off) and Owner-class opt-in; `host.upgrade_package` and `host.snapshot_dir` are not built |
| `resource.go` `ContainerSpec` hardening fields; `pkgspec.go` `enum:` shape | Container hardening (protocol minor 18) and closed-choice params for the Hermes package |

## Proposed W047 companion extension: research and workspace capabilities

Pending acceptance of [ADR-W047](ADR-W047-native-agent-research-and-workspaces.md)
and the [v1 interface contract](../design/native-agent-capability-contract-v1.md).
This is capability extension work, not a completed API/runtime implementation.

New individually grantable `web_*`, `workspace_*` and `artifact_export`
tools use the existing Manifest/Invoke envelopes. Keystone owns networking,
storage, current authority, budgets, privacy, publication and effect receipts.
Native sidecars receive no workspace mount, provider credential or local
filesystem/network authority. New tools do not add another runtime API.

Artifact reads extend resource authorization and processing-class disclosure.
Derived artifact sources retain source audience/privacy across tasks. Outbound
queries/URLs and export additionally check full task/session/workspace
provenance at the external-egress boundary, including resume and ledger/cache
reuse; an isolated call-scoped disclosure turn cannot replace that check.
Human preview/download uses current Person authorization. The existing
closed Invoke outcomes, untrusted/redacted/bounded content and structural
no-self-grant rule remain unchanged.
