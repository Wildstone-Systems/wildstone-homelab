# ADR-W050: Composed homelab platform — Kubernetes, Helm and Flux

- **Status:** accepted direction, amended by owner instruction on 2026-11-01; implementation is incremental and not yet complete.
- **Original acceptance:** 2026-10-24, following #152/#153.
- **Amendment:** owner selected a minimal experience/integration platform over established tools after an independent single-node proof. This replaces the custom application execution direction below; it does not remove working code or approve deployment.
- **Implementation:** [migration plan](../design/w050-migration-plan.md); first integration #254.
- **Related:** ADR-W003, W005, W007/W008, W012, W014, W045, W049 and proposed W051. Existing security/recovery contracts remain binding except where explicitly superseded here.

## 1. Decision and product boundary

Wildstone is a small household platform composed of established services, configuration and capabilities. It provides guided setup, a curated application experience, authorization, approved changes, application integrations and understandable operational status. Established tools perform deployment, reconciliation, scheduling, routing, storage and backup execution.

Do not build a second scheduler, Helm renderer, Kubernetes apply controller or distributed storage engine. Prefer maintained upstream mechanisms; each added dependency must replace a named maintenance responsibility and have a clear owner.

Console, CLI, automation and Butler use the same public API. Agents can propose but cannot approve/apply infrastructure Plans. No arbitrary shell operation is added to Wardstone. Packages remain data by default; typed application adapters are bounded and permission checked.

## 2. Layers and ownership

| Layer | Owner | Wildstone responsibility |
|---|---|---|
| Machines and host administration | Owner; optional Proxmox below Kubernetes | Guided bootstrap, inventory and narrowly scoped host operations |
| Runtime, scheduling, restart and rollout | Standard Kubernetes; K3s on Linux initially | Configure supported policies and explain observed behavior |
| Application packaging | Maintained upstream Helm charts | Pin tested sources; validate household values and compatibility |
| Deployment convergence | Flux source/kustomize/helm controllers | Publish authorized configuration and observe results |
| HTTP/HTTPS ingress | Bundled Traefik | Access-profile configuration and tested identity integration |
| Floating addresses | kube-vip where required | Supported topology and exposure configuration |
| Persistent app storage | Longhorn where justified; local storage in limited single-node proofs | Capacity, locality, backup and recovery contracts |
| Large shared libraries | Optional NAS through NFS CSI; local storage otherwise | Explicit bindings and locality limits |
| Identity | Authelia and LLDAP | Onboarding, mappings and per-app integration; gateway login is not app authorization |
| Deployment secrets | SOPS/age with Flux, subject to custody/recovery verification | Opaque references, encryption and rotation workflow |
| Runtime user/app credentials | Existing scoped credential boundaries until separately amended | Keep credentials out of Git plaintext, model output and logs |
| Backups | Restic plus established application backup mechanisms | Consistency groups, policy and validated recovery |
| K3s upgrades | Evaluate system-upgrade-controller before new custom execution | Approve pinned targets and validate compatibility |
| OS/device/host operations | Fixed local Ansible/systemd or another explicitly selected host provider | Retain narrow typed Wardstone operations where necessary |
| Product | Keystone, Console/CLI and application/agent capability adapters | Household intent, authorization, integrations, audit and explanation |

SOPS does not replace all credential custody. Certificate issuance/internal CA, DNS reconciliation, backup scheduling and OS lifecycle ownership require bounded decisions before replacing existing providers. Prefer established controllers where they reduce total integration burden. No automatic addition of cert-manager, external-dns, external-secrets, Velero or another platform is implied.

Keystone owns product intent and approval; Flux owns application reconciliation; Kubernetes owns scheduling. Wardstone is not a competing application reconciler. Bootstrap/break-glass administration stays separate from runtime authority.

Retained enforcement requirements: ordinary runtime credentials cannot change admission/RBAC or mint privileged infrastructure grants. Privileged pods, host paths and host networking require explicitly reviewed infrastructure exceptions; chart adoption cannot silently bypass them. Agent-facing interfaces never expose pod exec/attach or cluster credentials. Direct owner/break-glass administration is a separately authorized path, not an agent capability. Fixed signed host playbooks remain local, selected by name/version with closed typed parameters; no user-supplied playbook, free extra-vars or cross-node administrative credentials are introduced.

## 3. Local configuration and standard deployment output

### Local-first configuration boundary — owner correction, 2026-11-01

The public Wildstone source repository contains software, documentation and synthetic fixtures only. Each installation keeps its Statefile, deployment repository, household settings, secrets and deployment identifiers on local storage, outside the source checkout. No external Git remote, GitHub account, Gitea deployment or Internet configuration-publication service is required. Installation checks for Git and installs the supported local prerequisite if absent; it never initializes deployment configuration inside the source checkout or inherits its remotes.

Git publication means promoting exact approved output to the locally served deployment ref. The canonical product Statefile and derived output may share an installation-owned repository with isolated refs, or use separate local repositories; neither may use the public source checkout. The deployment repository has no external push remote by default. No automatic push, synchronization, telemetry upload or backup of deployment details to GitHub is allowed.

Flux needs a supported source transport; a working directory or file:// URL is not a GitRepository source. #257 must integrate an established local read-only Git endpoint reachable by source-controller. Evaluate existing Git/OpenSSH upload-pack support with a restricted read-only account/key before adding a full Git hosting platform; HTTP(S) is another supported transport if separately selected. No arbitrary shell access, receive-pack/write access, public listener or unauthenticated exposure is implied. Verify pod-to-endpoint connectivity, access restrictions and endpoint independence from a running Keystone process. Avoid a custom Git protocol server. Protect the served repository from Kubernetes/application writes.

Configuration promotion is a local commit/ref update with expected-old-ref comparison, durable journaling and restart recovery; a network Git push is not required. Acknowledgment loss between local promotion and product DB recording still requires reconciliation. Flux only reads the promoted ref; partial/unapproved trees must never be visible through it. Owners' direct local ref changes are external administration and must be detected.

This is local configuration/control, not a claim of air-gapped artifact installation: acquiring public charts, images and updates may still use upstream registries. Never upload local deployment files to obtain those artifacts. Fully offline artifact caching is separate work.

### Standard projection

Keep Statefile household, capability, policy and audit semantics. Do not ask Flux to interpret proprietary Wildstone resources. Extend the existing accepted-revision/publication foundation to produce a narrow deterministic deployment projection containing ordinary Flux Kustomizations, HelmReleases, validated values and approved source/encrypted-secret references.

- Statefile is canonical product input; deployment YAML is derived, exportable output, not a second independently editable product database.
- Review binds the source revision, integration/renderer version, output tree digest, chart artifact identity and configuration/secret-reference versions. Pin immutable artifacts or verify content digests; a version string alone does not prove unchanged bytes.
- Flux reads only an explicitly managed deployment branch/path, never a developer working tree or an unaccepted Statefile branch. Separate privileged bootstrap configuration from ordinary application output.
- Only publish an approved projection. Journal prepared, accepted, locally promoted and Flux-observed revisions. Use expected-parent compare-and-swap, idempotent operation IDs and recovery after uncertain Git outcomes; never force-push over owner changes.
- Track publication, Flux source revision, Kustomization/Helm generation and application readiness separately. A promoted commit or Helm Ready alone is not proof of application usability.
- Flux and Helm reconciliation use scoped ServiceAccounts; ordinary application publication cannot modify cluster RBAC, admission, bootstrap or arbitrary namespaces. Verify chart dependencies, hooks, valuesFrom, post-rendering and decryption permissions as part of the authority boundary.
- Destructive pruning/uninstall, persistent-volume retention and chart-generated resource-name changes must be visible in review. Validate deletionPolicy, Helm uninstall behavior and PVC ownership before publication or handoff; an ordinary upgrade must not silently authorize data deletion.
- No chart auto-updates/image automation on the managed path may bypass review. Approved settings define ongoing runtime convergence; ordinary controller retries do not require a new approval for each attempt.
- Keystone needs scoped Git publication and read-only operational observation, not cluster-admin or direct workload apply.

Local Git refs and the runtime database are not one atomic transaction. Explicit recovery of crash-before-promotion, promotion-before-DB-ack, local ref conflict and superseded approval is required in #257. Local repository ownership, file permissions and read-only serving credentials enforce the normal approval route; hosted branch protection is not a required dependency. An unrestricted administrator can bypass it and must not be described as contained.

## 4. Direct Flux and power-user ownership

Owners may use standard Flux, Helm, Git and Kubernetes tools. Wildstone is a household interface and integration layer, not a fork of Flux or a mandatory gateway to its APIs.

- Wildstone-managed instances have an explicit namespace/release/path inventory. Separate owner-managed releases are supported and not overwritten.
- Changes to managed product input enter the normal proposal/review path. A privileged direct deployment-ref update is an external administrative change, not implicit Wildstone approval: report divergence, stop automatic publication on conflict and require adoption/review or relinquishment.
- Arbitrary deployment YAML is not automatically reverse-translated to Statefile. Unsupported external configurations remain owner-managed.
- Adoption/relinquishment is explicit and reviewed, with data retention and Flux inventory/pruning behavior checked before handoff. Two reconcilers must never own the same object.
- Live edits to Flux-owned objects may be reverted by Flux. Durable owner edits belong in the relevant Git source.
- Exported output remains ordinary usable Flux/Helm configuration. Wildstone unavailability must not stop running apps or Flux reconciliation. New proposals and Wildstone capability actions may be unavailable.
- Existing break-glass protections and auditing apply. Direct owner administration does not grant agents cluster credentials or approval rights.

## 5. Capabilities and household experience

A catalog entry references an upstream chart and adds tested configuration, household-facing inputs, identity integration, storage/dependency requirements, safe deployment capabilities, backup/restore procedures and optional typed application API tools.

Reuse the accepted deployment/device capability semantics (#179/#182) where applicable; do not blindly copy the old Container renderer schema into a new Helm wrapper. Any semantic change is an explicit schema/contract amendment. Chart installability is not supported integration, and deployment support is separate from automation support.

The Console is a household GUI, not a complete Flux administration dashboard. Status should aggregate upstream facts with timestamps, readiness/dependency distinctions and actionable explanations. Application adapters remain necessary for actions such as searching a library; Helm only handles deployment.

## 6. Recovery and availability

### Sequence: operate locally, integrate optional backups later

Get the local approval -> Git promotion -> Flux -> application readiness path operating first (#257/#258). New backup destination setup or recovery implementation is not a prerequisite for that synthetic non-destructive development proof. Preserve existing data and backup behavior; deferring integration is not permission for destructive migration or a recoverability/HA claim.

The existing S3 endpoint work has been built/tested according to the owner. Reuse it later to back up installation configuration/history, application data and required recovery material with appropriate consistency/encryption. The backup endpoint is optional and must not be Flux's authoritative deployment source or required for normal operation. Hetzner support is a later separately selected adapter; determine object-storage versus Storage Box protocol requirements then rather than assuming all Hetzner services are S3.

Local Git history is not off-host backup: host/storage loss can remove both live configuration and history. Retain independent-recovery requirements for later supported migration/release acceptance, with keys stored separately from encrypted backup data. No GitHub configuration backup is part of this design.


Preserve W051/recovery requirements: management-independent recovery, off-host keys/instructions, application-consistent recovery groups, restore alongside, no destructive reset, truthful stale/unknown status and external failure observation.

The owner's 2026-10-29 unattended single-machine-loss requirement (#180/#240) remains. Supported HA requires surviving server quorum, available storage, eligible capacity and independent physical failure domains. Automatic writable takeover requires verified exclusion/fencing or application-native replication. Node NotReady, a Lease, RWOP or a timeout alone is insufficient. Stop safely and alert when ownership is uncertain.

Replacing the custom Wardstone leader does not remove storage fencing requirements. Multi-node acceptance must exercise actual Flux/controller recovery and Git/identity/certificate/storage dependencies; tests specifically for retired custom leader epochs become historical evidence rather than target requirements.

Several VMs on one physical host do not prove host/power HA. The owner-reported single-node proof establishes only its reported tests.

## 7. Migration and implementation status

Merged foundations include #174–#178, #184/#188; engine bootstrap and custom leadership #189–#200; QEMU/resource evidence #207/#208; typed workload execution #217; backend reconciliation #229; signed RunPlaybook #241.

Keep existing Docker and custom k3s code operational until bounded replacement and migration tests authorize retirement. Do not delete merged work as part of this documentation amendment. Custom workload apply/leader code is a retirement candidate after Flux replacement, not a foundation to expand.

The owner reports an independent established-tools proof at commit 6079c5b covering K3s, Flux, SOPS, Traefik, Podinfo and Audiobookshelf reconciliation, persistence, update and isolated restore. It is not evidence of Wildstone approval integration, host rebuild, multi-node safety or production acceptance. Separate backup-durability/identity work was reported by the owner; link sanitized evidence before claiming particular gates passed. Do not publish private environment details.

Execution order:
1. Reconcile backlog and preserve completed foundations.
2. #254: one approved Audiobookshelf projection -> Git -> Flux/Helm -> observed readiness.
3. Rework ingress/identity, storage, credentials and backup integration around established owners.
4. Prove safe migration and independent management recovery.
5. Multi-node safety and unattended-recovery evidence before broad supported migration/HA claims.
6. Explicit retirement after compatibility, data-preserving rollback and recovery acceptance.

No production migration, new host provisioning, destructive test or merge is authorized by this ADR alone. Existing owner-directed experiments on a development VM are limited to their authorized non-disruptive scope. Fault tests require an explicitly selected safe target.

## 8. Historical decision record

The 2026-10-24 decision selected Kubernetes but excluded Flux and retained a Wardstone application leader, fixed workload rendering, Caddy ingress and custom application convergence. Those execution choices are superseded by this amendment; security and recovery requirements are retained above.

The pre-amendment text, including A1/A2, is preserved in Git history at fca3e9ca357a1d8e9598c7a36f6cdccc964308f4. Linked older leader/safety/slice documents describe the old implementation where they conflict with this ADR. The migration plan records their disposition. Talos remains deferred and requires a separate decision before implementation.
