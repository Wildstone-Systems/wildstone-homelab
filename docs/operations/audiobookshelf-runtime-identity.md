# Audiobookshelf runtime-identity migration

This procedure moves an existing `audiobooks-audiobookshelf` installation
from the image-default user to an explicit numeric UID/GID. It changes only
the approved container specification. Wildstone does not change ownership,
mode or ACLs on any file or directory.

## Before proposing the change

1. Confirm the current `audiobooks` BackupPlan has a recent successful backup
   of `audiobookshelf-config` and `audiobookshelf-metadata`, and perform the
   normal restore-alongside check. The podcasts volume and audiobook library
   are content on the storage layer and are not covered by that plan.
2. Inventory the paths. `audiobookshelf-config`, `audiobookshelf-metadata` and
   `audiobookshelf-podcasts` are Wildstone-owned Docker volumes. The selected
   `audiobooks_path` is user-owned storage. Record existing ownership and mode
   privately; do not paste household paths or listings into a public issue.
3. Outside Wildstone, prepare every path so the intended numeric identity can
   create, write, sync, rename and delete entries without recursively changing
   unrelated data. NFS export identity mapping, ownership and ACL changes are
   owner administration, not an effect of this package.

## Migrate

Set `runtime_uid` to the intended numeric UID. Set `runtime_gid` when the
storage group differs; when omitted it equals `runtime_uid`. Review the Plan:
the only Container changes should be `runtimeIdentity`,
`preflightMountAccess: true`, and the package revision.

On approved apply, Wardstone checks `/config`, `/metadata`, `/podcasts` and
`/audiobooks` under that exact identity before replacing the existing
container. A denied, unavailable, timed-out, unsupported or cleanup-incomplete
check leaves the current container and desired revision unapplied. Correct the
storage outside Wildstone and retry the same reviewed change.

After a successful replacement, confirm the service is healthy, existing
accounts and listening progress are present, podcasts are present, and a
synthetic library scan succeeds. A representative root-squashed NFS scan and
browser upload are owner-operated evidence; repository tests do not establish
them.

## Roll back

Propose the prior package settings with both runtime fields empty. Review and
approve the resulting Plan. The image-default identity is restored while the
same three volumes and library mount remain attached. Rollback does not delete
post-migration data and does not repair ownership or permissions; prepare
access for the image-default identity outside Wildstone first.

If either direction fails its preflight, keep the currently running container,
inspect the redacted failure class and correct the named container mount. Do
not delete volumes, recursively change the library, or treat a repository test
as final household acceptance.
