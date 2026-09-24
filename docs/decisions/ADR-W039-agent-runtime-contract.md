# ADR-W039: The AgentRuntime contract and the native runtime

Status: proposed (#357)

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

This ADR does not remove that code. `Runner` keeps serving live Butler
traffic until the native runtime is switched on for it (see Follow-up).

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
  provider.

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
- CI runs all three (the `native-agent` job).

## Follow-up

1. Switch `butlerlive` to the native runtime behind a per-deployment
   setting. This needs a `Recorder` over the Butler task ledger, so native
   runs keep ADR-W030's durable evidence and fencing. The sidecar is already
   installed at `native.DefaultInstallDir` (see Packaging).
2. Hermes and OpenClaw adapters implement `AgentRuntime`, reusing
   `agentruntime.Guard` so every runtime stops for the same reasons.
