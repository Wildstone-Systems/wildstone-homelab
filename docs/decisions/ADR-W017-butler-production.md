# ADR-W017: Butler productionization — sessions, host, diagnostics, hardening

- **Status:** accepted
- **Relates to:** ADR-W001 (acting identity), ADR-W006 (Butler spike S4),
  ADR-W007 (one API), ADR-W009 (policy), implementation scope E12
- **Audience:** decision record AND implementation spec for E12.

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
