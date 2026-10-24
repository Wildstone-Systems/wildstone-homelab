# W050 slice 10: Keystone as the `management` failover package

## Implementation review amendment — 2026-10-24

Add #180 safety-contract review as a prerequisite to implementation. Before offering household migration, prove actual Keystone failover and recovery on the early test topology, beyond the management fixture in #180. wild-recover must diagnose and perform documented bootstrap/restore when Keystone is stopped, corrupt or unschedulable; a proxy to its API is insufficient. Define behavior with cluster API/storage unavailable and exercise the off-host kit. Prove secret continuity, single-writer behavior under partition, and resumption without replaying stale approvals. Priority/Guaranteed QoS reduce eviction risk; they do not guarantee survival of host/PV disk exhaustion. Reserve capacity for OS, Kubernetes and storage as well as management.

This supersedes conflicting scope, ordering and acceptance language below. See [revised proof gates](../w050-migration-plan.md#implementation-review-and-proof-gates--2026-10-24). No runtime work is authorized by this amendment.


> Tracked in #166 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

Keystone (API, Console, Statefile, runtime DB) runs as a pod with one writer, guaranteed resources and top priority, bootstrapped by the installer and then managed through Plans.

## User-visible outcome

- Nothing visible normally. Under memory pressure, apps are evicted before management.
- `wild-recover` on a machine works when the website does not.

## Scope

- OCI image for Keystone in `release.yml`.
- `management` package: `replicas: 1`, `Recreate`, PVC `ReadWriteOncePod`, PriorityClass `wildstone-management`, requests = limits.
- `keystone-bootstrap.yml`: one-time local apply through Wardstone; Keystone adopts it as a package.
- `LoadBalancer` Service `:9440` on kube-vip for Wardstone; CA pinning unchanged.
- Migration from host Keystone: import Job copying `/var/lib/wildstone/keystone`; old dir kept read-only.
- Keystone-native packages (`knowledge-wildstone`, `workspace-wildstone`, `filedrop-wildstone`, `homepage-wildstone`) get their shared-folder PVCs.
- `wild-recover` host CLI through the local Wardstone (ADR-W033 A7).

## Non-goals

Keystone failover between nodes (slice 14).

## Dependencies

Slices 7 and 9; ADR-W051 accepted.

## Acceptance criteria

- [ ] e2e: fresh k3s install brings Keystone up as a pod; join token and CA fingerprint printed as today.
- [ ] e2e: host-Keystone → pod migration keeps Plans, owners, secrets versions.
- [ ] Fault (W051 gate 2): app memory hog → app evicted, Keystone stays.
- [ ] Only one Keystone pod can mount the volume (RWOP test).
- [ ] Exit drill restores management recovery set.

## Risks

Chicken-and-egg during bootstrap; keep the bootstrap path fixed and tested in CI.

## Size

**L.** New image, bootstrap path and data migration.

## ADR amendment

**ADR-W051 §4.1** additions (RWOP, bootstrap then adopt).
