# W050 slice 6: secrets, ConfigFiles and SeedFiles on k3s

## Implementation review amendment — 2026-10-24

Before implementation, review secret continuity in #180. Specify the canonical source, how a newly elected leader obtains existing values and versions, and recovery of datastore encryption keys plus the off-host export bundle. A new leader must never generate a replacement for an existing secret because its local node store is empty. Add a secret-bearing workload and export/import test across leader change to #180. Rotation restart strategy follows the workload's singleton/replica safety; no universal zero-downtime claim.

This supersedes conflicting scope, ordering and acceptance language below. See [revised proof gates](../w050-migration-plan.md#implementation-review-and-proof-gates--2026-10-24). No runtime work is authorized by this amendment.


> Tracked in #162 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

Packages that need generated secrets, rendered config files or first-run seed files run on k3s without Keystone ever reading a secret value.

## User-visible outcome

`metrics-grafana` (ConfigFile, Secret, OidcClient, Volume) runs on the new engine with login working. Rotating a secret restarts it with a rolling restart.

## Scope

- `EnsureClusterSecret`: leader generates or imports the value from its node store and writes a Kubernetes Secret (encrypted at rest). Keystone gets existence + version only, as today (`keystone/reconcile/secret.go`).
- `secret/<name>` env → `secretKeyRef`.
- `EnsureClusterConfig`: ConfigFile → ConfigMap, or Secret when it contains `secret/` refs (materialized by the leader, `materialize.go` forms incl. `pbkdf2:`).
- Config digest → pod-template annotation (replaces `configdigest.go` spec-hash folding).
- SeedFile → init container with a new Wildstone seeder image (Go, static, no shell): O_EXCL write from a mounted ConfigMap into the PVC; existence observed via `ObserveWorkload`.
- Secret export/import bundle (`ExportSecretBundle`/`ImportSecretBundle`) works on k3s through the leader.
- Release: seeder image in `release.yml`.

## Non-goals

OidcClient into a k3s Authelia (slice 9); identity move.

## Dependencies

Slice 5. ADR-W012 amendment accepted.

## Acceptance criteria

- [ ] Unit: Keystone never receives a secret value (existing red-team tests extended to cluster ops).
- [ ] e2e: grafana login works; rotating its secret causes one rolling restart.
- [ ] e2e: SeedFile written once; a user edit survives a reconcile and a pod restart.
- [ ] Recovery: export bundle on k3s, import on a fresh install, secret versions match.
- [ ] Audit log never contains Secret data (`wardstone/internal/agent/audit.go` cases).

## Risks

Secret material now lives in the cluster datastore; limit RBAC to the leader and kubelet.

## Size

**L.** Three kinds, a new image and a security-sensitive amendment.

## ADR amendment

**ADR-W012** (where values live on k3s).
