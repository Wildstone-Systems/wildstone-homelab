# W050 slice 7: backup Jobs, hooks, restore and exit drill on k3s

> Draft issue body. Plan: §6.6.

## Goal

The Wildstone restic engine backs up and restores k3s packages using Kubernetes Jobs, including database hooks, and the exit drill proves total-loss recovery on k3s.

## User-visible outcome

- Backups and "Restore alongside" work the same for apps on the new engine.
- Release is blocked if the k3s exit drill fails, as for Docker.

## Scope

- `keystone/backup`: `JobRunner` implementing `Runner` (`backup.go:59`) via `RunJob`/`ObserveJob`: pod affinity to the app pod for RWO volumes, read-only source mounts, repository PVC or S3 target.
- Hooks (`hooks.go:34-102`): `pg_dump` Job to the Postgres Service; `sqlite` Job on the same node.
- Guard (`guard.go`): free space via Longhorn capacity.
- Restore alongside into `<plan>-restore` PVC; cutover Plan.
- Proof packages: `recipes-mealie` (sqlite hook) and a new fixture `testing/fixtures/pg-fixture` (Postgres hook).
- `scripts/exit-drill-k3s.sh` + a job in `.github/workflows/exit-drill.yml` running in the QEMU VM path; uses only Keystone's API.
- Off-site (ADR-W045) unchanged; verify with the S3-only drill in the VM.

## Non-goals

Longhorn snapshots as backups (never, W051 §4.8); NFS media backups (excluded as today).

## Dependencies

Slices 5 and 6.

## Acceptance criteria

- [ ] e2e: mealie backup → wipe → restore alongside → cutover → recipe present.
- [ ] e2e: Postgres fixture `pg_dump` restore matches row count.
- [ ] k3s exit drill green in CI: destroy cluster, reinstall, restore from Statefile + repository, bytes match.
- [ ] A missing NAS or repository fails only the backup Job (#132).

## Risks

Job placement next to RWO volumes on multi-node (re-proved in slice 14).

## Size

**L.** New runner, two hook paths, a new drill in CI.

## ADR amendment

**ADR-W018:** exit drill runs per backend during coexistence.
