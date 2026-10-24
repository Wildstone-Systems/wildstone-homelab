# Failure-domain and recovery review

Status: proposed investigation; no production migration or removal authorized.
Date: 2026-10-22; retargeted 2026-10-24 at ADR-W050's runtime (this was the "Proxmox deployment migration review").
Companions: [ADR-W051](../decisions/ADR-W051-failure-domains-and-recovery.md), [ADR-W050](../decisions/ADR-W050-multi-node-runtime-kubernetes-engine-host-providers-editions.md), [contract](application-management-contract-v0.1.md), [package comparison](application-management-contract-v0.1-comparison.md).

## Review queue

This is a decomposition seed, not an implementation-ready tracker or a schedule. Maintainer acceptance of ADR-W051 and closure of the relevant investigation gates precede implementation issues. Runtime work itself is scheduled through ADR-W050's issues (#151, then #129).

| Work area | Initial disposition | Evidence needed before change |
| --- | --- | --- |
| Keystone, API, Console, Statefile, runtime DB | Keep together as one priority `failover` package | Bootstrap independent of household login; bounded footprint; one writer; recover state and quarantine |
| Wildstone household web surface | Evaluate co-location with management | Identify actual owning process/package and identity, file and calendar dependencies |
| Identity and ingress | Keep behavior; placement by Kubernetes | Secret scope across nodes, direct emergency access and readiness dependencies |
| Wardstone execution | Docker backend now; k3s backend per ADR-W050 | Runtime seam (#151) keeps typed operations; no new privilege |
| Immich and database/cache | One package = one recovery group | Data plus database consistency, versions, capacity, application readiness and restored login |
| Jellyfin and other bulk media | Separate package; media on NAS via NFS | Storage mount identity, permissions, iGPU access, explicit library backup exclusions |
| Home Assistant and radio controllers | `pinned` packages | LAN discovery, USB/D-Bus access, MQTT secrets and same-node constraints; controller restore to matching hardware |
| Monitoring and alert delivery | Add an observer outside the failure domain | Prove host-loss detection and alert receipt when this installation is unreachable |
| Butler / external Agents | Preserve authority boundaries | Management recovery quarantine, connection locality, one active coordinator; no new permissions |
| Proxmox, if used | Hosts k3s node VMs only | VM autostart order; no Proxmox HA for workloads; no Butler credentials |
| Existing single-host install | Preserve as baseline during investigation | Existing users remain documented; no silent topology conversion |
| Feature retirement | None approved | Explicit dependency review and maintainer decision per feature |

## Ordered investigation slices

1. **Inventory and dependency map.** Identify pod, package, node, host, storage and power domains separately. Record external prerequisites and who owns every configuration and credential.
2. **Management isolation.** On single-node k3s (#129), give management guaranteed resources and top priority; measure its footprint; test access with identity/ingress unavailable.
3. **Recovery-group experiment.** Use a disposable package, then a database-backed one. Demonstrate resource limits, readiness, approved restart and alongside restore.
4. **Fault and power experiment.** Exercise app failure, storage outage, disk/memory pressure, power loss and (multi-node) management-node loss; record recovery times, lost-data window and external alert delivery.
5. **Decision and decomposition.** Amend ADR-W051 with evidence and create bounded implementation issues. Only then propose production migration, rollback and cutover criteria.

## Evidence record for each experiment

Record Wildstone SHA, OS/k3s/runtime versions (and Proxmox, if used), sanitized topology, prerequisite configuration, fault injected, expected versus observed behavior, timestamped readiness and alert evidence, recovery duration, backup age, data verification, and cleanup/rollback result. A running pod, a successful backup job or a green repository test is not enough to mark the application recovered.

## Review stop conditions

Stop migration planning if recovery needs unavailable keys, state ownership is ambiguous, a package assumes same-node secrets/endpoints, external storage is missing from backup coverage, or a second coordinator can execute. Convert each blocker into an investigation item; do not bypass it with privileged containers, arbitrary shell hooks or silent feature removal.
