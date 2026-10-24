# ADR-W051: Proxmox-first deployment and recovery architecture

- **Status:** proposed; requires maintainer acceptance. No migration or runtime change is authorized.
- **Date:** 2026-10-22
- **Renumbered:** from ADR-W050 on 2026-10-24, after the accepted [ADR-W050](ADR-W050-multi-node-runtime-kubernetes-engine-host-providers-editions.md) (multi-node runtime) took that number. Where the two differ on runtime, placement or Proxmox's role, ADR-W050 governs; this proposal must be reconciled with it before acceptance.
- **Baseline:** `60df9dcbc1daa99d43f7692eb83f7d72f7070956` in `Wildstone-Systems/wildstone-homelab`.
- **Related:** ADR-W003, W004, W005, W014, W018, W033, W045 and W049; [management contract](../design/application-management-contract-v0.1.md), [glossary](../glossary.md), [migration review](../design/proxmox-deployment-migration-review.md).

## Observed behavior and motivation

The operator reports that Wildstone and managed applications currently share a single VM. When that VM fails, applications, management access and much of the monitoring disappear together. After a power outage the VM returned, but essential access services did not recover correctly. Storage exhaustion and memory pressure are additional failure scenarios the design must address. These are operator observations and requirements, not failures reproduced in this review. Loss of access does not establish loss of stored data.

The desired starting experience is one computer running Proxmox, followed by a guided Wildstone installation. The operator should see and control meaningful application groups individually in Proxmox, without having to administer every process inside one large VM. More computers may be added later. A cluster must not be required to obtain a useful first installation.

## Current implementation

The installer installs Linux systemd services; it does not provision Proxmox guests. Keystone expands packages into typed desired resources and Wardstone executes scoped Docker and managed-file operations. Multiple enrolled workload nodes are representable today, but that does not establish that every package is portable across nodes: secrets, loopback endpoints, identity configuration and hardware access have locality constraints.

Proxmox VM helper code in `testing/e2e/proxmox.go` is a test harness, not a supported infrastructure provider. No production Proxmox guest-management contract is introduced by this ADR. Existing accepted decisions remain authoritative until this proposal is accepted and specific amendments are reviewed.

## Proposed direction

### 1. Separate management from application failure domains

Keep Keystone, its API and Console, Statefile and runtime DB together in a small management guest. Evaluate the Wildstone-owned household web surface with that group; do not assume that every service named Homepage belongs there. Inventory identity, ingress, calendar and other dependencies explicitly. Large application data and unrelated workloads should not share the management guest merely for installation convenience.

An application recovery group contains components and data that need coherent upgrades and restores, for example Immich plus its dedicated database and cache. A recovery group is not necessarily one container or one guest. Begin with the few separations that provide independent limits, restart, diagnosis and restore; do not mandate one guest per process.

The running applications should not require a healthy Keystone merely to continue serving. Document which operations still require identity, ingress, DNS or storage. Management itself must retain a host-access recovery path when household SSO or ingress is unavailable.

### 2. Keep the hypervisor boundary explicit

Evaluate a dedicated VM and a dedicated unprivileged LXC for management; leave the selection open until tested. Evaluate application placement separately: native LXC, Docker in a VM, or Docker in an LXC have different execution and isolation requirements. Existing Wardstone Docker operations do not become native LXC operations by changing placement.

Do not use Docker installed directly on the Proxmox hypervisor as the default. Proxmox-managed LXC guests and Docker application containers on the hypervisor are different architectures. The hypervisor should retain a small, deliberate administrative surface. No arbitrary host shell or new broad-privilege agent is authorized by this proposal.

Upstream references consulted on 2026-10-22: [Proxmox container documentation](https://github.com/proxmox/pve-docs/blob/master/pct.adoc) and [FAQ](https://github.com/proxmox/pve-docs/blob/master/pve-faq.adoc). Their isolation, container nesting and migration tradeoffs inform the evaluation; installed-version compatibility must be tested rather than inferred from these moving documents.

### 3. Separate provisioning, execution and application behavior

Infrastructure providers own scoped guest provisioning, placement, limits, startup settings and observation. Application adapters implement supported application semantics under the [management contract](../design/application-management-contract-v0.1.md). Existing packages, traits, Plans, policy and APIs remain the composition boundary; do not create a parallel manifest or approval route.

OpenTofu or Ansible may be evaluated for provisioning and host preparation. Their inclusion is not decided here. If adopted, define one owner for each resource, credential scope, state locking, import/adoption, idempotency, partial failure, rollback and approval of an exact change. Proxmox guest operations must not silently compete with another controller or broaden Wardstone's closed operation surface.

### 4. Make restart recovery observable and bounded

The required chain is: power returns; hardware starts; Proxmox boots; required storage/network become usable; management and workload guests start; application readiness is verified. Start order and delays alone are not readiness checks.

Hardware power restoration is a BIOS/UEFI or out-of-band management concern. Wake-on-LAN requires supported, configured hardware and another powered sender; a container cannot wake its own powered-off host. Record hardware prerequisites without promising them on all machines.

Distinguish guest running, process running, application ready, dependency unavailable and unknown/stale observations. Preserve diagnostic evidence and provide bounded retry budgets, backoff and escalation. Existing Docker restart policies remain separate from any new Wildstone-initiated remediation authority. This proposal grants none.

Database corruption must stop blind restart loops. Restart, repair, restore and data deletion are distinct operations. Preserve the original data; offer a reviewed restore to a separate location, application/version validation and an explicit cutover. No automatic destructive reset.

### 5. Keep recovery independent of the failed system

Observe host loss from another physical or administrative failure domain. Uptime Kuma on the same host cannot report that host's complete outage without an external observer. Alert delivery and emergency access must not depend entirely on the failed service's SSO, DNS or notification instance.

Back up application consistency groups and the complete management recovery set. Preserve off-host instructions, recovery keys, enrollment configuration and bindings required by the existing recovery contract, plus host prerequisites. Rebuild the CA and node identities and rejoin nodes as that contract requires. VM/LXC backup coverage of bind mounts, external storage and passthrough must be verified explicitly; a guest backup is not assumed to contain those bytes.

One computer still has shared host, power and storage failure domains. Additional Proxmox nodes may improve availability only with explicit storage, quorum, fencing, network and capacity design. Do not run multiple active Keystone coordinators against copied state. Future VPS participation is an independent-provider/networking investigation, not a promise of a stretched Proxmox cluster.

## Alternatives and tradeoffs

| Alternative | Benefit | Cost / reason to evaluate |
| --- | --- | --- |
| Keep one combined VM | Smallest operational footprint; existing execution model | Shared management/application guest failure and resource pressure; remains a valid baseline |
| Dedicated management VM and workload VMs | Stronger guest boundary; preserves existing Docker execution | More memory and guest maintenance; Proxmox sees recovery groups, not each inner container |
| Dedicated management LXC and selected workload LXCs | Low overhead; individual Proxmox status, limits and backups | Shared host kernel; nesting, permissions, mounts and device access require validation |
| Docker directly on hypervisor | Fewer guest layers | Couples application runtime and privileges to virtualization host; rejected as default in this proposal |
| Kubernetes-based replacement | Standard workload orchestration | Adds a different platform and migration burden; outside this proposal |

## Acceptance gates before implementation readiness

1. Inventory current packages and all management bootstrap dependencies; record keep, move, defer or retire with rationale. No feature removal by assumption.
2. Compare VM/LXC candidates on one disposable Proxmox host: install/resume, updates, resource limits, host reboot and access when SSO is down. Define minimum tested hardware from measurements.
3. Move one representative application group in the test environment; prove secret placement, cross-guest reachability, storage availability, hardware constraints where applicable, and coherent backup/restore.
4. Inject application failure, guest failure, unavailable NAS, full guest disk and memory pressure. Verify management access remains usable where its failure domain survives, diagnostics remain available, and retries stop.
5. Test power/start recovery with explicit readiness deadlines and external alert evidence. Define measured recovery-time and recovery-point objectives before claiming them.
6. Test management guest loss with existing quarantine/fencing and an off-host kit. Confirm one active coordinator and preserved revocations.
7. Review provider permissions and version compatibility; decompose implementation issues with acceptance criteria. Obtain a separate cutover decision after evidence and rollback procedures exist.

## Consequences and open decisions

This prioritizes simple adoption, independent management and tested recovery over removing a VM layer. It adds guest maintenance and may consume more RAM. Management VM versus LXC, recovery-group boundaries, identity/ingress placement, provider/tool choice, monitoring location and numerical recovery targets remain open. No automatic HA, cluster orchestration, native LXC support or universal application adapter is claimed to exist.
