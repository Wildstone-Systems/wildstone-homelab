# ADR-W017: Butler productionization — sessions, host, diagnostics, hardening

- **Status:** accepted
- **Relates to:** ADR-W001 (acting identity), ADR-W006 (Butler spike S4),
  ADR-W007 (one API), ADR-W009 (policy), implementation scope E12
- **Audience:** decision record AND implementation spec for E12.

## Conditional amendment from ADR-W030

This amendment becomes binding when ADR-W030 is accepted. A conversation turn
may create or continue durable work whose task ledger survives provider errors,
process restarts, and per-turn limits. Checkpoint each tool invocation before
dispatch and after result. Resumption reconstructs context from current
permissions plus the validated ledger; historical tool traffic is not replayed
as fresh truth.

Provider configuration adds per-surface iteration slices and stall deadlines,
with initial evaluation defaults of 16 model rounds for Household and 24 for
Operator; individual tool calls are counted separately. Tasks also persist
aggregate model-call, tool-call, active-work, and absolute-deadline budgets.
Automatic retry/fallback stays within those budgets. A task that still cannot
proceed becomes `stalled, resumable`, appears in Console, and can issue one
deduplicated task-bound continuation prompt on its originating surface.
Continuation adds no authority and does not reset the aggregate budget.

Every turn/task emits the structured work trace from W030 A11: selected model
and verdict, prompt/config/tool/policy versions, permission-filtered context
source references and freshness, redacted/bounded tool attempts/results,
timings/retries/waits, approvals/denials, delivery, feedback, and completion
evidence. It records no private chain-of-thought. Detailed debug capture is
explicit, scoped, visibly active, and expires automatically. Butler can file an
improvement request from a trace but cannot alter its own prompt, tools, policy,
permissions, or evaluation suite.

---

# Part A — Decisions (binding)

### A1. Every session is a principal-bound record (ADR-W001, non-negotiable)

A Butler conversation exists only as a `butler_sessions` row (runtime
DB, migration 0003) created for the AUTHENTICATED caller: principal
"butler" acting-for `<caller>`, with the full transcript audit
(provider, every tool call and its already-redacted result, every plan)
persisted as the session closes. There is no singleton admin session
and no anonymous path — the scope doc's explicit "do not optimize this
away". Transcripts store only post-Dispatch content, so nothing
unredacted ever reaches the DB.

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

### A4. Provider config and degraded modes

`-butler-provider` (off | anthropic | openai | ollama) with
`-butler-model`/`-butler-endpoint`; API keys come ONLY from the
provider's environment variable, never a flag. On startup the provider
runs the S4 tool-reliability probe:

- probe passes → full mode (read + propose);
- probe fails → **QAOnly**: sessions run with the read-only tool set,
  and dispatch refuses `propose_change` even if a confused model names
  it (the existing ADR-W006 A4 enforcement);
- provider off/unconfigured → the butler endpoints answer 503 with a
  clear reason. The morning report (A5) is deterministic and keeps
  working in every mode.

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

### A9. Provider config is runtime state, not a startup flag

A4 made the provider, model and endpoint startup flags. That was right
while the Butler was a thing you turned on once, and wrong the moment
anybody wanted to change their mind.

What it costs today, measured on the alpha VM: pointing the Butler at a
household's own Ollama meant editing `/etc/wildstone/keystone.env` and
restarting the control plane. Restarting Keystone to change a model also
restarts nothing else — but it does drop every agent session, re-run the
observe cycle, and (F75/F76) propose a drift repair that reports
healthy resources as absent. A model change should not put the Console
into a state where the household looks broken.

Worse, the model name had to be known in advance. Nothing could list
what an endpoint offered, so the name was typed from memory or read off
a terminal on another machine — and a typo produced a Butler that
configured cleanly and failed on first use.

So provider, model and endpoint become **runtime state**, editable
through the API and the Console, persisted in the runtime DB, and
applied without restarting Keystone.

**The flags remain, and remain the bootstrap.** A fresh install with
`-butler-provider ollama` still works and still needs no Console. The
flags are the DEFAULT; stored config, once set, wins. An operator who
has never opened the Console is unaffected, and one who has is not
overruled at the next restart.

**API keys stay in the environment, unchanged.** (Amended by A10,
2026-09-19: a key may now also be ENTERED in the Console, write-only;
everything below about what a key must never do still holds.) A4's rule
is not relaxed: `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` are read from the
process environment and are never accepted over the API, never stored in
the runtime DB, and never returned by a status endpoint. What becomes
editable is which provider to use and which model to ask — never the
credential. A provider selected without its key configured reports
unconfigured with that reason, exactly as an unset flag does now.

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
  key (`VerbManage` — an Owner in `console_users` through the identity
  proxy, or an admin token); `DELETE` forgets it. Neither returns it.
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

