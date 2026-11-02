# Wildstone management image and Helm chart

Status: repository-only packaging from #268. This is not a published release,
Kubernetes deployment, management migration, recovery proof, or HA claim.

## Image contents and build

`packaging/management/Dockerfile` builds one OCI image containing:

- the `keystone` executable, built with `CGO_ENABLED=0`, `-trimpath`, and an
  explicit version;
- the production Console build served by Keystone through `-console-dir`;
- the package catalog and theme wallpapers used by the current startup flags;
- the native-agent bundle and its pinned Node v24.21.0 runtime, in the same
  `/usr/lib/wildstone/native-agent` layout as release archives; and
- Git (required by the Statefile), CA roots, and timezone data.

The Docker Official Images base manifests are digest-pinned and support Linux
amd64 and arm64. Runtime/build packages come from a dated signed Debian
snapshot, top-level package versions are pinned, npm uses committed lockfiles,
and the native Node download is checked against the SHA-256 values in
`scripts/build-native-agent.sh`. Native archives now normalize owner, order,
and timestamp. This PR does not add registry credentials or publish an image.

Build and smoke-test a local image:

```sh
docker build -f packaging/management/Dockerfile \
  --build-arg VERSION=0.0.0-local \
  --build-arg VCS_REF="$(git rev-parse HEAD)" \
  -t wildstone-management:local .
scripts/test-management-image.sh wildstone-management:local
```

The final process runs as UID/GID 65532. The smoke test starts the real image
with a read-only root filesystem, all capabilities dropped, and a persistent
Docker volume; it verifies the API health route, Console, native runtime,
restart, retained bootstrap authorization, and absence of a replacement
bootstrap credential. That is synthetic repository evidence, not Kubernetes
storage or recovery acceptance.

## Chart contract

`charts/wildstone-management` renders exactly one Keystone Deployment with a
`Recreate` strategy, one ClusterIP Service, and one dedicated ServiceAccount.
Both the Pod and ServiceAccount disable automatic API-token mounting. The chart
creates no Ingress, RBAC, NetworkPolicy, Secret, ConfigMap, or PVC.

The default image digest is an intentionally nonexistent all-zero sentinel
because no image is published by this slice. A reviewed build digest must be
set before use. Tags are not accepted by the schema.

Important values:

| Value | Contract |
|---|---|
| `image.repository`, `image.digest` | Explicit immutable image reference |
| `storage.runtime.existingClaim` | Required existing claim for Statefile, runtime DB, CA, signing/provider keys, and coordinator/task records |
| `storage.sharedUploads` | Optional, separately named existing claim and mount; no fallback directory |
| `storage.privateWorkspaces` | Optional, separately named existing claim and mount; no fallback directory |
| `configuration.existingConfigMap` | Optional read-only reference mounted under `/etc/wildstone/config` |
| `configuration.existingSecret` | Optional read-only reference mounted under `/etc/wildstone/secrets` |
| `secretEnv` | Individual Secret key references; there is deliberately no broad `envFrom` |
| `startup.extraArgs` | Reviewed non-secret Keystone flags; secret values must not be placed here |
| `resources`, `probes` | Resource bounds and timings, without changing probe semantics |

Enabling either optional data mount without an existing claim name fails Helm
rendering. A missing referenced PVC leaves the Pod Pending; it never produces
an `emptyDir` replacement. The only `emptyDir` is a size-bounded `/tmp` for the
read-only container filesystem.

Chart 0.2.0 has no root container. The Pod, Keystone process, and one bounded
runtime-layout init container use UID/GID 65532; the Pod also sets
`fsGroup: 65532` and `fsGroupChangePolicy: OnRootMismatch`. On a supported
volume implementation, kubelet or the CSI driver makes the volume root
group-writable. This applies to the runtime, `/tmp`, upload and workspace
volumes without giving any workload root or `CHOWN`.

These settings follow Kubernetes' [volume ownership behavior][security-context]
and the [Restricted Pod Security standard][restricted-pss]. CSI drivers that
implement `VOLUME_MOUNT_GROUP` establish the group at mount time; otherwise
kubelet applies supported volume ownership itself.

[security-context]: https://kubernetes.io/docs/tasks/configure-pod-container/security-context/
[restricted-pss]: https://kubernetes.io/docs/concepts/security/pod-security-standards/

Kubelet adds group permissions but does not remove pre-existing "other" bits
from a provisioned volume root. The chart therefore does not put secret state
directly at that root. The non-root init mounts the claim root at `/volume`,
requires it to be empty (or already contain only a valid private `keystone/`
child), and creates that child as 65532:65532 mode 0700. The main container
mounts only the child through `subPath` at the unchanged
`/var/lib/wildstone/keystone` application path. Keystone retains its existing
owner-only chmod check before SQLite opens.

An old-layout file, unrelated entry, symlink, wrong ownership, or wrong child
mode makes the init fail with an explicit migration error. A missing claim
leaves the Pod Pending, and an inaccessible child makes Keystone fail before
SQLite opens. Neither condition can fall back to an image directory or create
a replacement database. SQLite, Statefile, key, credential and recovery paths
remain beneath `/var/lib/wildstone/keystone` unchanged.

`fsGroup` support is a storage compatibility requirement, not a universal CSI
property. Some CSI drivers delegate group mounting through
`VOLUME_MOUNT_GROUP`; other volume plugins apply ownership recursively; some
NAS exports or drivers ignore it. `OnRootMismatch` can reduce repeat scans but
does not override driver behavior. Validate the selected StorageClass with a
fresh synthetic claim before adoption.

Existing data needs a separate migration decision. Chart 0.1.0 placed database
and Statefile content at the claim root; 0.2.0 expects it beneath the private
`keystone/` child. The init deliberately refuses the old nonempty layout.
Quiesce Keystone, capture and independently verify the full management
consistency group, and test a restore alongside before an administrator moves
the complete set into an owner-65532 mode-0700 child. On implementations that
apply ownership recursively, first use can also change group and permission
bits on the outer volume root. Do not enable this chart against an unreviewed
household claim. The chart contains no fallback permission fixer or data mover.

Keystone's real unauthenticated `/api/v1/healthz` is used for readiness. It
means the public API handler is serving; it does not establish coordinator,
identity, deployment-observer, storage-backend, or application health. Because
the route shares the strict anonymous rate limiter, startup and liveness use a
TCP probe so unrelated anonymous traffic cannot restart the management Pod.

Validate the chart with Helm v4.3.1:

```sh
HELM=helm scripts/test-management-chart.sh
```

The script runs lint, schema and deterministic-render checks plus negative
checks for multiple replicas, mutable image references, missing claims,
hostPath, public Services/Ingress, RBAC, cluster credentials, privileged
containers, root initialization, added capabilities, embedded Secrets, and
silent storage fallback. The image smoke test also exercises writable `/tmp`,
non-root layout creation on an fsGroup-shaped 2777 volume root, refusal of a
legacy/nonempty volume, explicit refusal of inaccessible required storage,
native-agent compatibility, restart, persistence and authorization continuity.

## Authority and security boundaries

Helm/Flux own deployment reconciliation only. Statefile intent, capability and
per-person authorization, Owner approval, agents, tasks, automations, lists,
conversations, and execution records remain in their current Keystone-owned
models and stores. None becomes a chart value.

The native agent remains a trusted per-run Node subprocess, not a Service or
second Pod. The adapter constructs its complete child environment from a small
allowlist and the current model credential; `secretEnv` does not become ambient
child environment. It receives no Kubernetes token, Git publication key, host
socket, or filesystem tool. Because it shares Keystone's container and Unix
identity, OS-level isolation from volumes mounted into that same container is
not established by this chart. Introducing a second process identity or
transport is a separate security/architecture decision; live adoption must
review this residual boundary rather than claim container-level separation.

The ServiceAccount is intentionally tokenless. Current cluster observation or
legacy engine authority is not enabled merely by installing the chart. Any
later scoped token/CA file must be an explicitly reviewed Secret reference
under #162/#166, and must never be an automatically mounted administration
credential.

## Persistence, upgrade, and rollback

The runtime claim is the current management consistency group, but it is not
the whole household recovery set. Shared uploads and private workspace bytes
have separate ownership and claims; deployment configuration/local Git,
decryption material, and external application data also retain their own
custody. The chart never deletes claims.

`Recreate` deliberately accepts management downtime and prevents concurrent
SQLite writers during an ordinary rollout. An image upgrade may migrate the
runtime database. Downgrading an image is not a database rollback. Before live
adoption, #166 must define quiescing, complete consistent capture, compatible
restore, revocation continuity, restore-alongside validation, and an
independent administrative recovery path.

## Remaining live gates

Repository validation does not verify:

- a real Kubernetes install, CSI ownership behavior, Service routing, probe
  behavior under kubelet, or measured resource sizing;
- admission under a namespace enforcing the Restricted Pod Security standard;
- Flux install/upgrade/rollback and management-independent operation;
- migration of an existing root-owned runtime claim or real household data;
- SOPS/age custody and rotation (#162), backup/restore execution (#163),
  Traefik/identity/TLS (#165), management recovery (#166), or reviewed
  adoption/cutover (#168); or
- host loss, multi-node behavior, physical failure domains, or HA.

Those gates require separate bounded work. Do not retire the existing
deployment path on the strength of this packaging proof.

The accepted #270 proof used chart 0.1.0 and its root ownership init
containers. Its retained namespace and claims are evidence for that version,
not validation of chart 0.2.0, and must not be reused or changed without fresh
Owner approval. A new isolated proof must enforce Restricted admission, use
new synthetic PVCs, confirm the selected provisioner's actual mount ownership,
exercise SQLite/Statefile/identity persistence across restart, and verify that
an inaccessible required claim prevents startup.

