# W050 slice 10: Management bootstrap, failover and independent recovery

> Tracked in #166. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Run Keystone as a singleton management package and prove recovery when Keystone cannot start.

## Scope

- Ship Keystone OCI image and management package: one replica, Recreate, RWOP, PriorityClass and CPU/memory requests=limits.
- Fixed local bootstrap applies once, then Keystone adopts approved state; migrate host data through import Job retaining old directory read-only.
- Wardstone reaches management Service on kube-vip with CA pinning; Keystone-native shared folders use PVCs.
- wild-recover supports diagnosis and fixed bootstrap/restore without a running Keystone API. Define off-host recovery when cluster API/storage is unavailable.
- Prove actual Keystone failover, old-node return, partition single-writer safety, secret/revocation continuity and stale-approval protection on early topology.
- Reserve OS/Kubernetes/storage capacity and test pressure. Priority/QoS reduce risk, not a guarantee against node or PV disk exhaustion.

## Dependencies

#163 and #165; ADR-W051 accepted; #180 authority/secret/recovery contracts reviewed before implementation. #180 fixture proof precedes household migration; actual Keystone evidence here is required before #169.

## Acceptance criteria

- [ ] Fresh bootstrap and host-to-pod migration preserve Plans, owners, CA, secrets and versions.
- [ ] Power loss and network partition of actual management node produce no concurrent writers; recovery preserves revocations and rejects stale approvals.
- [ ] Stopped, corrupt and unschedulable Keystone recover via documented host/off-host procedure, with no dependency on household SSO/ingress.
- [ ] Memory/disk-pressure tests record reachability, limits and failure modes rather than claiming unconditional survival.
- [ ] Exit drill restores complete management recovery set; owner accepts evidence before household migration.

## Non-goals

Full Console multi-node onboarding and rolling-update productization (#170); automatic destructive reset.

## Size and decomposition

L/XL tracker; split image/bootstrap, import, host recovery and actual failover tests.
