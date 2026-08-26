# ADR-W016: Console — information architecture and MVP implementation

- **Status:** accepted
- **Relates to:** ADR-W007 (one public API, TS codegen), ADR-W014
  (pairing), ADR-W015 (endpoint families the views consume),
  implementation scope E10
- **Audience:** decision record AND implementation spec for E10.

---

# Part A — Decisions (binding)

### A1. Stack: Vite + React + TypeScript, and nothing else

No UI framework, no component library, no state manager: the scope doc
pins "deliberately boring visual design at MVP", and a homelab control
plane must stay auditable by its owner. Hand-written CSS, React's own
state, and a thin typed client (A3). The generated
`src/generated/resources.ts` stays the single source of resource types
(ADR-W007 A7's drift check keeps it honest).

### A2. Information architecture (scope E10 ▲)

One left nav, six views:

- **Home** — capability-centric: Statefile resources grouped by their
  `capability/<name>` owner, each chip carrying observed health. The
  answer to "is my house okay" in one screen.
- **Plans** — the product's signature interaction: the list, and a
  review pane showing summary, risk, principal/acting-for, the stored
  plan steps, with Approve/Reject. Approval here is THE human gate the
  whole architecture funnels through.
- **Events** — the operational stream, live via cursor polling (A4).
- **Backups** — the run ledger, with on-demand run/restore buttons
  (approve verb; the API refuses the Butler already).
- **Nodes** — pairing: mint a join token and show the ready-to-run
  install command (ADR-W014 A3). This is the first slice of the §7.2
  onboarding flow; the storage/DNS/tunnel probes and the Restore
  branch remain E10's later work package, NOT silently dropped.
- **Secrets** — per-node secret names/versions with generate/set/
  rotate (admin-only; the API enforces).

Family-member management (LLDAP CRUD) needs API surface that does not
exist yet — deferred to the LLDAP provider work, noted here rather
than faked.

### A3. One typed client, token in memory

`src/api.ts` is the only fetch path: base URL + bearer token,
JSON in/out, typed by hand against the API's wire shapes (which tests
pin server-side). The token is pasted at a gate screen and kept in
sessionStorage — surviving a reload but not the browser session; a
401 anywhere returns to the gate. No cookies, no OAuth at MVP: the
Console is an API client like `wild`, not a second auth system.

### A4. Live updates: cursor polling, not websockets

The events endpoint already serves `?after=<seq>` cursors; the Console
polls it every few seconds and appends. A push channel (SSE/websocket)
is a later nicety — polling a loopback/tailnet API is fine at MVP and
keeps the API surface unchanged (ground rule 3).

### A5. CI builds it

A `console` job: `npm ci`, `tsc --noEmit`, `vite build` — plus the
existing codegen drift check. The build output is static files; serving
them from Keystone is a later wiring (a `-console-dir` static mount),
kept out of this increment.

# Part B — Work packages

- **U1** — A1 scaffold, A3 client, the six A2 views, A5 CI job.
- **U2** — onboarding probes + Restore branch (consuming ADR-W014 A3
  and the probe ops), Keystone static serving, live-update polish.
