# ADR-W051: Failure domains and recovery

- **Status:** proposed; requires maintainer acceptance. No migration, runtime change or new remediation authority is authorized.
- **Date:** 2026-10-22; rewritten 2026-10-24 to sit under ADR-W050.
- **Governed by:** [ADR-W050](ADR-W050-multi-node-runtime-kubernetes-engine-host-providers-editions.md) (accepted): standard Kubernetes behind Wardstone, Keystone as the only writer, Proxmox as an optional machine layer. This ADR adds failure-domain and recovery requirements on top of that runtime; where they differ, ADR-W050 governs.
- **History:** first proposed as ADR-W050 "Proxmox-first deployment and recovery architecture". Renumbered and rewritten after the multi-node runtime took that number; §6 lists what the earlier draft proposed and what replaced it.
- **Related:** ADR-W003, W004, W005, W014, W018, W033, W045 and W049; [management contract](../design/application-management-contract-v0.1.md), [glossary](../glossary.md), [failure-domain review](../design/failure-domain-recovery-review.md).

## 1. Observed behavior and motivation

The operator reports that Wildstone and managed applications currently share a single VM. When that VM fails, applications, management access and much of the monitoring disappear together. After a power outage the VM returned, but essential access services did not recover correctly. Storage exhaustion and memory pressure are additional failure scenarios the design must address. These are operator observations and requirements, not failures reproduced in this review. Loss of access does not establish loss of stored data.

The household starts with one computer and a guided install, and may add machines later. A cluster must not be required for a useful first installation. That shape is already ADR-W050's (k3s on every size of install). This ADR states what that runtime must guarantee when something fails.

## 2. Current implementation

The installer installs Linux systemd services and Wardstone executes scoped Docker and managed-file operations. ADR-W050's k3s backend does not exist yet; its first proof is #129. Multiple enrolled workload nodes are representable today, but that does not make every package portable across nodes: secrets, loopback endpoints, identity configuration and hardware access have locality constraints (ADR-W050's `pinned` availability).

`testing/e2e/proxmox.go` is a test harness, not a supported infrastructure provider.

## 3. Failure domains

| Domain | What fails together | Protection under ADR-W050 |
|---|---|---|
| Process / pod | One app component | Kubernetes restart with backoff; readiness gates traffic |
| Package (recovery group) | An app and its database/cache | Upgraded, backed up and restored as one unit (§4.2) |
| Management | Keystone, API, Console, Statefile, runtime DB | Its own `failover` package with reserved resources and top priority (§4.1) |
| Node | Everything on one machine (or one Proxmox VM) | Failover to another node at 2+ nodes; nothing at 1 node |
| Host hardware, power, storage | Every VM on that machine; every volume on that disk or NAS | More machines, replicated storage, off-host backups; never claimed on one machine |
| Site | The whole house | Off-site backups and recovery kit only (ADR-W045) |

Several Proxmox VMs on one machine are still one hardware, power and storage domain. Wildstone must not describe them as high availability.

## 4. Decisions

### 4.1 Management is its own failure domain

- Keystone, its API and Console, the Statefile and the runtime DB run together as one `failover` package. They don't share a package with household apps.
- It gets **guaranteed resources** (requests equal limits) and the **highest workload PriorityClass**, so memory or disk pressure evicts household apps before management.
- At 2+ nodes it prefers a node apart from the heaviest apps (soft anti-affinity). At 1 node the protection is resource isolation and priority, not hardware separation.
- **Exactly one Keystone writes.** One replica, a `Recreate` update strategy and a `ReadWriteOncePod` volume. Two coordinators are never promoted from copied or replicated state.
- The Wildstone household web surface is evaluated for co-location with management. Do not assume every service named Homepage belongs there.

### 4.2 A package is the recovery group

- A recovery group is the set of components and data that must be upgraded, backed up and restored together, for example Immich with its PostgreSQL and Redis. In Wildstone that unit is the **package**, not a pod and not a guest.
- Backups capture a package's consistency group together, using the package's declared hooks (#40). Restore is alongside by default, then a reviewed cutover.
- Splitting a package into separate failure domains (for example moving a database to its own node) is a package design decision, made only with evidence of an independent limit, restart or restore benefit.

### 4.3 Apps keep serving without management

- Kubernetes keeps running workloads when Keystone is down. Losing Keystone stops changes, not service.
- Each package documents which of its operations still need identity, ingress, DNS or storage when management is unavailable.
- **Break-glass access does not depend on household SSO or ingress:** the host CLI (ADR-W033 A7) on a node, and the owner-only terminal (#127) once it exists.

### 4.4 Restart recovery is observable and ordered by readiness

The required chain after power returns:

```
hardware powers on → OS boots (→ Proxmox and its VMs, if used)
→ k3s ready → storage usable (Longhorn volumes attached, NAS reachable)
→ Keystone ready → identity and ingress ready → household apps ready
```

- **Readiness, not delays, orders the chain.** A package that depends on another waits on that dependency's readiness with a declared deadline, then reports "dependency unavailable". It never just sleeps and starts.
- Status distinguishes node ready, pod running, application ready, dependency unavailable and unknown/stale.
- **Hardware power restoration** ("restore on AC power loss" in BIOS/UEFI, or out-of-band management) is a documented prerequisite, not a promise. Wake-on-LAN needs supported hardware and another powered sender; a machine cannot wake itself.
- **On Proxmox (optional):** k3s node VMs autostart, server nodes first. Proxmox autostart is the only start ordering Proxmox owns.

### 4.5 Remediation is bounded

- Kubernetes restart backoff is the only automatic restart. This ADR grants Wildstone no new automatic remediation.
- **Corrupt data must not cause a restart loop.** A package's startup check that detects corruption reports "recovery required" and waits, instead of exiting into another restart. Wildstone shows that state and offers restore as a Plan. Restart, repair, restore and data deletion are separate operations, and the last three are Plans.
- Restore goes to a separate volume first, is validated against the application and version, and only then cuts over. There is never an automatic destructive reset.
- Diagnostic evidence (logs, last probe results, events) is preserved across restarts so the Butler and the Owner can see why something failed.

### 4.6 Resource exhaustion is contained

- Every package declares resource requests and limits (#41), including ephemeral storage, so one app filling memory or disk is evicted before it takes down its neighbours.
- Node disk, Longhorn capacity and NAS free space are observed and alerted (#131, #134, #136).
- A missing mount fails only the workload or backup Job that needs it and never writes locally in its place (#132).

### 4.7 Host loss is observed from outside

- Complete host loss is detected by an observer **outside that failure domain**: another physical node on a different power path, or an off-site uptime check. Uptime Kuma on the same host cannot report that host's outage.
- Alert delivery must not depend entirely on the failed system's SSO, DNS or notification service.

### 4.8 Backups survive the failed system

- Back up each package's consistency group and the complete management recovery set (Statefile, runtime DB, sealed secrets), as the recovery contract and ADR-W045 require.
- Keep off-host instructions, recovery keys and enrollment configuration. Rebuilding the CA and node identities and rejoining nodes follow the existing recovery contract.
- **Storage snapshots are not app backups.** Longhorn snapshots and Proxmox VM backups are crash-consistent at best and miss external mounts and passthrough devices. They may speed up a rollback; they never replace the restic engine or the exit drill.

### 4.9 Proxmox, if used

Per ADR-W050, Proxmox hosts k3s nodes as VMs and never moves Wildstone apps:

- No Proxmox HA for Wildstone workloads. Kubernetes owns placement.
- No Docker or k3s directly on the Proxmox host by default. The hypervisor keeps a small administrative surface.
- No Proxmox credentials for the Butler, and no new broad-privilege agent.

## 5. Acceptance gates

These run on ADR-W050's proof environment (#129) and, later, its multi-node test cluster:

1. Inventory packages and management bootstrap dependencies; record keep, move, defer or retire with rationale. No feature removal by assumption.
2. On single-node k3s, inject app failure, full disk, memory pressure and an unavailable NAS. Verify management stays reachable, diagnostics remain and restarts stop where §4.5 says.
3. Cut power and restore it. Verify the §4.4 chain completes with readiness deadlines and external alert evidence.
4. Restore one database-backed package alongside, validate it, and cut over.
5. Lose the management node (multi-node only). Confirm one active Keystone, preserved revocations and recovery through the off-host kit.
6. Define measured recovery-time and recovery-point objectives from these runs before claiming any.

## 6. What the earlier draft proposed, and what replaced it

| Earlier draft (as ADR-W050) | Now |
|---|---|
| Proxmox-first starting experience | Plain Linux first, Talos appliance later; Proxmox optional (ADR-W050 §2.6) |
| One Proxmox guest per app group, visible in Proxmox | One package per recovery group, placed by Kubernetes (§4.2) |
| Management VM vs LXC, left open | Management is a priority `failover` package (§4.1); LXC is not a default node type |
| OpenTofu or Ansible for provisioning, undecided | Signed local Ansible playbooks for the Linux edition (ADR-W050 §2.7); OpenTofu deferred |
| Kubernetes listed as out of scope | Kubernetes is the runtime (ADR-W050) |
| Start order and guest autostart | Readiness-ordered chain; Proxmox autostart only for node VMs (§4.4) |

Kept unchanged: the observed failures, separating management from apps, recovery groups, readiness over delays, bounded remediation, no destructive reset, external observation and backup independence.

## 7. Consequences and open decisions

- Management reserves resources even when idle, which costs some RAM on small machines.
- A one-machine install still shares host, power and storage. This ADR makes failures contained and recoverable there, not survivable.
- Open: PriorityClass values, the external observer (second node or off-site check), how packages declare a corruption signal, and numeric recovery targets.
- No automatic HA, native LXC support or universal application adapter is claimed to exist.

## Amendments

### A1. Implementation review clarification (2026-10-24, #181)

Status remains proposed. Follow the [W050 revised proof gates](../design/w050-migration-plan.md#implementation-review-and-proof-gates--2026-10-24), #179 and #180. Priority and Guaranteed QoS reduce eviction risk but do not guarantee management survives disk/PV exhaustion. RWOP/Recreate do not alone prove partition-safe failover. Host recovery must work when Keystone cannot start and document offline rebuild/restore when cluster API/storage is unavailable; a proxy to Keystone is insufficient. Node-count banners must account for actual quorum, storage replica health, independent failure domains and eligible capacity. Prove the actual management recovery set before household migration is offered.

### A2. Recovery path on a cluster (2026-10-25, #180)

The [multi-node safety contract](../design/w050-multi-node-safety-contract.md) §5–§6, accepted by the owner, sets the recovery path this ADR requires. The status of this ADR is unchanged (proposed).

- **`wild-recover`** is a host binary on every server node. It runs diagnostics, applies the fixed `management` workload and restores Keystone's data with Keystone stopped, corrupt or unschedulable.
- **An off-host kit** holds the Statefile bundle, runtime DB backup, sealed secret bundle, k3s encryption key, an etcd snapshot and rebuild steps. It is used when the cluster API or storage is gone. A proxy to Keystone is not a recovery path.
- **One writer per volume,** with no forced detach by Wildstone or by the k3s timer. A stateful app moves only after the owner confirms the lost machine is off.
- **Pressure is tested:** disk space is reserved for the OS, Kubernetes and storage, and #180 fills a disk to show management survives.
