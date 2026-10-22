# Wildstone terminology

Status: draft glossary, 2026-10-22. Existing code and accepted ADRs remain authoritative. Proposed terms are not shipped interfaces.
Related: [contract draft](design/application-management-contract-v0.1.md).

## Existing concepts

| Term | Definition and source |
| --- | --- |
| Capability | A user-facing function, such as photos or media, implemented by a package profile. Also participates in installed-resource ownership; instance naming follows the schema. See ADR-W005 and `keystone/pkgspec/pkgspec.go`. |
| Profile | A concrete implementation of a capability, such as Jellyfin for media. Profiles are not universally exclusive; exclusivity is explicitly declared. |
| Package | Versioned declarative data describing a capability profile, its parameters, traits, policies, and typed resources. It is not an arbitrary executable plugin. |
| Application | The upstream software being integrated, such as Jellyfin or Immich; it may require multiple components. |
| Instance | A particular installation. The existing instanced-package mechanism creates separately named capabilities using its instance parameter; not every package supports this mechanism. |
| Trait | A reviewed dependency/provision vocabulary entry in `keystone/trait/trait.go`, such as identity or MQTT. Not a synonym for a user-facing capability. |
| Mode | A functional variant of a trait, such as identity:oidc. |
| Class | A nonfunctional trait qualifier, such as storage:bulk. Current class declarations do not establish enforced placement or durability. |
| Export | A provider-declared value made available to consumers through trait resolution and `.Resolved`; credentials remain references. |
| Resource | A typed desired-state object authored directly or produced through package expansion, with explicit ownership and execution constraints. |
| Plan | The reviewable proposed host-state change; human approval remains required under existing policy. |
| Keystone | Wildstone's control-plane component for package expansion, desired-state management, and related public management operations. |
| Wardstone | The node execution boundary using typed operations and managed access; not an arbitrary remote shell. |
| Secret reference | An identifier resolved through the approved secret path, not the secret value itself. Existing node-local restrictions must be preserved. |

| Person | Canonical human identity. Fundamental household roles are Owner and Member; directory accounts, sessions and chat identifiers bind to it. See ADR-W033. |
| Agent / Butler | Same architectural object; Owner/Member class bounds its eligible tools. Runtime choice and actual grants are separate. |
| Principal | Machine/API credential identity with a closed role set; not synonymous with Person role or Agent class. See `keystone/runtimedb/principals.go`. |
| Connection | Person/Agent conversation-route binding governed by ADR-W033 and amendments. Not a blanket grant or a network socket. |
| Wardstone node | Enrolled workload execution environment. May be a Linux guest; not necessarily a physical Proxmox node. |
| Proxmox node | Hypervisor host managed by Proxmox. Several Wardstone nodes may share its failure domain. |
| Host | Context-dependent machine boundary; qualify it as hypervisor, Linux workload node or management guest in deployment docs. |
| Domain adapter | Existing service-domain query/action implementation using integration declarations and the work queue. Distinct from the broader proposed management lifecycle adapter. |

## Proposed management terms

| Term | Definition |
| --- | --- |
| Application Management Contract | Versioned semantics for application management, independent of adapter transport or hosting backend. Draft specification; not an industry standard. |
| Application adapter | Translation from common management operations to supported application-specific behavior, composed with existing declarative machinery where possible. No executable extension mechanism is authorized by this name. |
| Infrastructure provider | Translation and scoped execution for a hosting backend, such as Proxmox or Docker; exact interface remains proposed. Different from a package that provides a trait. |
| Management facet | Optional area of the contract, such as identity or recovery. Avoid calling this a capability where it would conflict with the existing user-facing meaning. |
| Recovery group | Components and data that must be recovered coherently, often an application plus its dedicated database. Not necessarily one container or one guest. |
| Failure domain | Resources subject to a common failure, such as a VM, physical host, power source, or storage pool. |
| Guest | VM or Proxmox-managed LXC environment. An LXC guest is different from Docker installed directly on the Proxmox host. |
| Runtime state | Whether a guest or process is running; does not establish application readiness. |
| Readiness | Whether the application can currently serve its intended operations, based on relevant checks. |
| Liveness | Evidence that the process is responsive; not sufficient evidence that dependencies or application workflows work. |
| Observed state | Timestamped evidence collected from a system; may be stale or unknown. |
| Desired state | The approved target configuration Wildstone reconciles toward. |
| Restart | Stop/start execution without intentionally resetting persistent application data. |
| Repair | An application-specific corrective procedure with declared risks; not interchangeable with restart or restore. |
| Restore | Recover data/configuration from a selected backup, with consistency and version prerequisites and validation. |
| Reset | Ambiguous shorthand; must be replaced by the exact restart, repair, restore, or deletion operation before approval. |
| Protocol | A defined communication format and exchange behavior. The management contract does not yet select a wire protocol. |

## Maintenance

Changes must identify existing versus proposed meaning and update companion specifications when semantics change. Code remains the authoritative closed vocabulary for traits; this glossary does not create a second registry. Resolve conflicts explicitly rather than silently redefining capability, profile, node, provider, or instance. Add implementation-source references as the proposal is audited and accepted.
