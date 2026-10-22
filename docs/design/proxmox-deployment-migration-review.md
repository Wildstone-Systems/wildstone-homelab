# Proxmox deployment migration review

Status: proposed investigation; no production migration or removal authorized.
Date: 2026-10-22. Baseline: `60df9dcbc1daa99d43f7692eb83f7d72f7070956`.
Companions: [ADR-W050](../decisions/ADR-W050-proxmox-first-deployment-and-recovery.md), [contract](application-management-contract-v0.1.md), [package comparison](application-management-contract-v0.1-comparison.md).

## Review queue

This is a decomposition seed, not an implementation-ready tracker or a schedule. Maintainer acceptance of the ADR and closure of the relevant investigation gates precede implementation issues.

| Work area | Initial disposition | Evidence needed before change |
| --- | --- | --- |
| Keystone, API, Console, Statefile, runtime DB | Keep together; evaluate dedicated management guest | Bootstrap independent of household login; bounded footprint; recover state and quarantine |
| Wildstone household web surface | Evaluate co-location with management | Identify actual owning process/package and identity, file and calendar dependencies |
| Identity and ingress | Keep behavior; placement undecided | Cross-guest configuration/secret scope, direct emergency access and startup dependencies |
| Wardstone / Docker execution | Keep typed execution model initially | Dedicated workload nodes remain compatible; LXC support requires explicit tests, native LXC requires a new provider |
| Immich and database/cache | Candidate coherent recovery group | Data plus database consistency, versions, capacity, application readiness and restored login |
| Jellyfin and other bulk media | Candidate separate workload group | Storage mount identity, permissions, GPU access if needed, explicit library backup exclusions |
| Home Assistant and radio controllers | Audit before relocation | LAN discovery, USB/D-Bus access, MQTT secrets and same-node constraints; controller restore to matching hardware |
| Monitoring and alert delivery | Add independently placed observation | Prove host-loss detection and alert receipt when this installation is unreachable |
| Butler / external Agents | Preserve authority boundaries | Management recovery quarantine, connection locality, one active coordinator; no new permissions |
| Guest provisioning | Investigate provider and tool ownership | Proxmox scoped credentials, locking, exact Plans, partial failure, adoption and rollback |
| Existing single-host install | Preserve as baseline during investigation | Existing users remain documented; no silent topology conversion |
| Feature retirement | None approved | Explicit dependency review and maintainer decision per feature |

## Ordered investigation slices

1. **Inventory and dependency map.** Identify process, guest, host, storage and power domains separately. Record external prerequisites and who owns every configuration and credential.
2. **Management placement experiment.** Compare a VM and unprivileged LXC; measure footprint and boot behavior. Test access with identity/ingress unavailable. Do not install the workload runtime on the hypervisor to avoid the guest decision.
3. **Application group experiment.** Use a disposable group, then validate a database-backed group. Demonstrate independent resource limits, readiness, approved restart and alongside restore.
4. **Recovery and monitoring experiment.** Exercise guest failure, storage outage, disk/memory pressure, host restart and management loss; record recovery times, lost-data window and external alert delivery.
5. **Decision and decomposition.** Amend the ADR with evidence, define provider contracts and create bounded implementation issues. Only then propose production migration, rollback and cutover criteria.

## Evidence record for each experiment

Record Wildstone SHA, Proxmox/guest/runtime versions, sanitized topology, prerequisite configuration, fault injected, expected versus observed behavior, timestamped readiness and alert evidence, recovery duration, backup age, data verification, and cleanup/rollback result. A running guest, a successful backup job or a green repository test is not enough to mark the application recovered.

## Review stop conditions

Stop migration planning if recovery needs unavailable keys, state ownership is ambiguous, a package assumes same-node secrets/endpoints, external storage is missing from backup coverage, or a second coordinator can execute. Convert each blocker into an investigation item; do not bypass it with privileged containers, arbitrary shell hooks or silent feature removal.
