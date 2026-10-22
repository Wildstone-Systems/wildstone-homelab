# ADR-W017: Butler productionization — sessions, host, diagnostics, hardening

- **Status:** accepted; A1, A4 and A9 amended by ADR-W033
- **Governed by:** ADR-W033 where they overlap.
- **Relates to:** ADR-W001 (acting identity), ADR-W006 (Butler spike S4),
  ADR-W007 (one API), ADR-W009 (policy), implementation scope E12
- **Audience:** decision record AND implementation spec for E12.

## Constraints from ADR-W030

- **Durable work.** A conversation turn may create or continue durable
  work whose task ledger survives provider errors, process restarts, and
  per-turn limits. Checkpoint each tool invocation before dispatch and
  after result. Resumption reconstructs context from current permissions
  plus the validated ledger; historical tool traffic is not replayed as
  fresh truth.
- **Budgets.** Iteration slices and stall deadlines are set per Agent
  class, with initial evaluation defaults of 16 model rounds for Member
  Agents and 24 for Owner Agents; individual tool calls are counted
  separately. Tasks also persist aggregate model-call, tool-call,
  active-work, and absolute-deadline budgets. Retry and fallback along
  the Agent's own model chain (A4) stay within those budgets. A task
  that still cannot proceed becomes `stalled, resumable`, appears in
  Console, and can issue one deduplicated task-bound continuation prompt
  on its originating Connection. Continuation adds no authority and does
  not reset the aggregate budget.
- **Work traces.** Every turn/task emits the structured work trace from
  W030 A11: selected model and verdict, prompt/config/tool/policy
  versions, permission-filtered context source references and freshness,
  redacted/bounded tool attempts/results, timings/retries/waits,
  approvals/denials, delivery, feedback, and completion evidence. It
  records no private chain-of-thought. Detailed debug capture is
  explicit, scoped, visibly active, and expires automatically. An Agent
  can file an improvement request from a trace but cannot alter its own
  prompt, tools, policy, permissions, or evaluation suite.

---

# Part A — Decisions (binding)

### A1. Every session is a Person-bound record (ADR-W001, non-negotiable)

A Butler conversation exists only as a `butler_sessions` row (runtime
DB) created for the AUTHENTICATED caller and keyed by Person + Agent +
session id (ADR-W033 B10). Each message records the Connection it
arrived and left on; the Connection is not the session identity. The
full transcript audit (provider, every tool call and its
already-redacted result, every plan) is persisted as the session
closes. There is no singleton admin session and no anonymous path — the
scope doc's explicit "do not optimize this away". Transcripts store only
post-Dispatch content, so nothing unredacted ever reaches the DB.

_Changed by ADR-W033 §B10: a session belongs to a Person and an Agent, not to an acting-for principal._

### A2. The Butler is hosted behind the ONE API

`POST /api/v1/butler/ask` (read verb — conversing is not a write) runs
one session over the configured provider and returns the final text,
the session id, and any plan ids proposed. Session history is
`GET /api/v1/butler/sessions[/{id}]` (read). Ground rule 3 holds: the
Console and `wild` consume these same endpoints; there is no private
Butler channel.

### A3. Proposals land as real plan records

`propose_change` now also carries the validated resource's canonical
YAML in its outcome; after the session, the host submits each accepted
proposal through the SAME pipeline the HTTP propose endpoint uses
(extracted as one shared helper: validate, policy, 422-on-violation,
insert) as principal "butler" acting-for the caller. The Butler's
structural ceiling is unchanged — its proposals enter the plans table
in state `proposed`, where only a human's approve verb can move them.

### A4. Model config and degraded modes

Each Agent owns an ordered model/provider chain (ADR-W033 B6). Fallback
stays within that Agent's chain: there is no deployment-wide fallback
order, no Agent is substituted for another, and a local-only task fails
rather than falling through to a cloud model. The `-butler-provider`
(off | anthropic | openai | ollama), `-butler-model` and
`-butler-endpoint` flags at most seed a legacy single-provider default
used while no Agent exists (A9). API keys never come from a flag (A10).
Each model in a chain carries the S4 tool-reliability probe verdict:

- probe passes → full mode (read + propose);
- probe fails → **QAOnly**: sessions run with the read-only tool set,
  and dispatch refuses `propose_change` even if a confused model names
  it (the existing ADR-W006 A4 enforcement);
- no usable model configured → the butler endpoints answer 503 with a
  clear reason. The morning report (A5) is deterministic and keeps
  working in every mode.

_Changed by ADR-W033 §B6: models are chosen per Agent, not per deployment._

### A5. Diagnostics: dependency-chain walking + the morning report

- A new read tool, `diagnose`, walks a capability's resources in
  dependency order (the plan kindOrder: identity wiring → volumes →
  containers → routes) against observed health and names the DEEPEST
  broken dependency as the likely root cause, listing downstream
  unhealthy resources as consequences — the difference between "photos
  is broken" and "immich-postgres is down, which is why immich-server
  and the photos route are failing". Observed state reaches the
  gateway through an injected read-only seam; the butler package still
  never imports reconcile.
- The failure-fixture library: eight scripted scenarios (dead DB,
  stopped app container, broken OIDC wiring, missing secret/env,
  detached storage volume, dead route upstream, full-disk detail,
  drifted spec) pinned as tests over the walker — the scope's "build
  the failure-fixture library" as executable fixtures.
- The **morning report** is a deterministic builder (unhealthy
  resources, last-24h event summary, pending plans, backup ledger
  verdict) run by a daily Keystone timer, appended to the event stream
  and served at `GET /api/v1/reports/morning`. No LLM required — a
  provider, when configured, may narrate on top later, but the facts
  never depend on one.

### A6. Injection hardening (§6.3) is a growing test suite

The S4 protections (centralized redaction, untrusted envelope with
breakout neutralization, size bounds, dispatch-level QAOnly refusal)
stay; E12 adds red-team cases as PINNED TESTS: envelope forgery via
case/spacing variants, instruction-bearing health details attempting
tool escalation, secret-shaped exfiltration through every read tool,
and hostile user messages over the ask endpoint (bounded input,
transcript redaction). A failure here is a release blocker, not a
flaky test.

# Part B — Work packages

- **B1** — A1 session store; A2 endpoints; A3 shared proposal pipeline;
  A4 provider config + degraded modes in cmd/keystone.
- **B2** — A5 diagnose tool + fixture library + morning report.
- **B3** — A6 red-team test additions.

# Part D — Amendment (2026-09-06, binding)

### A9. Model config is runtime state, not a startup flag

Each Agent's model chain — provider, model and endpoint per entry — is
**runtime state**: editable by an Owner through the API and the Console,
persisted in the runtime DB, and applied without restarting Keystone.
Restarting the control plane to change a model drops every agent
session and re-runs the observe cycle, and a model name typed from
memory produces an Agent that configures cleanly and fails on first
use; the Console therefore lists what an endpoint offers.

**The flags are only the legacy bootstrap.** A fresh install with
`-butler-provider ollama` still works and needs no Console, but only
while no Agent exists; once one does, each Agent answers on its own
chain and the flags are ignored.

**API keys are never runtime DB state.** They come from the process
environment or are entered write-only in the Console (A10); they are
never stored in the runtime DB, never returned by a status endpoint, and
never a flag. What is editable here is which provider and which model —
never the credential. A provider selected without its key configured
reports unconfigured with that reason.

**The probe re-runs on change, and its verdict travels with the
config.** A4's degraded modes are unchanged, but they stop being a
property of how the process started. Selecting a model runs the S4
tool-reliability probe against THAT model and records the verdict, so
QAOnly means "this model did not pass" rather than "something did not
pass, once, at boot".

That distinction is not academic. On the alpha VM the startup probe
timed out against a cold Ollama model and pinned the Butler to QAOnly
until the next restart — while the model, once warm, called tools
correctly. A verdict earned at the worst possible moment should not
outlive the moment.

**Rejected: a config file.** It would need a watcher, a parse-failure
story, and a second place for an operator to look. The runtime DB
already holds sessions and plans, is backed up with them, and is what
the API already reads.

**Rejected: leaving the flags authoritative and having the Console print
the line to paste.** It solves the discovery half — which model exists —
and leaves the operator editing a systemd environment file to act on the
answer. The Console would be showing someone what they cannot do.

_Changed by ADR-W033 §B6: config is a per-Agent chain; the flags no longer set a deployment-wide model._

### A10. A provider key may be entered in the Console, write-only (2026-09-19)

**Why.** With models per Butler (ADR-W031) and hosted providers offered
in the Console's provider drawer, "keys come only from the environment"
meant an Owner had to reach the machine's shell and edit
`/etc/wildstone/keystone.env` to connect OpenAI — the one step of setting
up an assistant the Console could not do. The rule was protecting a set
of properties, not the shell: a key is never read back, never replicated
into the places Wildstone copies (the runtime DB, the Statefile and its
git history, plans), never logged, never returned. None of those needs
the key to be typed on the host.

**Decision.**

- `PUT /api/v1/butler/providers/{name}/key` accepts a hosted provider's
  key (`VerbManage` — an Owner signed in to the Console, ADR-W033 A4.1,
  or a host bearer token); `DELETE` forgets it. Neither returns it.
- It is stored in `<data-dir>/provider-keys.json`, mode 0600, written
  atomically — outside the runtime DB, the Statefile, plans and every
  backup. A restore re-enters it, as the secrets path already requires
  (ADR-W012).
- Each provider has its own key (`OPENAI_API_KEY`, `OPENROUTER_API_KEY`,
  `ANTHROPIC_API_KEY`, `OPENAI_COMPATIBLE_API_KEY`), so OpenAI and
  OpenRouter can be used side by side. **An environment variable still
  wins**: an operator who manages keys in the environment is never
  overruled from a browser, and the Console says so.
- `GET /api/v1/butler/providers` reports `keySet` and `keySource`
  (`environment` | `console`) — presence and origin, never the value.
- Setting or removing one is an event (`provider.key_set`,
  `provider.key_removed`) naming the provider and who; the API audit
  record carries method and path only. The value travels in the request
  body and nowhere else, and a malformed key is refused without being
  echoed.
- A key change applies to the next turn, not the next restart: cached
  provider clients are keyed on the key generation.

**What this does not change.** Keys are still never returned by any
endpoint, never stored in the runtime DB or Statefile, and never in a
flag. Somebody who can open the Console as an Owner can REPLACE a key,
not read one — the same power they already had over which model answers.
The residual exposure is the moment of entry: the key crosses the
browser and the identity proxy once, over the tailnet's TLS, as any
secret typed into a form does.

