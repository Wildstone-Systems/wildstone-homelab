# W050 slice 4: first stateless package on k3s (`tools-it-tools`)

> Draft issue body. Plan: §4, §6.8 "coexistence trick".

## Goal

One package runs on k3s end to end: proposed as a Plan, approved, applied by the leader, served through the existing (Docker) Caddy with Authelia login, and removed cleanly.

## User-visible outcome

- On a k3s-enabled install, `tools-it-tools` can be installed "on the new engine". It looks and logs in exactly like the Docker version.

## Scope

- Leader: `ApplyWorkload`, `DeleteWorkload`, `ObserveWorkload`, `WorkloadLogs` (generated proto from slice 2's design).
- k3s translator in Keystone: Container → Deployment + Service; Route upstream `cluster/<svc>:<port>` → kube-vip address + per-app `LoadBalancer` port for the Docker Caddy.
- Per-package backend choice: `backend: k3s` recorded in the Statefile (default `docker`).
- CoreDNS stub zone for the tailnet domain (prove MagicDNS resolution from a pod).
- API: `GET /api/v1/apps/{package}` status and logs; Console shows the same status card.

## Non-goals

Volumes, secrets, ConfigFiles, backups; moving Caddy.

## Dependencies

Slice 3.

## Acceptance criteria

- [ ] e2e (QEMU, k3s): install → Plan → approve → page reachable through Caddy → Authelia login → remove → no objects left with `managed-by: wildstone`.
- [ ] Drift: editing the Deployment by hand raises a drift Plan; it is not silently reverted.
- [ ] Leader restart mid-apply converges on the next tick.
- [ ] Golden test: `tools-it-tools` translation snapshot.
- [ ] Docker-backend e2e unchanged.

## Risks

- `LoadBalancer` per app port may not scale; acceptable for coexistence only (removed in slice 9).
- MagicDNS from pods unproven.

## Size

**M.** First real use of the leader path, but the package is trivial.

## ADR amendment

No.
