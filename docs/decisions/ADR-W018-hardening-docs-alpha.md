# ADR-W018: Hardening, docs, alpha release (E13)

- **Status:** accepted
- **Relates to:** ADR-W002 (e2e boundary), ADR-W011 (backup engine),
  ADR-W012 (secrets path), ADR-W014 (installer), implementation scope E13
- **Audience:** decision record AND implementation spec for E13.

---

# Part A — Decisions (binding)

### A1. The exit drill is a script over real binaries, run weekly in CI

The scope's exit drill — "total-host-loss restore, unattended, on the
e2e harness" — runs as `scripts/exit-drill.sh`: an unattended script
that builds the real `keystone`, `wardstoned`, and `wild` binaries and
drives them against the runner's real Docker daemon and the pinned
restic image. No mocks anywhere in the drill path: real join, real
plan proposal/approval over the public API, real reconcile creating
real Docker volumes, a real restic backup, real host loss (processes
killed, data dirs and the data volume destroyed), and a real
restore-alongside from the surviving repository.

Hosted runners provide Docker but not nested-VM comfort, so the drill's
"host" is the process+state boundary, not a VM: everything Keystone and
Wardstone persist is destroyed except the two things the recovery doc
(A3) tells operators to keep off-host — the restic repository and a
replica of the Statefile git repo. A VM-boundary variant on the tailnet
runner (ADR-W002's proxmox provider) can wrap the same script later;
the script is the drill either way.

`.github/workflows/exit-drill.yml` runs it weekly (cron) and on
`workflow_dispatch`. A drill failure is a release blocker: the alpha
promise is "you can lose the host and keep your data", and this is the
only executable proof of it.

### A2. What must survive, verbatim (the recovery contract)

The drill pins the recovery contract the docs teach:

1. **The restic repository** (per BackupPlan) — holds the data.
2. **The Statefile git repo** — holds the desired state.
3. **The operator's secret values** (in their password manager) —
   secrets are deliberately NOT backed up by Wildstone (ADR-W012:
   write-only path, values never leave the node), so recovery re-enters
   them via `wild secret set`.

Everything else — runtime DB, CA and op-signing keys, node identity,
containers, volumes — is rebuilt from those three. A new CA on rebirth
is correct behavior, not a defect: nodes re-join with fresh join
tokens.

### A3. Docs are part of the deliverable, refreshed against as-built

`docs/install.md` (both roles, from the released installer),
`docs/concepts.md` (resources, plans, capabilities, the Butler's
ceiling), `docs/recovery.md` (the A2 contract as a runbook, mirroring
the drill step-for-step), and `docs/threat-model.md` (refreshed against
what was actually built: closed op surface, plan-mediated writes,
encoder-level redaction, envelope hardening, RBAC, rate limits). Docs
state what the code does today, citing the ADR that decided it — no
aspirational features.

### A4. Dogfood migration is a runbook with a hard cutover rule

`docs/migration.md` captures the HeadOverHalls parallel-VM migration:
hand-managed and Wildstone-managed workloads may coexist (the drift
classifier's never-delete-foreign rule protects foreign resources), but
**ingress and identity are singular**. Apps cut over per-capability via
DNS; identity migrates exactly once (LLDAP export/import, old directory
retired same day); ingress ownership transfers atomically per hostname,
never incrementally. Friction found during the migration is filed in
`docs/friction-log.md` and burned down before alpha exit.

# Part B — Work packages

- **H1** — A1/A2: `scripts/exit-drill.sh` + weekly workflow + CI lint
  coverage for all of `scripts/`.
- **H2** — A3: install/concepts/recovery/threat-model docs.
- **H3** — A4: migration runbook + friction log.
