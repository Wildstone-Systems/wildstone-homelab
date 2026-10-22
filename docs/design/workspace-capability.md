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
| `required_mount` | empty (local disk) | Canonical absolute mount directly hosting the root; intervening mounts and filesystem root refused |
| `quota_bytes` | 1 GiB | Positive integer, at most 1 TiB |
| `max_file_bytes` | 10 MiB | Positive integer, at most 10 MiB and no greater than quota |
| `scratch_retention_days` | 7 | Integer 1–3650, after completed/failed/cancelled state |
| `trash_retention_days` | 30 | Integer 1–3650 |

These settings supply limits to later storage/retention implementations; this
slice does not implement accounting, publication, cleanup or backup.

Quota enforcement is explicitly owned by S06/old private issue 479.
Do not connect this seam to S02's default-only reservation API and claim that
configured limits are enforced: its 1 GiB reservation/aggregate bound must be
replaced by atomic admission against the current authorized quota. Keep the
10 MiB platform file ceiling and additionally enforce any lower configured
file limit. Never silently clamp configured values to S02 defaults or ignore
lower limits. If migration 0097 has merged, schema changes need a new forward
migration. This is a required integration before write-tool activation, not an
optional enhancement; S08/old private issue 481
already depends on S06.

S06 must consume the current settings/revision at reservation and publication,
revalidate or refuse stale configuration, and cover concurrent reservations,
quotas above and below 1 GiB, lower file limits, and lowering limits below
existing usage. Refusals preserve existing bytes and recovery-pinned charges.
The settings revision is a freshness input, never model-supplied authority.
Root/mount readiness alone does not establish quota admission or write readiness.

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
must use `Settings.OpenRoot()`, which pins and verifies both the required mount
and the actual root descriptor against the same kernel mount ID and never calls
`MkdirAll`. Set `required_mount` to the filesystem actually hosting the root:
intervening mounts are refused, including a nested mount added during open.
Required-mount
storage is unavailable where Linux mount metadata cannot be verified. Consumers
must additionally apply resource authorization, publication/recovery checks and
execution fences; a ready directory alone grants no access.

Canonical workspace tool registration, per-Agent opt-in grants and new-native
Agent seeding remain #480/#481/#482/#483/#501. This package declares no domestic
`agentTools`, avoiding implicit bundles or upgrade/runtime-switch grants.
