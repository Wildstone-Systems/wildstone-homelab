# ADR-W040: Agent API transport — REST and long-poll first, MCP second

- **Status:** accepted (#342); implemented
- **Governed by:** ADR-W033 where they overlap.
- **Scope:** the wire transport for the `agentapi` contract (ADR-W039 §3–§4): how a runtime authenticates, carries a task handle, fetches a manifest, invokes a capability, moves memory, checkpoints work and receives deliveries, over HTTP. It does not reopen the resource families, runtime record, memory boundary or migration plan; those are ADR-W039's and stand.
- **Depends on / relates to:** [`ADR-W039`](ADR-W039-canonical-agent-api-and-pluggable-runtimes.md) §3.2, §3.4, §3.6–§3.8, §4, §4.1 (the contract this ADR gives a wire form); [`docs/design/hermes-capability-package-and-execution-profiles.md`](../design/hermes-capability-package-and-execution-profiles.md) §2.1, §3.3, §6 (the first external consumer); `keystone/agentapi` (`types.go`, `client.go`).
- **Implementation:** `keystone/api/handlers_agent.go` (REST), `keystone/agentapi/httpclient` (shared client), `cli/cmd/agent-bridge` (MCP translation layer).

`Agent` and `Butler` mean the same architectural object. `Runtime` means whatever turns a turn or a delivery into model calls and capability calls.

Runtime type `native` = Keystone-hosted; its loop is chosen per deployment by `-agent-runtime legacy|native`: `legacy` is `keystone/agentruntime/legacy.Runner`, `native` is the OpenAI Agents SDK sidecar (ADR-W041). Runtime types `hermes` and `openclaw` are external Agent API clients.

---

## 1. Context

This ADR gives the `agentapi` contract (ADR-W039 §3–§4) its wire form: how a process outside Keystone reaches the `agentapi.Client` methods that `keystone/agentgateway.Gateway` implements in-process, authenticated by a runtime principal and scoped by a Wildstone-issued task handle.

The owner's decision: **plain REST plus long-poll over the existing Keystone HTTP API; MCP as a second transport, translated onto the same handlers.** Two subsidiary calls follow and are also closed here: invoking a capability reuses the RBAC verbs the `butler` role already has (`read`, `propose`), no new one; and both packaged and externally-hosted runtimes use pull delivery; push is an optional negotiated mode, never the baseline, and is not built.

---

## 2. Decision

Wildstone exposes the `agentapi` contract as an ordinary part of its existing HTTP API, under `/api/v1/agent/...`, alongside `/api/v1/butlers/...`, `/api/v1/chat/...` and `/api/v1/integration-work/...`. A runtime is an HTTP client of Keystone, exactly as a chat adapter or an integration adapter is today — not a new server Keystone must learn to speak.

**Authentication** is the runtime principal's bearer token, minted by `POST /api/v1/butlers/{name}/runtime/credential`, checked like every other Keystone credential: `Authorization: Bearer wst_<64 hex>`, resolved by `Principals.Authenticate`, refused if disabled or revoked, on every request. No session, no credential that outlives a request — revocation lands on the runtime's very next call, the property ADR-W039 §3.7 asks for, which a stateful transport would make harder, not easier.

The bearer is also **route-confined**. After authentication, Keystone resolves
the credential through the immutable `runtime_principals` binding and permits
only the exact method/path pairs in §3. This check runs before request-body
decoding. General resource, raw-event, Plan, approval and management routes are
denied even though the underlying `butler` role has `read` or `propose`; a
runtime uses assigned tools and the task-bound `/agent/propose` operation
instead. Principals without a runtime binding retain their existing RBAC path.

**Bodies are the `agentapi` Go types, 1:1, with no second schema.** `Manifest`, `InvokeRequest`, `InvokeResult`, `RecallRequest`, `MemoryHit`, `RememberRequest`, `Checkpoint`, `CheckpointReceipt`, `WorkStart`, `WorkWait`, `WorkComplete`, `WorkFail`, `Delivery`, `Heartbeat`, `ProposeRequest`, `ProposalRef`, `NotifyRequest` marshal via `encoding/json` using their existing tags. The `POST /api/v1/agent/invoke` handler decodes an `agentapi.InvokeRequest` directly and hands it to the same `agentgateway.Gateway.Invoke` the in-process client calls — no wire-specific type to translate. This is ADR-W006 A3's discipline ("tools mirror the API 1:1") one layer out: `keystone/agentapi` is the one place a family's shape is decided.

**MCP is a second transport, not an alternative design.** It is a translation layer in the `agent-bridge` sidecar (§6) in front of the same `/api/v1/agent/...` handlers — `tools/list` becomes `Manifest`, `tools/call` becomes `Invoke` — with no authority the REST transport does not already grant. A runtime speaking MCP and one speaking REST directly are indistinguishable to the gateway.

```text
                    REST + long-poll
runtime ──Bearer──▶ POST /api/v1/agent/{manifest,invoke,recall,remember,
                          checkpoint,start,wait,complete,fail,propose,notify}
                    GET  /api/v1/agent/deliveries?wait=<seconds>
                    POST /api/v1/agent/deliveries/{id}/ack
                    POST /api/v1/agent/heartbeat
                          │
                          ▼
                    keystone/agentgateway.Gateway
                          │
                          ▼
                    Toolset · ToolAuthorizer · Ledger · MemorySeams · policy

                    MCP (second transport, §6)
runtime ──MCP──▶ agent-bridge MCP server (sidecar)
                          │  tools/list  → Manifest
                          │  tools/call  → Invoke
                          ▼  Bearer + handle, over REST
                    the same /api/v1/agent/... handlers above
                    (deliveries, checkpoints stay REST-only, §6)
```

---

## 3. Endpoint map (conceptual, not exhaustive)

All routes live under `/api/v1/agent/`, authenticated as every Keystone route is (`handle()`: authn → audited runtime-route gate → RBAC → rate limit), with the runtime principal resolved by `RuntimePrincipalButler(id.Name)` and the task/delivery's own Butler checked against it first. The classifier is closed: wrong methods, sibling prefixes, extra segments and newly registered routes are denied until deliberately added. The delivery acknowledgment wildcard accepts exactly one ID segment.

| Endpoint (conceptual) | Family | Verb | Consequential? | Handle authorises | Credential authorises |
|---|---|---|---|---|---|
| `POST /agent/manifest` | Capabilities | `read` | No | Which task narrows the tool list | Which Agent's grants are consulted |
| `POST /agent/invoke` | Capabilities | `propose` on the route; the tool-tier check runs inside `Invoke` | Tool-dependent | Person, Connection, audience, budget | Which Agent the resolver/ceiling evaluate |
| `POST /agent/recall` | Memory | `read` | No | Reader/destination; processing-class filter | Per-Agent memory restrictions |
| `POST /agent/remember` | Memory | `propose` (gated like `propose_change`) | No | Scope `personal`/`agent_local`, never `household`; provenance | That this Agent may `remember` |
| `POST /agent/checkpoint`, `/start`, `/wait`, `/complete`, `/fail` | Work | `read` | No | Which task's ledger; epoch fencing; evidence stamped with the task's own label/audience | That this credential is that task's own runtime |
| `POST /agent/propose` | Proposals | `propose` | Yes (files a Plan) | `ProposableKinds`; attribution to the handle's Person | That this Agent may propose this kind |
| `POST /agent/notify` | Notifications | `propose` | Yes (sends outward) | Connection, admission, disclosure | That this Agent has a Connection |
| `GET /agent/deliveries?wait=<n>`, `POST .../{id}/ack` | Events & subscriptions | `read` | No | Which subscriptions feed the queue; which delivery advances which cursor | Which Agent's queue |
| `POST /agent/heartbeat` | Identity & session | `read` | No | — (no handle) | Which Agent's `health` this touches |

Two points are easy to misstate. First, **there is no new RBAC verb.** `RoleButler` already means "read and propose" (`keystone/principal/rbac.go`); a call that only observes is `read`, a call that changes something outside the runtime is never applied directly by the handler and is `propose`-gated, same as `propose_change` today. The fine-grained work a new verb would duplicate is already done by `Invoke`'s own tier check (ADR-W039 §3.4 step 4). Second, **the handle and the credential authorise different things and never swap roles.** The credential says which Agent is calling, checked once by `RuntimePrincipalButler`. The handle says which task, Person, Connection and audience the call runs under, resolved fresh from the ledger at every call (ADR-W039 §3.1 principle 4). No `agentapi` request carries a Person field, and a handle from one task cannot reach another Agent's authority. Reversing this — letting the handle imply the Agent, or the credential imply the Person — is the shortcut this contract closes off.

---

## 4. Delivery

**Pull (long-poll) is the baseline**, used identically by a packaged runtime and an externally-hosted one. `GET /api/v1/agent/deliveries?wait=<seconds>` mirrors `keystone/adapter/adapter.go`'s `Claim`: a bounded wait, an empty response with no error is a quiet period, and the runtime asks again — reusing a pattern proven by the chat adapter's outbound poll and the integration adapter's work claim, under the constraint that motivates it here: **Keystone never dials a node.** Keystone runs under systemd on the control host; node containers dial out; container-name DNS resolves only between containers on one node; ADR-W039 §6 forbids a runtime port sitting behind a Route with `auth: none`. A packaged Hermes reaches this endpoint through the bridge sidecar the Hermes design doc names (§2.1: "the bridge long-polls Wildstone deliveries with the runtime credential, posts each as `POST /v1/runs` to Hermes over the container network"); an externally-hosted runtime reaches it directly, or via the same bridge binary.

**Push is optional and negotiated**, never assumed, and not built: `RuntimeRecord.Negotiated.Delivery` (`push | pull`) records what a runtime declared, and nothing sends. If built, push is a Keystone-signed webhook per delivery whose `200` means "admitted", never "done" — the shape Hermes's `hooks.outbound` and OpenClaw's `/hooks/agent` both document; completion is still reported over the ordinary `Checkpoint`/`Complete`/`Fail` calls.

**Cursor, ack, TTL and idempotency** are the terms ADR-W039 §3.6 and `agentapi.Delivery` already fix, given wire form here. Every delivery carries an opaque `Cursor` and an `ID` that is also its idempotency key; a runtime acknowledges by `POST /agent/deliveries/{id}/ack`, and the cursor advances on ack, not on send. Redelivery is idempotent by construction: `Delivery.Attempt` counts from 1, and a runtime seeing the same `ID` twice performs the same effect at most once because the triggered calls are themselves idempotency-keyed (`InvokeRequest.IdempotencyKey`, `CheckpointReceipt.Epoch` fencing a stale attempt). A delivery unacknowledged past `ExpiresAt` is recorded `expired`, never silently dropped.

**Offline means queued, not lost.** A runtime with `health: offline` (or simply not polling) leaves its pending deliveries queued, behind their subscription's cursor, within retention (ADR-W039 §3.7). Deterministic work continues around the gap; an escalation falls back to its author's wording; a task step needing the runtime reports `waiting: runtime_offline` rather than failing. On reconnect the runtime resumes from its last acknowledged cursor and, for any task mid-checkpoint, from ledger-verified state — never a replayed transcript.

---

## 5. Why not MCP-first, why not WebSocket/SSE-first

**Why not MCP-first.** Two research findings agree. Both reference runtimes are already MCP *clients* and already expose ordinary HTTP: Hermes is an MCP client over Streamable HTTP/SSE for tools, and separately runs an OpenAI-compatible API server and signed webhooks for runs and lifecycle; OpenClaw is an MCP client with `toolFilter`, and separately runs `/v1/chat/completions`, `/hooks/agent` and a WebSocket control plane. Neither treats MCP as its only surface — Events, Work and lifecycle already go over HTTP upstream, Decisive: **there is no Go MCP library in this dependency set**, and Keystone is a single Go binary with a closed dependency policy. REST needs neither a new dependency nor hand-rolled framing: `net/http` and `encoding/json`, already used throughout `keystone/api`. The MCP layer is a hand-rolled subset outside Keystone (§6).

**Why not WebSocket/SSE-first.** A stateful, long-lived connection fits poorly with ADR-W039 §3.7's leading property: immediate, no-cooperation-required revocation. A bearer checked per request is revoked the instant the disabled flag is set; a live WebSocket or SSE stream must be torn down out-of-band — either Keystone keeps a registry of live connections to kill, or revocation is only as fast as the runtime's reconnect logic. Long-poll gets the same low-latency delivery without Keystone ever holding a connection past one authenticated request.

**The chat-adapter and integration-adapter precedent already exists, in production shape.** `keystone/adapter/adapter.go`'s `Claim`/`Report` against `/api/v1/integration-work/{claim,result}`, and `cli/cmd/chat-adapter`'s poll/ack against `/api/v1/chat/outbound`, are both bearer-authenticated, long-poll with a bounded wait, treat "nothing yet" as a quiet period, and are processes outside Keystone holding one narrow credential. REST plus long-poll is a pattern this codebase already knows how to build, test and operate.

---

## 6. MCP

MCP is a hand-rolled JSON-RPC translation layer in the `agent-bridge` sidecar (`cli/cmd/agent-bridge/mcp.go`), not a server inside Keystone and not a parallel implementation. It adds no Go dependency.

- **`tools/list` → `Manifest`, `tools/call` → `Invoke`,** each made over the REST Agent API with the runtime's bearer through `keystone/agentapi/httpclient`. Each `ToolSpec` maps onto an MCP tool declaration (`Name`, `Description`, `InputSchema` verbatim).
- **`readOnlyHint` mapping.** `ToolSpec.ReadOnlyHint` maps onto MCP's own `readOnlyHint`. The Wildstone MCP server is marked *trusted* in the runtime (Wildstone's gateway is the enforcement point; a second gate adds only denials), while `readOnlyHint` still tells the runtime which calls are reads (Hermes design doc §6).
- **Results.** `Outcome`/`Content`/`Reason`/`WaitReason` fold into the tool result; `waiting` is an ordinary non-error result carrying the wait reason, so a `confirm`-tier wait reaches a runtime expecting a synchronous result as "not yet".
- **Handle resolution, header first.** The bridge takes the task handle from an `X-Wildstone-Handle` header; else from `X-Hermes-Run-Id` through the bridge's own run-id → handle map; else, when exactly one run is in flight, that run's handle (`solePending`, relying on the packaged Hermes config's `max_concurrent_runs: 1`). Otherwise the call is refused. The rule: a handle is never guessed across runs, and the gateway refuses a handle not issued to the credential's Agent.
- **`tools/list_changed` is not implemented.** A mid-session grant applies from the next run; a runtime refreshes by calling `Manifest` again.
- **No new authority in the translation.** The bridge calls the identical REST handlers, so a call denied over REST is denied over MCP for the same reason, at the same layer, with the same audit row.
- **What stays REST-only.** Deliveries and Work checkpoints are not MCP tools. Deliveries are a runtime polling a queue, not a capability a model chooses to invoke, and routing them through `tools/call` would make task lifecycle bookkeeping nameable by the model. Checkpoints are the runtime's *report* of its run, reconciled against what Wildstone actually served; keeping them off the model-facing surface keeps that reconciliation honest.

_Open: the single-in-flight-run fallback (`solePending`) infers the handle; it is safe only while `max_concurrent_runs` is 1 — owner to confirm._

---

## 7. Security properties

- **Bearer plus binding.** Every request authenticates a `wst_` token to one principal, bound to exactly one Agent for its lifetime (`runtime_principals`, immutable). A handle is refused unless the resolved task's Butler matches the credential's bound Agent — neither half alone is enough.
- **Handle scoping.** A handle names one task: issuing a new one replaces the live one, a handle for a completed/failed/cancelled task is refused, and `WithdrawHandle`/`FenceTask` retire it the instant revocation or failure makes it stale.
- **Fail-closed on revoke.** Revoking disables the principal (checked per request), fences in-flight attempts so a late result is rejected, drops the Agent's subscriptions, and sets `health: revoked` — none of it requiring the runtime's cooperation, because the transport authenticates per request, not per connection.
- **Rate and size limits.** Every request passes Keystone's per-principal rate limiter (`api.go:925`); bodies are decoded through a bounded reader (`http.MaxBytesReader`, the chat-adapter's pattern) so an oversized body cannot exhaust memory before validation.
- **No secret in URLs; redaction in logs.** The bearer travels only in `Authorization`; the handle travels in bodies (or, between an MCP client and the bridge, in `X-Wildstone-Handle`); the one `GET` carries only a `wait` duration. A `Handle` is `<taskID>.<secret>`, and its `String()` redacts the secret half; every log line and audit row prints the handle through that method, so a leak never yields a working credential.
- **Idempotency keys.** `InvokeRequest.IdempotencyKey` protects a re-sent call; `Delivery.ID` keys a redelivered delivery; `CheckpointReceipt.Epoch` fences a checkpoint under a superseded attempt — covering invoke, delivery and checkpoint without the runtime reasoning about at-most-once semantics itself.

---

## 8. Consequences and settled conventions

**Consequences.** The Agent API is one more authenticated family inside the existing Keystone HTTP API — beside `/butlers`, `/chat` and `/integration-work`, exercised by the same `handle()` wrapper, audit pipeline and rate limiter. The in-process and HTTP clients are asserted byte-identical because both call the same `agentgateway.Gateway` methods; `keystone/agentapi/conformance` runs one suite against either. A packaged runtime's bridge sidecar is an ordinary long-poll client, the same shape as the chat-adapter and integration-adapter binaries in `cli/cmd/`.

**Settled conventions.**

- `Invoke` returns HTTP 200 with the outcome (`denied`, `waiting`) in the body; every other Agent API method returns HTTP 403 on an authorization failure.
- The deliveries long-poll is bounded by `maxDeliveryPoll` = 30s (`keystone/api/handlers_agent.go`); `wait` above that is capped.
- The bridge and direct Go clients share one client, `keystone/agentapi/httpclient`.
- MCP is hand-rolled in `agent-bridge` (§6); no Go MCP dependency.
