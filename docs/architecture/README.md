# New architecture: layout and dependency contract

Status: **authoritative for all new development** (#350, parent #338).
Everything else in this repository is the **legacy architecture**:
non-production, frozen except for security fixes and CI upkeep, and
retired path by path through [the inventory](legacy-inventory.md).

## Decision in one paragraph

Wildstone Homelab is replaced **in place, in this repository**. There are
no production users of the legacy architecture, so there is no
compatibility layer, no user-data migration and no second installer mode
(#338). The target is one standard profile: Ubuntu Server 24.04 on bare
metal, K3s with Traefik, Longhorn V1, Forgejo (direct Helm), Flux + Helm,
Rancher, Authelia + LLDAP, Homarr, CLI-installed optional apps and
independent Restic backup/recovery. #340 is the storage and resilience
audit behind it. #341–#349 own the implementation.

Until the new installer ships, the **published** installer
(`scripts/install.sh`, Docker/systemd) stays as it is. Nothing here
changes it.

## Layout

Four roots. Each is created **by the issue that first puts real code in
it** — never empty, never for symmetry.

| Root | Holds | Created by |
|---|---|---|
| `bootstrap/` | The installer and CLI: preflight, create/resume, join, app selection. Thin wrapper over upstream tools. | #341 |
| `platform/` | Pinned upstream configuration and its tests: K3s, Longhorn, Forgejo, Flux, Rancher, identity, Homarr. | #341 |
| `catalog/` | One versioned app catalog (chart pins, storage roles, placement, singleton and backup fields) and its schema. | #343 |
| `recovery/` | Backup recipes, restore procedures and the total-site recovery drill. | #347 |

Tests live **next to the code they test** (for example
`platform/longhorn/tests/`). A test that spans roots (a full first-install
or site-loss drill) lives in the root of the issue that owns it.

`tools/architecture/` holds the boundary checker below. It is CI tooling,
not product code.

## Dependency rules

1. **No legacy dependency.** Code under the four roots must not import,
   call, run, package or read Keystone, Wardstone (`wardstoned`,
   `wild-playbooks`), the old Console, the Statefile, the old pairing,
   publisher or management chart, or any legacy path (`deploy/ansible`,
   `packages/`, `scripts/install.sh`, …). This includes runtime
   dependencies: no calls to their services, ports, sockets or files.
2. **Reuse by moving.** To reuse legacy code, the owning issue **moves**
   it into a new root (`git mv`), removes the legacy coupling and adds a
   test there. Copying is allowed only while a legacy consumer still
   needs the old file; the inventory row then says which deletion removes
   the copy. Never reference the old path.
3. **Go modules build alone.** A Go module under a new root has its own
   `go.mod`, is **not** listed in `go.work`, and must build and test with
   `GOWORK=off`. It must not `require` or `replace` a legacy module.
4. **Legacy never depends on new code.** Legacy release, installer and
   tests must not call into the new roots. This keeps every retirement a
   plain deletion. (Checked in review, not by CI.)
5. **Upstream first.** No new daemon, custom scheduler, custom GitOps
   reconciler, controller or compatibility layer. Flux is the only
   reconciler for ordinary apps; Longhorn and Forgejo are the two
   documented direct-Helm bootstrap exceptions (#338, #340 §7).
6. **Safety carries over.** No secrets or real deployment identifiers in
   the repository. No automatic disk formatting, no destructive rerun, no
   silent fallback. Secret custody must not regress below
   [`flux-secret-custody.md`](../design/flux-secret-custody.md).

Markdown files are exempt from rule 1's text check, so a README may say
what it replaces. File **names** are still checked.

## How the rules are enforced

`tools/architecture/check_boundaries.py`, with rules in
`tools/architecture/boundaries.json`, runs in the
[`architecture`](../../.github/workflows/architecture.yml) workflow on
every pull request and push to `main`. It fails when:

- a file under a new root names a legacy component or path, or a legacy
  Go module;
- `go.work` lists a module under a new root;
- [the inventory](legacy-inventory.md) misses a top-level entry, or lists
  a path that no longer exists.

The same job runs `shellcheck` (pinned, same version and checksums as
`ci.yml`) on every `*.sh` under the new roots.

Run it locally:

```bash
python3 tools/architecture/check_boundaries.py
python3 -m unittest discover -s tools/architecture -p 'test_*.py' -v
```

### CI cost and legacy jobs

- The `architecture` job takes seconds and needs no toolchain beyond the
  runner's Python. It does not depend on any legacy job.
- **Legacy CI is unchanged.** Its jobs stay required until the code they
  test is deleted. The gitleaks secret scan covers the whole repository,
  new tree included, and moves into `architecture.yml` before `ci.yml`
  is deleted. Never drop it.
- When a root gains a toolchain (a Go module, Ansible, Helm charts), the
  PR that adds it also adds that toolchain's checks to `architecture.yml`
  — vet/test with `GOWORK=off`, `ansible-playbook --syntax-check`,
  pinned `helm lint`/`template`. No path filters on required jobs: a
  path-filtered required check never reports and blocks the PR.

## Follow-up PR order

Smallest useful steps. Each is one PR, reviewed before the next.
Backup design (#347) starts alongside bootstrap, as #338 requires.

1. **#341a — preflight + K3s create/resume.** Create `bootstrap/` and
   `platform/k3s/`; move `prereqs.yml`, `k3s-server.yml` and templates
   (re-enable Traefik); move the QEMU harness from `testing/e2e`; add a
   VM job. Delete `installer/`.
2. **#341b — Longhorn V1 by direct Helm.** Pinned chart, explicit r1
   Retain class, move `engine-storage-check.sh` and `disk-guard.sh` with
   reboot and missing-mount tests.
3. **Retire K3s legacy.** Delete the `deploy/ansible` retire rows,
   `engine-try.sh`, `engine-fence-check.sh`, `engine_test.go`,
   `engine-guest.sh` and the `engine-local` job.
4. **#342 — Forgejo + local Git**, with its own backup/restore test.
5. **#347a — recovery baseline**: Restic, key custody, Forgejo and Git
   export. Create `recovery/`.
6. **#343 — Flux + catalog + CLI app install**, proven with
   Audiobookshelf. Create `catalog/`.
7. **#345 — identity, ingress, Homarr.** Then retire the Console.
8. **#344 — Rancher** with the Fleet/Flux boundary.
9. **#346 — curated app profiles**, then retire `packages/`.
10. **Installer switch** — new release asset, README/install docs, then
    retire `scripts/install.sh` and the legacy release.
11. **#348 — worker join and replica expansion**; **#347b — isolated
    total-site drill**. Then retire Wardstone, then Keystone and the
    rest (see [deletion gates](legacy-inventory.md#deletion-gates)).
12. **#349** — later HA, not part of the single-server MVP.
