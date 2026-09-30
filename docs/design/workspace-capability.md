# Workspace capability configuration

`workspace-wildstone` implements the configuration/readiness slice of
[ADR-W047](../decisions/ADR-W047-native-agent-research-and-workspaces.md)
and its [v1 contract](native-agent-capability-contract-v1.md), issue #476.
It renders only a `workspace.yaml` ConfigFile owned by `capability/workspace`.
There is no container, Volume, runtime mount, backup claim or tool grant.

Install/reconfigure through the existing public capability proposal API and
approve the resulting Plan with current Owner freshness. A proposal changes
nothing until approval; an intervening desired-state change supersedes stale
Plans. `node` must identify Keystone's host. The root defaults to
`/srv/wildstone/agent-workspaces` and must already exist. Provision the private
storage directory separately through the installation's approved host-storage
workflow. Configuration and readiness checks never create replacement storage.
Removing the package removes only configuration: workspace bytes and metadata
remain. Reinstallation can point to the same retained root. Changing the root
does not migrate data; migration remains separate work.

| Parameter | Default | Bounds |
| --- | --- | --- |
| `workspace_path` | `/srv/wildstone/agent-workspaces` | Canonical absolute non-root directory; no symlink root |
| `required_mount` | empty (local disk) | Canonical absolute mount containing the root; filesystem root refused |
| `quota_bytes` | 1 GiB | Positive integer, at most 1 TiB |
| `max_file_bytes` | 10 MiB | Positive integer, at most 10 MiB and no greater than quota |
| `scratch_retention_days` | 7 | Integer 1–3650, after completed/failed/cancelled state |
| `trash_retention_days` | 30 | Integer 1–3650 |

These settings supply limits to later storage/retention implementations; this
slice does not implement accounting, publication, cleanup or backup.

`Server.WorkspaceSettings()` reloads desired state and checks dependency
readiness on every call. Consumers must not cache it between invocations.
Missing configuration, unknown/wrong placement, duplicate configuration,
malformed fields and unavailable storage fail closed with redacted
`storage_unavailable` reasons. The Services/package inventory exposes blockers.
The settings revision includes placement and all configuration bytes, allowing
later consumers to detect a configuration change without treating it as a grant.

For local storage, readiness checks an existing nonsymlinked directory. For
required mounts it checks kernel mount metadata without remote I/O: this does
not claim that an NFS server is reachable or writable. Actual storage operations
must use `Settings.OpenRoot()`, which pins and verifies the mount's kernel ID
before opening the existing root and never calls `MkdirAll`. Required-mount
storage is unavailable where Linux mount metadata cannot be verified. Consumers
must additionally apply resource authorization, publication/recovery checks and
execution fences; a ready directory alone grants no access.

Canonical workspace tool registration, per-Agent opt-in grants and new-native
Agent seeding remain #480/#481/#482/#483/#501. This package declares no domestic
`agentTools`, avoiding implicit bundles or upgrade/runtime-switch grants.
