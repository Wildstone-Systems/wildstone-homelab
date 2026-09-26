# ADR-W041: The AgentRuntime contract and the native runtime

> **Numbering and as-built note (2026-09-25).** This ADR was first filed as ADR-W039, a number already taken by *Canonical Agent API and pluggable agent runtimes* (#333), which has its own follow-up ADR-W040. It was renumbered W041 when the two lines of work were merged. As merged, the loop this ADR calls `Runner` lives in `keystone/agentruntime/legacy` (it moved out of `keystone/butler` under ADR-W039 Phase 2b and reaches Wildstone only through `agentapi.Client`), and `butler.Slice` below is `legacy.Slice`, built on that same client. `keystone/butler/runner.go` and `butler/slice.go` no longer exist; references to them below describe the design as proposed.

- **Status:** implemented (#357); the native runtime is the fresh-install default
- **Governed by:** ADR-W033 where they overlap.

## Decision

Wildstone is an agent platform, not an agent framework. Wildstone owns the
agent's lifecycle, identity, tool authorization, audit, timeouts,
cancellation and runtime selection. A runtime owns only the model's reasoning
loop. The line between the two is `keystone/agentruntime.AgentRuntime`:

```text
Name() · Start(job) · Resume(job, event) · Cancel(job) · Status(job)
Wait(job) · Capabilities() · Health()
```

- The native runtime delegates its loop to the official OpenAI Agents SDK
  (TypeScript, `@openai/agents-core`). Its adapter is `NativeAgentAdapter`
  (`keystone/agentruntime/native`).
- Hermes and OpenClaw become further adapters of the same interface.
- Choosing a runtime is configuration, not a fork in the architecture.

## Where the loop was

Before this change the Butler's loop was written by hand, in Go.

- `keystone/butler/session.go` `Session.Run` is the in-memory loop.
- `keystone/butler/runner.go` `Runner.Run` is the durable loop, with a
  checkpointed ledger (ADR-W030).
- `keystone/butlerlive/serve.go` drives it for live traffic.
- The model is called through `butler/provider.Provider`, which has
  hand-written HTTP clients for Anthropic, OpenAI-compatible servers and
  Ollama.

This ADR does not remove that code. `Runner` stays the default loop, and
still serves every route the native runtime cannot speak (see Butler
conversations).

## States

Every runtime reports these states, whatever its engine calls them:

`QUEUED → RUNNING ⇄ WAITING_FOR_TOOL →
{WAITING_FOR_USER | BLOCKED | COMPLETED | FAILED | CANCELLED}`

- **Settled** means the job has stopped moving on its own. A settled job is
  terminal (`COMPLETED`, `FAILED`, `CANCELLED`) or parked (`WAITING_FOR_USER`,
  `BLOCKED`).
- A parked job resumes with a user reply or is cancelled.
- The guarantee: **every job reaches a settled state.** "Still thinking"
  forever is not a state.

## Guardrails live outside the SDK

`agentruntime.Limits` and `Guard` are enforced by the adapter. They do not
rely on the SDK or the model behaving well.

| Guardrail | Outcome |
|---|---|
| Hard job timeout (wall clock per run) | FAILED |
| Turn timeout (no progress; heartbeats do not count) | FAILED |
| Stall timeout (no message at all: a frozen process) | FAILED |
| Tool timeout (a tool that ignores its deadline is abandoned, and its outcome is audited as unknown) | the model is told, and it counts as a failure |
| Turn budget (per run; enforced by the SDK and again by Keystone) | BLOCKED |
| Tool-call budget (per job; does not reset on resume) | BLOCKED |
| Same tool with the same input more than N times | BLOCKED |
| N tool failures or denials in a row | BLOCKED |
| Too many malformed actions (bad JSON, unknown tool) | FAILED |
| Model stops with no answer and no next action | BLOCKED |
| Model calls `ask_user` / `report_blocked` | WAITING_FOR_USER / BLOCKED |
| Cancel | CANCELLED (the sidecar is killed) |

The last two rows are the model-facing half of the guarantee. Every agent is
given `ask_user` and `report_blocked`, and is told to use them rather than
keep trying. Wildstone tools may not use those names.

## The native runtime's boundary

The SDK runs in `runtimes/native-agent`, a Node sidecar started once per run.
It talks to Keystone over newline-delimited JSON on stdin and stdout
(`native/protocol.go` ⇄ `src/protocol.ts`).

- **No SDK type crosses the boundary.** The conversation history travels
  as opaque JSON that only the sidecar reads.
- **The sidecar holds no Wildstone tool.** Each tool in the sidecar is a stub
  that sends the call back to Keystone. Keystone checks the arguments, applies
  the guard, writes the audit record, and dispatches through
  `agentruntime.ToolHost`.
  - `ButlerTools` serves the existing Butler gateways as a `ToolHost`, so
    authorization, redaction, untrusted-content envelopes and size bounds
    are the same code every other Butler path uses.
- **Audit fails closed.** If `tool.requested` cannot be recorded, the tool
  does not run and the job fails. Every job's settled state is recorded.
  `EventLogRecorder` writes the audit to the runtime event stream.
- **Least privilege.** The sidecar does not inherit Keystone's environment.
  It gets a short allowlist (PATH, proxy and CA settings) and its API key in
  `WILDSTONE_AGENT_API_KEY`. The key is never written into the protocol
  stream.
- **Tracing is off.** SDK tracing would export conversation content to
  OpenAI's trace service, so the sidecar disables it.
- **The model stays behind the adapter.** `native.Model` is an
  OpenAI-compatible endpoint: Chat Completions by default, so it works with
  Ollama and other local servers, or the Responses API. The contract names no
  provider. Which endpoint a job gets is decided by Keystone from the Butler's
  ordered model/provider chain (ADR-W033 B6), not by the runtime.

## Why a sidecar

- OpenAI publishes the Agents SDK for Python and TypeScript only. There is
  no official Go version, and the only Go port is unofficial with a single
  release.
- TypeScript was chosen over Python because the project already builds and
  ships Node (the Console).
- One process per run keeps the watchdog simple: cancelling or timing out a
  run kills that process, with no shared state to clean up.

## Packaging

- A release carries `native-agent_linux_<arch>.tar.gz` for amd64 and arm64,
  built by `scripts/build-native-agent.sh`. Each archive holds:
  - `bin/node`: the official Node LTS build. The version and the SHA-256 of
    each architecture are pinned in the script, and a download that doesn't
    match is refused.
  - `main.mjs`: the sidecar bundled into a single file with esbuild, so no
    `node_modules` is needed.
  - `THIRD_PARTY_LICENSES.txt`, `LICENSE.node` and a `VERSION` file.
- `install.sh` verifies the archive against `checksums.txt` and unpacks it to
  `/usr/lib/wildstone/native-agent` (`native.DefaultInstallDir`;
  `native.InstalledCommand` is the matching `Config.Command`).
  - It installs into a staging directory and renames it into place, so a
    sidecar that is mid-job keeps running.
  - It then runs the self-check (`--health`). A failure is reported but does
    not fail the install, because nothing depends on the runtime yet.
  - It is skipped for releases that predate the asset.
- `uninstall.sh` removes it.
- A control-plane host still needs no Node of its own (#34).
- CI builds the amd64 package the same way the release does, and runs the
  end-to-end tests with the package's own Node and bundle.

## Testing

- `native/adapter_test.go` covers every guardrail against a scripted fake
  sidecar: completion, tool failure, denial, malformed and unknown actions,
  no next action, repeats, budgets, turn, stall and job timeouts, cancel
  (running, mid-tool, parked), resume, crash, and audit failure.
- `runtimes/native-agent/test` covers the SDK loop itself.
- `native/sidecar_e2e_test.go` runs the real sidecar with the real SDK and a
  scripted model. It includes the formerly stalling case: a model that keeps
  calling the same tool now ends `BLOCKED`.
- `butlerstore/slice_test.go` runs `butler.Slice` against the real ledger:
  - evidence;
  - verified-read reuse;
  - the budget park;
  - question, blocked, pause and outage waits;
  - failed-attempt fencing;
  - withdrawn authority.
- `butlerlive/native_test.go` runs `Serve` on a scripted runtime. It covers
  route selection, every outcome, withdrawn authority, and an audit trail
  with no detail text.
- `butlerlive/native_e2e_test.go` runs a whole Butler conversation through
  the real sidecar and SDK, over HTTP to a fake Ollama `/v1` server.
- CI runs all three (the `native-agent` job).

## Butler conversations

Which loop drives a Butler conversation is a deployment choice. The model is
not: it comes from the Butler's own ordered model/provider chain (ADR-W033
B6), with no global fallback, no substitution of another Butler, and a
local-only task failing rather than falling through to a cloud model. Choosing
the loop never changes which model may be used. Code not yet conformant: today
a Butler has a single assigned route rather than a chain.

- **Turning it on:**
  - A **fresh install** turns it on. `install.sh` writes
    `-agent-runtime native` into `/etc/wildstone/keystone.env` when the
    runtime installed and passed its self-check. If it didn't, the install
    stays on `legacy` and says so.
  - `--agent-runtime legacy` opts a fresh install out.
  - An **upgrade is never switched**. It keeps whatever `keystone.env`
    already says, and an install from before this runtime has no setting,
    so it stays on `legacy`. Pass `--agent-runtime native` to switch it.
  - Keystone's own flag default stays `legacy`, so the choice is always
    visible in `keystone.env`.
- **Which Butlers use it:** Butlers on OpenAI, OpenRouter, OpenAI-compatible
  and Ollama routes. Ollama is reached through its OpenAI-compatible `/v1`
  endpoint. Anthropic routes stay on `Runner`. The loop is chosen per route,
  and the route is taken from the Butler's B6 chain in order: the runtime
  follows the chain, it never picks a route of its own.
- **If the runtime is missing or fails its self-check:** Keystone logs an
  error at startup and every conversation stays on `Runner`. A broken
  sidecar never stops the control plane.

### What does not change

Moving the loop does not change the ledger, authority, privacy or the tools.
`butler.Slice` is the Runner's own code with the loop taken out. `Runner.Run`
and `Slice` share the slice start (`begin`), final-answer acceptance
(`finish`), the budget park and the pause. `butlerlive/native.go` wires them
to the runtime:

- **Before every model attempt, retries included,** the runtime's
  `BeforeModel` hook runs three checks:
  1. the turn's privacy policy (`PrivacyTurn.Check`);
  2. the task's authority;
  3. the coordinator fence.

  It then records a model step and attempt.
- **Retries.** The sidecar's OpenAI client does no retries of its own. The
  sidecar retries once itself, and every attempt asks Keystone first
  (protocol v2 `turn` → `proceed`).
- **After each attempt,** `AfterModel` closes the checkpoint. A failed
  attempt fences the task's attempts. A successful one is accepted only if
  this coordinator still holds the database and the privacy policy still
  allows it.
- **Before an outcome is accepted,** `BeforeOutcome` re-checks privacy.
- **Every tool call** goes through `Slice.Call`, so the same things apply as
  on `Runner`:
  - the aggregate budget;
  - the dispatch authority check;
  - verified-read reuse from the ledger;
  - effect receipts.
- **How each outcome maps onto the ledger:**

  | Outcome | What happens |
  |---|---|
  | `COMPLETED` | The objective check runs, then completion with evidence. |
  | `WAITING_FOR_USER` | The task waits for a reply, and the question is the answer. |
  | `BLOCKED` on the turn budget | Pause ("say continue"). |
  | Other `BLOCKED` | The task waits for a reply, with the reason shown. |
  | `FAILED` | The task waits as model-unavailable, and the stalled-task notice is sent. |
  | A refused boundary | The turn fails with that error. |

- **The audit trail** goes to the event stream as structure only: job,
  tool, state and actor. It never carries detail text, because a detail can
  hold what the model said about a private conversation.

Conversations are sent as a fresh job each turn: the earlier turns' text
travels in `Job.Conversation`, as `Runner.History` does, and a parked task
resumes on the next message exactly as it does on `Runner`.

## Follow-up

- Hermes and OpenClaw adapters implement `AgentRuntime`, reusing
  `agentruntime.Guard` so every runtime stops for the same reasons.
