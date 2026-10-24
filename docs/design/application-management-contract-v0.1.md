# Wildstone Application Management Contract — draft 0.1

Status: proposed; design review only. Date: 2026-10-22.
Baseline: repository main at 60df9dcbc1daa99d43f7692eb83f7d72f7070956.
Companions: [glossary](../glossary.md), [current-state comparison](application-management-contract-v0.1-comparison.md).

Originally drafted in private-repository PR #609; carried into the public repository and revalidated against the baseline above. The old source commit is historical provenance, not the current implementation baseline.

## Purpose and authority

Describe a consistent application-management surface across hosting environments, grounded in existing Packages, traits, typed resources, Plans, and public APIs. This document does not introduce executable YAML fields, a wire protocol, an adapter executor, or a new source of desired state. Existing accepted ADRs and validation rules remain authoritative. In particular ADR-W005 (amended by ADR-W033), ADR-W003, and CONTRIBUTING.md retain their approval, execution, and secrets boundaries.

Contract version, Package schema version, package release, and upstream application version are distinct. Draft 0.1 is not a supported runtime API version. A future implementation must define compatibility negotiation, validation, migrations, and conformance tests before making interoperability guarantees.

## Motivation and deployment boundary

The operator reports Wildstone and managed services sharing one VM. Guest failure removes application access, management, and much of the diagnostic visibility together. Following a power outage, the VM returned without essential access services recovering correctly. These are operator observations, not independently reproduced failures; unavailability does not establish data loss.

The proposed [ADR-W051](../decisions/ADR-W051-proxmox-first-deployment-and-recovery.md) evaluates an independent management guest and application recovery groups, VM versus LXC placement, startup dependencies, host power recovery prerequisites, and external monitoring. This contract does not select that topology or authorize migration. A shared host or storage pool remains a shared failure domain.

## Required foundation and optional facets

Every integration must identify its application and installed instance, compatible versions, resource ownership, supported management operations, and a way to report observed status or explicitly report it unknown. It must declare persistent-data ownership or explicitly state that it owns no persistent data.

Optional facets are identity, storage binding, configuration, network access, hardware access, backup/restore, telemetry, and events. Each has an explicit support state: supported, unsupported, or unknown, with version-specific limitations. Supported is not permission to execute. Missing optional facets must not silently become assumed support.

This management contract covers the operational lifecycle. Existing `integrates`, `answers`, `agentTools` and `publishes` declarations already describe service-domain queries/actions/events; their runtime lives in `keystone/integration`, `keystone/adapter` and the existing gateways. Reuse them where applicable. The older [catalog integration design note](../catalog-integration-contract.md) contains historical proposals and must be read against current code, not treated as proof those paths are absent.

Application adapters translate application-specific behavior; infrastructure providers translate typed infrastructure operations. An adapter may be data-driven using existing machinery. This proposal does not require executable plugins or changes to third-party applications.

## Proposed facet semantics

| Facet | Information and guarantees |
| --- | --- |
| Identity and compatibility | Stable application, package, profile, and instance identities; upstream version constraints; contract compatibility; supported facets and limitations. |
| Dependencies and placement | Required versus optional traits; component relationships; private service addresses; locality constraints; resource and architecture requirements. A component is not automatically a separate guest. |
| Storage | Named roles for configuration, database, library, downloads, cache, and backup staging as applicable; ownership; persistence; read/write access; shared access; capacity expectations; backup inclusion or explicit exclusion. Logical bindings resolve to deployment locations without changing application semantics. |
| User identity | Supported OIDC, LDAP, proxy authentication, or application-local methods; provisioning and role-mapping limitations; native-client and API compatibility; emergency access policy. Declaring identity support is not proof it is wired correctly. |
| Secrets | Opaque references, credential scope, issuing authority, location, rotation limitations, and revocation behavior. No plaintext credentials in manifests, status, logs, events, or model-visible output. |
| Configuration | Validated parameters, defaults, secret references, managed versus application-owned fields, reload/restart requirements, and migration implications. No competing controller may silently overwrite application-owned settings. |
| Network and hardware | Internal and user-facing endpoints, exposure policy, protocols, discovery requirements, GPU/radio/device access and privilege requirements. Enforcement remains in typed providers and policy, never arbitrary host commands. |
| Health and lifecycle | Guest/process state, application readiness, dependency state, and last observation time separately. A running guest is not proof of a usable application. Checks have bounded timeouts and explicit unknown/stale/error results. |
| Backup and recovery | Data consistency group, dump/quiesce requirements, exclusions, staging and repository locations, retention, supported restore versions, prerequisites, and post-restore validation. Snapshot or successful copy alone is not proof of recoverability. |
| Events and observability | Versioned event identities, instance provenance, audience policy, timestamps, correlation and operation identifiers; protected log/metric access. Consumers must tolerate duplicates and gaps. Delivery, reconciliation, and retention mechanisms remain open. |

## Operation contract

Each supported action must declare: target scope, validated inputs and outputs, required authority, whether it mutates state, approval requirements, expected impact, timeout, retry safety, concurrency restrictions, and supported application versions.

Long operations need durable identifiers and states sufficient to distinguish pending approval, queued, running, succeeded, failed, and unknown outcome. Cancellation and retry are supported only when explicitly safe. Status must distinguish unsupported, unauthorized, unavailable dependency, validation failure, conflict, and application failure. An observation timeout must not imply that a mutation failed to occur.

Installation, configuration change, start/stop/restart, upgrade, backup, repair, restore, and deletion are different operations. Support is explicit per integration; generic repair must never mean erase data.

Console, CLI, automation, and MCP must call the same authorized management layer. Adapters do not independently approve or apply Plans. Packages remain data. Existing rejected lifecycle hooks do not become executable through this proposal.

## Recovery guardrail

Detection can produce diagnostics and a proposed Plan. Existing Plan approval rules and separately scoped, opt-in host-action confirmation rules remain unchanged; this draft creates no new execution or approval authority. Existing declared runtime restart policies are distinct from new Wildstone-initiated remediation.

Bounded automatic remediation is a future decision requiring an explicit authority model, retry budgets, backoff, dependency checks, durable evidence, and escalation. No approval bypass is authorized here. Corruption must stop blind restart loops and preserve evidence; destructive repair or restore requires explicit review of its data consequences.

## Ownership and failure behavior

Wildstone owns declared desired state; application adapters own application-specific translation; infrastructure providers own approved execution within their scope. Exact provider interfaces and process placement require review of existing Keystone/Wardstone boundaries.

Management unavailability should not itself stop already running applications. Recovery must not depend solely on the service being recovered: document bootstrap access, essential identity/network/storage dependencies, and externally observed host failure. Provider operations need scope-limited credentials and an ownership check before changing resources.

## Review and acceptance gates

1. Reconcile terms and facets with the existing Package schema and trait registry using the companion comparison.
2. Inspect each representative package and its application version; distinguish declaration, adapter behavior, and verified live behavior.
3. Select an incremental schema extension only where existing structures cannot express the requirement; do not ship a parallel manifest or controller.
4. Define schemas, error models, compatibility rules, permission tests, and conformance fixtures before implementation readiness.
5. Prove installation, health failure detection, approved restart, management outage independence, and backup/restore for one application recovery group in an isolated environment.
6. Review Proxmox placement and any automatic-remediation authority in separate decisions.

This draft authorizes neither feature removal nor a hosting migration.
