# Native Wildstone capabilities on Helm and Flux

Status: code-backed inventory, 2026-11-02; repository-only image/chart implementation added under #268 and Restricted Pod Security compatibility under #271. See the [management packaging runbook](../operations/w050-management-packaging.md). No image publication, household migration, recovery acceptance, or runtime topology change is established here.

## Deployment unit versus capability

Deploy processes through Helm; enable product features through the existing capability and authorization model. A capability does not need its own chart, Pod or database. Flux manages declarative deployment configuration, never household runtime records.

The initial first-party chart deploys one Keystone instance, serves the built Console from that instance, and includes the existing native runtime executable. It does not invent microservices or independent feature flags for modules that currently have no such switch. Release publication and live deployment verification remain separate work.

## Code-backed inventory

Paths below are relative to the repository; baseline is main at `7c447da79bcf71a794b3e41a1cb474724250e8fe`.

| Capability/surface | Current owning process and evidence | Configuration and persistent state | Proposed deployment mapping |
|---|---|---|---|
| Management API and Console | `keystone/cmd/keystone/main.go`: public API listener and `console-dir` static serving | Accepted Statefile, runtime DB, keys and administration configuration | One Keystone Deployment, Service and persistent management data; ship Console build in image, not a separate web server by default |
| Household homepage | `packages/homepage-wildstone/package.yaml`; `keystone/api/handlers_portal_home.go` | Package settings, membership and route; application data remains in its owning stores | Configuration-only capability plus route to the existing Service |
| Lists | `packages/lists-wildstone/package.yaml`; `keystone/domestic/lists.go`; portal lists handlers | Household lists in runtime DB; installed capability/membership gates tools and card | No new workload/PVC; preserve shared-household semantics and confirmation tiers |
| Filedrop | `packages/filedrop-wildstone/package.yaml`; `keystone/portal/files*.go`; `keystone/domestic/filedrop.go` | Configured upload root, required-mount check, bounded upload size; shared/private file namespaces | Explicit storage binding on Keystone plus capability settings and route; absence of storage must fail closed |
| Private workspaces | `packages/workspace-wildstone/package.yaml` | Existing private root, mount requirement, per-Agent quotas, revisions/staging/trash | Separate protected storage binding; not automatically mounted into agent runtime containers |
| Assistant/chat surface | `packages/assistant-wildstone/package.yaml`; portal assistant handlers | Surface membership/route only; does not assign a model, Agent or tools | Route/configuration on Keystone; enabling the surface grants no new Agent authority |
| Native reasoning runtime | `keystone/agentruntime/native/adapter.go`, `protocol.go`; `runtimes/native-agent`; main's `nativeRuntime` | Per-run model settings, bounded protocol and brokered tool calls; task state remains in product stores | Current runtime is a Node subprocess launched per run over stdin/stdout, not an independently reachable network service. Package its pinned Node/bundle in Keystone image initially; a separate container would require an explicit transport/lifecycle design |
| Agent work/tasks | `keystone/agentgateway/work.go`; `keystone/butlerstore`; runtime DB | Handles, task ledger, attempts, checkpoints, receipts and coordinator fences | Internal module; durable management storage. Distinguish agent work from household task/commitment UX before defining toggles |
| Deterministic automation | `keystone/automation/automation.go`, `act.go`; main's engine wiring | Approved rules in Statefile; events/decisions/effects in runtime DB | Existing coordinator-gated module, not a Kubernetes CronJob per rule |
| Scheduled Agent subscriptions | `keystone/automation/scheduled.go`; main's subscription wiring | Approved version, Person/source authority, leases/checkpoints and durable run outcomes | Keep existing durable scheduler and authorization; do not duplicate with host cron or cluster CronJobs |

The current management coordinator lease is a runtime-data authority boundary, distinct from the superseded Wardstone Kubernetes workload leader. Retiring the latter must not remove the former.

## Chart and image responsibility

The existing `.github/workflows/release.yml` publishes architecture-specific Keystone/Wardstone/CLI tarballs, Console assets, and a native-agent archive built by `scripts/build-native-agent.sh`. This is not evidence of a published management OCI image. A packaging implementation must define reproducible images for supported architectures, provenance, pinned Node/bundle compatibility, non-root identity and writable paths before chart support is claimed.

Proposed chart resources:

- Singleton Keystone Deployment and Service. Start with a declared-outage update strategy appropriate for the runtime DB; do not raise replicas to claim HA.
- Console files and native subprocess runtime packaged with the image. No Docker socket, host namespace or Kubernetes/Git administrative credentials available to the native subprocess. Preserve the adapter's explicit child environment rather than inheriting all Keystone secrets.
- ConfigMap/Secret references for deployment settings; scoped ServiceAccount only for the observation/publishing behavior explicitly implemented. Flux is the workload writer, not Keystone.
- Management persistence, private workspace and optional shared-file claims/mounts with separate ownership and recovery policies. Never combine private and household data simply to reduce PVC count.
- A fixed non-root UID/GID and Pod `fsGroup` may establish access only on a
  storage implementation that supports Kubernetes volume ownership. A bounded
  non-root init creates a private owner-only `keystone/` child inside a fresh
  claim; Keystone mounts that child at its unchanged data path. It refuses a
  nonempty legacy claim rather than silently initializing beside old data. Do
  not add a root init container or silently adopt an existing claim to
  compensate for an incompatible provisioner.
- Traefik routes and approved authentication integration. Forwarded identity must be accepted only across an explicitly verified trusted proxy boundary; arbitrary pod-CIDR trust is not a replacement for the current checks.
- External identity/model/notification services remain separately deployed through their own integrations. A Helm release does not itself provision LDAP groups; preserve reviewed identity-provider operations.

Readiness, resources, termination handling, probes, migration sequencing, storage identity and network policy must be tested against the actual binary. Do not invent a health endpoint or container port because a chart convention expects one.

## Configuration authority

| Input/state | Authority | What belongs in Git/Helm |
|---|---|---|
| Image, resources, storage bindings, service exposure and runtime installation choice | Reviewed deployment configuration | Pinned chart values and references |
| Package selection, household settings, approved automation rules and access declarations | Existing accepted Statefile/product model | Existing canonical input and derived deployment projection; not a second editable values database |
| People, Agent/model assignment, sessions, tasks, lists, events, execution checkpoints and transcripts | Existing product APIs/runtime stores | Never individual Helm values or Git objects |
| Keys and credentials | Existing scoped custody; SOPS for approved deployment secrets where applicable | Opaque/encrypted references only; private decryption/recovery keys outside Git |
| Uploads and workspaces | Owning storage and access policy | Binding/retention metadata, never file contents |

The implementation must explicitly bridge ConfigFile/Route/identity declarations to the new deployment, retaining the existing capability enablement checks. A chart toggle is not sufficient authorization. It must not overwrite application-owned runtime settings on every reconciliation.

## Disable, upgrade and recovery contract

- Disabling lists, assistant, homepage or filedrop revokes the relevant UI/API/tool access; it does not delete data, Agents, or other features. Current native-runtime fallback behavior remains until separately amended.
- Disabling workspace access leaves workspace bytes and metadata intact. Missing required mounts cannot fall back to empty local directories.
- Quiesce writers and capture the runtime DB consistently, accepted Statefile and required secret/revocation recovery material. Treat upload and workspace data as explicit additional recovery sets; a management PVC snapshot alone is insufficient.
- Treat a change from owner-owned storage to `fsGroup` storage as a migration.
  Confirm provisioner behavior on synthetic data, quiesce and back up the full
  consistency group, then perform an explicitly reviewed ownership migration
  or restore alongside. A chart upgrade is not authorization to recursively
  alter an unreviewed household claim.
- Restore alongside and verify permissions, per-person isolation, deletion/revocation continuity and application data before cutover. Schema migration may require restoring the complete pre-upgrade set rather than downgrading an image.
- Chart uninstall/pruning must preserve data by explicit tested retention policy. Configuration removal is not permission to delete PVCs or external files.
- Coordinator/task leases, scheduled checkpoints and effect receipts must prevent stale or duplicate execution after restart. This requires tests of actual management recovery under #166 and multi-node safety under #180.

The code-backed native-task and deterministic-automation lifecycle, synthetic
fixture boundaries, restart behavior, and restore-alongside limits are recorded
in [the #272 recovery contract](native-task-automation-recovery-contract.md).

## Remaining implementation gates

1. Review #268's image inputs, chart schema, storage identity and repository-only evidence; no runner or registry credential is added.
2. Verify actual Kubernetes/CSI startup, resource sizing, enable/disable behavior, backup/restore and identity integration with synthetic data. No feature relocation is implied by this document.
3. Prove actual management adoption/migration and independent restore under #166; retain Docker/systemd compatibility until then.

Image publication, live chart deployment, migration and recovery acceptance remain open in #259/#166 even after repository packaging is reviewed.
