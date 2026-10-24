# W050 slice 9: ingress and identity on k3s, per-hostname cutover

> Tracked in #165 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

Caddy, Authelia and LLDAP run on k3s. The kube-vip address owns `:80`/`:443`. Hostnames move from Docker Caddy to k3s Caddy one at a time; identity moves exactly once.

## User-visible outcome

- Same login page, same addresses. A short "switching" window per hostname, stated in the Plan.

## Scope

- Caddy package on k3s: Deployment (`failover`, one copy), `LoadBalancer` Service on kube-vip, routes ConfigMap + Secret, certificate PVC; reload strategy proven (`--watch` vs admin-API reload).
- Ingress profiles: `ingress`, `ingress-tailnet`, `ingress-tailnet-public`; `ingress-tailnet-ts` as `pinned` (tailscaled socket host path, admission exemption).
- Identity: Authelia + Redis + LLDAP + provisioning Jobs; writable `/srv/wildstone/identity` becomes a PVC; `install.sh` password steps (`:1156-1347`) become a typed Keystone call that runs a Job.
- Forward-auth trust: pod CIDR + NetworkPolicy instead of `ingressaddr.go`.
- NetworkPolicy default-deny in `wildstone-apps` (#42).
- Cutover tool: one Plan per hostname (Route + DNS record), plus a single identity Plan (export → import → repoint → retire).
- Remove `cluster/` per-app `LoadBalancer` ports used during coexistence.

## Non-goals

Several Caddy copies (O5); Keystone in the cluster.

## Dependencies

Slices 6 and 7.

## Acceptance criteria

- [ ] e2e: Docker install → move identity → move each hostname → all OIDC apps log in → Docker Caddy and identity gone.
- [ ] Rollback per hostname tested.
- [ ] Two Caddys never hold the same hostname (test asserts it).
- [ ] Break-glass works with Caddy stopped.
- [ ] Exit drill (both backends) green.

## Risks

Highest-risk cutover in the plan: a household can lose every login. Run only after backups proven (slice 7).

## Size

**XL.** Four ingress profiles, identity, NetworkPolicy and two cutover flows.

## ADR amendment

No.
