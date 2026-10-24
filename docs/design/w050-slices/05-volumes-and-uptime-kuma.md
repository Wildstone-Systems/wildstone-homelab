# W050 slice 5: volumes on Longhorn + Uptime Kuma + rolling update

> Tracked in #161 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

A package with persistent data (`monitoring`, Uptime Kuma, the #129 proof app) runs on k3s, keeps data across pod restart and machine reboot, and updates its image with a health-gated rollout and rollback.

## User-visible outcome

- Uptime Kuma on the new engine keeps its monitors after a reboot.
- An image update Plan says "short restart (~N s)" and offers rollback if the new version is unhealthy.

## Scope

- `Volume` → PVC on Longhorn (1 copy); `failover` → `Recreate`.
- Readiness/startup probe from #39 (if not landed, a TCP probe on the Route port as an interim).
- Rollout status as the health gate; rollback = previous ReplicaSet via a Plan.
- `DiskSpace` for Longhorn volumes.
- `availability` read by the translator for the first time.

## Non-goals

Backups (slice 7); shared folders.

## Dependencies

Slice 4.

## Acceptance criteria

- [ ] e2e: install, add a monitor, reboot VM, monitor still there.
- [ ] e2e: image update to a known-bad digest stops, old pod keeps serving, rollback Plan restores.
- [ ] Removing the package keeps the PVC until a separate High-risk Plan deletes it (same as Docker volumes today).

## Risks

Longhorn on a single small disk; iSCSI prerequisites.

## Size

**M.** One kind plus probes and rollout gating.

## ADR amendment

No.
