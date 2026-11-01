# W050 migration plan: a minimal product over established tools

- **Status:** revised by owner instruction on 2026-11-01; documentation and backlog reconciliation, not runtime cutover.
- **Authority:** [ADR-W050](../decisions/ADR-W050-multi-node-runtime-kubernetes-engine-host-providers-editions.md), amended in place.
- **First integration:** #254. Existing Docker compatibility and recovery gates remain.
- **Evidence:** repository/issue audit against main fca3e9ca357a1d8e9598c7a36f6cdccc964308f4 plus owner-reported proof commit 6079c5b. No private proof repository or live VM was independently inspected in this revision.

## 1. Purpose

Replace custom application execution and reconciliation with established Kubernetes, Flux, Helm and Traefik mechanisms. Wildstone keeps guided setup, permissions/Plans, application integrations, the household GUI/API and understandable outcomes. Plugins/capabilities are primarily configuration and typed adapters, not arbitrary privileged code.

This is a transition from existing implementation, not a new greenfield scheduler or mass rewrite. Keep working code until its replacement is proven. The historical Docker coupling inventory and original estimates remain available in Git history at the baseline above; they are not a current implementation schedule.

## 2. Completed work and disposition

| Issues / merged PRs | Actual state | Disposition |
|---|---|---|
| #157; #174/#175/#176 | Immutable accepted revision, bounded approval, crash-safe publication merged | Complete foundation; extend publication rather than recreate approval |
| #151; #177/#178; #179/#182 | Docker encapsulation and deployment/device parser/contract merged | Complete original bounded work; adapt contract separately for chart-backed capabilities |
| #158; #184/#185/#188 | Translator/target/typed-operation foundation merged | Complete original scope; do not expand custom cluster transport |
| #159; #189–#200, #207/#208 | Bootstrap, custom leadership/fencing and helper evidence merged | Closed implementation; not acceptance of household migration or HA |
| #210/#217 | Typed leader workload executor merged | Preserve pending tested replacement; target retirement candidate |
| #211/#229 | K3s backend selection/reconciliation merged | Preserve compatibility; replace application path with Flux |
| #215/#241 | Fixed signed host playbook executor merged | Preserve; restrict future use to host responsibilities not delegated elsewhere |
| #230/#238 and #231/#239 | Runtime identity/hardening and platform eligibility merged | Preserve safety semantics, use upstream securityContext/selectors where appropriate |

Closing completed issues records original bounded implementation, not that the new architecture or live gates passed. Do not reopen merged issues merely because a later architecture supersedes their design.

## 3. Backlog reconciliation

Issue numbers remain traceable. Update live bodies and labels; remove agent:ready where architecture changed. Old slice files under w050-slices/ are historical scope until explicitly rewritten; they do not override this plan or the amended ADR.

| Issue | Disposition | Revised scope / release condition |
|---|---|---|
| #151, #157, #158, #179 | Close completed | Linked merged foundations above; no deployment/HA claim |
| #212 | Close not planned in its current form | Bespoke per-app temporary LB/custom leader bridge is superseded; any necessary coexistence bridge is designed narrowly under #168 |
| #214 | Close not planned in its current form | Custom tools-it-tools executor proof superseded by #254; no claim it passed |
| #213 | Block and revise | Product status/logs from scoped Flux/Kubernetes observations; preserve authz/redaction/API parity; coordinate with #254 |
| #216 | Block and revise | Separate K3s controller-driven upgrades from OS/Wardstone updates; select owner before implementation |
| #129 | Block and revise | Track established-tools evidence plus Wildstone integration gaps; remove obsolete no-Flux/Caddy/custom-executor requirements; do not falsely close from partial proof |
| #160 | Block and revise | First application product integration now #254; parent tracks accepted live evidence |
| #161 | Block and revise | Established storage integration and stateful rollout/restore requirements; Longhorn only where justified, not forced into the single-node proof |
| #162 | Block and revise | SOPS/Flux deployment secrets and ConfigMaps; distinguish runtime credential custody, key recovery and continuity |
| #163 | Block and revise | App-consistent backup and established scheduling/execution; preserve exit drill and restore validation, select tools without duplicate controllers |
| #164 | Block and revise | Parent host lifecycle evidence; K3s upgrade controller evaluation, OS and Wardstone updates separately scoped |
| #165 | Block and revise | Traefik access profiles, certificates/internal CA and existing identity integration; bounded per-hostname migration |
| #166 | Block and revise | Wildstone management deployed through standard Helm/Flux; independent recovery and credential/revocation continuity |
| #167 | Block and revise | Device plugins, chart security settings, inventory and locality; preserve supported hardware requirements |
| #168 | Block and revise | Data-preserving adoption/migration and minimal coexistence, explicit ownership/prune rules and rollback retaining new writes |
| #169 | Block and revise | Curated upstream-chart integration waves; no chart forks/mass automatic import; depends on integration/recovery/safety evidence |
| #170 | Block and revise | Product multi-node join/updates/observability using established controllers; remove custom workload-leader dependency |
| #171 | Remain blocked | Replicas/workers only for supported apps after multi-node proof |
| #172 | Remain blocked | Explicit Docker/custom-path retirement after warning, compatibility and replacement restore gates |
| #173 | Remain blocked | Talos separately decided and scheduled; not needed for this transition |
| #180 | Block and revise | Keep unattended recovery, partitions, single-writer storage, least privilege and independent recovery; replace custom leader-specific tests with actual selected controller behavior |
| #240 | Block and revise | Stable URLs through Traefik and Services; LAN/tailnet/optional public connector survival; automatic recovery requirement retained |
| #254 | New, blocked | Approval-bound Audiobookshelf projection/publication/observation; architecture merge and explicit scheduling first |

Adjacent package-audit/Docker compatibility work (#242/#243, #244, #246/#247) is not automatically canceled: it protects current installs or provides useful operational evidence. Do not expand it into a second Kubernetes deployment system. Open PR #245 is existing Docker mount-access safety, not a Flux integration PR.

## 4. Statefile to Flux integration

Flux consumes standard configuration, not Wildstone resource schemas.

1. Proposal snapshots accepted Statefile input and the tested integration definition.
2. Deterministic projection produces standard Flux/Helm resources. Bind reviewed output digest and immutable chart/source identity before approval.
3. Owner approves through existing Plan path; agents cannot approve/apply.
4. Existing durable publication machinery is extended to publish only authorized output to a dedicated deployment branch/path, with expected-parent checks.
5. Flux reads that branch/path and reconciles through scoped Kustomization/HelmRelease ServiceAccounts.
6. Keystone observes exact source revision, generations, Helm outcome and application readiness through its public API.
7. Report distinct accepted, publishing, published, reconciling, ready, failed and unknown outcomes; no inferred success after timeout.

Keep canonical product Statefile and derived deployment output distinct without creating two editable sources of truth. Reuse a single Git repository with isolated branches/paths if suitable; no mandatory new Git service. The first slice must specify remote/local crash recovery and concurrent-edit behavior, not assume a database transaction makes Git push atomic.

Privileged bootstrap, RBAC/admission and key installation are separately owned. Scope chart hooks, source references, dependencies and valuesFrom; Helm configuration can carry substantial authority. No plaintext secrets or model-visible credentials.

## 5. Direct use and ownership

Owners can use Flux directly. Wildstone provides a household GUI, tested integrations and optional automation over standard tools; it is not a replacement Flux API or generic Flux dashboard.

- Separate owner-managed releases/namespaces are supported without Wildstone mutation.
- Managed input edits use normal reviewed proposals; arbitrary deployment output is not reverse-translated.
- External writes to the deployment branch invalidate publication assumptions. Report unreviewed external administration, refuse conflicting publication and require explicit review/adoption or relinquishment.
- Branch permissions and Git credentials are part of approval enforcement; no claim an unrestricted administrator cannot bypass Wildstone.
- Handoff/adoption checks Flux inventories and pruning first; deleting an old declaration must not inadvertently uninstall a live app or delete data.
- Export is standard usable YAML. Flux/apps continue when Keystone is unavailable; new product changes may wait.
- API-level application capabilities remain separate from deployment and use scoped user/service credentials.

## 6. Evidence and acceptance gates

| Gate | Evidence | Must not infer |
|---|---|---|
| A — architecture/backlog | In-place W050/plan revision; completed vs superseded issues recorded; incompatible ready work blocked | Runtime migration or removal |
| B — #254 integration | Deterministic reviewed output; publish crash/retry/conflict tests; denied unauthorized changes; scoped observation; owner-operated synthetic Audiobookshelf change | Broad catalog support or HA |
| C — platform integration | Link existing sanitized identity/backup work; prove required certificate, secret, storage and restore behavior for selected deployment; no automatic rerun of work merely because done elsewhere | Untested combinations or recovery methods |
| D — migration/management | #165/#166/#168; existing-data migration, new-write-preserving rollback, management-independent restore | Fixture success equals actual management recovery |
| E — multi-node safety | #180/#240; partitions, node return, quorum, storage exclusion, unattended core recovery and actual controller restart; measured RTO/RPO | Single-node results or same-host VMs prove physical HA |
| F — supported waves/retirement | B–E where applicable; #169/#170/#172 bounded acceptance, warning and fallback | All charts are supported or mass migration approved |

Single-node/non-HA support can be evaluated separately from HA, but broad migration remains gated on explicit supported topology and recovery acceptance. No release claim silently drops the existing safety gates.

The owner reports proof commit 6079c5b passed Git reconciliation, routing/readiness, replacement, invalid-config recovery, Audiobookshelf persistence/upgrade and isolated application-consistent restore. Host reboot/rebuild/loss, partition, pressure and HA were deferred in that report. Existing off-host durability and identity work should be linked with exact scope rather than repeated or treated as automatically satisfying all platform gates.

## 7. First implementation slice and follow-ups

#254 creates the one product path. It may be implemented as bounded PRs for projection, publication and observation. Use synthetic fixtures; don't copy private proof configuration into the public repository. Repository tests do not authorize production deployment or fault injection.

Before retirement, inventory direct uses of:
- wardstone/internal/leader and typed workload protocol arms;
- Keystone cluster reconciliation/translation;
- custom Caddy route generation;
- package Container/Job rendering and seed/config delivery;
- secret, backup and host-update execution providers.

For each, document callers, compatibility obligations, upstream replacement and deletion gate. Keep application-domain adapters, permissions, audit and useful host inventory. No speculative rewrite of unrelated agent/memory systems.

Additional established-tool evaluations are bounded decisions: certificate lifecycle/internal CA, DNS reconciliation, backup scheduling and OS lifecycle. Pick one owner per operation and quantify removed maintenance. System-upgrade-controller is the first K3s-update candidate; it does not automatically own OS or Wardstone updates.

## 8. Historical traceability and verification

Original slices #157/#151/#158–#173, contracts #179/#180 and PRs remain history. Pre-amendment inventory, numbered gates and custom leader design are available at fca3e9ca357a1d8e9598c7a36f6cdccc964308f4. Older leader-operation/safety docs are not authorization to implement superseded workload execution; retained safety requirements are in the amended W050 and live issue revisions.

This amendment changes documentation and issue governance only. Verify links, issue dispositions, consistent ownership, no contradictory active no-Flux/Caddy/custom-leader requirements in these two documents and no private data. Runtime tests are not applicable until implementation changes.
