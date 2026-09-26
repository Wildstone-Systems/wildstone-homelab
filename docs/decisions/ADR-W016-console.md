# ADR-W016: Console — information architecture and MVP implementation

- **Status:** accepted; A2 and A3 amended by ADR-W033
- **Governed by:** ADR-W033 where they overlap.
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
  (approve verb; the API refuses the Butler already). Restore is
  consequential (ADR-W033 A4.1).
- **Nodes** — pairing: mint a join token and show the ready-to-run
  install command (ADR-W014 A3); minting is consequential (ADR-W033
  A4.1). This is the first slice of the §7.2
  onboarding flow; the storage/DNS/tunnel probes and the Restore
  branch remain E10's later work package, NOT silently dropped.
- **Secrets** — per-node secret names/versions with generate/set/
  rotate (Owner-only; the API enforces; rotation is consequential,
  ADR-W033 A4.1).

Administering Persons — roles, admission, disabling and offboarding —
is Console work (ADR-W033 A2, A6, F1, F2). Changing a role, disabling
or offboarding is consequential; admission is ordinary (ADR-W033 A4.1).

### A3. One typed client; Owners sign in through the identity provider

`src/api.ts` is the only fetch path: JSON in/out, typed by hand against
the API's wire shapes (which tests pin server-side).

Only Owners may enter the Console (ADR-W033 A4). It signs in as a public
OpenID Connect client of Authelia with PKCE, holding no client secret,
and Wildstone reads `auth_time` from the verified ID token; no request
header can grant or refresh a session. Staying in the Console needs an
authentication no older than 12 hours; a consequential operation —
restore, minting a join token or any other bearer credential, node
secret rotation, Household-secret `manage` and `reveal`, role change,
disable, offboarding, purge — needs one no older than 15 minutes. A
refused operation offers step-up and is resumed by the Owner with one
click, never replayed (ADR-W033 A4.1).

Bearer tokens are for the `wild` CLI, the installer and host recovery
only; they are not a way into the Console.

_Changed by ADR-W033 §A4, §A4.1: a pasted token gate cannot establish who is signing in or how recently._

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
