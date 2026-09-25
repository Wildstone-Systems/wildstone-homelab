# ADR-W040: Agent API transport — REST and long-poll first, MCP second

- **Status:** accepted (#342)
- **Scope:** the wire transport for the `agentapi` contract (ADR-W039 §3–§4): how a runtime authenticates, carries a task handle, fetches a manifest, invokes a capability, moves memory, checkpoints work and receives deliveries, over HTTP. It does not reopen the resource families, runtime record, memory boundary or migration plan; those are ADR-W039's and stand.
- **Depends on / relates to:** [`ADR-W039`](ADR-W039-canonical-agent-api-and-pluggable-runtimes.md) §3.2, §3.4, §3.6–§3.8, §4, §4.1 (the contract this ADR gives a wire form); [`docs/design/hermes-capability-package-and-execution-profiles.md`](../design/hermes-capability-package-and-execution-profiles.md) §2.1, §3.3, §6 (the first external consumer); `keystone/agentapi` (`types.go`, `client.go`), unchanged; ADR-W039 Phase 0–2a, already implemented and not redone here.
- **Implementation:** the candidate follow-up issues in §6 and §8; this ADR authorises none of them directly.

`Agent` and `Butler` mean the same architectural object. `Runtime` means whatever turns a turn or a delivery into model calls and capability calls: the native `butler.Runner` today, an external process such as Hermes tomorrow.

---

## 1. Context

ADR-W039 §3.8 named the transport question and refused to answer it until the resource families and runtime contract were settled. It recorded what any answer must satisfy and a leading candidate, without choosing:

> Required properties: per-request authentication with a revocable credential; a Wildstone-issued handle on every call; a manifest with machine-readable schemas and a read-only hint; push delivery with "accepted is not completed" semantics and a pull fallback; idempotency keys on delivery and invoke; streaming or polling for long results; health both ways; all traffic inside the platform network (ADR-W019), never a browser-facing model API (`ai-ollama` package header).
>
> Leading candidate: an MCP server for Capabilities and Memory, plus a signed webhook / run-API pair for Events and Work. […]
>
> Open questions for the transport issue: how MCP carries the task handle (a header, as Hermes `identity_header`, or a per-task server URL); whether `tools/list_changed` suffices for mid-session grant changes given Hermes freezes a session's tool set; how a `confirm`-tier wait is shown to a runtime expecting a synchronous tool result; whether Work checkpoints are MCP tools or a REST family; and how the in-process native client shares handler code without re-implementing MCP framing.

Since §3.8, three things landed that this ADR treats as given, not reopened. Phase 0 (`be6e580`) fixed the `agentapi` Go types — `Manifest`, `InvokeRequest`/`InvokeResult`, `RecallRequest`/`RememberRequest`, `Checkpoint`/`CheckpointReceipt`, `WorkStart`/`WorkWait`/`WorkComplete`/`WorkFail`, `Delivery`, `Heartbeat`, `ProposeRequest`, `NotifyRequest`, and a `Client` interface — as JSON-tagged, closed-enum, wire-ready shapes. Phase 1a/1b (#336) gave every Agent a `butler_runtimes` row and a runtime principal `runtime-<name>-<n>` with a `wst_`-prefixed bearer secret, plus runtime read/rotate/revoke routes. Phase 2a (#337) built `keystone/agentgateway.Gateway`, an in-process `agentapi.Client` deriving `operation.Identity` from a Wildstone-issued task handle (`<taskID>.<secret>`), not a context closure. What remains, and what this ADR settles, is the one thing Phase 0–2a deliberately left a function call: how a process *outside* Keystone reaches those same methods over a network — ADR-W039 §8's "Phase 5 — Transport choice".

The owner's decision, recorded here: **plain REST plus long-poll first, over the existing Keystone HTTP API; MCP as a second transport later, behind the same handlers.** Two subsidiary calls follow and are also closed here: invoking a capability reuses the RBAC verbs the `butler` role already has (`read`, `propose`), no new one; and both packaged and externally-hosted runtimes start from pull delivery, with push added later as a negotiated option, never the baseline.

---

## 2. Decision

Wildstone exposes the `agentapi` contract as an ordinary part of its existing HTTP API, under `/api/v1/agent/...`, alongside `/api/v1/butlers/...`, `/api/v1/chat/...` and `/api/v1/integration-work/...`. A runtime is an HTTP client of Keystone, exactly as a chat adapter or an integration adapter is today — not a new server Keystone must learn to speak.

**Authentication** is the runtime principal's bearer token, minted by `POST /api/v1/butlers/{name}/runtime/credential` (Phase 1b), checked like every other Keystone credential: `Authorization: Bearer wst_<64 hex>`, resolved by `Principals.Authenticate`, refused if disabled or revoked, on every request. No session, no credential that outlives a request — revocation lands on the runtime's very next call, the property ADR-W039 §3.7 asks for, which a stateful transport would make harder, not easier.

**Bodies are the `agentapi` Go types, 1:1, with no second schema.** `Manifest`, `InvokeRequest`, `InvokeResult`, `RecallRequest`, `MemoryHit`, `RememberRequest`, `Checkpoint`, `CheckpointReceipt`, `WorkStart`, `WorkWait`, `WorkComplete`, `WorkFail`, `Delivery`, `Heartbeat`, `ProposeRequest`, `ProposalRef`, `NotifyRequest` marshal via `encoding/json` using their existing tags. The `POST /api/v1/agent/invoke` handler decodes an `agentapi.InvokeRequest` directly and hands it to the same `agentgateway.Gateway.Invoke` the in-process client calls — no wire-specific type to translate. This is ADR-W006 A3's discipline ("tools mirror the API 1:1") one layer out: `keystone/agentapi` is the one place a family's shape is decided.

**MCP is a later, second transport, not an alternative design.** When it arrives (§6), an MCP server sits in front of the same `/api/v1/agent/...` handlers — `tools/list` becomes `Manifest`, `tools/call` becomes `Invoke` — with no authority the REST transport does not already grant. A runtime speaking MCP and one speaking REST directly are indistinguishable to the gateway.

```text
                    REST + long-poll (this ADR, first)
runtime ──Bearer──▶ POST /api/v1/agent/{manifest,invoke,recall,remember,
                          checkpoint,start,wait,complete,fail,propose,notify}
                    GET  /api/v1/agent/deliveries?wait=<seconds>
                    POST /api/v1/agent/deliveries/{id}/ack
                    POST /api/v1/agent/heartbeat
                          │
                          ▼
                    keystone/agentgateway.Gateway  (Phase 2a, unchanged)
                          │
                          ▼
                    Toolset · ToolAuthorizer · Ledger · MemorySeams · policy

                    MCP (later, second transport, §6)
runtime ──Bearer + handle-as-argument──▶ Wildstone MCP server
                          │  tools/list  → Manifest
                          │  tools/call  → Invoke
                          ▼
                    the same /api/v1/agent/... handlers above
                    (deliveries, checkpoints stay REST-only, §6)
```

---

## 3. Endpoint map (conceptual, not exhaustive)

All routes live under `/api/v1/agent/`, authenticated as every Keystone route is (`handle()`: authn → RBAC → audit → rate limit), with the runtime principal resolved by `RuntimePrincipalButler(id.Name)` (Phase 1b) and the task/delivery's own Butler checked against it first.

| Endpoint (conceptual) | Family | Verb | Consequential? | Handle authorises | Credential authorises |
|---|---|---|---|---|---|
| `POST /agent/manifest` | Capabilities | `read` | No | Which task narrows the tool list | Which Agent's grants are consulted |
| `POST /agent/invoke` | Capabilities | `read` or `propose`, by tool tier, never the request | Tool-dependent | Person, Connection, audience, budget | Which Agent the resolver/ceiling evaluate |
| `POST /agent/recall` | Memory | `read` | No | Reader/destination; processing-class filter | Per-Agent memory restrictions |
| `POST /agent/remember` | Memory | `propose` (gated like `propose_change`) | No | Scope `personal`/`agent_local`, never `household`; provenance | That this Agent may `remember` |
| `POST /agent/checkpoint`, `/start`, `/wait`, `/complete`, `/fail` | Work | `read` | No | Which task's ledger; epoch fencing; evidence stamped with the task's own label/audience | That this credential is that task's own runtime |
| `POST /agent/propose` | Proposals | `propose` | Yes (files a Plan) | `ProposableKinds`; attribution to the handle's Person | That this Agent may propose this kind |
| `POST /agent/notify` | Notifications | `propose` | Yes (sends outward) | Connection, admission, disclosure | That this Agent has a Connection |
| `GET /agent/deliveries?wait=<n>`, `POST .../{id}/ack` | Events & subscriptions | `read` | No | Which subscriptions feed the queue; which delivery advances which cursor | Which Agent's queue |
| `POST /agent/heartbeat` | Identity & session | `read` | No | — (no handle) | Which Agent's `health` this touches |

Two points are easy to misstate. First, **there is no new RBAC verb.** `RoleButler` already means "read and propose" (`keystone/principal/rbac.go`); a call that only observes is `read`, a call that changes something outside the runtime is never applied directly by the handler and is `propose`-gated, same as `propose_change` today. The fine-grained work a new verb would duplicate is already done by `Invoke`'s own tier check (ADR-W039 §3.4 step 4). Second, **the handle and the credential authorise different things and never swap roles.** The credential says which Agent is calling, checked once by `RuntimePrincipalButler`. The handle says which task, Person, Connection and audience the call runs under, resolved fresh from the ledger at every call (ADR-W039 §3.1 principle 4). No `agentapi` request carries a Person field, and a handle from one task cannot reach another Agent's authority. Reversing this — letting the handle imply the Agent, or the credential imply the Person — is the shortcut ADR-W039 §1.6 named as the thing this contract closes off.

---

## 4. Delivery

**Pull (long-poll) is the baseline**, used identically by a packaged runtime and an externally-hosted one. `GET /api/v1/agent/deliveries?wait=<seconds>` mirrors `keystone/adapter/adapter.go`'s `Claim`: a bounded wait, an empty response with no error is a quiet period, and the runtime asks again — reusing a pattern proven by the chat adapter's outbound poll and the integration adapter's work claim, under the constraint that motivates it here: **Keystone never dials a node.** Keystone runs under systemd on the control host; node containers dial out; container-name DNS resolves only between containers on one node; ADR-W039 §6 forbids a runtime port sitting behind a Route with `auth: none`. A packaged Hermes reaches this endpoint through the bridge sidecar the Hermes design doc names (§2.1: "the bridge long-polls Wildstone deliveries with the runtime credential, posts each as `POST /v1/runs` to Hermes over the container network"); an externally-hosted runtime reaches it directly, or via the same bridge binary.

**Push is optional and negotiated**, never assumed. `RuntimeRecord.Negotiated.Delivery` (`push | pull` in the Phase 0 types) records what a runtime declared at registration; an operator with a public endpoint may set `push` and receive a signed webhook per delivery, the shape Hermes's `hooks.outbound` and OpenClaw's `/hooks/agent` both document (`accepted is not completed`, §3.8's required property). Keystone signs the delivery; the endpoint verifies it and answers `200` meaning "admitted", never "done" — completion is still reported over the ordinary `Checkpoint`/`Complete`/`Fail` calls, whichever mode was chosen.

**Cursor, ack, TTL and idempotency** are the terms ADR-W039 §3.6 and `agentapi.Delivery` already fix, given wire form here. Every delivery carries an opaque `Cursor` and an `ID` that is also its idempotency key; a runtime acknowledges by `POST /agent/deliveries/{id}/ack`, and the cursor advances on ack, not on send. Redelivery is idempotent by construction: `Delivery.Attempt` counts from 1, and a runtime seeing the same `ID` twice performs the same effect at most once because the triggered calls are themselves idempotency-keyed (`InvokeRequest.IdempotencyKey`, `CheckpointReceipt.Epoch` fencing a stale attempt). A delivery unacknowledged past `ExpiresAt` is recorded `expired`, never silently dropped.

**Offline means queued, not lost.** A runtime with `health: offline` (or simply not polling) leaves its pending deliveries queued, behind their subscription's cursor, within retention (ADR-W039 §3.7). Deterministic work continues around the gap; an escalation falls back to its author's wording; a task step needing the runtime reports `waiting: runtime_offline` rather than failing. On reconnect the runtime resumes from its last acknowledged cursor and, for any task mid-checkpoint, from ledger-verified state — never a replayed transcript.

---

## 5. Why not MCP-first, why not WebSocket/SSE-first

**Why not MCP-first.** Two research findings agree. Both reference runtimes are already MCP *clients* and already expose ordinary HTTP: Hermes is an MCP client over Streamable HTTP/SSE for tools, and separately runs an OpenAI-compatible API server and signed webhooks for runs and lifecycle; OpenClaw is an MCP client with `toolFilter`, and separately runs `/v1/chat/completions`, `/hooks/agent` and a WebSocket control plane. Neither treats MCP as its only surface — Events, Work and lifecycle already go over HTTP upstream, the split §3.8's leading candidate leaned toward, simplified further by not standing up the MCP half yet. Decisive for a first cut: **there is no Go MCP library in this dependency set**, and Keystone is a single Go binary with a closed dependency policy (ADR-W039 §8 Phase 5: "MCP server (Go MCP library or hand-rolled Streamable-HTTP subset — new dependency decision)"). REST needs neither a new dependency nor hand-rolled framing: `net/http` and `encoding/json`, already used throughout `keystone/api`.

**Why not WebSocket/SSE-first.** A stateful, long-lived connection fits poorly with ADR-W039 §3.7's leading property: immediate, no-cooperation-required revocation. A bearer checked per request is revoked the instant the disabled flag is set; a live WebSocket or SSE stream must be torn down out-of-band — either Keystone keeps a registry of live connections to kill, or revocation is only as fast as the runtime's reconnect logic. Long-poll gets the same low-latency delivery without Keystone ever holding a connection past one authenticated request.

**The chat-adapter and integration-adapter precedent already exists, in production shape.** `keystone/adapter/adapter.go`'s `Claim`/`Report` against `/api/v1/integration-work/{claim,result}`, and `cli/cmd/chat-adapter`'s poll/ack against `/api/v1/chat/outbound`, are both bearer-authenticated, long-poll with a bounded wait, treat "nothing yet" as a quiet period, and are processes outside Keystone holding one narrow credential — the runtime contract's own self-description (ADR-W039 §2.2). REST plus long-poll is a pattern this codebase already knows how to build, test and operate.

---

## 6. How MCP arrives later

When a second transport is warranted — an operator's runtime speaks MCP more fluently than raw HTTP, or `tools/list_changed` turns out to matter more than the "assume next run" fallback ADR-W039 §5.3 accepts today — it is added as a thin translation layer, not a parallel implementation:

- **`tools/list` → `Manifest`.** The MCP server calls the same `agentgateway.Gateway.Manifest(ctx, handle)` the REST handler calls, mapping each `ToolSpec` onto an MCP tool declaration (`Name`, `Description`, `InputSchema` verbatim).
- **`readOnlyHint` mapping.** `ToolSpec.ReadOnlyHint` maps onto MCP's own `readOnlyHint`. This matters: the Hermes design doc (§6) found that with `trust: untrusted`, a runtime routes every non-`readOnlyHint` tool through its own approval, and an unattended run then denies every domestic-tier call before Wildstone ever sees it — so the Wildstone MCP server is marked *trusted* (Wildstone's gateway is the enforcement point; a second gate adds only denials), while `readOnlyHint` still tells the runtime which calls are safe to retry.
- **`tools/call` → `Invoke`.** Arguments pass through as `InvokeRequest.Args` unchanged; `Outcome`/`Content`/`Reason`/`WaitReason` fold into the tool result, `waiting` returned as an ordinary non-error result carrying the wait reason as text — a `confirm`-tier wait reaching a runtime expecting a synchronous result gets one that says "not yet" (§3.8's question).
- **Bearer and handle travel the same way.** The credential is a static header (as Hermes's `identity_header` and OpenClaw's bearer both work), the handle an explicit `tools/call` argument, exactly as it is an explicit REST field — never inferred from a per-task URL or session identity, per the Hermes design doc §6's own recommendation: "the task handle is an opaque, Wildstone-issued, Agent-bound token carried as an argument on every capability, memory and work call; the gateway refuses a handle not issued to the credential's Agent."
- **No new authority in the translation.** The MCP server holds no state the REST handlers lack; it calls the identical `agentgateway.Gateway` methods, so a call denied over REST is denied over MCP for the same reason, at the same layer, with the same audit row.
- **What stays REST-only.** Deliveries and Work checkpoints are not MCP tools. Deliveries are Wildstone calling out, or a runtime polling a queue — not a capability a model chooses to invoke — and routing them through `tools/call` would make task lifecycle bookkeeping nameable by the model, the authority-by-side-channel this contract exists to prevent. Checkpoints are the runtime's *report* of its run, reconciled against what Wildstone actually served; keeping them off the model-facing surface keeps that reconciliation honest.

---

## 7. Security properties

- **Bearer plus binding.** Every request authenticates a `wst_` token to one principal, bound to exactly one Agent for its lifetime (`runtime_principals`, immutable, Phase 1a). A handle is refused unless the resolved task's Butler matches the credential's bound Agent — neither half alone is enough.
- **Handle scoping.** A handle names one task: issuing a new one replaces the live one, a handle for a completed/failed/cancelled task is refused, and `WithdrawHandle`/`FenceTask` retire it the instant revocation or failure makes it stale.
- **Fail-closed on revoke.** Revoking disables the principal (checked per request), fences in-flight attempts so a late result is rejected, drops the Agent's subscriptions, and sets `health: revoked` — none of it requiring the runtime's cooperation, because the transport authenticates per request, not per connection.
- **Rate and size limits.** Every request passes Keystone's per-principal rate limiter (`api.go:925`); bodies are decoded through a bounded reader (`http.MaxBytesReader`, the chat-adapter's pattern) so an oversized body cannot exhaust memory before validation.
- **No secret in URLs; redaction in logs.** The bearer travels only in `Authorization`; the handle travels in bodies, or, for the one `GET`, as a `wait` duration. A `Handle` is `<taskID>.<secret>`, and its `String()` redacts the secret half; every log line and audit row prints the handle through that method, so a leak never yields a working credential.
- **Idempotency keys.** `InvokeRequest.IdempotencyKey` protects a re-sent call; `Delivery.ID` keys a redelivered delivery; `CheckpointReceipt.Epoch` fences a checkpoint under a superseded attempt — covering invoke, delivery and checkpoint without the runtime reasoning about at-most-once semantics itself.

---

## 8. Consequences and open questions

**Consequences.** The Agent API becomes one more authenticated family inside the existing Keystone HTTP API — added to `api.go`'s router beside `/butlers`, `/chat` and `/integration-work`, exercised by the same `handle()` wrapper, audit pipeline and rate limiter. The in-process and eventual HTTP clients can be asserted byte-identical (ADR-W039 Phase 2's own evidence bar) because both call the same `agentgateway.Gateway` methods; the REST handlers are a thin skin over calls already tested. A packaged runtime's bridge sidecar is a small, ordinary long-poll client, the same shape as the chat-adapter and integration-adapter binaries already in `cli/cmd/`. The cost is deferred, not avoided: a runtime that only speaks MCP needs §6's translation layer to integrate at all, and that layer is gated on a dependency decision.

**Open questions**, carried to the follow-up issues: exact REST envelope conventions (whether `denied`/`waiting`/`rejected` return HTTP 200 with the outcome in the body, matching `agentapi`'s "a refusal is not an error" stance, or a 4xx whose body a runtime must still inspect); long-poll wait bounds for `/agent/deliveries?wait=` (the adapter's ten-second default is a starting point, not proven for this traffic shape); the MCP server's dependency choice, deferred to whichever issue implements §6; and whether a bridge and a direct client should share one Go module for idempotency-key generation, redelivery and backoff.

**What this closes from ADR-W039 §3.8.** All five named questions now have an answer, some of them "does not arise yet, because MCP is not first": the task handle is an explicit REST field, later an explicit MCP argument, never a header naming only the profile; `tools/list_changed` is a §6-only concern, since a REST client simply calls `Manifest` again; a `confirm`-tier wait is an ordinary `InvokeResult{Outcome: waiting, WaitReason: confirm_pending}` over REST, and a same-shaped non-error MCP result later; Work checkpoints are their own REST family, never MCP tools; and the in-process client shares handler code by calling the identical `agentgateway.Gateway` methods the REST handlers call, with no MCP framing anywhere in that path. The transport choice itself — REST plus long-poll, MCP second — is the decision §3.8 deferred and this ADR makes.
</content>
