# Application management contract 0.1 — baseline comparison

Status: initial static comparison, not a live deployment audit.
Baseline: 60df9dcbc1daa99d43f7692eb83f7d72f7070956, 2026-10-22.
Related: [contract](application-management-contract-v0.1.md), [glossary](../glossary.md).

## Sources and limits

Carried from old private-repository PR #609 and refreshed against the public baseline above. Inspected ADR-W005/W033, CONTRIBUTING.md, the package catalog, `keystone/trait/trait.go`, `keystone/pkgspec/pkgspec.go`, `keystone/capability/capability.go`, the resource registry, `keystone/integration/integration.go`, and the package manifests linked below. Read backup engine/drill and Agent host-tool evidence as recorded in the [documentation review](documentation-refresh-2026-10-22.md). Manifest inspection establishes declarations, not working application adapters, successful deployment or recoverability. No runtime, restart, fault injection, or restore tests were performed.

## Current model versus intended contract

| Area | Current evidence | Proposed next step / unresolved question |
| --- | --- | --- |
| Package identity | Existing schema identifies package version, capability, profile, exclusivity, and optional instances. | Preserve these meanings; define application/contract compatibility separately. |
| Dependency composition | Closed trait registry, modes, conditional requirements, provider exports and intrinsic resolved values already exist. | Extend reviewed vocabulary only for proven requirements; no parallel dependency registry. |
| Storage | Volume resources and managed paths; bulk class is explicitly declarative. | Define logical roles, consistency groups, capacity semantics and deployment bindings without promising enforced storage classes today. |
| Identity | OIDC and LDAP traits; catalog distinguishes app authentication, proxy authentication, and local credentials. | Record actual provisioning and client compatibility; do not equate registered OIDC client with working application login. |
| Secret locality | Engine and trait comments identify node-local secret constraints, including MQTT consumers. | Guest separation needs an explicit credential-distribution design; do not assume an integration can move across nodes unchanged. |
| Networking/locality | Existing MQTT, Matter and Z-Wave exports encode real address and same-node restrictions. | Evaluate host-network discovery, loopback services and placement before moving any dependent guest. |
| Configuration | Declared params and rendered resources/configuration; some app-owned authentication setup remains a catalog limitation. | Define ownership and supported provisioning path; do not create an arbitrary script executor. |
| Health | Runtime restart policies appear in inspected manifests. These declarations alone do not prove application readiness. | Audit observer behavior and define timestamped readiness, dependency and unknown states. |
| Backup | Inspected packages declare BackupPlan resources, included volumes, retention and secrets; Immich declares a pg_dump pre-backup step. | Audit execution and restoration separately; establish complete consistency and post-restore verification. |
| Automatic remediation | Infrastructure proposals use Plans; the existing opt-in Owner host-action family uses separate tier/confirmation checks. Declared Docker restart policies are another mechanism. | Reuse existing authority. A generic health-driven retry policy needs an explicit decision and cannot inherit permission from a support declaration. |
| Service-domain integrations | `integrates`, `answers`, `agentTools`, `publishes`, the domain registry and adapter work queue already exist. | Extend only operational semantics that cannot be represented today; do not replace domain adapters or add a parallel registry. |
| Common access | Console, CLI and Butler must use the same public API. | Reuse this boundary for application management and MCP; adapters must not create private bypasses. |

## Representative application declarations and review questions

These are contract design questions, not promises of upstream features or claims that all listed behavior is already implemented.

| Application | Current evidence | Contract questions to resolve |
| --- | --- | --- |
| Immich | [photos-immich](../../packages/photos-immich/package.yaml): dedicated PostgreSQL/cache, upload and backup-staging volumes, OIDC and `identity_node`; BackupPlan includes upload and database dump via `pg_dump`. | Validate server/database versions, coherent media/database cutover, restored login and application readiness. |
| Jellyfin | [media-jellyfin](../../packages/media-jellyfin/package.yaml): config/cache volumes, read-only media binding, LDAP posture and `identity_node`; BackupPlan includes config, excludes library/cache. No device mapping in this manifest. | Declare library backup ownership and manual LDAP setup. Hardware schema support alone does not establish GPU enablement in this package. |
| Home Assistant | [home-assistant](../../packages/home-assistant/package.yaml): deployed app, host networking, optional same-node MQTT, Bluetooth choices, serial device input and config BackupPlan. [home-automation](../../packages/home-automation/package.yaml) is a separate config/event adapter for an existing HA. | Preserve discovery, radio ownership, controller backups, MQTT secret locality and external HA boundaries; do not migrate the adapter as though it deployed the app. |
| Uptime Kuma | [monitoring](../../packages/monitoring/package.yaml): persistent data volume, container restart policy and admin route; no BackupPlan declared. | Decide monitor configuration backup and restore. Place host-loss observation outside the monitored physical host; guest state is not endpoint health. |
| ntfy | [notifications](../../packages/notifications/package.yaml): persistent cache volume, `serve` command, restart policy and route; no BackupPlan declared. | Decide persistent state/config and credential coverage; separate management credentials from notification delivery; test delivery during the relevant outage. |
| Prometheus | [metrics-prometheus](../../packages/metrics-prometheus/package.yaml): persistent data, managed scrape config and retention parameter (default 30d); no BackupPlan declared. | Bound capacity and scraping scope; decide whether historical metrics are disposable. Independent monitoring is not provided by retaining samples on the failed host. |
| Grafana | [metrics-grafana](../../packages/metrics-grafana/package.yaml): data volume, generated admin secret, provisioned datasource, OIDC registration; default Prometheus URL is same-node; no BackupPlan declared. | Preserve user-owned dashboards/database, account recovery and datasource credentials; audit identity-node placement before splitting guests. |
| Gitea | [code-gitea](../../packages/code-gitea/package.yaml): persistent data, required SSH listener address, app login; BackupPlan includes data and SQLite backup staging with a typed SQLite hook. | Verify repositories/database/settings consistency and restore ordering; test Git SSH/HTTPS independently of browser SSO and preserve source ownership. |
| Audiobookshelf | [audiobooks-audiobookshelf](../../packages/audiobooks-audiobookshelf/package.yaml): config, metadata, podcasts and writable library bindings; BackupPlan includes config/metadata, excludes podcasts and library. | Expose those exclusions, restore listening progress and account state, and test native clients and library write permissions. |
| Calibre-Web | [books-calibre-web](../../packages/books-calibre-web/package.yaml): config plus external books binding; BackupPlan includes config only. This is not a generic Calibre desktop deployment. | Audit library metadata/write ownership, external library backup coverage, OPDS access, identity and database consistency. |
| qBittorrent | [downloads-qbittorrent](../../packages/downloads-qbittorrent/package.yaml): config volume, downloads/media bindings and private route; no BackupPlan declared. | Identify resume/config versus bulk data, ownership and API credentials; optional VPN behavior is a requirement to investigate, not a declared feature here. |

## Required follow-up before implementation

Audit applicable resource/observer/provider and application-adapter paths at a pinned revision; record declared, implemented, unsupported, manual, unknown, and live-verified behavior separately. The missing BackupPlans above are declaration gaps, not a conclusion that existing installations have no operator-managed backups. Resolve glossary conflicts and schema reuse first. Then create narrowly scoped implementation issues with acceptance criteria and dependencies. Keep migration and automatic-remediation changes blocked on their architecture decisions.

Documentation review is the only validation performed for this proposal; existing application packages and runtime behavior are unchanged.
