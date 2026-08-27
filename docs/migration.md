# Dogfood migration runbook (HeadOverHalls → Wildstone)

The E13 dogfood migration (ADR-W018 A4): recreate the hand-managed
infra VM's capabilities on a parallel Wildstone-managed VM, cut over
per capability, and file every friction point in
[friction-log.md](friction-log.md). This runbook is written for the
HeadOverHalls deployment but is the template for any migration from a
hand-managed compose/Caddy/LLDAP host.

## The two-masters rule (binding)

Hand-managed and Wildstone-managed **workloads** may coexist for as
long as the migration takes — the drift classifier never deletes
foreign resources, and the agent refuses to touch containers it does
not own, so neither side can eat the other. But two things are
**singular** and must never have two masters:

- **Ingress:** two Caddys cannot both own `:443` for a hostname.
  Ownership of a hostname transfers **atomically per hostname** (the
  Route + its DNS record move in one cutover), never incrementally.
- **Identity:** two directories cannot both be the source of truth.
  Identity migrates **exactly once**: LLDAP export → import into the
  Wildstone-managed directory → every remaining consumer repointed →
  the old directory retired the same day. No dual-running period.

## Phase 0 — parallel VM up

1. Provision the parallel VM; install Wildstone per
   [install.md](install.md) (keystone + node, or node joined to an
   existing keystone).
2. Set up the recovery contract FIRST ([recovery.md](recovery.md)):
   off-host restic repository storage, statefile replica remote,
   secrets in the vault. Migrating onto a host that can't lose itself
   safely is moving the problem, not solving it.
3. Install the infra capabilities (ingress, identity, dns, tunnel
   packages) — their routes/clients serve test hostnames only at this
   point.

## Phase 1 — per-capability migration (repeat per app)

For each capability (photos, media, documents, ...):

1. **Recreate** it from its package on the Wildstone VM, pointing at a
   test hostname (`photos-new.<domain>`). Secrets via
   `wild secret set`; data volume empty for now.
2. **Move data**: snapshot the old host's data (stop the old app
   briefly or use app-level export), copy into the Wildstone volume,
   start, verify against the test hostname.
3. **Backups on before cutover**: the capability's BackupPlan runs and
   its ledger shows `succeeded` — a capability is not migrated until
   it is backed up.
4. **Cut over via DNS**: repoint the real hostname's DNS/tunnel to the
   Wildstone ingress and move the Route to the real hostname in the
   same plan. The old vhost is removed from the old Caddy at the same
   moment (atomic per hostname — never both serving).
5. **Freshness pass**: re-sync any data delta accrued during the
   cutover window (or accept the window by stopping the old app at
   step 4).
6. **Observe for a week** before deleting the old container. The old
   copy is the rollback: rolling back is repointing DNS, nothing else.
7. File every surprise — missing spec field, unclear error, manual
   step the plan didn't cover — in the friction log **as it happens**.

Order: start with the lowest-stakes capability (media), end with the
highest (photos, documents). Infra capabilities are validated by the
first app cutover.

## Phase 2 — identity, exactly once

Preconditions: every remaining hand-managed consumer is inventoried
with its bind/OIDC config; the Wildstone identity capability is up and
already serving the migrated apps' OIDC clients.

1. LLDAP export from the old directory (users, groups, password
   hashes).
2. Import into the Wildstone-managed directory; verify every user can
   authenticate against a migrated app.
3. Repoint every remaining consumer (old-host apps included) at the
   new directory.
4. Retire the old directory **the same day**. From this moment there
   is one identity master, permanently.

## Phase 3 — decommission

When every hostname and the directory are Wildstone-owned: final
backup verification (`wild backup list` all green, restore-drill
ledger clean), then the old infra VM is archived and powered off.
Keep its disk image until the first weekly exit drill passes after
decommission.

## Exit criteria (alpha)

- Every HeadOverHalls capability serving from Wildstone.
- One ingress, one identity master.
- All BackupPlans green, exit drill green.
- Friction log empty or every entry explicitly deferred with a reason.
