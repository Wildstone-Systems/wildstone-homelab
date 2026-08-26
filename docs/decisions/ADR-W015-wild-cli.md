# ADR-W015: `wild` CLI — thin client, and the API it still needed

- **Status:** accepted
- **Relates to:** ADR-W007 (one public API), ADR-W011/W012/W014 (the
  subcommand families already landed), implementation scope E11
- **Audience:** decision record AND implementation spec for E11.

---

# Part A — Decisions (binding)

### A1. Thin mapping, shared client, no private endpoints

Every subcommand is a thin call over the ONE public API (ground rule
3) using the client conventions `wild secret` set: token via
`WILD_TOKEN`, `-api` base URL, pretty-printed JSON out. Where the
scope's command list needed API surface that didn't exist yet, the
endpoint is added to the public plane — never a CLI-only side door.

### A2. The command set (scope E11), and its two new endpoints

- `wild status` — composed client-side from whoami + observed +
  recent events (read verb; no new endpoint).
- `wild plan list|show|approve|reject` — existing plan endpoints.
- `wild capability enable <pkg-dir> -set k=v` — expands the package
  LOCALLY (the same engine lint uses) and proposes the rendered
  resources through POST /plans: enable is an ordinary proposal, so
  policy, risk grading, and approval apply unchanged.
  `wild capability disable <name>` lists the resources owned by
  `capability/<name>` and proposes their deletion — same path.
- `wild logs <container> -node <id>` — NEW endpoint
  `GET /api/v1/nodes/{node}/containers/{name}/logs` (read verb):
  dispatches ContainerLogs, demuxes docker's framed stream
  server-side, and returns text — which therefore passes through the
  redacting encoder like every response (a secret echoed into a log
  comes back redacted; that is a feature).
- `wild audit` — existing audit endpoint (manage).
- `wild backup list|run|restore` — NEW endpoints `GET /api/v1/backups`
  (read; the ledger) and `POST /api/v1/backups/{plan}/run|restore`
  (approve verb — on-demand runs and restores are human-initiated
  executions, so the Butler's propose-only role cannot trigger them;
  scheduled runs remain the engine's standing duty). Restore is
  RestoreAlongside — in-place stays reserved per ADR-W011/W012.

### A3. Backup engine exposure

The engine gains `RunNow(name)` (run one plan immediately, schedule
notwithstanding, with the same provenance) and the API reaches it —
like MintJoin — through function hooks cmd/keystone wires, keeping
keystone/api free of a backup-engine dependency.

# Part B — Work package

- **C1** — the two endpoint families + demux; engine RunNow; the
  subcommand set; tests at the API/engine layer.
