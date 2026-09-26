# ADR-W039: Canonical Agent API and pluggable agent runtimes

- **Status:** accepted (#333); amends ADR-W033 B6 for external runtimes (§9). Phases 0–3 are implemented and merged; Phases 4–6 are not started (§10).
- **Governed by:** ADR-W033 where they overlap, as amended by §9 of this ADR.
- **Scope:** the contract between Wildstone and whatever performs an Agent's reasoning: the resource boundaries of an Agent API, the minimum behaviour of a compatible runtime, how the native Butler runner and an external runtime (Hermes first, OpenClaw as a check) both sit behind it, and the order in which the current code could move there
- **Depends on / relates to:** #333 (this exploration), #334 (Hermes capability package and execution profiles, the first consumer of this contract), [`ADR-W033`](ADR-W033-the-agent-design.md) and [`docs/agent-system-design.md`](../agent-system-design.md) (the design of record this ADR extends rather than reopens), ADR-W024 A5, ADR-W026 A10, ADR-W030 B3, ADR-W017 A2, ADR-W006 A1/A3, CONTRIBUTING ground rules 3 and 5
- **Implementation:** Phases 0–3 as built are mapped in §10; section 8 splits the remaining work into candidate follow-up issues.

`Agent` and `Butler` mean the same architectural object. Product-facing text may say Butler; technical text may say Agent. `Runtime` means the thing that turns a turn or an event into model calls and capability calls — today the in-process loop (`keystone/agentruntime/legacy.Runner`) or the native Agents SDK runtime (ADR-W041), tomorrow possibly Hermes or OpenClaw.

---

## 0. The decision in one paragraph

Wildstone owns the platform and the Agent contract; a runtime owns cognition. Every runtime — native or external — is a **client of one Agent API**, holding its own runtime credential, discovering capabilities from Wildstone, invoking them through the same `authn → authz → policy/approval → adapter → execute → audit` chain, reading and writing durable memory through Wildstone's scope and audience rules, and receiving events only where the deterministic engine or an Owner-approved subscription escalates. The ceiling does not move: a runtime is at most a `butler`-role principal (read and propose), never approves, never widens an audience, never creates identity. The native runner keeps its policy-enforcing pieces (`Toolset`, `ToolAuthorizer`, `TaskLedger`) on the Wildstone side of the line and becomes just another client of the gateway, first in-process through an interface that mirrors the wire contract, later optionally out-of-process. Changing an Agent's runtime replaces one record and one credential; identity, permissions, Connections, routines and memory stay. This is not "replace Wildstone with Hermes" (ADR-W030 B3 stands); it is the separate compatibility decision B3 said would be needed.

---

## 1. Current-state map

All claims here come from the working tree; `path:line` refers to it.

### 1.1 What an Agent is today

The record is `runtimedb.Butler` (`keystone/runtimedb/butlers.go:99-140`): name, directive ("instructs and authorises nothing", `butlers.go:102-104`), surface `admin|family` (`keystone/runtimedb/butlersessions.go:25-34`), agent class `owner|member` (`butlers.go:44-47`), exactly one assigned model with provider, endpoint and probe readiness (`butlers.go:109-126`), enabled/retired state. `CarriesActionAuthority()` requires readiness `ready` (`butlers.go:157-159`). Around the record hang the things the issue calls the Agent's Wildstone-owned properties:

| Property | Where it lives |
|---|---|
| People admitted, with role and revocation generation | `ButlerMember` `keystone/runtimedb/butleridentity.go:93-114` |
| Tool controls, work-bound grants, per-agent assignments and bundles | `keystone/runtimedb/butlertools.go:56-63,137-156`; `keystone/runtimedb/toolassignments.go:35-44,67-242` |
| Connections (messaging reach) with admission and disclosure | `keystone/runtimedb/connection.go:48-90` |
| Assigned routines; routine versions carry `assigned_butler` | `keystone/runtimedb/butlers.go:788`; `keystone/runtimedb/routineprincipal.go:47-52` |
| Memory restrictions per agent | `keystone/runtimedb/memoryscopes.go:14-31` |
| Runtime settings (rounds, stall, quiet hours) | `keystone/runtimedb/butlersettings.go:45-58` |

Ownership: every Butler is owned by the Console holder and "a Butler cannot create or modify a Butler" (`butlers.go:17-22`). There is no `Agent` type distinct from `Butler`, no `Runtime` type or interface anywhere (grep), and no delegation between Butlers — only member-driven conversation handoff where "tool traffic never crosses" (`keystone/runtimedb/butlerhandoff.go:5-22`).

### 1.2 The execution path of one turn

```text
Console POST /api/v1/butlers/{name}/ask        chat adapter POST /api/v1/chat/messages
  (api.go:638)                                    (api.go:709, VerbConverse)
        │                                                │
        └──────── api.handle(): authn → RBAC → audit → rate limit (api.go:881-938)
                                    │
                    s.Butlers.Serve(ctx, turn)   in-process call (handlers_butler_named.go:142)
                                    │
                 butlerlive.Butlers.Serve (serve.go:97-153)
                   authority := &butlerstore.Authority{DB: bs.DB, Butler: b.Name}   ← DB handle
                   sc.SetAuthorizer(WithConnection(authority, t.Connection.Permits))
                   runner := butler.Runner{Gateway: <Toolset>, Ledger, ...}   (serve.go:130-149)
                   runner.Run(ctx, privacyProvider{p, privacy}, task.ID, text)  (serve.go:76,151)
                                    │
                 butler.Runner.Run (runner.go:375-543)
                   Fence → Authorize(task) → Running
                   manifest := Gateway.Capabilities(readOnly)      (runner.go:422)
                   specs    := Gateway.OfferedSpecs(readOnly)      (runner.go:434)
                   rounds: Provider.Chat → tool calls → toolCall (runner.go:702-806)
                     ledger AppendStep/StartAttempt (runner.go:748-754)
                     Gateway.Execute(operation.With(ctx, Identity{TaskID, Step, PermissionLabel, Audience}), ...)  (runner.go:764)
                     FinishAttempt/SetStepState (runner.go:787-791)
                                    │
                 Toolset = ToolGateway (admin) or HouseholdGateway (family)
                   ToolGateway.Store *statefile.Store, Reader *AgentReader       (gateway.go:110-117)
                   HouseholdSeams closures over DB, domestic.Gateway, memory    (main.go:1610-1660)
                                    │
                 back in the API handler: proposals filed as principal
                   {Name:"butler", Role: RoleButler, ActingFor: member}         (handlers_butler_named.go:175-176)
                   via submitSessionProposalsWithCalls → submitProposalOpts     (same function as POST /plans)
```

The loop owns very little state: `Runner.History []provider.Message` holds user/assistant text only, never tool traffic (`runner.go:233`; `keystone/butler/session.go:118-131`). Steps, attempts, traces, waits and evidence live in the ledger (`TaskLedger` `runner.go:138-180`, direct SQL under the coordinator lease at `keystone/butlerstore/butlerstore.go:92-96,374`). Pending approvals are not loop state: `propose_change` returns a `Plan` and `ResourceYAML` and never writes (`keystone/butler/tools_write.go:151`); the handler files the Plan after the turn. Limits are package constants, not the `ButlerSettings` row: 8 rounds per slice, 64 tool calls, 15 minute observation TTL (`keystone/butlerlive/serve.go:23,28,34`). Authority is re-checked before each slice, model round, tool call and final answer (`runner.go:545-566`).

Everything runs in one binary under systemd (`deploy/systemd/keystone.service:11`); live Butlers are wired only on the coordinator (`keystone/cmd/keystone/main.go:1572-1667`). `packages/assistant-wildstone/package.yaml` installs no process (`:4-8`). Chat transports are the only separate containers: the `chat-adapter` binary dials out with a token that "holds the converse verb and nothing else" (`packages/chat-telegram/package.yaml:19-22,64-81`; `cli/cmd/chat-adapter/keystone.go:61-170`).

### 1.3 Events, automation, routines

Household events are append-only rows with an idempotent `(source, event_id)` key (`keystone/runtimedb/householdevents.go:16-35`), emitted by `RolePublisher` tokens (`keystone/api/api.go:405-407`). There is no pub/sub: the deterministic engine follows a cursor (`keystone/automation/automation.go:390`), is poked on every emit (`main.go:1165`), evaluates pure rules (`automation.go:116`) and records `automation.fired|suppressed` (`automation.go:44-47`). "Nothing here calls a model" (`automation.go:8-11`), pinned by `keystone/automation/structural_test.go:15`. Acting is claim-before-dispatch, at most once (`keystone/automation/act.go:9-24`), through the domestic gateway (`keystone/domestic/domestic.go:148-181`). The only model involvement is escalation, which chooses the *wording* of one declared action param and falls back to the author's text when the model does not answer (`keystone/automation/escalate.go:1-16`); it receives a tool-free `butler.Phraser` and the assigned Butler's provider object directly (`main.go:1729-1797`). Reviewed routines run on their own cursor (`keystone/automation/routine_execution.go:29`) as a `RoutinePrincipal`, "the exact approved Plan, never a Person or Agent grant" (`keystone/runtimedb/routineprincipal.go:10-16`).

### 1.4 Memory

Three durable scopes `personal|household|agent_local` (`keystone/runtimedb/memoryscopes.go:7-11`); records carry kind, state, scope and audience (`keystone/runtimedb/butlermemory.go:68-76`); derivation takes the strictest audience (`butlermemory.go:266`). "The authority resolver does not read this table" (`butlermemory.go:8-13`). Disclosure is bound from trusted ingress: `MemoryDestination` is "never a tool argument" and a zero `MemoryReadContext` denies (`keystone/runtimedb/memorydisclosure.go:3-25`). The model-facing seams are closures built per session over the DB and the destination (`keystone/butlerstore/memory.go:63-70`). The HTTP memory routes that exist are person-facing Console/portal routes (`api.go:509-514,745-750`), not agent-facing.

### 1.5 Authority, policy, approval

RBAC is a closed table: `butler` = read + propose (`keystone/principal/rbac.go:63-64`); `approve` is a separate verb it does not hold; adding a verb "is a code change reviewed against the role table" (`rbac.go:5-6`). The policy engine validates every proposed tree, Butler included (`keystone/policy/policy.go:21-27,116`). Three-layer authority is implemented in `keystone/butlerstore/authority.go:8-15,46-77` with the query shape `ToolAuthorityQuery{Caller, Person, Connection, Resource, AgentClass, Tool, Structural, GrantRequired, TaskID, RoutineName, RoutinePlanID}` (`keystone/butler/authority.go:181-229`). Derived task authority is re-evaluated at every boundary (`butlerstore.go:113,155`). The Connection ceiling is a decorator that "can only ever turn a yes into a no" (`keystone/butlerlive/connection.go:3-14,125`). Confirm-tier actions ask a person through the notices table (`keystone/domestic/domestic.go:46-58`).

### 1.6 Ground rule 3, honestly

CONTRIBUTING ground rule 3 says Console, CLI and Butler consume the same public API (`CONTRIBUTING.md:29-30`); ADR-W017 A2 says "there is no private Butler channel"; ADR-W007 A2 says the Butler holds "a first-class principal — not a special code path". Today the *proposal pipeline* honours this: a Butler proposal is filed through `submitProposalOpts` as a `butler`-role principal, the same function `POST /plans` uses. The *transport* does not: the handler calls `Serve` in-process (`handlers_butler_named.go:142`), the runner's tools reach the DB, Statefile, node gateway and domestic gateway through Go closures (`main.go:1610-1660`), trust travels in `context.Context` (`keystone/operation/context.go:6-17`; `main.go:1633-1641`), and the `butler` principal is synthesised without a token (`handlers_butler_named.go:175`). The ground rule is true of the write path and untrue of everything else. This ADR's job is to make it a transport-level property for every runtime, native included.

### 1.7 Divergence from the documented design

In code and matching the design: two agent classes, tool classes/opt-in/bundles, Connections with admission and disclosure, Tasks with derived authority, routine-as-principal, three durable memory scopes with ephemeral session context, a household secret store separate from node secrets. Not in code: the ordered model/provider chain (one model, explicit no-fallback at `keystone/butlerlive/butlerlive.go:25-30`), group admission ("admits nobody", `connection.go:76`), Connection disclosure and cross-Connection fallback ("not wired here yet", `serve.go:112-119`), first-Owner bootstrap (`api.go:986`), and any `Agent`/`Runtime` type. Nothing in the design of record describes an API consumed identically by the native agent and an external runtime; the closest statements are ADR-W007's "one public API" and ADR-W030 A7.1's single effective-authority resolver.

---

## 2. Gap analysis

### 2.1 Coupling that blocks external runtimes

Each item names a place where the native loop reaches something an external runtime could not. Section 7 says which phase removes which.

| # | Coupling | Evidence |
|---|---|---|
| C1 | Runner constructed inline with a DB-bound `Authority`; no token, no principal on the wire | `serve.go:97-153` |
| C2 | Gateway factory closes over `*runtimedb.DB`, `*statefile.Store`, `domestic.Gateway` | `main.go:1610-1660` |
| C3 | Operator tools read the Statefile and node gateway directly | `gateway.go:110-117`; `keystone/butler/doc.go:21-30` |
| C4 | Trust and identity travel in Go context (`operation.Identity`, secret-use context), not in a credential or a Wildstone-issued handle | `runner.go:764`; `operation/context.go:6-17`; `main.go:1633-1641` |
| C5 | Executors resolve household secrets from context-bound vault calls | `keystone/cmd/keystone/calendar_secret.go:10-38` |
| C6 | Ledger checkpoints are direct SQL under the process's coordinator lease | `butlerstore.go:92-96,374` |
| C7 | Provider clients and API keys are built in-process from the key file | `main.go:422-427`; `keystone/butler/provider/keystore.go:26-30` |
| C8 | `butler` principal synthesised without a token; an RBAC row exists but no minting or binding path for a runtime | `handlers_butler_named.go:175`; `rbac.go:63-64` |
| C9 | The API handler post-processes `ToolCallRecord` internals (ResourceYAML, Plan) to file proposals and transcripts | `handlers_butler_named.go:176-250` |
| C10 | `LocalOnly` privacy is enforced by wrapping the provider object | `keystone/butlerlive/privacy.go:9-25` |
| C11 | Escalation hands a provider object and the DB to a Phraser | `main.go:1771-1797` |
| C12 | Memory seams are closures over DB and destination; no agent-facing memory API with equivalent disclosure filtering | `butlerstore/memory.go:63-70` |

Two further facts shape the design though they are not couplings: there is no subscription concept at all (the engine is the only consumer of the household stream), and the native runner's rounds/budgets are constants rather than a negotiated profile.

### 2.2 Existing reusable boundaries

These already have the shape the contract needs; they stay on the Wildstone side and are reused, not rewritten.

- `butler.Toolset` — `OfferedSpecs`, `Execute`, `Permits`, `Capabilities` (`session.go:68-104`): offered / run one / would it be allowed / the manifest, with read-only enforced at dispatch, not by omission.
- `butler.ToolAuthorizer` + `ToolAuthorityQuery` (`keystone/butler/authority.go:181-230`), decorator-stackable (`butlerlive/connection.go:125`): the three-layer decision.
- `butler.TaskLedger` (`runner.go:138-180`): checkpoint before dispatch and after result, fence, wait, complete with evidence — already DB-free in the `butler` package.
- `provider.Provider` (`keystone/butler/provider/types.go:84-87`): the native runtime's stateless model interface.
- The chat-adapter and integration-adapter contracts (`cli/cmd/chat-adapter/keystone.go`; `keystone/adapter/adapter.go:1-22`; `api.go:505-506`): two worked examples of a process outside Keystone holding one narrow credential.
- The proposal pipeline keyed on a principal (`submitProposalOpts`) and the owner-review primitives (`keystone/runtimedb/ownerreview.go:49,169`).
- The cursor-based engine over the idempotent event log: a durable, replayable event source.
- `MemoryDestination`/`MemoryReadContext`/`BindMemoryRead` (`memorydisclosure.go`): the disclosure rules an agent-facing memory API must apply.
- `automation.Escalator` (`escalate.go:75-83`) and `domestic.Executor` (`domestic.go:134-143`): the seams a runtime's output and a capability's effect already pass through.

### 2.3 Missing abstractions

| # | Missing | Why it is needed |
|---|---|---|
| M1 | A **runtime record** on the Agent (type, endpoint, credential ref, health, negotiated capabilities, execution profile) | Q17: change runtime without recreating the Agent |
| M2 | A **runtime credential** and a binding of that credential to exactly one Agent | Q13, Q15: enforcement and revocation independent of the runtime |
| M3 | A **Wildstone-issued work handle** (task/turn id) that carries Person, Connection and audience, replacing context-borne trust | Q7, Q13: identity binding without the runtime asserting who asked |
| M4 | **Capabilities on the wire**: manifest + invoke, backed by the existing Toolsets | Q6, Q14 |
| M5 | An **agent-facing memory API** applying the existing scope/audience rules | Q10 |
| M6 | **Subscriptions** with per-subscription cursor, ack, replay and TTL, sitting after the deterministic engine | Q11, Q16 |
| M7 | A **work API**: task lifecycle, checkpoints and evidence over the wire; a closed `waiting` reason set including runtime-offline and approval-pending | Q12 |
| M8 | **Proposal outcome delivery** to the runtime (approved/rejected) as an event, not as handler post-processing | C9 |
| M9 | A **declared processing class** on the runtime so the gateway can apply `local_only` disclosure without wrapping a provider object | C10 |

---

## 3. Proposed Agent API

### 3.1 Principles

1. **One contract, all runtimes.** The native runner and a Hermes profile use the same Agent API. Nothing the native runner can do is unavailable to an external runtime, and nothing an external runtime is denied reaches the native runner by a side door. Ground rule 3 becomes a transport property, checked structurally (7.4).
2. **The chain stays in Wildstone.** Every capability call runs `authn (runtime credential) → authz (three-layer resolver) → policy/approval (tier, Plan, confirm) → adapter/executor → audit`. The runtime sees a manifest and a result, never an integration credential, a Statefile handle or a DB.
3. **The ceiling is the ceiling.** A runtime credential carries at most the `butler` role's verbs: it cannot approve, manage, emit, converse, create identity, widen an audience or grant a tool. `TestStructuralCeilingCannotBeWidened` (`keystone/butlerstore/authority_test.go:58`) and the no-reconcile-import test (`keystone/butler/structural_test.go:19`) pin Wildstone-side code that does not move.
4. **Authority is derived at use time, never copied to the runtime.** A runtime names a Wildstone-issued task handle; Wildstone resolves Person, Connection, audience and grants from it at every call (W033 A0, DSN §10.1). A runtime's own claim about who is asking is ignored, as `MemoryDestination` already is (`memorydisclosure.go:5`).
5. **Deterministic first.** The automation engine consumes the household stream. A runtime is invoked only where an approved routine or subscription escalates, and a delivered event is untrusted input that confers nothing (ADR-W024 A4).

### 3.2 Resource families

The families below are conceptual boundaries, not endpoint definitions. For each: what Wildstone stays authoritative for, what the runtime owns, and what existing code backs it.

| Family | Wildstone is authoritative for | Runtime owns | Backed by today |
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
agent: mika                      # unchanged: identity, class, surface, members,
                                  # tools, connections, routines, memory restrictions
runtime:
  type: native | hermes | openclaw | <future>
  endpoint: https://hermes-mika.internal/   # absent for native; a Route host or
                                             # container/<name>:<port> for a packaged runtime
  credentialRef: principal/runtime-mika-3   # a Keystone principal; rotated, revoked here
  health: unregistered | connected | degraded | offline | revoked
  lastHeartbeat: 2026-09-24T09:12:00Z
  negotiated:
    capabilities: [get_household_events, do_household_action, recall, remember, ...]
    delivery: push | pull
    processingClass: local_only | cloud      # declared by the Owner at registration
  executionProfileRef: platform-only         # #334: platform-only | sandboxed | host-admin
```

`runtime` is one field of the Agent. Replacing it replaces nothing else. For `type: native` the endpoint is absent, the credential is still a real principal (minted, not synthesised), and the negotiated set is whatever the Toolset offers. For a native runtime the Agent's ordered model/provider chain (ADR-W033 B6) applies unchanged; code not yet conformant: today it is one assigned model (`butlers.go:110-125`). For an external runtime the model chain is runtime-owned and Wildstone records only the Owner-declared processing class (3.5); this is the ADR-W033 B6 amendment in §9.

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

Steps 3–7 are today's `ToolGateway`/`HouseholdGateway` dispatch (`gateway.go:378-403`; `household.go:648-694`) and today's ledger checkpoints (`runner.go:748-797`), moved from "called by the runner" to "called by the gateway on the runner's behalf". The only genuinely new step is 1–2: a credential and a handle instead of a closure and a context.

### 3.5 Memory boundary — decided

**Wildstone-authoritative:** the three durable scopes (`personal`, `household`, `agent_local`), people and relationships, permissions and grants, routines, home and application state (live applications remain authoritative for live state, DSN §11.8), and any cross-agent context an Owner has authorised. `agent_local` stays Wildstone-owned even though it is per-Agent: an Owner can inspect, export, transfer or promote it at Agent deletion (DSN §11.5), which a file inside a runtime container cannot offer.

**Runtime-local:** reasoning state, `SOUL.md`/`USER.md`/`MEMORY.md`-style profile files, session transcripts and their FTS indexes, embeddings, skills, scratch files, and whatever "learning" the runtime does about how to work. The native runtime's equivalents are `Runner.History` and nothing else.

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

A **subscription** is Agent configuration set by an Owner at the Console, versioned and approved like a routine (DSN §15): match rule, output audience, permitted capability set, budget. A subscription version is an unattended principal treated exactly like an approved routine version: only a human Owner approves its exact version and any edit needs reapproval (ADR-W033 E2, E4); it runs with no active Person and never borrows its proposer's or approver's authority (E6, E6.0); what it may read is bounded by its approved output audience (C7); it may use Household secrets only through a grant to that exact version and never a Personal secret (D1, D3); and any Owner may stop it without a Plan (E6.1). The engine evaluates the match deterministically; on a match it creates a Task whose originating authority is the subscription version, not any Person — the same shape as a routine principal (`routineprincipal.go:10-16`). Delivery to the runtime is then a Work item plus an event envelope. The LLM is therefore invoked only where a routine escalates or an approved subscription matches, and what it may do afterwards is bounded by the subscription's approved capability set and audience. That set can only narrow the Agent's effective authority, never widen it: every invoke still runs the full resolver against the Agent's assignments, controls and grants, and a capability the Agent does not hold is refused however the subscription names it. Per-subscription cursor, ack, replay window and TTL are Wildstone-side state (the engine is already cursor-based; the log is already idempotent). Unacknowledged deliveries are redelivered; expired ones are recorded as `expired`, never silently dropped.

### 3.7 Offline, revocation, runtime change

- **Offline** is a health state on the Agent (3.3), not a failure of anything else. Deterministic steps continue; an escalation falls back to the author's wording as it already does (`escalate.go:12-16`); a routine or subscription step that requires the runtime reports `waiting: runtime_offline`, in the spirit of ADR-W028 ("a capability waiting is not a node failing"). Queued deliveries wait behind the cursor within retention. On reconnect the runtime resumes from its cursor and from ledger-verified steps, not from a replayed transcript (ADR-W030 A4).
- **Immediate revocation** = disable the runtime principal (every call authenticates per request, `api.go:881-938`), drop the Agent's subscriptions, fence in-flight attempts (`FenceAttempts`, `runner.go:170-173`) so late results are refused, and set `health: revoked`. All capability calls fail closed from the next request. No runtime cooperation is required; a revoked Hermes profile keeps reasoning into a wall.
- **Runtime change** replaces the `runtime` field: revoke the old credential, register the new runtime, negotiate, keep everything else. Identity, members, tools, Connections, routines, subscriptions and all three memory scopes are untouched. Runtime-local memory is the runtime's; exporting a Hermes profile into an OpenClaw workspace is between those two projects (OpenClaw documents importing Hermes memory files, [memory](https://docs.openclaw.ai/concepts/memory)), and is not Wildstone's problem.

### 3.8 Transport: not chosen; the shortlist and the properties

This ADR does not choose REST, WebSocket, SSE, webhooks or MCP (issue non-goal). It records what the choice must satisfy and the leading candidate.

Required properties: per-request authentication with a revocable credential; a Wildstone-issued handle on every call; a manifest with machine-readable schemas and a read-only hint; push delivery with "accepted is not completed" semantics and a pull fallback; idempotency keys on delivery and invoke; streaming or polling for long results; health both ways; all traffic inside the platform network (ADR-W019), never a browser-facing model API (`ai-ollama` package header).

Leading candidate: **an MCP server for Capabilities and Memory, plus a signed webhook / run-API pair for Events and Work.** Both reference runtimes are MCP clients over HTTP with per-server tool filtering and headers (Hermes [mcp](https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp); OpenClaw [tools/mcp](https://docs.openclaw.ai/tools/mcp)), both accept signed inbound webhooks that start an isolated run (Hermes [webhooks](https://hermes-agent.nousresearch.com/docs/user-guide/messaging/webhooks); OpenClaw [webhooks](https://docs.openclaw.ai/automation/cron-jobs/webhooks)), and both expose a run or OpenAI-compatible API (Hermes `POST /v1/runs` [api-server](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server); OpenClaw `/v1/chat/completions` [openai-http-api](https://docs.openclaw.ai/gateway/openai-http-api)). The repository already treats MCP as "a transport detail, never the package format" (`docs/02-implementation-scope.md:343`).

Open questions for the transport issue: how MCP carries the task handle (a header, as Hermes `identity_header`, or a per-task server URL); whether `tools/list_changed` suffices for mid-session grant changes given Hermes freezes a session's tool set; how a `confirm`-tier wait is shown to a runtime expecting a synchronous tool result; whether Work checkpoints are MCP tools or a REST family; and how the in-process native client shares handler code without re-implementing MCP framing.

This section is closed by [`ADR-W040`](ADR-W040-agent-api-transport.md): REST + long-poll first (Phases 2–4), MCP as a second transport in Phase 5.

---

## 4. Runtime contract

The minimum a Wildstone-compatible runtime must do. Everything else — reasoning strategy, skills, model choice, sandboxing of its own workspace — is its own business.

A compatible runtime **must**:

1. Authenticate every call with its runtime credential and accept rotation and revocation without restart.
2. Act only as the one Agent its credential is bound to; carry the Wildstone-issued task handle on every capability, memory and work call; never assert a Person, Connection or audience itself.
3. Discover capabilities from the manifest and treat it as the complete tool surface for Wildstone actions; never hold an integration credential or reach a household service directly.
4. Treat every capability result, memory record and delivered event as untrusted data (they arrive enveloped; Hermes and OpenClaw both already wrap external content).
5. Accept `denied`, `waiting(approval_pending | confirm_pending | runtime_offline | budget_exhausted)` and `expired` as ordinary outcomes and stop, rather than retrying a consequential effect blindly (DSN §16).
6. Accept deliveries (push) or poll (pull) and acknowledge each one; tolerate redelivery by idempotency key.
7. Report its run lifecycle for a task: started, ended with outcome, and each capability call it believes it made (Wildstone records the calls it actually served; the two are compared in audit).
8. Send outbound text only via the Notifications family; never operate a household messaging credential.
9. Write nothing into Wildstone memory except through `remember`; keep its own memory local and never treat it as authority.
10. Answer a health probe.
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

- **Who calls whom:** both directions, over two channels. The runtime calls Wildstone for everything with authority (capabilities, memory, work, proposals, notifications, heartbeat). Wildstone calls the runtime only to deliver (a Connection turn, a subscription match, an escalation, a proposal outcome) and to probe health. Nothing delivered grants anything; everything asked for is checked.
- **Pull fallback:** a runtime that cannot accept inbound connections long-polls deliveries against its cursor, as integration adapters long-poll work today (`adapter.go:165,177`).
- **Credential issue and rotation:** a runtime principal is a normal Keystone principal (`name, role, disabled, actingFor`), minted by an Owner at the Console under the consequential-freshness rule, rotated by minting a successor and disabling the predecessor. For a Wildstone-packaged runtime it is a `provided: true` package Secret reaching the container as env, like the chat-adapter token (`packages/chat-telegram/package.yaml:81`); for a user-managed runtime elsewhere it is pasted into that runtime's config. Same credential, same API — #334's "not a separate integration protocol".
- **Identity binding:** principal → Agent, stored by Wildstone, immutable for the principal's life. The Person on a call comes from the task handle Wildstone issued when it accepted the turn or matched the subscription; there is no runtime-side claim.
- **Offline, queuing, revocation:** section 3.7.

---

## 5. Hermes integration spike

Hermes Agent is a Python, MIT-licensed personal agent: one `AIAgent` loop, a long-running gateway with 25+ platform adapters, an OpenAI-compatible API server, inbound webhooks, cron, profile-scoped memory and skills ([architecture](https://hermes-agent.nousresearch.com/docs/developer-guide/architecture)). Everything below is composition of documented configuration; nothing needs a fork.

### 5.1 Contract item → Hermes mechanism

| Contract item | Hermes mechanism | Adapter or upstream? |
|---|---|---|
| Authenticate, act as one Agent | `mcp_servers.wildstone: {url, headers: {Authorization: Bearer ${WILDSTONE_RUNTIME_TOKEN}}, identity_header: {value_from: profile}}`, `${ENV}` from the profile's `.env` ([mcp-config-reference](https://hermes-agent.nousresearch.com/docs/reference/mcp-config-reference)) | Adapter (config) |
| Capability discovery and invoke | MCP client over Streamable HTTP or SSE; tools appear as `mcp_wildstone_<tool>`; `tools: {include}`; `tools/list_changed` honoured; `trust: untrusted` routes non-`readOnlyHint` tools through approval ([mcp](https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp)) | Adapter |
| No integration credentials, no direct household reach | `platform_toolsets` / `agent.disabled_toolsets` strip `terminal`, `file`, `browser`, `code_execution`, `delegation`, `cronjob`, `homeassistant`, `image_gen` ([toolsets-reference](https://hermes-agent.nousresearch.com/docs/reference/toolsets-reference); [configuration](https://hermes-agent.nousresearch.com/docs/user-guide/configuration#global-toolset-disable)) | Adapter, see 5.3 |
| Deliveries (push) | Signed webhook route (`WEBHOOK_SECRET`, HMAC headers, per-route `prompt`/`toolsets`/`deliver`, 1-hour idempotency) or `POST /v1/runs` with `Idempotency-Key` and `X-Hermes-Session-Key` ([webhooks](https://hermes-agent.nousresearch.com/docs/user-guide/messaging/webhooks); [api-server](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server)) | Adapter |
| Deliveries (pull) | Not native; a small sidecar polling Wildstone and posting to `/v1/runs` | Adapter (new, small) |
| Run lifecycle reporting | `hooks.outbound` signed webhooks (`on_session_end`, `post_tool_call`, …) ([hooks](https://hermes-agent.nousresearch.com/docs/user-guide/features/hooks#outbound-webhooks)); `GET /v1/runs/{id}/events` SSE | Adapter |
| Waiting / approval outcomes | `approval.request` run event resolved via `POST /v1/runs/{id}/approval`; `approvals.unattended_mode: deny` for webhook runs ([api-server](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server); [security](https://hermes-agent.nousresearch.com/docs/user-guide/security)) | Adapter; Wildstone's `confirm` wait is a tool result of `waiting`, not a Hermes approval |
| Runtime-local memory | `MEMORY.md`/`USER.md` (bounded, snapshot at session start), `SOUL.md`, `state.db` FTS ([memory](https://hermes-agent.nousresearch.com/docs/user-guide/features/memory)) | Nothing to do |
| Wildstone memory | `recall`/`remember` as MCP tools; optionally a `MemoryProvider` plugin fronting them ([memory-provider-plugin](https://hermes-agent.nousresearch.com/docs/developer-guide/memory-provider-plugin)) | Adapter (optional plugin) |
| Outbound text via Wildstone | `notify` over MCP; Hermes has no general agent-callable `send_message` tool anyway ([tools](https://hermes-agent.nousresearch.com/docs/user-guide/features/tools)) | Adapter |
| Health | `GET /health` on 8642, `GET :8644/health` ([docker](https://hermes-agent.nousresearch.com/docs/user-guide/docker)) | Adapter |
| Isolation unit | One **profile** (`HERMES_HOME`) per Wildstone Agent, with its own `config.yaml`, `.env`, memory, sessions, skills ([profiles](https://hermes-agent.nousresearch.com/docs/user-guide/profiles)) | Adapter |
| Policy belt-and-braces | `pre_tool_call` shell hook with `fail_closed: true` ([hooks](https://hermes-agent.nousresearch.com/docs/user-guide/features/hooks)) | Adapter; defence in depth, never the enforcement |

One conflict in that table has to be resolved in the profile, not left to defaults. With `trust: untrusted`, every non-`readOnlyHint` Wildstone tool goes through Hermes's own approval, and `approvals.unattended_mode: deny` would then refuse every domestic-tier capability on an unattended webhook run before Wildstone ever saw the call. For the platform-only profile the Wildstone MCP server is therefore marked trusted in Hermes — Wildstone's gateway is the enforcement point, and a second gate in the runtime adds nothing but denials — with `readOnlyHint` set correctly on every read capability so the manifest still tells Hermes which tools are reads. Hermes's approval mechanism is reserved for runtime-local actions (the sandboxed profile's own terminal and file tools), where it is the only gate there is.

### 5.2 Why one profile per Agent

Upstream is explicit: "Hermes Agent is a single-tenant personal agent"; "Session identifiers are routing handles, not authorization boundaries"; for capability separation "run separate agent instances with separate allowlists" ([SECURITY.md §2, §2.6](https://github.com/NousResearch/hermes-agent/blob/main/SECURITY.md)). Built-in memory is per profile, not per user ([profiles](https://hermes-agent.nousresearch.com/docs/user-guide/profiles)). A Wildstone Agent already serves several people with per-Person authority resolved by Wildstone, so a profile per Agent is the right unit: the profile shares nothing with another Agent, and per-Person separation inside it is Wildstone's job at the gateway, not Hermes's. Per-user runtime memory inside one profile would need an external provider (Honcho maps gateway user ids to peers, [memory-providers](https://hermes-agent.nousresearch.com/docs/user-guide/features/memory-providers#gateway-identity-mapping)) — a runtime-local choice Wildstone neither requires nor forbids. Whether profiles share one container or get one each is #334's question; either way "never run two gateway containers on one data dir" ([docker](https://hermes-agent.nousresearch.com/docs/user-guide/docker)).

### 5.3 What is lost behind the boundary

- **Hermes's own messaging adapters are switched off.** Telegram, Discord, Slack and the rest stay Wildstone Connections through the chat-adapter packages, where admission, disclosure and the platform token live. Hermes talks to people only through deliveries and `notify`. The loop, skills, memory, cron-as-config and API server remain.
- **File and terminal tools cannot be routed through MCP.** `read_file`/`write_file`/`patch` are built on the terminal backend and no configuration keeps them while routing them elsewhere ([SECURITY.md §2.2](https://github.com/NousResearch/hermes-agent/blob/main/SECURITY.md)). Platform-only removes them; a sandboxed profile runs them inside Hermes's own container (`terminal.backend: docker`, `docker_network: false`, `container_persistent: false`, [configuration](https://hermes-agent.nousresearch.com/docs/user-guide/configuration#docker-backend)), never on a Wildstone node. Host administration (#334 tier 3) is only ever a Wildstone executor behind policy, never a Hermes shell.
- **Dynamic per-run tool grants.** A session's tool set is frozen at start; `delegate_task` cannot widen it; a webhook route's `toolsets:` is the one exception. A mid-session Wildstone grant is visible via `tools/list_changed`, but whether a running session picks it up is not documented; assume "next run".
- **Hermes-native Home Assistant tools (`ha_*`, `HASS_TOKEN`)** are disabled; Home Assistant is a Wildstone capability with the credential in Wildstone.
- **Dangerous-command heuristics** are "skipped entirely on container backends" ([security](https://hermes-agent.nousresearch.com/docs/user-guide/security)) and are not containment; upstream says "the only security boundary against an adversarial LLM is the operating system" ([SECURITY.md](https://github.com/NousResearch/hermes-agent/blob/main/SECURITY.md)).

### 5.4 Adapter versus upstream change

Adapter (configuration, a small sidecar, an optional memory-provider plugin) covers every contract item. Upstream change would be needed only for per-user capability separation inside one profile (not needed here), file tools over MCP (not needed under these profiles), and approval semantics finer than `deny|approve` on unattended runs (worked around by returning `waiting` as a tool result). Hermes ships date-versioned releases every few days and renamed import paths in a September 2026 decomposition ([releases](https://github.com/NousResearch/hermes-agent/releases)), which argues for HTTP/MCP integration and pinned image tags rather than touching Python internals.

To the issue's question — can a capable Hermes agent become a first-class Wildstone Agent without materially changing how Hermes operates? — yes for cognition, skills, runtime memory and model flexibility; no for its roles as messaging gateway and host shell, which are exactly what Wildstone exists to own.

---

## 6. OpenClaw comparison

OpenClaw (TypeScript, MIT, formerly Clawdbot/Moltbot) is a single Gateway daemon that owns every messaging surface and exposes one WebSocket + HTTP control plane on `127.0.0.1:18789` ([architecture](https://docs.openclaw.ai/concepts/architecture)). The check is whether the contract in sections 3–4 maps onto it without Hermes-specific assumptions.

| Contract item | Hermes | OpenClaw |
|---|---|---|
| Capability discovery/invoke | MCP client (stdio, Streamable HTTP, SSE), `headers`, `identity_header`, `tools.include/exclude`, `trust: untrusted` | MCP client (stdio, SSE, streamable-http), `toolFilter.include/exclude`, OAuth via `openclaw mcp login`; MCP tools pass the same tool-policy chain ([tools/mcp](https://docs.openclaw.ai/tools/mcp)) |
| Strip native tools | `platform_toolsets`, `disabled_toolsets` | `tools.allow/deny` globally and under `agents.entries.<id>.tools`; "if policy removes a tool, the model does not receive that tool's schema" ([tool-permissions](https://docs.openclaw.ai/gateway/security/tool-permissions)) |
| Push delivery | signed webhook route on `:8644`, `/v1/runs` | `POST /hooks/agent` (bearer hook token, `agentId`, `sessionKey`, `Idempotency-Key`, "200 = admitted, not completed"); `/hooks/wake` ([webhooks](https://docs.openclaw.ai/automation/cron-jobs/webhooks)) |
| Chat/run API | `/v1/chat/completions`, `/v1/runs` + SSE events | `/v1/chat/completions` with `model: openclaw/<agentId>`, `user` → stable session key, SSE; WS `agent` RPC with `lifecycle`/`tool` streams ([openai-http-api](https://docs.openclaw.ai/gateway/openai-http-api); [agent-loop](https://docs.openclaw.ai/concepts/agent-loop)) |
| Lifecycle reporting | `hooks.outbound` signed webhooks | internal hooks (`session:*`, `message:received`, `agent:bootstrap`) in `<workspace>/hooks`; WS `lifecycle` events ([hooks](https://docs.openclaw.ai/automation/hooks)) |
| Isolation unit | profile = `HERMES_HOME` | `agents.entries.<id>` with own `workspace` and `agentDir` ("never share `agentDir`"); `bindings` route channel → agent ([multi-agent](https://docs.openclaw.ai/concepts/multi-agent)) |
| Runtime-local memory | `MEMORY.md`, `USER.md`, `SOUL.md`, `state.db` | `MEMORY.md`, `USER.md`, `SOUL.md`, `memory/YYYY-MM-DD.md`, `memory-core` SQLite hybrid search; engines Honcho, LanceDB ([memory](https://docs.openclaw.ai/concepts/memory)) |
| Per-user memory in one agent | not supported natively; Honcho | not documented; multi-user is "a usability feature, not a security boundary" ([multi-user](https://docs.openclaw.ai/concepts/multi-user)) |
| Sandbox | terminal backend per profile (docker/ssh/…) | `sandbox.mode off|non-main|all`, `scope session|agent|shared`, Docker/Podman; "not a perfect security boundary" ([sandboxing](https://docs.openclaw.ai/gateway/sandboxing)) |
| Health | `/health`, `/health/detailed` | `/healthz`, `/startupz`, `/readyz` ([docker](https://docs.openclaw.ai/install/docker)) |
| Credential the platform must guard | `API_SERVER_KEY`, `WEBHOOK_SECRET` | `OPENCLAW_GATEWAY_TOKEN` and any `/v1/*` or `/tools/invoke` credential = full operator access, effectively root for the agent set ([openai-http-api](https://docs.openclaw.ai/gateway/openai-http-api); [tools-invoke](https://docs.openclaw.ai/gateway/tools-invoke-http-api)) |
| Trust boundary | one per profile/instance | **one per gateway**; mutually untrusting tenants need separate gateways ("cells") ([multi-tenant-hosting](https://docs.openclaw.ai/gateway/multi-tenant-hosting)) |

Verdict: every contract item has an OpenClaw counterpart; the abstraction is not Hermes-specific. Three differences the contract must tolerate rather than paper over:

1. **Addressing style.** Hermes addresses a profile by URL prefix (`/p/<profile>/…`) or port; OpenClaw by `agentId` in the body or `model` string. The per-runtime delivery adapter owns this; the Agent API does not care.
2. **Trust boundary.** OpenClaw's gateway token is root for every agent on the gateway and agent-to-agent messaging is on by default ([multi-agent](https://docs.openclaw.ai/concepts/multi-agent)). An OpenClaw-backed Agent therefore needs its own gateway "cell", or a gateway whose other agents share its household trust level, and `tools.deny` must include `gateway`, `cron`, `sessions_spawn`, `sessions_send` as the docs recommend for untrusted-content agents. The contract still holds because Wildstone hands out one runtime credential per Agent and never the gateway token.
3. **Approval surface.** OpenClaw's exec approvals go to its Control UI, never chat ([exec-approvals](https://docs.openclaw.ai/tools/exec-approvals)); Hermes surfaces them in chat or the run API. Wildstone's `confirm` tier is neither: a notice to a Person through a Wildstone Connection (`domestic.go:46-58`), seen by the runtime as `waiting`. Approval UX is runtime-specific for the runtime's own sandbox and Wildstone-specific for anything with household authority.

OpenClaw's incident history (CVE-2026-25253 Control UI token leak, CVE-2026-32922 pairing escalation, tens of thousands of exposed gateways, [hunt.io](https://hunt.io/blog/cve-2026-25253-openclaw-ai-agent-exposure), [ARMO](https://www.armosec.io/blog/cve-2026-32922-openclaw-privilege-escalation-cloud-security/)) reinforces contract rule 11: no runtime port is ever a Route with `auth: none`, and the only credential Wildstone issues is the runtime principal.

---

## 7. Native-agent migration

### 7.1 The line

The native runner does not get demoted; it gets split along a line that already exists in the code. What enforces policy stays on the Wildstone side and becomes the gateway; what reasons becomes a client.

```text
                    today                               target
  ┌─────────────────────────────────┐    ┌──────────────────┐   ┌──────────────────────────┐
  │ butlerlive.Serve                │    │ native runtime   │   │ Agent Gateway (Wildstone)│
  │   builds Authority{DB}          │    │ (client)         │   │                          │
  │   builds Runner{Gateway,Ledger, │    │  provider.Provider│   │  runtime principal authn │
  │          Provider}              │ ⇒  │  rounds/history  │──▶│  Toolset (unchanged)     │
  │ Runner.Run                      │    │  agentapi client │   │  ToolAuthorizer ∘ ceiling│
  │   Toolset.Execute (closures)    │    │                  │◀──│  TaskLedger (unchanged)  │
  │   Ledger (direct SQL)           │    │                  │   │  MemorySeams → memory API│
  │ handler files proposals         │    │                  │   │  proposal pipeline       │
  └─────────────────────────────────┘    └──────────────────┘   └──────────────────────────┘
```

`Runner` keeps its loop (rounds, `stillAuthorized`, `verifyObjective`, history) but calls `Capabilities`, `Execute`, ledger checkpoints, `recall`/`remember` and proposal submission through an `agentapi` client interface whose methods and payloads mirror the wire contract byte-for-byte. Its first implementation is **in-process**: it calls the same handler functions the HTTP transport will call, with a minted runtime principal and a real task handle, so authn, authz, audit and redaction run identically for a Go struct and a Hermes process. The loop lives in `keystone/agentruntime/legacy`, and a structural test pins that it reaches Wildstone state only through `agentapi`. Later, optionally, the same client runs out-of-process as a packaged container with its provider key as a package secret; the contract does not change.

### 7.2 What each phase removes

| Coupling | Removed by | How |
|---|---|---|
| C8 principal synthesised | Phase 1 | runtime principal minted per Agent; native included |
| C1 DB-bound Authority inline; C2 gateway factory closures; C3 Statefile/node reads | Phase 2 | `Toolset` construction moves into the gateway; the runner receives a manifest and an invoke, not a `Toolset` |
| C4 context-borne trust | Phase 2 | `operation.Identity` is derived by the gateway from the task handle; the runner never constructs it |
| C9 handler post-processing of `ToolCallRecord` | Phase 3 | `propose_change` files the Plan inside the gateway; the outcome is delivered as an event |
| C12 memory closures | Phase 3 | agent-facing `recall`/`remember` bind `MemoryReadContext` from the handle |
| C6 direct-SQL ledger | Phase 4 | checkpoints are Work calls; the Wildstone-side `Ledger` implementation is unchanged, only its caller moves |
| C11 escalation gets provider + DB | Phase 4 | escalation becomes a tool-free run request delivered to the Agent's runtime; fallback wording unchanged |
| C5 secrets from context | Phase 4 | executors receive the handle and call `SecretUseContext`/`RoutineSecretUseContext` themselves, below inference |
| C7 in-process provider keys | Phase 5 | the native runtime, if out-of-process, receives only its own provider key as a package secret |
| C10 privacy wraps provider | Phase 5 | gateway filters `local_only` by declared processing class; native runtime declares itself from its assigned model's kind (`ModelKind local|cloud`, `butlers.go:110-125`) |

### 7.3 Incremental and reversible

Each phase adds the gateway path beside the closure path and switches the native runner over behind a single wiring point in `main.go`, the way `DerivedAuthorityChecks()` is switched on today (`main.go:1589`). The legacy `ButlerHost` and `butler.Session` paths (`keystone/api/handlers_butler.go:38-64`; `session.go:169+`) are not migrated; they remain until no deployment lacks a Butler.

### 7.4 The pinned tests are preserved

The reasoning loop (`Runner`, `Phraser`) lives in `keystone/agentruntime/legacy`, so the strict "forbids `statefile`/`domestic` imports" guarantee is pinned on `keystone/agentruntime/legacy/structural_test.go`; `ToolGateway`/`HouseholdGateway` (and `butler.Session`, per §7.3) stay in `keystone/butler` and keep those imports (§10.a decision 1).

- `keystone/butler/structural_test.go:19` (no `reconcile` import) and `keystone/butler/propose_test.go:8` (`propose_change` never applies): the `butler` package loses imports, gains none; `propose_change` still returns a Plan and `ResourceYAML`.
- `keystone/automation/structural_test.go:15` (no apply, no model): the subscription evaluator is pure code; escalation still goes through the `Escalator` seam, now to a run request instead of a provider object.
- `keystone/butlerstore/authority_test.go:58,385`, `derived_authority_test.go:240,373`, `connection_boundary_test.go:32,88,131`: resolver, derived authority and Connection ceiling do not move; they are the gateway's steps 2–3. Their `ToolAuthorityQuery` inputs come from the handle instead of closures — a wiring change, not a rule change.
- `keystone/butler/redteam_test.go`, `injection_test.go`, `hardening_test.go`, `household_test.go`, `secret_context_test.go`: exercise `Toolset` implementations and `finishOutcome`, which stay put. `secret_context_test.go:11` changes fixture shape (a handle instead of a context value); its assertion — secrets resolve only below inference under a bound context — does not.
- `keystone/butlerlive/connection_test.go`, `connection_person_test.go`, `privacy_test.go`, `memory_disclosure_test.go`: `WithConnection` stays as the gateway's ceiling decorator; `privacy_test.go:32` is rewritten only in Phase 5 when the provider wrapper is replaced by the gateway-side filter, and must assert the same refusal.

A red-team or structural test may move to the package where its subject now lives; it may not be weakened, and no phase lands while one is skipped.

---

## 8. Migration plan

Dependency-ordered. Each phase names what stays unchanged, what is new, and the evidence that it landed. No phase is authorised by this ADR.

**Phase 0 — Name the contract in Go.** *Unchanged:* all behaviour. *New:* an `agentapi` package with the resource-family types (manifest, invoke, memory, delivery, checkpoint, proposal, notify, health) and a client interface. *Evidence:* it compiles with no dependency on `runtimedb`, `statefile`, `domestic` or `provider`, asserted by a structural test.

**Phase 1 — Runtime record and runtime principal.** *Unchanged:* Butler record semantics, RBAC verbs, all flows. *New:* `runtime` fields and `health` on the Agent; a Console flow minting a runtime principal bound to one Agent under the consequential-freshness rule, with rotate and revoke; the native runtime gets a minted principal at Butler creation. *Evidence:* a principal for Agent A cannot act for Agent B; `disabled` refuses the next request; the audit row carries the runtime principal, not a synthesised name. Removes C8.

**Phase 2 — Capabilities family, in-process native client.** *Unchanged:* `ToolGateway`, `HouseholdGateway`, `ToolAuthorizer`, `WithConnection`, redaction. *New:* gateway handlers for manifest and invoke that build the `Toolset` and `operation.Identity` from a task handle; the native `Runner` calls them through the in-process client. *Evidence:* every existing `butler`, `butlerstore`, `butlerlive` test passes; the same handler driven over HTTP with a runtime token returns a byte-identical manifest and outcome; the `keystone/butler` structural test also forbids `runtimedb`/`statefile`/`domestic` imports. Removes C1–C4. As built, `Runner`/`Phraser` live in `keystone/agentruntime/legacy` and carry the strict import ban there; `keystone/butler` keeps `ToolGateway`/`HouseholdGateway` and their `statefile`/`domestic` imports, per §7.4 and §10.

**Phase 3 — Memory and proposals families.** *Unchanged:* memory store, audience rules, `submitProposalOpts`, owner review. *New:* agent-facing `recall`/`remember` binding `MemoryReadContext` from the handle; `propose_change` files the Plan inside the gateway; outcome delivered as an event. *Evidence:* `memory_disclosure_test.go` cases pass via the API path; `remember` cannot widen an audience; the handler no longer reads `ResourceYAML`. Removes C9, C12.

**Phase 4 — Work and Events families.** *Unchanged:* `Ledger` implementation, engine rules, routine execution, escalation fallback. *New:* task lifecycle and checkpoints over the wire; closed `waiting` reasons including `runtime_offline`; Owner-approved subscriptions with cursor/ack/TTL; escalation as a tool-free run request; executors resolve secrets from the handle. *Evidence:* `structural_test.go:15` still passes; a subscription match creates a Task bounded to the subscription version; an offline runtime yields `waiting`, not `failed`, and resumes from its cursor. Removes C5, C6, C11.

**Phase 5 — Transport choice; optional out-of-process native runtime.** *Unchanged:* everything Wildstone-side. *New:* the wire transport from the 3.8 shortlist; the native runtime as an optional package with its provider key as a package secret; gateway-side `local_only` filtering by declared processing class. *Evidence:* one conformance suite passes against the in-process client and the packaged runtime; `privacy_test.go` assertions hold in their new home. Removes C7, C10.

**Phase 6 — Hermes capability package (#334).** *Unchanged:* the Agent API. *New:* a package rendering the Hermes container, one profile per Agent, MCP and webhook config, execution profiles. *Evidence:* a Hermes-backed and a native-backed Agent answer the same household turn with the same audit shape; revoking the runtime principal stops Hermes mid-task with a fenced attempt.

### Candidate follow-up issues

1. `agentapi` types and in-process client interface (Phase 0).
2. Runtime record, health state and runtime principal minting/rotation/revocation (Phase 1).
3. Capabilities family: manifest and invoke handlers; native runner through the in-process client; structural import test (Phase 2).
4. Agent-facing memory family with disclosure binding; `remember` promotion rules (Phase 3).
5. Proposals family: in-gateway Plan filing and outcome delivery (Phase 3).
6. Work family: task lifecycle and checkpoints over the wire; closed waiting reasons (Phase 4).
7. Events family: Owner-approved subscriptions, per-subscription cursor, ack, TTL; escalation as run request (Phase 4).
8. Transport decision ADR (the 3.8 shortlist and open questions) — an ADR, not code.
9. Conformance suite runnable against any runtime (Phase 5 prerequisite).
10. Optional out-of-process native runtime package and gateway-side processing-class filter (Phase 5).
11. #334 Hermes capability package and execution profiles (Phase 6).
12. RBAC decision: whether capability invoke is a new closed verb on the `butler` row or lives entirely inside the tool resolver (needed by 3).

---

## 9. Amendments and what is kept

This ADR amends ADR-W033 only where listed below and keeps everything else.

- **ADR-W024 A5, "Actions run through the `domestic` tier gateway", its "External agents" bullet (amended).** Retained for an external agent that merely holds a `butler` token: read and propose, no domestic gateway. Amended for an external *runtime bound to a Wildstone Agent*: it acts as that Agent, whose sessions are principal-bound, redacted and bounded by the gateway rather than by the runtime, so the Agent sits behind the domestic gateway on exactly the native runtime's terms. A5's reason ("only its sessions are principal-bound, redacted, and bounded") is preserved by moving that property into the gateway.
- **ADR-W026 A10, "External agents are not surfaces" (amended).** Retained: a runtime never gets a surface, a chat binding or a chat credential, and "somebody else's agent, calling Wildstone" stays distinct from "Wildstone's Butler, reachable from Telegram". Added: a third thing, "Wildstone's Butler whose cognition runs in Hermes", which has the Butler's surface, bindings and Connections because the *Agent* has them, while the runtime holds only the runtime principal. The reasoning at ADR-W026 lines 158-166 for keeping `converse` off the `butler` row stands.
- **ADR-W030 B3 (kept).** "Replace Wildstone with an external agent runtime" remains not selected. This ADR is the "separate compatibility decision" B3 anticipated, and it selects one contract with both native and external clients, not replacement.
- **ADR-W017 A2, ADR-W006 A1, CONTRIBUTING ground rule 3 (strengthened).** "There is no private Butler channel" becomes a transport-level property for every runtime, native included, checked by a structural import test. The in-process closure path of section 1.6 is recorded as the deviation to close, not as compliant.
- **ADR-W006 A3, "tools mirror the API 1:1" (kept and generalised).** Capabilities *are* the API; a runtime's tools are the manifest, nothing more.
- **ADR-W033 B6, per-Agent ordered model/provider chain (amended for external runtimes; accepted).** Unchanged for the runtimes Wildstone runs itself (`legacy` and `native`): the Agent's own chain applies, with no global fallback, no Agent substitution, and a local-only task failing rather than falling to cloud (code today still has one assigned model, 1.7). For an external runtime the model chain is runtime configuration; Wildstone records the Owner-declared processing class and applies it as a disclosure filter. Stated plainly: Wildstone cannot say by name which model an external runtime called, and only the Owner-declared processing class remains, which is why 3.5 rule 3 withholds `local_only` content from any runtime not declared local-only and undeclared fails closed. An Agent has exactly one runtime.
- **The rest of ADR-W033; DSN §10, §11, §14–§16 (kept).** Apart from B6 above, the runtime contract is expressed in ADR-W033's terms (A0, A4, B1–B5, the C-series memory scopes, D-series secrets, E-series Plans and routines) and changes none of them. The one new principal, the subscription version (3.6), is not a new rule: it is treated exactly like an approved routine version under E2, E6, E6.0, C7 and D3.
- **ADR-W012, ADR-W029 A10–A11, DSN §12 (kept).** Node secrets on nodes; provider keys in the root-only key file or as a package secret for a packaged runtime; household and personal secrets resolved by trusted executor code below inference, by reference.
- **ADR-W030 A5.1 (kept, extended in spirit).** A runtime is a second package-managed-container shape with a second narrow credential; "adapters on non-node machines need a later credential/identity design" is answered for runtimes by the runtime principal.

---

## 10. Implementation decisions and as-built map (2026-09-24)

### 10.a Decisions taken during implementation

1. Split direction: the reasoning loop (`Runner`, `Phraser`) moved out to a new package, now `keystone/agentruntime/legacy`, rather than the two tool gateways moving out of `keystone/butler` (amends §7.1, §7.4 and §8 Phase 2 above).
2. Invoke authorization: reused the existing `read`/`propose` RBAC verbs; no new closed verb was added (closes candidate follow-up issue 12).
3. Runtime storage: a side table, `butler_runtimes` (migration `0081_agent_runtimes.sql`), keyed by `butlers.seq`, plus an immutable `runtime_principals` binding table — not columns on `butlers`.
4. First external target: an externally-hosted Hermes (Phases 0–3 plus the transport ADR) precedes the Wildstone-packaged Hermes container, which needs H1/H2/H3 first.
5. Transport: REST + long-poll first, decided in [`ADR-W040`](ADR-W040-agent-api-transport.md); MCP is reserved as a second transport for Phase 5.
6. Scope: the plan (§8 follow-up 10) was to stop and re-scope after Phases 0–2; in practice work continued through Phases 2b, 2c and 3 and cherry-picked Phase 6 items H1, H2 and H7, all on one feature branch, `claude/issues-333-334-orchestration-dix2u1`.
7. GitHub issues #335–#356 were created across all phases and the H1–H10 sub-issues, beyond the first batch (1–3, 12, H1, H2) §8 originally proposed.
8. Session/audit principal: the literal `"butler"` principal name was deliberately replaced by the runtime principal (`runtime-<butler>-<n>`) in Phase 3, updating `handlers_butler_test.go`'s pinned expectation as §7.4 and §9's ADR-W017 A2 amendment anticipated.

### 10.b As-built map

| Package / file | Phase | What it owns |
|---|---|---|
| `keystone/agentapi` | 0 | Manifest/Invoke/Memory/Work/Proposal/Delivery/Heartbeat types, `Client` interface, opaque `Handle`; structural test forbidding `runtimedb`/`statefile`/`domestic`/`butler` imports |
| `keystone/agentapi/httpclient` | 2c | HTTP implementation of `agentapi.Client`; own structural test |
| `keystone/agentgateway` | 2a–3 | `Gateway{DB,Ledger,Gateways,Log}`; `Manifest`/`Invoke` (`invoke.go`), `memory.go`, `propose.go`, `work.go`; `InProcess(*Gateway) agentapi.Client`; `WithPrincipal` |
| `keystone/agentruntime` | 2b | Shared leaf vocabulary: `ToolCallRecord`, `Audit`, `Transcript`, ledger step/attempt/evidence types |
| `keystone/agentruntime/legacy` | 2b | `Runner`, `Phraser`, objective, and `Slice` (the Runner's boundaries for a loop that runs elsewhere); the strict import-ban structural test (§7.4) |
| `keystone/agentruntime/native` | — | The OpenAI Agents SDK sidecar adapter (ADR-W041), not this ADR's loop |
| `keystone/proposals` | 3 | Plan-filing pipeline moved out of `keystone/api`; `Deps{DB,State,Policy,Log,Event}` |
| `runtimedb/migrations/0081_agent_runtimes.sql` | 1a | `butler_runtimes` + `runtime_principals` tables, native backfill |
| `runtimedb/migrations/0082_task_handles.sql` | 2a | `butler_tasks.handle_hash` |
| `runtimedb/migrations/0083_task_turn_context.sql` | 2b | `butler_tasks.as_owner`, `butler_tasks.system_extra` |
| `runtimedb/migrations/0084_agent_deliveries.sql` | 3 | `agent_deliveries` table |
| `api/handlers_butler_runtime.go` | 1a/1b | `GET`/`PUT .../runtime`, `POST .../runtime/credential`, `POST .../runtime/revoke` |
| `api/handlers_agent.go` | 2c/3 | `POST /api/v1/agent/{manifest,invoke,recall,remember,checkpoint,start,wait,complete,fail,propose,notify,heartbeat}` |
| `butlerlive.Butlers.UseAgentGateway` + `butlerlive/closure_client.go` | 2b | Wiring switch (`main.go` sets it true); the closure path kept as an `agentapi.Client` mirroring `agentgateway/invoke.go` (avoids an `agentgateway`→`butlerlive` import cycle) |
| `domestic/hostops.go` + `butler/tools_hostops.go` | 6/H7 | Seven `host.*` executors behind `-hostops-enabled` (default off) and Owner-class opt-in |
| `resource.go` `ContainerSpec` hardening fields + `resource/hostaccess.go` `HardeningProtocolMinor = 18` | 6/H1 | `ReadOnlyRootfs`, `CapDrop`, `NoNewPrivileges`, `Resources{Memory,Cpus,PidsLimit}`, `ShmSize`, all `omitempty` |
| `pkgspec.go` `enum:` shape | 6/H2 | Closed-choice param validation (`validate: enum:a\|b\|c`) |

"Native" means ADR-W041's OpenAI Agents SDK runtime (`-agent-runtime native`, a sidecar); Keystone's own loop is `-agent-runtime legacy`. `legacy.Slice` (the Runner's ledger boundaries for a loop driven elsewhere) runs over `agentapi.Client`, so an SDK-driven Butler checkpoints, spends budget and completes through the Agent API exactly as an external runtime does. `agentgateway.Invoke` holds a reservation lock across its budget check and step append, because the SDK (and any external runtime) can issue tool calls concurrently.

### 10.c Couplings closed vs. remaining (§2.1)

Removed as of Phase 3: **C8** (Phase 1, minted runtime principal), **C1–C4** (Phase 2, `agentgateway` builds `Toolset`/`operation.Identity` from a handle), **C9** and **C12** (Phase 3, in-gateway Plan filing and handle-bound `recall`/`remember`).

Not yet removed: **C5** (secrets from context), **C6** (direct-SQL ledger checkpoints) and **C11** (escalation holding a provider object and the DB) — all Phase 4, unstarted. **C7** (in-process provider keys) and **C10** (privacy wraps the provider object) — Phase 5, unstarted.

### 10.d Known deviations from the ADR text

- `Client.Checkpoint` returns a `CheckpointReceipt{Seq,Attempt,Epoch}`, not a bare acknowledgement.
- Two closed wait-reason sets exist rather than one: `WaitReason` (invoke-level: `approval_pending|confirm_pending|runtime_offline|budget_exhausted`) and `WorkWaitReason` (task-level, the ledger's own kebab-case values).
- `RuntimeHealth` values are `unregistered|connected|degraded|offline|revoked` — there is no `healthy` state.
- `Invoke` folds an authorization refusal into `Outcome: denied` with HTTP 200 (an ordinary outcome the runtime should handle); every other Agent API method (manifest, remember, checkpoint, start/wait/complete/fail) returns HTTP 403 on an auth failure, since those are transport-level refusals rather than tool-level ones.
- H7's `host.*` family left out `host.upgrade_package` and `host.snapshot_dir`: producing a Plan for either needs a Person→principal identity and a resource kind that do not exist today, and adding either was judged out of that issue's scope.

---

## Answers to the exploration questions

1. **What constitutes a Wildstone Agent today?** A `runtimedb.Butler` row (`butlers.go:99-140`): name, directive, surface, class, one assigned model, enabled state — plus members with revocation generation, tool controls and grants, Connections, routines, memory restrictions and settings hanging off it. Every Butler is owned by the Console holder and cannot create another (`butlers.go:17-22`). There is no `Agent` or `Runtime` type.
2. **Where does the reasoning loop live?** In `butler.Runner.Run` (`runner.go:375-543`), constructed inline by `butlerlive.Butlers.Serve` (`serve.go:130-151`) inside the single Keystone process on the coordinator, driven by `provider.Provider.Chat` and a `Toolset`.
3. **What state does it own?** Only `Runner.History` (user/assistant text, `runner.go:233`). Steps, attempts, traces, waits, evidence and budgets are in the ledger; proposals become Plans after the turn.
4. **Which responsibilities are Wildstone-specific?** Identity and Person binding, the three-layer resolver and Connection ceiling, Plans and human approval, the domestic gateway and executors, secrets by reference, the event log and deterministic engine, routines as principals, the durable memory scopes with audiences, Connections and outbound delivery, audit.
5. **Which are generic runtime functionality?** Model rounds and provider clients, prompt assembly, tool-call orchestration, conversation history, runtime-local memory and skills, retries and stall handling, sandboxing of the runtime's own workspace.
6. **Minimum canonical Agent API?** The nine families of 3.2, each backed by code that already exists behind closures.
7. **Can the native agent consume it without privileged shortcuts?** Yes. `Toolset`, `ToolAuthorizer` and `TaskLedger` stay on the Wildstone side as the gateway; the runner keeps only its loop and calls the gateway through an in-process client that mirrors the wire contract and runs the same authn/authz/audit code, pinned by a structural import test (7.1, 7.4).
8. **Can Hermes?** Yes, by configuration: MCP client with headers and `identity_header`, signed webhooks or `/v1/runs` for deliveries, `hooks.outbound` for lifecycle, `platform_toolsets` to strip native tools, one profile per Agent (5.1). Lost: its messaging adapters, file/terminal tools outside its own sandbox, mid-session grants (5.3). No fork, no upstream change (5.4).
9. **Can OpenClaw?** Yes: MCP client with `toolFilter`, `/hooks/agent` with idempotency, `/v1/chat/completions` per `agentId`, `agents.entries` + `bindings` as the isolation unit (6). The contract must tolerate `agentId`-in-body addressing and one trust boundary per gateway, which means a cell per household trust level and a strict `tools.deny`.
10. **What memory is runtime-local vs Wildstone-authoritative?** Wildstone: personal, household and agent_local scopes, people, permissions, routines, home state, authorised cross-agent context. Runtime: reasoning state, profile files, transcripts, embeddings, skills, scratch. Promotion only through `remember` under audience rules; runtime-local memory never confers authority; `local_only` records are withheld from runtimes not declared local-only (3.5).
11. **How are events delivered to external agents?** Never raw. The engine consumes the stream; a runtime receives a bounded Task when a routine escalates or an Owner-approved subscription matches, pushed to its signed ingress or pulled against a per-subscription cursor, with ack, redelivery and TTL (3.6).
12. **How are long-running jobs represented?** As Wildstone Tasks with derived authority, budgets, checkpoints before dispatch and after result, a closed set of `waiting` reasons, and evidence-based completion — the existing ledger, with the runtime calling checkpoints rather than owning the SQL (Phase 4).
13. **How are permissions and approvals enforced independently of runtime?** Every call authenticates a runtime principal and names a Wildstone-issued handle from which Person, Connection, audience and grants are resolved at use time; resolver, ceiling, tier check, Plan approval and confirm notices all run in the gateway (3.4). The runtime's own approvals govern only its own sandbox.
14. **How does a runtime discover capabilities?** By fetching the manifest — today's `Toolset.Capabilities/OfferedSpecs` narrowed by effective authority, with schemas and a read-only hint. Grant changes appear on the next fetch (or `tools/list_changed`), and dispatch re-checks regardless of what was offered.
15. **How does Wildstone revoke a runtime immediately?** Disable the runtime principal, drop the Agent's subscriptions, fence in-flight attempts, mark `health: revoked`. No cooperation from the runtime is needed (3.7).
16. **What happens when a runtime is offline?** `health: offline`; deterministic steps continue; escalations use the author's fallback wording; steps needing the runtime report `waiting: runtime_offline`; deliveries queue behind the cursor within retention and resume from ledger-verified state (3.7).
17. **Can a user's runtime be changed without recreating the Agent?** Yes: the `runtime` field is replaced, the old credential revoked, a new one minted; identity, members, tools, Connections, routines, subscriptions and all Wildstone memory stay (3.3, 3.7). Runtime-local memory does not follow.
18. **What existing code can remain unchanged?** `ToolGateway`/`HouseholdGateway` and their handlers, `ToolAuthorizer` and `butlerstore.Authority`, `WithConnection`, `Ledger` and the task/step/trace tables, the memory store and disclosure rules, the policy engine, the proposal pipeline and owner review, the automation engine and routine execution, the domestic gateway and executors, the chat adapters and Connection model, the provider clients, and every test named in 7.4 — moved in their callers, not in their rules.
