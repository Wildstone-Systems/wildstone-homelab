# ADR-W050: Multi-node runtime — a Kubernetes engine, host providers and two editions

- **Status:** proposed; design direction, not yet implemented. Post-alpha.
- **Relates to:** ADR-W002 (Proxmox test isolation), ADR-W003 (Wardstone
  protocol, no `Exec(string)`), ADR-W004/W008 (resources, Plans,
  reconcile), ADR-W005 (package format), ADR-W014 (installer bootstrap),
  ADR-W045 (off-site backup), #129 (Proxmox LXC evaluation), #128 (IaC
  audit), `docs/audit/findings/566-iac-architecture.md`.
- **Keeps:** every ground rule in the README — no secrets in the repo, no
  `Exec(string)` on the wire, one public API for Console/CLI/Butler, and
  every intentional change is a Plan.

## 1. Context

Today every resource names its node (`metadata.node`), and Wardstone
applies typed Docker Engine operations on that one machine. That is
correct for a single host. It cannot:

- **place** an app on a suitable node by itself;
- **fail over** an app when its node dies;
- **scale out** an app that supports several copies;
- **roll** OS and app updates across machines while the house stays up.

The target household is 1 to N small machines (for example four mini PCs)
with a few people using services like Jellyfin at the same time. The
product goal is that this stays as easy as a single box: the household
sees "Add a machine" and "Keep this running if a machine fails: on/off",
never a scheduler.

Options considered:

| Option | Why not (as the core) |
|---|---|
| Build placement, failover and quorum in Keystone | Fencing, quorum and storage replication are where data gets corrupted. Not worth owning. |
| Proxmox HA with one CT per app (#129) | Failover only; no copies or load balancing. Requires Proxmox, so a plain Linux box gets a different model. |
| Podman Quadlets | Excellent single-host runtime; no multi-node at all. |
| Docker Swarm | Little active development; small ecosystem. |
| Nomad | Not open-source licensed. |
| Kubernetes (k3s) | Chosen: one model from 1 to N, with placement, failover, copies, health checks and rolling updates built in. |

[khuedoan/homelab](https://github.com/khuedoan/homelab) (GPL-3.0) is a
useful reference for a k3s homelab on the same class of hardware. It is a
blueprint for the engine, not a base to fork: it applies whatever is
merged to Git (ArgoCD), runs Ansible over SSH as root and leaves backup as
a TODO, which conflict with Plans, the no-shell rule and the recovery
contract.

## 2. Decision

### 2.1 Wildstone targets standard Kubernetes, on every size of install

- The runtime is **conformant Kubernetes**, including a single machine,
  so adding a node is a join, not a migration.
- The distribution is an edition detail (§2.6): **k3s** on Linux,
  Talos's upstream Kubernetes on the appliance.
- Households, Owners and the Butler never see Kubernetes: no `kubectl`,
  no kubeconfig, no shell into an app.

### 2.2 Layers and owners

| Layer | Contents | Owner |
|---|---|---|
| Machines | Bare metal, or Proxmox VMs (optional) | Household |
| Host | `LinuxHost` (Ansible + systemd) or `TalosHost` (Talos API) | Wardstone on each node |
| Engine | Kubernetes + add-ons (§2.4) | Wardstone leader |
| Control | Desired state, Plans, policy, placement | Keystone |
| Product | Packages, Caddy, Authelia/LLDAP, backups, Console, Butler | Keystone via packages |

- **Keystone is the only writer.** It turns approved resources into
  desired state; there is no ArgoCD or Flux beside it.
- **One Wardstone per node** does host work: disks, devices, OS and k3s
  updates, node labels from its device inventory.
- **One elected Wardstone leader** (a Kubernetes Lease) is the only
  component that holds cluster-admin credentials and applies Kubernetes
  objects. It translates typed resources into objects and reverts drift
  on objects it owns.
- **Keystone runs as a `failover` app** with its data on replicated
  storage, so it restarts elsewhere when its node is lost.
- **Proxmox is optional and below the engine.** It may host nodes as
  VMs; it never moves or restarts Wildstone apps. One controller per
  decision: Kubernetes places apps, Proxmox (if present) owns VMs.

### 2.3 Packages gain one field: `availability`

| Value | Meaning | Examples |
|---|---|---|
| `pinned` | Needs hardware on one node; never moves | Zigbee or Z-Wave stick |
| `failover` (default for stateful apps) | One copy; restarts on another node | Jellyfin, Immich, Keystone |
| `replicated` | Several copies behind one Service | Stateless web front ends |
| `offload` | One main copy plus workers on other nodes | Jellyfin + transcode workers |

- A package declares which values it supports; the household toggle only
  chooses between them. A package that cannot run as two copies (for
  example one using SQLite) never offers `replicated`.
- Hardware needs become node selectors on Wardstone-applied labels
  (`has-igpu`, `has-zigbee`), not host paths in the package.

### 2.4 The engine is small

Only what must be cluster-aware, or what Wildstone does not already do:

| Job | Choice |
|---|---|
| Kubernetes | k3s (SQLite at 1 server node, embedded etcd at 3) |
| Networking and isolation | k3s defaults: flannel + built-in NetworkPolicy controller |
| Floating home IP | kube-vip |
| App storage | Longhorn (1 copy on 1 node, replicated at 2+) |
| Media libraries | NFS CSI driver to a NAS |
| Image cache | k3s embedded registry mirror |
| iGPU and USB devices | Intel GPU device plugin; generic device plugin |
| Safety rules | Pod Security Admission + ValidatingAdmissionPolicy |

k3s's bundled Traefik and ServiceLB are disabled. Cilium is a later
upgrade if measured needs justify it.

Wildstone keeps its own tools as packages, rather than the Kubernetes
ecosystem equivalents:

| Job | Keeps | Instead of |
|---|---|---|
| Ingress / HTTPS | Caddy | ingress-nginx (retired upstream), Traefik |
| Certificates | Caddy + internal CA (ADR-W019) | cert-manager |
| DNS records | Keystone `DnsRecord` provider | external-dns |
| Login | Authelia + LLDAP | Kanidm, Dex |
| Secrets | Wildstone store (ADR-W012) → encrypted Kubernetes Secrets | external-secrets |
| Backups | Wildstone restic engine as Kubernetes Jobs | VolSync, Velero |
| Node reboots | Rolling-update Plans (§2.5) | kured |

Admission policy must refuse privileged pods, host-path mounts and host
networking except where a signed infrastructure package declares them,
and must refuse pod `exec`/`attach` for every principal except
break-glass (#127).

### 2.5 Updates

**App updates** use Kubernetes rolling updates, never Ansible:

1. An approved Plan changes the image version.
2. Kubernetes replaces one copy at a time, waiting for its health check.
3. Workload rollout limits (`maxUnavailable`, `maxSurge`) keep the
   package's minimum copies running. A Pod Disruption Budget does not
   limit rolling updates; it guards evictions such as node drains.
4. A failed health check stops the rollout; old copies keep serving and
   the Plan offers a rollback.

A `failover` app has a short restart; the Plan states it before approval.

**Host updates** (OS, drivers, k3s, Wardstone itself) run Ansible
**locally on each node**, ordered by the leader:

```
Plan approved: "Update OS on all nodes, one at a time"

for each node:
  1. leader cordons and drains the node (Pod Disruption Budgets respected)
  2. leader → node's Wardstone: RunPlaybook("os-update", <signed version>)
  3. node's Wardstone runs the playbook on itself, reboots if needed
  4. node returns; Wardstone reports versions
  5. leader waits for node Ready and storage copies rebuilt
  6. leader uncordons → next node
```

- Any failure stops the rollout and leaves untouched nodes as they were.
- k3s upgrades go server nodes first, then agents.
- The leader hands its Lease to another node before its own node drains.
- A one-node install cannot drain; the Plan states the expected downtime
  and the Owner chooses when.

### 2.6 Editions

| | **Wildstone on Linux** (first) | **Wildstone-Homelab-Talos** (much later) |
|---|---|---|
| Starts from | An existing Ubuntu/Debian machine | Bare metal, booted from a Wildstone image |
| Host provider | `LinuxHost`: signed Ansible playbooks + systemd | `TalosHost`: Talos API |
| Kubernetes | k3s | Talos (upstream Kubernetes) |
| OS updates | Ansible playbook, per §2.5 | Talos A/B upgrade with rollback, per §2.5 |

- **Linux ships first.** Talos is a later supported option and, once it
  exists, the recommended path for new households, because it is one OS
  install instead of OS plus Wildstone.
- **One edition per cluster.** k3s and Talos nodes cannot share a
  cluster. Switching editions is backup → reinstall → restore, which the
  exit drill already proves.
- **Talos joins need pairing.** A node waiting in Talos maintenance mode
  accepts configuration unauthenticated on the LAN, so the Console only
  joins it after the Owner enters a code shown on that machine's screen.

### 2.7 Ansible stays inside the no-shell rule

Ansible covers first install and later host updates on the Linux edition,
under these limits:

- **Playbooks are fixed and signed,** shipped and versioned with
  Wildstone like packages. There are no user- or Butler-supplied
  playbooks.
- **Always local.** Ansible runs on the node it changes
  (`connection: local`). Nothing SSHes between machines, and no node
  holds credentials to another.
- **Typed on the wire.** The Wardstone protocol carries
  `RunPlaybook(name, version, typed params)`; there is still no
  `Exec(string)` (ADR-W003).
- **First install keeps ADR-W014's front door.** `install.sh` keeps its
  roles, join tokens and CA pinning. It verifies the release, installs
  `ansible-core` and runs the bundled playbook for its role. Keystone
  does not exist yet at that point, so the person running the installer
  is the approval, as today. Every later host change is a Plan.

### 2.8 What the household gets at each size

| Nodes | Behaviour |
|---|---|
| 1 | Apps restart on crash; backups; no failover |
| 2 | App failover, except when the single server node is the one lost; the Console says so |
| 3+ | Three server nodes, replicated storage; any one node may fail |

## 3. Migration

1. **Runtime seam first.** Wardstone's Docker code moves behind one
   runtime-backend interface, and packages gain `availability`. Both
   land before any Kubernetes work and change no behaviour.
2. **Single-node k3s proof** against the current Docker VM baseline
   (#129, re-scoped), with the same lifecycle and recovery checks.
3. **k3s backend** passes the e2e suite and the weekly exit drill.
4. **Packages migrate one at a time.** The Docker backend stays until the
   last one has moved.
5. **Multi-node:** join, placement and failover on an owner-operated test
   cluster (ADR-W002), then `replicated` and `offload`.
6. **Talos edition** much later.

## 4. Non-goals

- Multiple sites, cloud nodes, live migration, load-based autoscaling.
- Exposing Kubernetes, Helm or Ansible to households, Owners or the Butler.
- Mixing editions in one cluster.

## 5. Open questions

- Leader handover timing and what Keystone shows while no leader holds
  the Lease.
- Longhorn copy count at 3+ nodes (2 or 3).
- Jellyfin transcode offload (rffmpeg or another tool).
- Caddy with more than one copy needs shared certificate storage; start
  with one `failover` copy.
- Backup Jobs must run where a volume is attached; to be proven in the
  single-node proof and again on multi-node.
- How a two-node household is warned that it is not fully highly
  available.

## 6. Consequences

- Wildstone stays the product; Kubernetes becomes a replaceable engine
  under Wardstone.
- The Docker backend and the k3s backend coexist during migration, which
  doubles e2e and exit-drill coverage for that period.
- Each edition needs its own CI and exit drill once Talos exists.
- The ground rules now also bind cluster credentials, admission policy
  and playbooks, not only the Wardstone wire format.
