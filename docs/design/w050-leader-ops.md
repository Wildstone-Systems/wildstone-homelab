# W050 leader operations (provisional)

- **Issue:** [#158](https://github.com/Wildstone-Systems/wildstone-homelab/issues/158), W050 slice 2b
- **Status:** **provisional draft** for owner review. No proto field, generated code or runtime behaviour is authorized here. Field numbers are not reserved.
- **Evidence base:** `main` after #184, 2026-10-25. Wardstone protocol minor 27; the last `Operation` arm is `home_automation_readiness = 38` and the last `OpResult` arm is `= 40`.
- **Depends on:** the authority, storage and secret contract in [#180](https://github.com/Wildstone-Systems/wildstone-homelab/issues/180) before **any** operation here is implemented, and [#129](https://github.com/Wildstone-Systems/wildstone-homelab/issues/129) evidence before the shape is frozen ([gates](w050-migration-plan.md#revised-execution-gates), G1 and G4).
- **Governed by:** ADR-W003 (typed operations, no `Exec(string)`), ADR-W050 §2 and A1, the [package deployment capabilities contract](package-deployment-capabilities-contract.md).

This document answers one question: **what does Keystone ask the Wardstone leader to do, once a cluster exists?** Today Keystone sends Docker-shaped operations to each node. Since #184, the reconciler builds all of them in one place, its Docker translator (`keystone/reconcile/docker.go`). A cluster backend is a second translator that sends the operations below to the leader.

## 1. Rules every operation keeps

1. **Typed fields only.** A request names *what* to run, using fields that mirror `resource` types (image, env, mounts, ports, limits, probes). It never carries a manifest, YAML, JSON patch, shell command, Helm values or free Ansible variables. A "send this manifest" operation is `Exec(string)` in disguise, which ADR-W003 forbids.
2. **The leader renders fixed objects.** Wardstone turns a typed request into Kubernetes objects from templates compiled into its binary. It applies them with server-side apply and one fixed field manager. Packages never reach the object shape directly.
3. **Every write carries its authority.** Each mutating request includes:
   - `accepted_revision`: the Statefile commit it was planned from (ADR-W007 A8);
   - `plan_id` and `step_digest`: the approved step it executes (ADR-W008 A2a);
   - `leader_epoch`: which leadership term sent it.

   *How the leader rejects a stale or replayed write is #180's to define. A Lease alone is not fencing (ADR-W050 A1). This draft only reserves the fields.*
4. **Idempotent by design.** Applying the same request twice gives the same objects. `op_id` (already on every operation) identifies a retry.
5. **Results are not Docker types.** A cluster result reports Kubernetes-level facts: generation, observed and ready replicas, conditions, and the node a pod runs on. The reconciler's health gate reads those, not `InspectContainer`.
6. **Host work stays host work.** Managed files, mounts, probes, block devices and radios remain per-node operations to that node's Wardstone (ADR-W050 §2.2). The leader does not proxy them.

## 2. Operations

Each row is one request and one result. Names are working names.

| Operation | Replaces (Docker) | What it does | Writes? |
|---|---|---|---|
| `ApplyWorkload` | `EnsureContainer` (+ `PullImage`, `EnsureDockerNetwork`) | Create or update one component as a Deployment (or a StatefulSet, if #129 shows it's needed) from a typed `WorkloadSpec` | Yes |
| `DeleteWorkload` | `StopContainer` + `RemoveContainer` | Remove a component's objects. Never its volumes (ADR-W008 volume rule) | Yes |
| `WorkloadStatus` | `InspectContainer`, `ContainerStats` | Generation, ready/updated replicas, conditions, pod node, restart count, image digest actually running | No |
| `ApplyVolumeClaim` | `EnsureVolume` | A PersistentVolumeClaim with the storage class D4 and the capability contract select (Longhorn RWO, NFS share, or a local volume) | Yes |
| `RunJob` / `JobStatus` | `EnsureContainer` for a Job's run + `InspectContainer` polling | A Kubernetes Job from a typed `JobSpec`; status reports completion and exit code | Yes / No |
| `ApplyConfig` | `WriteManagedFile` for ConfigFiles read by containers | A ConfigMap holding rendered config, mounted by name | Yes |
| `SeedVolumeFile` / `StatVolumeFile` (cluster form) | the Docker volume-file ops | Write a first-run file into a claim through a short-lived Job | Yes / No |
| `ClusterFacts` | `HostFacts` (cluster view) | Nodes, their labels (`gpu`, `has-zigbee`), readiness, versions, storage health | No |
| `RunPlaybook` | none (new) | A signed, versioned host playbook with typed params per playbook (ADR-W050 §2.7). Sent to the target node's Wardstone, not through the leader | Yes |

**Secrets have no row yet, on purpose.** Where secret values live on k3s (W050 D3), how they move between leaders, and how keys are recovered are #180's secret-custody contract. Until that's accepted, there's no `ApplySecret`. A missing local store must never regenerate an existing secret.

## 3. `WorkloadSpec` (sketch)

The fields map one-to-one from `resource.ContainerSpec` (#41 limits and runtime user, #39 probes) and from the package's deployment declaration:

```text
WorkloadSpec
  name, image (digest-pinned), command, env (secret refs stay refs)
  ports[]           -> Service ports; host ports only where the spec has them today
  mounts[]          -> claim, config or allowlisted host path by reference
  devices[]         -> device-plugin resource or allowlisted path, plus node selector
  resources         -> requests/limits (W051 QoS rules)
  security          -> runAsUser, readOnlyRootfs, capAdd/capDrop, noNewPrivileges
  network_mode      -> host networking only where the spec has it today
  readiness         -> probe (#39)
  placement         -> node selector / affinity, derived from the deployment capabilities
                       and the household policy (contract §8), never chosen by the package
  replicas          -> 1 unless policy allows more for a copies: many component
  strategy          -> Recreate for one-copy components (declared outage); rolling only for copies: many
```

Placement and replicas come from the household policy bounded by the package's declaration. A package can never pick its own replica count (contract §2).

## 4. What stays the same

- The Plan is the same Plan. The diff, risk, approval and step order don't change with the backend. Only the translator differs (#184).
- The Docker translator and Docker operations stay, unchanged, for every node that isn't in a cluster.
- Secrets are still not readable through any operation. `ExportSecretBundle` keeps its own rules.

## 5. Open questions (for #180 and #129)

1. **Stale writes:** where does the leader check `leader_epoch` and `accepted_revision` before applying (a Lease annotation, a ConfigMap compare-and-swap, or an admission check)? What happens to work already in flight when leadership moves?
2. **Delete preconditions:** what proves a `DeleteWorkload` targets the generation the Plan saw?
3. **Deployment or StatefulSet** for one-copy components with a claim? #129's Uptime Kuma run decides.
4. **Status polling vs watch:** is a polled `WorkloadStatus` enough for the health gate, or does the leader need to stream rollout events?
5. **Volume files:** is a short-lived Job the right way to seed a claim, or should the claim's first mount do it?
6. **Numbering:** reserve `Operation` and `OpResult` arm numbers only when the first operation is implemented, after re-checking the proto at that time.

## 6. What happens next

1. The owner reviews this draft.
2. #180's contract answers questions 1 and 2 and the secret row.
3. Then: a cluster target in the reconciler and a `FakeCluster` recorder that records these typed requests (slice 2c, no real client).
4. The first real operation (`ApplyWorkload`) is implemented in slice 3/4, against #129's single-node proof.
