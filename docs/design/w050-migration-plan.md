# W050 migration plan: from Docker to Kubernetes (k3s)

- **Status:** proposed plan for owner review. Documentation only. No implementation or runtime change is authorized by this document. Slice issues exist (all `agent:blocked`) so the owner can schedule them.
- **Date:** 2026-10-24, against `main` at `5a81358`.
- **Governed by:** [ADR-W050](../decisions/ADR-W050-multi-node-runtime-kubernetes-engine-host-providers-editions.md) (accepted). Respects [ADR-W051](../decisions/ADR-W051-failure-domains-and-recovery.md) (proposed) as requirements.
- **Slice issues:** [`w050-slices/`](w050-slices/), one file per slice; created on GitHub as #157 (slice 0), #151 (slice 1, existing) and #158–#173 (slices 2–17).
- **Evidence rule:** claims cite `path:line` on `5a81358`. Anything marked **(inferred)** was not verified in code or on a machine; it is for #129 or the slice to prove.

## Implementation review and proof gates — 2026-10-24

This section supersedes conflicting ordering, defaults, authority and acceptance claims elsewhere in this plan. Issue numbers and slice numbers remain stable; slice order is no longer a strict execution sequence. Historical code inventory remains evidence against the cited revision, not a fresh code audit.

### Work already in progress
At review time #177 and #178 are open, not merged. Preserve #177 as Docker encapsulation; Docker-shaped types are not the Kubernetes contract. #178 remains informational and requires capability review #179 before merge (or a compatible follow-up before runtime use if it merges meanwhile).

### Revised execution gates
1. Bounded approvals/accepted revisions (#157); Docker encapsulation (#151/#177); package capability contract (#179); Docker-only Keystone extraction (#158). Cluster wire design stays provisional until proof evidence.
2. **Small disposable single-node deployment, 1–3 service migrations (#129).** Use tools-it-tools, monitoring/Uptime Kuma, optionally Mealie or the Postgres fixture. Docker Caddy/identity may remain. Prove install, authenticated reachability, persistence, update failure, backup, replacement restore, and forward/reverse migration preserving new writes. Split an early proof child from #168 after #163; it does not require full #165 ingress migration. No broad catalog or live household migration.
3. #129 owns the pass/revise/reject recommendation after #159–#164 evidence and the bounded migration proof. #164 contributes host-update evidence; it cannot close the evaluation alone. Full identity relocation is later and must not silently become a prerequisite for this small proof.
4. **Early three-node safety and recovery proof (#180)** using the same limited services and a management fixture. Depends on #157–#159, #161–#163 and #179 review, not #169/#170. Prove partitions as well as powered-off nodes, stale leadership/operations, secret continuity and storage ownership. Owner-operated disposable evidence is required; three VMs on one host do not prove hardware/power independence.
5. Core migration and actual Keystone bootstrap/failover/offline recovery (#165/#166), bounded host-bound work (#167), household migration tooling (#168). Actual Keystone failure tests must pass before household migration is offered.
6. Catalog waves (#169) only after accepted #129 and #180 evidence, capability review, management recovery and migration tooling. #170 is later production multi-node productization, not the first architecture experiment. Split large trackers into bounded implementation children when scheduled.
7. Retire Docker (#172) only after migration/rollback and supported topology acceptance (#168/#170/#180), deprecation and replacement-restore evidence. Replica and worker capabilities (#171) remain separate; Talos stays deferred.

### Capability contract (#179)
Placement requirements, supported relocation, replica safety, worker components, and selected household policy are distinct. Do not use availability array order as a production replica default. No volume does not prove statelessness. Device identity differs from a device class: a specific radio may pin an app, while a GPU workload may relocate to another eligible GPU. Host networking alone does not prove exact-node affinity. Default-parameter rendering is insufficient for conditional hardware access. Review a small compatible schema evolution before rewriting the catalog.

### Authority, storage and recovery contract (#180)
- Keystone is the sole product-level desired-state/approval authority; Kubernetes controllers implement that intent. Bootstrap and emergency recovery are explicit audited exceptions.
- A Lease coordinates leadership; it is not fencing. Define cancellation, stale/in-flight request handling, revision checks, replay/idempotency and safe resumption before promising one effective writer.
- Separate administrative bootstrap/break-glass credentials from least-privilege runtime credentials. Server root/admin kubeconfigs remain authority regardless of who currently loads them. Runtime credentials must not alter admission/RBAC or mint their own signed-package grants. Admission protects against malformed workloads; it is not a security boundary against unrestricted cluster-admin compromise.
- Specify canonical secret storage, leader handover access, version continuity, export/import and encryption-key recovery. A missing local leader store must never regenerate existing secrets.
- Prove storage detach/reattach and partition behavior; RWOP plus short tolerations is not a complete fencing contract. State measured downtime instead of deriving it from eviction timeout.
- Host recovery must diagnose/rebuild/restore when Keystone cannot run, with a documented off-host kit when cluster API/storage is unavailable. A proxy to Keystone is not this recovery path.
- Resource priorities reduce risk but do not guarantee management survival of node/PV disk exhaustion. Reserve OS, Kubernetes and storage capacity and test pressure explicitly.

### Rollout and migration corrections
Singleton stateful workloads use Recreate with a declared outage. A failed update cannot keep the already terminated old pod serving. Rollback requires declared schema/data compatibility or validated restore alongside; old image selection alone is insufficient. Replicated stateless continuity is a separate test. Secret rotation uses the applicable workload strategy.

Final consistent data copy requires writers stopped/quiesced; estimate and disclose outage. Pre-copy may reduce it but needs final synchronization and application consistency checks. The initial 1–3-service migration proof is intentionally earlier and smaller than general household migration tooling.

### Sources for safety assumptions
- [client-go leader election explicitly disclaims fencing](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection).
- [Longhorn node failure and volume attachment](https://longhorn.io/docs/1.13.0/high-availability/node-failure/).
- [Kubernetes force-delete warning](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_delete/).
- [Kubernetes node-pressure eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/).

No runtime change, merge, or test deployment is executed by this documentation revision. Remaining implementation issues retain their scheduling restrictions.


## Contents

1. [Summary in plain language](#1-summary-in-plain-language)
2. [What must be decided first](#2-what-must-be-decided-first)
3. [Docker coupling inventory](#3-docker-coupling-inventory)
4. [Resource kinds → Kubernetes objects](#4-resource-kinds--kubernetes-objects)
5. [Package survey](#5-package-survey)
6. [Target architecture](#6-target-architecture)
7. [Slices, order and sizes](#7-slices-order-and-sizes)
8. [Test strategy](#8-test-strategy)
9. [Risks and open decisions](#9-risks-and-open-decisions)
10. [The first three slices in detail](#10-the-first-three-slices-in-detail)
11. [Proposed ADR amendments](#11-proposed-adr-amendments)

---

## 1. Summary in plain language

**What changes.** Today Wardstone tells Docker on one machine "run this container". After the migration, one Wardstone (the *leader*) tells Kubernetes "keep this app running somewhere suitable". Kubernetes then restarts it, moves it if a machine dies, and updates it one copy at a time.

**What stays the same.**
- Households still see packages, Plans and "Add a machine". Nobody sees Kubernetes.
- Keystone is still the only thing that decides what should run. Every change is still a Plan.
- Wardstone still accepts only typed operations. There is still no "run this command" (`Exec(string)`).
- Caddy, Authelia/LLDAP, the internal CA, the secret store and restic backups stay. They run *on* Kubernetes.

**A short example.** Installing Mealie today becomes, roughly:

| Today (Docker) | After (k3s) |
|---|---|
| Keystone sends `EnsureContainer(mealie)` to node `mini-1` | Keystone sends `ApplyWorkload(mealie)` to the leader |
| Docker starts one container on `mini-1` | The leader writes a Deployment, a Service and a volume claim; Kubernetes picks a node |
| A host folder holds the data | A Longhorn volume holds the data, copied to a second machine if there is one |
| Caddy finds it as `container/mealie:9000` on the Docker network | Caddy finds it as the Service `mealie:9000` through cluster DNS |
| Backup runs a restic container on `mini-1` | Backup runs a restic **Job** next to the volume |

**The size of the job.** 18 slices (0–17), roughly 12–18 months of part-time work for one maintainer with an agent **(inferred estimate)**. Each slice leaves `main` releasable with the Docker backend still working.

**The order, in one line.** Fix approval scope (0) → two refactors that change nothing (1, 2) → k3s on one machine (3) → apps on it, from simplest to hardest (4–11) → tools to move live households (12) → all packages (13) → several machines (14, 15) → remove Docker (16) → Talos (17, much later).

**The biggest surprise from reading the code.** #151 puts the seam *inside Wardstone*. That is necessary but not enough. Keystone's reconciler is per-node and speaks Docker's language: it names a node for every resource (`keystone/reconcile/loop/loop.go:685-696`), creates a Docker network (`keystone/reconcile/reconcile.go:1775-1790`), stamps spec-hash labels (`reconcile.go:1727`) and reads Docker container IPs (`keystone/reconcile/ingressaddr.go`). The k3s leader works on the whole cluster, not one node. So a **second, Keystone-side seam** is needed (slice 2) before any k3s code.

---

## 2. What must be decided first

Short list. Full list with recommendations in [§9](#9-risks-and-open-decisions).

| # | Decision | Recommendation | Blocks |
|---|---|---|---|
| D1 | Accept the two-seam design (Wardstone seam #151 **and** Keystone seam, slice 2) | Accept; amend #151's scope note | Slices 1–2 |
| D2 | New typed wire ops for the leader (`ApplyWorkload`, `RunJob`, `RunPlaybook`, …), never raw YAML | Accept. A "send this manifest" op is `Exec(string)` in disguise | Slice 2 |
| D3 | Where secret values live on k3s | Kubernetes Secrets, encrypted at rest, written only by the leader. Needs an ADR-W012 amendment ([§11](#11-proposed-adr-amendments)) | Slice 6 |
| D4 | Shared host folders (`/srv/wildstone/media`, …) on k3s | NAS → NFS CSI. No NAS → Longhorn RWX on one node. No `hostPath` | Slices 5, 13 |
| D5 | Owner-operated test runner | The PR template says every job stays on a GitHub-hosted runner. Multi-node tests need an exception or a manual owner run | Slice 14 |
| D6 | #128 G1/G2 before slice 3 | Yes. The leader will act on approvals across machines | Slice 3 |
| D7 | Accept ADR-W051 (with the small changes in §11) | Accept before slice 10 | Slice 10 |

---

## 3. Docker coupling inventory

**Legend:**
- **Seam:** moves behind the runtime-backend interface; a k3s implementation is added beside it.
- **Rewrite:** has no Docker-free shape and needs new code for k3s.
- **Host:** stays a per-node Wardstone host operation on every backend.
- **Gone:** disappears with the Docker backend (slice 16).

### 3.1 Wardstone (`wardstone/internal/agent/`)

| Code | What it does today | Under k3s |
|---|---|---|
| `docker.go:27-498` | Raw Docker Engine API client over the socket | **Gone** (only the Docker backend uses it) |
| `agent.go:190,252,286` | `DockerEndpoint` config; builds the Docker client at start | **Seam:** choose backend at start; Docker or leader-k8s client |
| `ops.go:45-243` | One switch over 38 op arms (`proto/wardstone/v1/wardstone.proto:104-146`) | **Seam:** container/volume/network/image arms go to the backend; host arms stay |
| `ops.go:535-801` `ensureContainer` | Digest check, mount checks, secret env, devices, caps, limits, host network, replace-on-spec-hash, port read-back | **Seam** for Docker. **Rewrite** for k3s as `ApplyWorkload` (leader only). Pre-checks (digest, allowlists) are shared |
| `ops.go:861-1027` stop/logs/pull/remove/stats/inspect/prune | Container lifecycle | **Seam**. k3s: scale-to-0, pod logs, (no pull: kubelet pulls), delete, metrics-server, rollout status, containerd image GC (#136) |
| `ops.go:1028-1076` `ensureVolume`, `ensureDockerNetwork` | Named volume; one shared `wildstone` network | **Seam** → PVC (Longhorn). Network → **Gone** (cluster network + NetworkPolicy, #42) |
| `resources.go:24-90` | Docker size and CPU parsing, cap-drop checks | **Seam:** shared validation; k3s maps to `resources` and `securityContext` (#41) |
| `devices.go:93-99` host system mounts; device/cap allowlists | Closed allowlists (`/dev/dri`, serial, `/run/dbus`, …) | **Host + admission:** the allowlist becomes ValidatingAdmissionPolicy input; devices via device plugins ([§6.7](#65-policy-and-admission)) |
| `registryauth.go` | Private-registry credential read from the node store, per pull | **Rewrite:** imagePullSecret written by the leader (k3s) |
| `seed.go:75` | Finds the Docker volume mountpoint; O_EXCL write-if-absent | **Rewrite:** init container with a Wildstone seeder image ([§4](#4-resource-kinds--kubernetes-objects)) |
| `diskspace.go:30` | Volume mountpoint → free space | **Seam:** k3s reads Longhorn volume capacity |
| `hostaliases.go`, `ops.go:761` | Node resolves names (MagicDNS) into container `/etc/hosts` | **Rewrite:** CoreDNS stub zone for the tailnet domain **(inferred, prove in slice 4)** |
| `secrets.go`, `materialize.go` | Sealed per-node store; values substituted into Docker env and managed files | **Host** for the store; **Rewrite** for delivery: leader writes Kubernetes Secrets (D3) |
| `readiness*.go`, `radios*.go` | Host IPv6, multicast, D-Bus, BlueZ, tun, radio inventory | **Host:** unchanged; results also become node labels |
| `mount_linux.go`, `fstab.go`, `blockdevices_*.go` | NFS mount + fstab, block devices | **Host** for diagnosis. App storage moves to NFS CSI; `EnsureMount` stays only for host needs **(inferred)** |
| `systemd_linux.go`, `agent.go:204` | Unit status; allowlist defaults to `wardstoned`, `docker` | **Host:** allowlist becomes `wardstoned`, `k3s`/`k3s-agent` |
| `selfupdate.go`, `updatestate.go` | Signed A/B self-update | **Host:** kept; also driven by `RunPlaybook("wardstone-update")` in rollouts |
| `fsops.go`, `listdir.go`, `netprobe.go`, `ports_*.go`, `facts_*.go`, `timesync_*.go` | Snapshot, usage, probes, ports, facts | **Host:** unchanged |
| `audit.go` | Per-arm audit allowlist | **Host:** every new arm needs an audit case (never log manifests with secret data) |

### 3.2 Keystone

| Code | What it does today | Under k3s |
|---|---|---|
| `keystone/reconcile/applier.go:19-40` | `Applier.Apply(nodeID, op)` | **Seam:** add a target type (node or cluster). Leader ops go to "cluster" |
| `keystone/reconcile/reconcile.go:552-790` `Reconcile(nodeID)` | Per-node observe → diff → approve → apply | **Seam (slice 2):** backend-neutral steps; a per-backend *translator* builds ops |
| `reconcile.go:1503-1700` container step + health gate | `EnsureContainer`, then polls `InspectContainer`/`ContainerStats` | **Rewrite** for k3s: `ApplyWorkload` + rollout status + readiness probe (#39) |
| `reconcile.go:1727` `specHash`, `configdigest.go` | Spec-hash label so config changes recreate the container | **Rewrite:** pod-template annotation `wildstone.dev/config-digest` triggers a rolling restart |
| `reconcile.go:1775-1790` `ensureNodePrereqs` | Creates the `wildstone` Docker network | **Gone** on k3s |
| `reconcile.go:1872` `containerEnsureOp` | Builds the Docker-shaped op | **Seam:** Docker translator; k3s translator builds `WorkloadSpec` |
| `reconcile.go:2101-2107` | Route probe skips `container/<name>` upstreams (no Docker IP) | **Better on k3s:** leader can probe the Service |
| `route.go`, `ingress.go`, `origin.go` | Caddy fragments written as host files via `WriteManagedFile` | **Rewrite:** fragments go into a Caddy ConfigMap ([§6.8](#68-networking-and-identity)) |
| `ingressaddr.go` | Reads the proxy container's Docker IP so forward-auth trusts it | **Rewrite:** trust the cluster pod CIDR + NetworkPolicy; leader reports Caddy pod IPs |
| `hostaliases.go` | Derives names containers must resolve | **Rewrite:** feeds the CoreDNS stub zone instead |
| `hostport.go` | Checks a host-network port is free | **Seam:** k3s scheduler handles host ports; keep check for `pinned` host-network pods |
| `hostbluez.go` | Reads `HomeAutomationReadiness` before BlueZ containers | **Host:** unchanged, but run against the node the pod is pinned to |
| `secret.go`, `configfile.go`, `seedfile.go`, `oidcclient.go`, `autheliaclients.go`, `lldap*.go`, `matrixappservices.go` | Managed files and secrets on one node | **Rewrite:** ConfigMaps/Secrets via the leader; "node" becomes "cluster" |
| `job.go` | One-shot container, exit code via `InspectContainer` | **Rewrite:** Kubernetes Job |
| `networkmount.go` | `EnsureMount` NFS + `DiskSpace` | **Rewrite:** NFS CSI PersistentVolume |
| `backupplan.go` | Declaration only | Unchanged |
| `keystone/reconcile/loop/loop.go:685-696` | Buckets desired state by `metadata.node` | **Seam:** a cluster bucket for k3s-backed packages |
| `loop.go:267` `allowMediumHigh` | Tick-wide approval (#128 G2) | **Fix first (slice 0)** |
| `keystone/resource/routeupstream.go:39` | `host/<port>` → `host.docker.internal` | **Rewrite:** a Service in front of the host-network pod |
| `keystone/resource/hostaccess.go:20-60` | Device and capability allowlists | Kept; also fed into admission policy |
| `keystone/backup/backup.go:59` `Runner` | `RunContainer(node, name, spec)` | **Seam already exists.** Add a `JobRunner` |
| `keystone/backup/runner.go:56-215` | Pull, ensure, poll, remove a restic container | **Rewrite** as Job runner |
| `keystone/backup/hooks.go:34-102` | `pg_dump` / `sqlite3 .backup` as separate one-shot containers (no exec) | Maps 1:1 to Jobs. `pg_dump` reaches the DB by Service name |
| `keystone/butler/tools_hostops.go:115-290` | `host.container_logs/stats/restart/prune_images/unit_status/disk_usage` | **Rename:** `app.logs`, `app.usage`, `app.restart`, `node.prune_images`, … (#66) |
| `keystone/domestic/hostops.go`, `keystone/butler/reader.go` | Same host ops | Same as above |
| `keystone/api/handlers_nodes.go:217` | `GET /nodes/{node}/containers/{name}/logs` | Add `GET /apps/{package}/logs`; keep old route for Docker |
| `keystone/api/handlers_secrets.go:328-331`, `handlers_personal_credentials.go:572-575` | Rotation restarts a container with Stop + Remove | **Seam:** "restart workload" op; k3s does a rolling restart |
| `keystone/api/handlers_probes.go` | Node probes | **Host:** unchanged |
| `keystone/internal/fakenode/fakenode.go:79-310` | Fake Docker node for tests | Add a fake cluster (fake leader) beside it |
| `keystone/policy/policy.go:22-32` | Semantic rules (auth, secrets, backup, upstream) | Unchanged; admission is a second, runtime layer |

### 3.3 Installer, drills and CI

| Code | Today | Under k3s |
|---|---|---|
| `scripts/install.sh:258-270`, `1923-1928` | Installs Docker from `get.docker.com` | **Rewrite:** thin wrapper → signed playbooks ([§6.1](#61-engine-bootstrap-and-ansible)) |
| `install.sh:1156-1347` | `docker exec lldap …` / `docker run --network container:lldap` to set passwords | **Rewrite:** typed Keystone API call that runs a Job. This also removes Docker shell use from the installer |
| `install.sh:1536` | `docker port caddy-tailnet-ts` | **Rewrite:** ask Keystone |
| `install.sh:1956-1985` | `wardstoned` unit `Requires=docker.service` | Edition-specific unit |
| `install.sh` keystone role | Keystone runs as a host systemd service | Keystone becomes a pod ([§6.3](#63-keystone-in-the-cluster)); no container image exists yet (`.github/workflows/release.yml` ships binaries) |
| `scripts/exit-drill.sh:43,123-229` | `docker volume`/`docker run` to seed and verify | k3s variant using the API, not `kubectl` |
| `.github/workflows/exit-drill.yml` | Runner's Docker daemon | Add a k3s job ([§8](#8-test-strategy)) |
| `.github/workflows/e2e.yml` | QEMU VM smoke, manual | Add a k3s smoke in the same VM path |

**Count:** of 38 op arms, **12 are Docker-shaped** (7 container arms, `PruneImages`, `EnsureVolume`, `EnsureDockerNetwork`, `SeedVolumeFile`, `StatVolumeFile`), **4 change meaning** because they deliver secrets or managed files to one node (`EnsureSecret`, `PutSecret`, `WriteManagedFile`, `ReadManagedFile`), and **22 stay host operations**.

---

## 4. Resource kinds → Kubernetes objects

"Leader" means the elected Wardstone that holds cluster-admin. "Host op" stays on the node's own Wardstone.

| Kind | Kubernetes objects | Stays a Wardstone host op | Difficulty |
|---|---|---|---|
| `Container` | Deployment (`replicas: 1`, `Recreate` for `failover`; `RollingUpdate` for `replicated`) + Service per port. `restart` → always (pods). `resources` → requests/limits. `capDrop`, `readOnlyRootfs`, `noNewPrivileges` → `securityContext` | – | Medium |
| `Container.ports` (`PortMapping`) | `ClusterIP` Service by default. A LAN-facing port (DNS :53, MQTT, syncthing) → `LoadBalancer` Service on the kube-vip address | – | Medium |
| `Container.networkMode: host` | `hostNetwork: true`, `dnsPolicy: ClusterFirstWithHostNet`, node selector, `pinned`. Allowed only with an admission exemption from a signed package | Port-free check (`hostport.go`) | **Hard** |
| `Container.devices` (`DeviceMapping`) | Device plugin resource (`gpu.intel.com/i915`, `squat.ai/serial-zigbee`, …) + node selector on a Wardstone label | Radio inventory → labels | **Hard** |
| `Container.capAdd` | `securityContext.capabilities.add`, admission-checked against `hostaccess.go` list | – | Medium |
| `Container.mounts` (named volume) | PVC | – | Easy |
| `Container.mounts` (`/etc/wildstone/<cap>` read-only) | ConfigMap or Secret volume | – | Medium |
| `Container.mounts` (`/srv/wildstone/<dir>` shared folder) | PVC on NFS CSI (NAS) or Longhorn RWX (D4) | – | Medium |
| `Container.mounts` (`/run/dbus`, `/srv/wildstone/bin`, `tailscaled.sock`) | `hostPath`, admission exemption, `pinned`. `/srv/wildstone/bin` should become an image instead | – | **Hard** |
| `Container.env` `secret/<name>` | `env.valueFrom.secretKeyRef` to a leader-written Secret | Store (D3) | Medium |
| Host aliases (`hostaliases.go`) | CoreDNS stub zone `*.ts.net → 100.100.100.100` **(inferred)** | Name list | **Hard** |
| `host/<port>` upstream (`routeupstream.go:39`) | Service selecting the host-network pod | – | Medium |
| `Volume` | PVC, Longhorn StorageClass, `ReadWriteOnce` (`ReadWriteOncePod` for Keystone) | – | Easy |
| `Route` | Entry in the Caddy routes ConfigMap; upstream → Service DNS name | – | Medium |
| `Ingress` | Caddy Deployment + `LoadBalancer` Service on kube-vip (`:80`, `:443`); Caddy data PVC | – | **Hard** |
| `DnsRecord` | None. Keystone provider, unchanged | – | Easy |
| `OidcClient` | Rendered into Authelia's clients **Secret** (it holds digests) | – | Medium |
| `LdapGroup` / `LdapUser` | Rendered list in a Secret + provisioning Job (as today) | – | Medium |
| `MatrixAppService` | Rendered registrations Secret + split Job | – | Medium |
| `Secret` | Kubernetes Secret, generated by the leader, never read by Keystone | Store, export, import | Medium |
| `ConfigFile` | ConfigMap, or Secret if it carries `secret/` references. Digest annotation on the pod template restarts the app on change | – | Medium |
| `SeedFile` | Init container using a tiny Wildstone seeder image (Go, no shell): O_EXCL write from a mounted ConfigMap into the PVC | – | **Hard** |
| `Job` | Kubernetes Job (`backoffLimit: 0`, `ttlSecondsAfterFinished`), Keystone keeps the "has run" record | – | Medium |
| `BackupPlan` | Keystone-only declaration; the engine runs Jobs | – | Easy |
| `BackupTarget` | Keystone-only; credentials as Secret for the Job | – | Easy |
| `NetworkMount` | NFS CSI PersistentVolume + PVC | `TestNfs` probe | Medium |
| `Automation`, `Meter`, `Commitment` | None (Keystone-owned) | – | – |

**Labels on every object:** `app.kubernetes.io/managed-by: wildstone`, `wildstone.dev/package`, `wildstone.dev/resource`, `wildstone.dev/revision` (the accepted revision, G1). The leader refuses to touch objects without them, the same rule as `OwnerLabel` today (`ops.go:661-664`).

---

## 5. Package survey

Collected by scanning every `packages/*/package.yaml` on `5a81358` (73 packages). "Host" lists devices (DEV), host networking (HOSTNET), extra capabilities and host paths outside `/etc/wildstone`. "Hook" is a declared `preBackup` hook.

**Difficulty rules:** *easy* = one container, no host access, no hook. *Medium* = database or hook, several containers, Jobs, LAN ports, shared folders or host binaries. *Hard* = host networking, devices, D-Bus or core infrastructure.

**Availability rules:** declared value(s) first; the default is in **bold**. `n/a` = no workload (Keystone-native or config only).

### 5.1 Core infrastructure

| Package | Avail. | State | Hook | Host | Depends on | Difficulty |
|---|---|---|---|---|---|---|
| `ingress` (Caddy ×2 + cloudflared) | **failover** | Caddy data | – | `/etc/wildstone` mount, ports | – | Hard |
| `ingress-tailnet` | **failover** | Caddy data | – | listen addr :443 | tailscale on host | Hard |
| `ingress-tailnet-public` | **failover** | Caddy data | – | port :443 | DNS provider | Hard |
| `ingress-tailnet-ts` | **pinned** | Caddy data | – | `tailscaled.sock` (`packages/ingress-tailnet-ts/package.yaml:155`) | tailscale on that node | Hard |
| `identity` (Authelia, Redis, LLDAP + 2 Jobs) | **failover** | SQLite, Redis | none (gap) | writable `/srv/wildstone/identity` (`packages/identity/package.yaml:417`) | ingress | Hard |
| `mqtt-mosquitto` | **failover** | yes | – | LAN port 1883 | – | Medium |
| `dns-adguard`, `dns-pihole` | **failover** | yes | – | LAN :53 tcp/udp | – | Medium |
| `vpn-gluetun` | **failover** | yes | – | `/dev/net/tun`, `NET_ADMIN` | – | Hard |

### 5.2 Host-bound (become `pinned`)

| Package | Avail. | State | Hook | Host | Depends on | Difficulty |
|---|---|---|---|---|---|---|
| `home-assistant` | **pinned** | yes, 6 SeedFiles | none | HOSTNET, DEV, `NET_ADMIN, NET_RAW`, `/run/dbus` | mqtt | Hard |
| `zigbee-zigbee2mqtt` | **pinned** | yes | none | DEV (serial) | mqtt, identity | Hard |
| `zwave-zwave-js-ui` | **pinned** | yes, SeedFile | none | DEV (serial) | identity | Hard |
| `thread-otbr` | **pinned** | yes | none | HOSTNET, DEV, `IPC_LOCK, NET_ADMIN`, `/dev/net/tun` | – | Hard |
| `matter-matterjs` | **pinned** | yes | none | HOSTNET | – | Hard |
| `cameras-frigate` | **pinned** if a device is set, else failover | recordings | – | DEV (GPU/Coral, optional) | mqtt, identity, bulk | Hard |

### 5.3 Database-backed apps

| Package | Avail. | State | Hook | Host | Depends on | Difficulty |
|---|---|---|---|---|---|---|
| `photos-immich` (server, ML, Postgres, Redis) | **failover**, offload (ML) later | Postgres, uploads | `pg_dump` | bulk folder | identity, ingress | Medium |
| `documents-paperless` (5 containers) | **failover** | Postgres, Redis | `pg_dump` | bulk folder | identity, ingress | Medium |
| `matrix-synapse` (+ 2 Jobs) | **failover** | Postgres | `pg_dump` | – | identity | Medium |
| `mail-roundcube` (+ Job) | **failover** | SQLite ×2 | `sqlite` ×2 | – | ingress | Medium |
| `recipes-mealie` | **failover** | SQLite | `sqlite` | – | identity | Medium |
| `code-gitea` | **failover** | SQLite | `sqlite` | LAN port 2222 | ingress | Medium |
| `calendar-radicale` (+ Job, 4 ConfigFiles) | **failover** | files | – | – | identity (LDAP) | Medium |

### 5.4 Media and shared folders

All read or write `/srv/wildstone/media` or `/downloads` (shared between packages), so all need D4.

| Package | Avail. | State | Hook | Host | Difficulty |
|---|---|---|---|---|---|
| `media-jellyfin` | **failover**, offload later | config | – | media folder; iGPU later (`packages/media-jellyfin/package.yaml:13`) | Medium |
| `media-plex` | **failover** | config | – | media folder | Medium |
| `tv-sonarr`, `movies-radarr`, `music-lidarr`, `subtitles-bazarr`, `indexers-prowlarr` | **failover** | SQLite (no hook) | – | media, downloads | Medium |
| `downloads-qbittorrent` | **failover** | config | – | downloads; LAN ports 6881 | Medium |
| `music-navidrome`, `audiobooks-audiobookshelf`, `books-calibre-web`, `books-kavita`, `podcasts-podgrab` | **failover** | SQLite (no hook) | – | media subfolder | Medium |
| `files-filebrowser` | **failover** | config | – | media | Medium |
| `sync-syncthing` | **failover** (pinned if LAN discovery needed) | config | – | sync folder; ports 22000, 21027/udp | Medium |
| `notes-silverbullet` | **failover** | knowledge folder | – | `/srv/wildstone/knowledge`, shared with Keystone | Medium |

### 5.5 Simple apps

| Package | Avail. | State | Notes | Difficulty |
|---|---|---|---|---|
| `tools-it-tools` | **replicated**, failover | none | No volume, no secret | **Easy (first)** |
| `pdf-stirling` | **replicated**, failover | scratch volume | | Easy |
| `captcha-flaresolverr`, `research-ddgs` | **replicated**, failover | none | No route | Easy |
| `matrix-element` | **replicated**, failover | none | ConfigFile only | Easy |
| `monitoring` (Uptime Kuma) | **failover** | SQLite | #129's proof app. Cannot see its own host's loss (W051 §4.7) | Easy |
| `notifications` (ntfy), `dashboard`, `dashboard-homarr`, `requests-seerr` | **failover** | small | | Easy |
| `bookmarks-linkding`, `budget-actual`, `feeds-freshrss`, `inventory-homebox`, `passwords-vaultwarden`, `ai-open-webui`, `monitoring-beszel` | **failover** | SQLite (no hook) | Vaultwarden is `dataSensitivity: high` with no hook: fix in #40 | Easy |
| `metrics-prometheus`, `metrics-grafana` | **failover** | TSDB / SQLite | ConfigFiles | Easy |
| `ai-ollama`, `speech-speaches` | **failover** | model cache | GPU later | Easy |

### 5.6 Wildstone host-binary packages

| Package | Avail. | Notes | Difficulty |
|---|---|---|---|
| `chat-discord`, `chat-slack`, `chat-telegram`, `chat-matrix` | **failover** only (one bot session; two copies would answer twice) | Run a binary from host `/srv/wildstone/bin` (`packages/chat-discord/package.yaml:78`). Needs a released OCI image instead | Medium |
| `agent-hermes` | **failover** | Same `/srv/wildstone/bin` mount; SeedFile; BackupPlan | Medium |

### 5.7 No workload (`n/a`)

`assistant-wildstone`, `filedrop-wildstone`, `homepage-wildstone`, `home-automation`, `knowledge-wildstone`, `lists-wildstone`, `pantry-pantryos`, `wildstone-console`, `workspace-wildstone`. These are Keystone features, config files, LDAP groups or routes. They move with Keystone (slice 10) and the ingress (slice 9). `knowledge-wildstone` and `workspace-wildstone` read host folders from Keystone **(inferred)**, so Keystone's pod needs the same shared-folder claims.

### 5.8 Totals

| | Count |
|---|---|
| Packages | 73 |
| No workload | 9 |
| Easy | 21 |
| Medium | 31 |
| Hard | 12 (4 ingress, identity, gluetun, 6 host-bound) |
| Declared DB hooks | 7 packages (3 `pg_dump`, 4 `sqlite`) |
| SQLite apps **without** a hook | ~20 **(inferred from images)**: crash-consistent backups only (#40) |
| Mount a templated host path (mostly shared folders) | 22 |
| `replicated` candidates | 5 |

---

## 6. Target architecture

### 6.1 Engine bootstrap and Ansible

**Shape.** `install.sh` keeps its front door (roles, join token, CA pinning, checksum check) and shrinks to:

1. Verify the release (`checksums.txt` + a release signature, see below).
2. Install `ansible-core` from the distro (Ubuntu 24.04 / Debian 12 packages) **(inferred: version check needed)**.
3. Unpack the bundled playbooks into `/usr/lib/wildstone/playbooks/<version>/`.
4. Run `ansible-playbook -c local -i localhost, <role>.yml -e @answers.json`.

**Playbooks** (all `connection: local`, all idempotent, all in `deploy/ansible/` **(new)**):

| Playbook | Does | Triggered by |
|---|---|---|
| `prereqs.yml` | `open-iscsi`, `nfs-common`, kernel modules, sysctls, time sync, GPU firmware/drivers when an iGPU is found | `server.yml`, `agent.yml`, `os-update.yml` |
| `k3s-server.yml` | k3s server with pinned version and flags (below), secrets encryption, audit log | installer `--role keystone` |
| `k3s-agent.yml` | k3s agent joining with a k3s token **delivered by Keystone** at join | installer `--role node` |
| `engine-addons.yml` | Longhorn, kube-vip, NFS CSI, device plugins, admission policies, PriorityClasses, from pinned, vendored manifests | first server only |
| `wardstone.yml` | `wardstoned` binary + unit + config | every role |
| `keystone-bootstrap.yml` | First Keystone pod ([§6.3](#63-keystone-in-the-cluster)) | first server only |
| `os-update.yml` | `apt` upgrade, reboot-required flag | `RunPlaybook` |
| `k3s-upgrade.yml` | Replace k3s binary at a pinned version, restart | `RunPlaybook` |
| `wardstone-update.yml` | Wraps the existing signed self-update | `RunPlaybook` |

**k3s flags** (from ADR-W050 §2.4; the reference repo's `metal/roles/k3s/defaults/main.yml` uses a similar set):

```
--disable=traefik --disable=servicelb --disable=local-storage
--secrets-encryption
--write-kubeconfig-mode=0600
--flannel-backend=vxlan
--embedded-registry
--kube-apiserver-arg=admission-control-config-file=/etc/wildstone/k3s/admission.yaml
--protect-kernel-defaults
--tls-san=<kube-vip address>
--cluster-init            (only when the household reaches 3 servers, slice 14)
```

**Signing.** Playbooks ship inside the release tarball, which is already sha256-checked (ADR-W014 A1). Add:
- an ed25519 signature over a `playbooks.manifest` (file → sha256), using the same key family as Wardstone self-update (`wardstone/internal/agent/selfupdate.go`);
- Wardstone verifies the manifest **before each run** and refuses any file not listed;
- `RunPlaybook(name, version, params)` names a playbook from that manifest; `params` is a typed proto message per playbook (for example `OsUpdateParams{reboot: ALLOW_IF_REQUIRED}`), never a free map of Ansible variables;
- Wardstone runs `ansible-playbook` with a fixed argv, `ANSIBLE_CONFIG` pinned, no inventory plugins, no `--extra-vars` from the wire except the typed params rendered to a JSON file.

### 6.2 Wardstone leader

- **Election:** a `coordination.k8s.io/v1` Lease `wildstone-leader` in namespace `wildstone-system`. Every Wardstone runs the elector; only the holder loads cluster-admin.
- **Credentials:** the admin kubeconfig stays root-only on server nodes (`/etc/rancher/k3s/k3s.yaml`, mode 0600). Non-leaders use a narrow ServiceAccount that can only read/update the Lease and its own Node labels. Agent-only nodes can never become leader (they have no admin kubeconfig) **(design choice)**.
- **Lease timing:** lease 15 s, renew 10 s, retry 2 s (client-go defaults). Takeover after ~15 s.
- **Handover on drain:** before its own node is cordoned, the leader releases the Lease (`ReleaseOnCancel`) and waits until another holder is seen, then the new leader continues the rollout from the Plan's recorded step. The rollout state lives in Keystone (the Plan), not in the leader.
- **Keystone with no leader:** status "Cluster changes paused: no coordinator". Plans with cluster steps queue, host-only ops still work, apps keep running. Shown per node in the Console.
- **How it applies:** server-side apply with field manager `wildstone-leader`, from typed `WorkloadSpec` → fixed Go templates. No Helm at runtime.
- **Drift:** a watch on objects with `managed-by: wildstone`; changes not made by the field manager are reported to Keystone as drift (a Plan), the same as `ForeignDrift` today. The leader reverts only when the Plan says so (no new automatic authority, W051 §4.5). Objects without the label are never touched.

### 6.3 Keystone in the cluster

- **Package:** a built-in `management` package: Keystone (API, Console, Statefile Git repo, runtime DB) in one pod.
- **Single writer:** `replicas: 1`, `strategy: Recreate`, PVC `accessModes: [ReadWriteOncePod]` on Longhorn, PriorityClass `wildstone-management` (highest workload class), requests = limits (W051 §4.1).
- **Image:** a new release artifact (OCI image of the existing binary). Today Keystone is a host systemd service (`scripts/install.sh` keystone unit).
- **Reach:** Wardstone dials `<kube-vip address>:9440` (a `LoadBalancer` Service). CA pinning is unchanged because the CA lives in Keystone's data.
- **Bootstrap order (no Keystone yet, so the person running the installer is the approval, ADR-W050 §2.7):**
  1. Playbook installs k3s + add-ons.
  2. Playbook starts Wardstone, which becomes leader (only node).
  3. `keystone-bootstrap.yml` asks the local Wardstone (local socket, root only) to apply the fixed `management` workload once.
  4. Keystone starts, creates its CA, prints the join token as today.
  5. Keystone records `management` in the Statefile as an installed package with the same revision. From here every change is a Plan.
- **Break-glass:** the host CLI (`keystone owner …`) must work when ingress is down. On k3s it becomes `wild-recover`, a host binary that talks to Keystone's pod through the local Wardstone, not through Caddy **(design choice; ADR-W033 A7)**.
- **Migration from host Keystone:** stop the systemd unit, copy `/var/lib/wildstone/keystone` into the PVC with a one-time import Job, start the pod, keep the old directory read-only until the owner confirms (slice 10).

### 6.4 Placement and `availability`

| Value | Kubernetes shape |
|---|---|
| `pinned` | `nodeSelector` on a Wardstone label (`wildstone.dev/has-zigbee=true`) or `kubernetes.io/hostname`; `Recreate` |
| `failover` | `replicas: 1`, `Recreate`, Longhorn RWO volume, short `tolerationSeconds` for node loss (30 s instead of 300 s) **(to tune in slice 14)** |
| `replicated` | `replicas: N`, `RollingUpdate` with `maxUnavailable: 0, maxSurge: 1`, PDB `minAvailable: 1`, soft pod anti-affinity |
| `offload` | main Deployment (`failover`) + a worker Deployment with its own selector (slice 15) |

The household toggle "Keep this running if a machine fails" chooses `failover` (on) vs. `pinned` to the current node (off).

### 6.5 Policy and admission

Two layers, as today:

1. **Keystone policy** (`keystone/policy/policy.go:22-32`) at proposal time. Unchanged; add rules for `availability`.
2. **Cluster admission** at apply time, so even a compromised leader template cannot run a privileged pod:
   - Pod Security Admission: `restricted` on `wildstone-apps`; `privileged` on `kube-system`, `longhorn-system`, `wildstone-system` and `wildstone-host`. PSA cannot express "host network for this one package only", so `wildstone-host` relies on the policy below, not on PSA.
   - ValidatingAdmissionPolicy (CEL), bound to **both** `wildstone-apps` and `wildstone-host` (and to any namespace labelled `wildstone.dev/managed=true`):
     - everywhere: refuse `privileged`, `hostPID`, `hostIPC`; images must be digest-pinned (`@sha256:`), like `ops.go:536`;
     - in `wildstone-apps`: refuse `hostNetwork`, `hostPath`, added capabilities and device resources outright;
     - in `wildstone-host`: allow only the grants the pod's `wildstone.dev/host-access` annotation names, and only when that names a signed package whose manifest digest the leader registered (#38). Each grant is still narrowed:
       - `hostPath` only for the paths in `hostSystemMounts` (`wardstone/internal/agent/devices.go:93-99`), read-only, same path inside;
       - capabilities only from `keystone/resource/hostaccess.go` `ContainerCapabilities`;
       - device resources only those the package declares;
       - a pod in `wildstone-host` without a valid annotation is refused, so the namespace is never a blanket exemption.
   - Refuse `pods/exec`, `pods/attach`, `pods/portforward` for every subject except the break-glass role (#127) via RBAC (no one else is bound to it) plus a VAP on `CONNECT`.
- **Namespaces:** `wildstone-system` (leader, Keystone), `wildstone-apps` (normal packages), `wildstone-host` (pinned host-access packages). One namespace per package was considered; it makes NetworkPolicy simpler but multiplies RBAC. Recommendation: shared namespaces + per-package NetworkPolicy by label.

### 6.6 Storage and backup

| Nodes | Longhorn | Notes |
|---|---|---|
| 1 | 1 copy | Same safety as a Docker volume. Backups are the protection |
| 2 | 2 copies | Survives one disk/node; the Console warns the server node is a single point (ADR-W050 §2.8) |
| 3+ | 2 copies (recommended) | 3 copies costs 50 % more disk for little gain at home **(open, O3)** |

- **Media:** NFS CSI StorageClass to the NAS, `ReadWriteMany`, `reclaimPolicy: Retain`. A missing NAS fails only the pods that mount it (#132).
- **Backup Jobs:** the engine's `Runner` seam (`keystone/backup/backup.go:59`) gets a `JobRunner`:
  - RWO volumes: the Job uses `podAffinity` to the app pod (Longhorn RWO allows several pods on the same node) **(inferred; prove in slice 7)**.
  - `pg_dump` Job reaches Postgres by Service name; output PVC separate from live data (as `hooks.go:42-46` already enforces).
  - `sqlite3 .backup` Job mounts the app volume read-only on the same node.
  - Restore is alongside: restic restore into a new `<plan>-restore` PVC, then a Plan cuts over (W051 §4.5).
- **Snapshots are not backups** (W051 §4.8). Longhorn snapshots may be offered for fast rollback only.
- **Off-site (W045):** unchanged; the restic Job gets the target credentials as a Secret.
- **Exit drill on k3s:** same story as `scripts/exit-drill.sh`: install → write data → back up → destroy the whole cluster (`k3s-uninstall.sh`) → reinstall → restore from Statefile + repository → verify bytes. Uses Keystone's API, not `kubectl`.

### 6.7 Devices and host networking

- Wardstone (host side) keeps reading radios and readiness (`radios_linux.go`, `readiness_linux.go`) and writes node labels: `wildstone.dev/has-zigbee`, `has-zwave`, `has-thread`, `has-igpu`, `has-coral`, `has-bluez`.
- Intel GPU device plugin for `/dev/dri`. Generic device plugin (squat/generic-device-plugin) for serial radios, Coral and `/dev/net/tun`, configured from the same allowlist as `hostaccess.go:20-31`.
- Serial devices keep their `/dev/serial/by-id/...` name, so a moved stick is still found (`devices_moved_test.go` intent).
- Host-network pods are always `pinned`, live in `wildstone-host` and need the admission exemption.

### 6.8 Networking and identity

- **Ingress:** Caddy as a `failover` Deployment, `LoadBalancer` Service on the kube-vip address for `:80`/`:443` (and the tailnet listen address). Route fragments and the base Caddyfile live in one ConfigMap (plus a Secret for anything with `secret/` references); Caddy keeps `--watch` **(inferred: verify that Caddy notices ConfigMap symlink swaps; else the leader triggers `caddy reload` through Caddy's admin API on localhost via a sidecar)**. Caddy data (certificates) on a Longhorn RWO volume, one copy (ADR-W050 §5).
- **Coexistence trick (key for slices 4–8):** while Caddy still runs on Docker, a `Route` to a k3s app uses an upstream `cluster/<service>:<port>` that Keystone renders to the kube-vip address and a per-app `LoadBalancer` port. Each app can move before the ingress does. Restricted to the node's own address.
- **Cluster DNS vs Route resolution (ADR-W019):** apps resolve each other by Service name (`mealie.wildstone-apps.svc`). The OIDC issuer stays the public or tailnet origin, because it must match what the browser saw (ADR-W019 A10). Pods reach it through CoreDNS: tailnet names via a stub zone; public names via normal DNS, then hairpin to kube-vip.
- **Authelia from pods:** reachable through the issuer URL (above) and, for server-side calls, through the internal Service with the issuer name pinned by `hostAliases` to the Caddy Service IP **(alternative if the stub zone fails)**.
- **Internal CA (ADR-W019):** unchanged. The CA bundle is a ConfigMap mounted where apps need it.
- **Forward-auth trust:** replace the Docker-IP refresh (`ingressaddr.go`) with: trust only the pod CIDR (`10.42.0.0/16` k3s default) *and* a NetworkPolicy allowing only Caddy pods to reach Keystone's forward-auth port. Narrower than today's "one remembered IP that can go stale" (#42).
- **NetworkPolicy (#42):** default-deny ingress in `wildstone-apps`; allow from Caddy to each Route's Service; allow declared dependencies (`requires`) between packages; allow DNS. Egress stays open at first (W050 does not ask for egress control) **(open, O8)**.

### 6.9 Butler, Console and API

- **One public API.** New endpoints under `/api/v1/apps/{package}` (status, logs, restart, usage) and `/api/v1/cluster` (nodes, leader, storage). Console, `wild` CLI and Butler call the same ones. Old `/nodes/{node}/containers/...` stays while Docker exists.
- **Butler host tools (#66):** rename to backend-neutral names, same tiers:
  - `host.container_logs` → `app.logs`; `host.container_stats` → `app.usage`; `host.container_restart` → `app.restart` (Plan-free restart remains Tier 3, as today, since the reconciler recreates from desired state);
  - `host.prune_images` → `node.prune_images` (containerd GC on k3s);
  - `host.unit_status`, `host.disk_usage` → unchanged (host);
  - new read-only: `cluster.status` (leader, nodes Ready, Longhorn health, pending pods with reason).
  - The Butler still cannot approve or apply.
- **Status (#39, #122):** status reads distinguish node ready / pod running / app ready / dependency unavailable / unknown (W051 §4.4). Readiness comes from probes declared by the package (#39), mapped 1:1 to `readinessProbe` and `startupProbe`.
- **Node pages:** show machine, role (server/agent), Ready, leader badge, labels found (Zigbee, iGPU), disk and Longhorn space.
- **"Add a machine":** Console asks Keystone to mint a join token (as today: `wild node mint-join`); the installer command shown to the owner gains nothing new. Keystone hands the k3s join secret to the new node's Wardstone after mTLS join, so it never appears in a URL or a shell history **(design choice)**.
- **"Keep this running if a machine fails":** a per-package switch, shown only when the package supports `failover` and there are ≥ 2 nodes. Turning it on is a Plan that states the restart time.

### 6.10 Migrating live installs

Per package, as a Plan with named steps (slice 12):

1. **Prepare:** k3s installed beside Docker on the same node (k3s brings its own containerd; Docker stays). Package marked `backend: docker`.
2. **Back up** with the existing engine (Docker), then check the backup.
3. **Stop** the Docker containers (scale to 0; keep them and their volumes).
4. **Copy data:** a one-time Job mounts the old Docker volume path read-only (`hostPath`, admission exemption scoped to this Job and to `/var/lib/docker/volumes/<owned-name>/_data`) and copies into the new PVC. Size and checksum are compared.
5. **Start on k3s**, wait for readiness.
6. **Cut over the hostname:** one Route change (Docker Caddy → `cluster/` upstream, or k3s Caddy after slice 9). Atomic per hostname (`docs/migration.md:10-24`).
7. **Hold** the Docker volume for N days (default 14). After N days a separate Plan deletes the old volume.
8. **Rollback (move back)** must not lose writes made on k3s after cutover. The Docker volume is frozen at step 3, so rollback is a mirror of steps 2–6, not a restart:
   1. back up the k3s copy (restic Job) and check it;
   2. stop the k3s workload (scale to 0) so nothing writes;
   3. reverse-copy Job: PVC → the held Docker volume (read-write `hostPath`, same scoped exemption as step 4), after keeping the frozen copy as `<volume>-premigration` until the Plan finishes; compare size and checksums;
   4. start the Docker containers, wait for readiness, revert the Route.
   - If the reverse copy fails, the Plan stops with k3s scaled to 0 and the Route unchanged; restarting k3s is the safe recovery. Docker is never started against the frozen pre-migration data.

- **Identity migrates exactly once** (two-masters rule): LLDAP + Authelia move in one Plan in slice 9; no dual running.
- **Ingress moves exactly once:** kube-vip takes `:80/:443` in the same Plan that stops Docker Caddy.
- **Keystone moves last** of the core (slice 10), with its own import Job.

---

## 7. Slices, order and sizes

**Sizes:** S ≈ ≤ 1 week, M ≈ 1–3 weeks, L ≈ 3–6 weeks, XL ≈ 6+ weeks (part-time maintainer with an agent; **inferred**).

| # | Slice | Size | Needs ADR change | Draft | Issue |
|---|---|---|---|---|---|
| 0 | Prerequisites: accepted revision + bounded approval (#128 G1/G2) | L | No | [00](w050-slices/00-prerequisites-g1-g2.md) | #157 |
| 1 | Wardstone runtime seam + `availability` field (#151) | M | No | [01](w050-slices/01-wardstone-runtime-seam.md) | #151 |
| 2 | Keystone backend seam + typed cluster ops (design + Docker-only refactor) | L | ADR-W003 amendment (new arms) | [02](w050-slices/02-keystone-backend-seam.md) | #158 |
| 3 | Engine on one node: signed playbooks, k3s, add-ons, admission, leader | L | ADR-W014 amendment (Ansible step) | [03](w050-slices/03-engine-bootstrap-single-node.md) | #159 |
| 4 | First stateless package on k3s (`tools-it-tools`) behind Docker Caddy | M | No | [04](w050-slices/04-first-stateless-package.md) | #160 |
| 5 | Volumes on Longhorn + `monitoring` (Uptime Kuma) + rolling update | M | No | [05](w050-slices/05-volumes-and-uptime-kuma.md) | #161 |
| 6 | Secrets, ConfigFiles, SeedFiles on k3s | L | **ADR-W012** amendment | [06](w050-slices/06-secrets-config-seed.md) | #162 |
| 7 | Backup Jobs, hooks, restore, exit drill on k3s | L | ADR-W018 note | [07](w050-slices/07-backup-restore-exit-drill.md) | #163 |
| 8 | Host updates on one node: `RunPlaybook` | M | No (W050 §2.7 covers it) | [08](w050-slices/08-runplaybook-single-node.md) | #164 |
| 9 | Ingress + identity on k3s, per-hostname cutover | XL | No | [09](w050-slices/09-ingress-identity.md) | #165 |
| 10 | Keystone as the `management` failover package | L | ADR-W051 acceptance | [10](w050-slices/10-keystone-in-cluster.md) | #166 |
| 11 | Host-bound packages: devices, labels, `pinned`, host networking | XL | No | [11](w050-slices/11-host-bound-packages.md) | #167 |
| 12 | Live-install migration tooling (per package, with rollback) | L | `docs/migration.md` update | [12](w050-slices/12-live-migration-tooling.md) | #168 |
| 13 | Package migration waves A / B / C | 3 × L | No | [13](w050-slices/13-package-waves.md) | #169 |
| 14 | Multi-node: join, placement, failover, rolling host updates, leader handover | XL | Possibly (2-node wording) | [14](w050-slices/14-multi-node.md) | #170 |
| 15 | `replicated` and `offload` | L | No | [15](w050-slices/15-replicated-offload.md) | #171 |
| 16 | Retire the Docker backend | M | ADR-W050 status note | [16](w050-slices/16-retire-docker.md) | #172 |
| 17 | Talos edition | XL | New ADR | [17](w050-slices/17-talos-edition.md) | #173 |

**Changes from the starting skeleton, and why:**
- **Added slice 2** (Keystone seam): the reconciler is per-node and Docker-shaped ([§3.2](#32-keystone)); doing this inside slice 4 would make it XL and hard to review.
- **Split "first package" into 4 and 5:** `tools-it-tools` has no volume or secret, so slice 4 proves only the apply path. Uptime Kuma needs a volume; that becomes slice 5.
- **Moved `RunPlaybook` to its own slice (8):** #129 asks for an OS update as a Plan step on one node; it does not need multi-node.
- **Added Keystone-in-cluster as its own slice (10):** it has its own bootstrap and data move ([§6.3](#63-keystone-in-the-cluster)).
- **Ingress/identity moved after storage and backup:** the coexistence trick ([§6.8](#68-networking-and-identity)) lets apps move first, so the riskiest cutover happens once, with backups proven.

**Dependency graph:**

```mermaid
graph TD
  S0[0 G1/G2] --> S3
  S1[1 Wardstone seam] --> S2[2 Keystone seam]
  S2 --> S3[3 Engine on one node]
  S3 --> S4[4 Stateless app]
  S4 --> S5[5 Volumes + Uptime Kuma]
  S5 --> S6[6 Secrets/config/seed]
  S5 --> S7[7 Backup + exit drill]
  S6 --> S7
  S3 --> S8[8 RunPlaybook one node]
  S6 --> S9[9 Ingress + identity]
  S7 --> S9
  S9 --> S10[10 Keystone in cluster]
  S7 --> S10
  S6 --> S11[11 Host-bound]
  S7 --> S12[12 Live migration tooling]
  S9 --> S12
  S12 --> S13[13 Package waves]
  S11 --> S13
  S10 --> S14[14 Multi-node]
  S8 --> S14
  S13 --> S14
  S14 --> S15[15 replicated/offload]
  S13 --> S16[16 Retire Docker]
  S10 --> S16
  S16 --> S17[17 Talos]
```

**Mapping to #129:** slices 3–8 together are #129's proof. #129's "recommendation for owner review" is written at the end of slice 8, before slice 9 starts. If #129 says no, slices 9+ stop and only 0–2 (useful on their own) stay.

**Alpha:** none of this gates alpha (#13, #15). Slices 0–2 can land on the Docker backend at any time after alpha.

---

## 8. Test strategy

### 8.1 What runs where

| Test | Where | Backend | When |
|---|---|---|---|
| Unit tests (`go test -race`) incl. fake cluster | GitHub-hosted | both | every PR |
| Leader translation golden tests (`WorkloadSpec` → YAML snapshot) | GitHub-hosted | k3s | every PR |
| Admission policy tests: apply bad pods to a real API server | GitHub-hosted, **k3d** single node (k3s in Docker) | k3s | every PR touching policy or leader |
| Package translation lint: every package translates and passes admission | GitHub-hosted, k3d | k3s | every PR touching `packages/` |
| e2e smoke in a QEMU VM (`testing/e2e`, `.github/workflows/e2e.yml`) | GitHub-hosted (KVM) | Docker **and** k3s (matrix) | manual + nightly |
| Exit drill (`exit-drill.yml`) | GitHub-hosted | Docker (today) + k3s in a QEMU VM | weekly + PRs touching backup |
| Multi-node (3 VMs), failover, rolling host update, power-loss | Owner-operated (Proxmox, `testing/e2e/proxmox.go`, #16) | k3s | before each slice-14+ merge, run by the owner |

- **Why k3d for policy tests:** fast (≈ 30 s start), real API server and admission. It cannot test Longhorn (needs iSCSI on the host) or kube-vip, so those stay in the QEMU VM path.
- **Why QEMU for e2e and drills:** real k3s with systemd, Longhorn and reboot; already works on hosted runners (`testing/e2e/README.md`).
- **Owner-operated runner (D5):** the PR template says every job stays on a GitHub-hosted runner. Recommendation: no self-hosted runner; the owner runs `go test -tags proxmox ./testing/e2e/...` from their machine and pastes the evidence record (format from `docs/design/failure-domain-recovery-review.md`) into the PR.

### 8.2 Coexistence

- A build tag is not needed: both backends compile; the backend is chosen at runtime.
- A **backend matrix** in e2e and the exit drill from slice 4 until slice 16.
- Each package carries `backends: [docker, k3s]` in CI metadata during waves; the lint test fails if a package claims `k3s` and does not translate or fails admission.
- Fixtures in `testing/fixtures/` gain a minimal Postgres package and a SeedFile package for slices 6–7.

---

## 9. Risks and open decisions

### 9.1 Risks

| Risk | Impact | Mitigation |
|---|---|---|
| **RAM on a 16 GB mini PC.** k3s server ≈ 0.5–0.8 GB, Longhorn ≈ 0.5–1 GB, kube-vip + NFS CSI + device plugins ≈ 0.2 GB, Keystone reservation ≈ 0.5 GB → **≈ 1.7–2.5 GB** before apps **(inferred, measure in #129)** | Fewer apps fit | Measure in slice 3; Longhorn `guaranteedInstanceManagerCPU` low; offer "light" mode without Longhorn on 1 node (local PV) only if measurements demand it |
| Two seams take longer than expected | Delays k3s proof | Slices 1–2 are useful alone (testability, cleaner reconciler) |
| Leader template bug runs something unsafe | Security | Admission policy is independent of the leader; golden tests; admission tests in k3d |
| Secrets in the cluster datastore | Widens where secret values exist | `--secrets-encryption`; datastore on root-only disk; ADR-W012 amendment states it |
| Longhorn on 1 node + disk full | Volume read-only, app down | Longhorn space alerts (#131, #134), requests on ephemeral storage (#41) |
| Two-node household thinks it is HA | False safety | Console banner (ADR-W050 §2.8), Plan text |
| Caddy ConfigMap update latency (~60 s kubelet sync) | Route appears slowly | Leader bumps a pod annotation or calls Caddy reload; measure in slice 4 |
| SQLite apps without hooks (~20) | Inconsistent restore | #40 before wave B; Job-based `sqlite` hook already exists |
| Talos and k3s diverge | Two editions to test | One edition per cluster; Talos only after 16 |

### 9.2 Open decisions (owner)

| # | Decision | Recommendation |
|---|---|---|
| O1 | Two seams (D1) | Accept |
| O2 | Typed leader ops, no manifest op (D2) | Accept |
| O3 | Longhorn copies at 3+ nodes | **2** (W050 §5) |
| O4 | Two-node households | Supported, "partial" banner; k3s stays single server (SQLite) at 2 nodes; recommend a third small node for real HA |
| O5 | Caddy copies | One `failover` copy until shared certificate storage is proven (W050 §5) |
| O6 | Jellyfin transcode offload | Defer; evaluate rffmpeg in slice 15 only with a measured need. iGPU on the main pod first |
| O7 | Where secret values live (D3) | Kubernetes Secrets, leader-written |
| O8 | Egress NetworkPolicy | Not in W050 scope; ingress default-deny only |
| O9 | Shared folders without a NAS (D4) | Longhorn RWX on 1 node; recommend a NAS at 2+ nodes |
| O10 | G1/G2 ordering (D6) | G1 and G2 before slice 3 |
| O11 | Chat adapters and agent-hermes ship a host binary | Publish them as OCI images in `release.yml` before wave B |
| O12 | Namespaces | `wildstone-system`, `wildstone-apps`, `wildstone-host` |
| O13 | Test runner (D5) | Owner runs multi-node tests manually; no self-hosted runner |
| O14 | Agent-only nodes as leader | No: only server nodes can lead |

---

## 10. The first three slices in detail

### Slice 0: accepted revision and bounded approval (#128 G1/G2)

- **Why first:** the leader will act on an approval across machines. Today one approved Plan lets every Medium/High step in that tick through (`keystone/reconcile/loop/loop.go:267`), and approvals and file writes are not one transaction (`docs/audit/findings/566-iac-architecture.md:97-98`).
- **G1:** every apply runs against an immutable accepted revision (Git commit ID) of the Statefile, not the working tree.
- **Contract:** [`desired-state-publication-and-approval-contract.md`](desired-state-publication-and-approval-contract.md) already proposes both; slice 0 implements it.
- **G2:** an approval names exact (kind, name, action, spec digest) tuples. The reconciler applies Medium/High steps only when the tuple matches.
- **Done when:** concurrent-approval and crash tests in `keystone/reconcile/loop` prove an unrelated drift step is not applied; e2e and exit drill pass.
- **Size L:** touches Plans, approvals, loop and Statefile; needs careful tests.

### Slice 1: Wardstone runtime seam + `availability` (#151)

- **Scope:** `wardstone/internal/agent/`: a `runtimeBackend` interface with the 13 methods the Docker client exposes today (`docker.go:172-498`: inspect, create, start, stop, logs, pull, remove, stats, volumeInspect/Create, networkInspect/Create, pruneImages), plus the raw `DELETE` at `ops.go:708`. `ops.go`, `seed.go:75`, `diskspace.go:30` call the interface. `keystone/pkgspec/pkgspec.go`: optional `availability: [..]` with lint in `keystone/pkgtool/lint.go`.
- **Recommended additions to #151:**
  - say plainly that the seam is per-node and the cluster-wide seam is slice 2;
  - default rules: device or host network → `pinned`; any Volume → `failover`; no Volume and no state → `replicated, failover`;
  - lint: `replicated` refused when any container mounts a Volume read-write (covers SQLite without parsing images).
- **Done when:** #151's own acceptance list, plus `grep -n "a.docker\." wardstone/internal/agent/*.go` finds nothing outside the Docker backend file.
- **Size M:** mechanical refactor + one schema field + 73 package edits.

### Slice 2: Keystone backend seam + typed cluster ops

- **Scope:**
  - `keystone/reconcile`: split into *diff/approve* (unchanged) and a `Translator` per backend that turns steps into ops. Move Docker-only code (`ensureNodePrereqs`, `specHash`, `containerEnsureOp`, `ingressaddr.go`, host aliases) into the Docker translator.
  - `keystone/reconcile/loop/loop.go:685-696`: add a "cluster" group for resources whose package runs on k3s (empty until slice 4).
  - `keystone/backup`: no change (seam exists at `backup.go:59`).
  - `proto/wardstone/v1/wardstone.proto`: **design only, reserved field numbers** for `ApplyWorkload`, `DeleteWorkload`, `ObserveWorkload`, `WorkloadLogs`, `RestartWorkload`, `RunJob`, `ObserveJob`, `EnsureClusterSecret`, `EnsureClusterConfig`, `CordonDrain`, `Uncordon`, `RunPlaybook`. Each is a typed message; none carries free YAML or a command string.
  - `keystone/internal/fakenode`: a `FakeCluster` that records leader ops.
- **Done when:** reconcile output and Plans are byte-identical on every existing test; a new test shows the Docker translator is the only producer of Docker-shaped ops; proto reserves numbers with comments.
- **Size L:** `reconcile.go` is 2,308 lines with many Docker assumptions.

---

## 11. Proposed ADR amendments

Do not edit the ADRs in this PR. Proposed for the owner:

| ADR | Proposed change | Reason |
|---|---|---|
| **W050 §3 step 1** | "Runtime seam" covers **two seams**: Wardstone-internal (#151) and Keystone translator (slice 2) | Keystone's reconciler is per-node and Docker-shaped ([§3.2](#32-keystone)) |
| **W050 §2.2** | Only **server** nodes may hold the leader Lease | Agent nodes must never hold admin credentials |
| **W050 §2.7** | Add the playbook manifest signature and "typed params per playbook, rendered to a file; no free extra-vars" | Closes a gap where `RunPlaybook` params could become a back door |
| **W003** | New arms (list in slice 2); the rule "no op carries manifests, YAML or commands" | Keeps the no-shell rule meaningful with a cluster API behind it |
| **W012** | On k3s, secret values are delivered as Kubernetes Secrets (encrypted at rest, written only by the leader); the node store remains the source for export/import | Today values only exist sealed on one node (`wardstone/internal/agent/secrets.go`); pods on any node need them |
| **W014** | Installer step "install `ansible-core` and run a signed local playbook" | Already in W050 §2.7; W014 should say it |
| **W018** | Exit drill runs per backend during coexistence | Release-blocker semantics apply to both |
| **W051 §4.1** | Add: Keystone's volume `ReadWriteOncePod`; Keystone bootstrap by the installer, then adopted as a package | Matches [§6.3](#63-keystone-in-the-cluster) |
| **W051 §4.4** | Add "leader elected" to the readiness chain after "k3s ready" | Cluster changes need a leader |
| **W051 §4.7** | Name a default external observer: an off-site uptime check until a second node exists | The open question blocks gate 3 |
| **W051 §5** | Gate 2 adds "no leader" fault | New failure mode under W050 |
