# ADR-W037: File trees in Shared and Mine

- **Status:** accepted; implemented (`keystone/portal/files_tree*.go`, `files_mount.go`)
- **Governed by:** ADR-W033 where they overlap.

## Scope

Shared and Mine are browsable file trees; Photos remains flat. The Owner
chooses the host root. A signed-in Person may select only a bounded relative
path beneath Shared or their own Mine root; os.Root confinement is the
filesystem boundary, and there is no API for a host path or another Person's
Mine. The recent-files widget and domestic agent tools keep their flat view.

Shared and Mine are file namespaces, not ADR-W033 memory scopes: Mine is not
Personal memory, and Shared is not Household memory.

## Decisions

* Paths are refused, never cleaned: maximum 32 components, 4,096 UTF-8 bytes,
  and 255 bytes per component. Absolute, empty interior, dot, parent, hidden,
  backslash, invalid UTF-8 and control-character components are refused.
  The empty directory path names the namespace root. File operations cannot
  mutate that root. Hidden entries include the internal upload staging files.
* Directories and regular files can be listed. Symlinks and special files below
  the namespace root are not visible or operable. Existing Owner-arranged
  namespace symlinks confined inside the configured root retain their legacy
  meaning. A namespace symlink outside the root remains forbidden.
* Each traversed directory is pinned through an os.Root. On Linux, opening a
  child uses a single validated leaf and `openat(O_NOFOLLOW)` relative to a file
  descriptor obtained from that Root. Go 1.25's Root.OpenFile follows in-root
  symlinks itself even if its flags contain O_NOFOLLOW, so that call alone is
  insufficient. `/proc/self/fd/<fd>` establishes the next Root from the pinned
  descriptor, never from a caller-controlled host path.
* Downloads open nonblocking with no-follow and inspect the opened descriptor's
  type. They remain opaque attachments with nosniff, never rendered HTML.
* New uploads stage, chmod and sync inside their destination, then publish by
  exclusive hard link. A same-name collision is HTTP 409. Legacy flat uploads
  keep their suffix policy. No upload publishes through an overwrite-capable
  reservation-and-rename fallback.
* Rename/move stays inside the selected namespace and uses `renameat2(RENAME_NOREPLACE)` between
  pinned parents. Cross-device moves are refused. Both the legacy flat rename
  and new move use that primitive, preventing collisions between API versions.
* Delete removes files or empty directories only. There is no recursive delete,
  trash or restore guarantee. The confirmation states that deletion is immediate.
* Mutations require the execution fence at entry and immediately before commit.
  Streamed uploads recheck after receiving the body. Staging cleanup remains
  allowed after a refusal. The existing portal identity, group and same-origin
  checks protect the new endpoints too.
* Operator events identify the Person, operation and relative path. Household
  events contain no filename, including in their event ID. Rename, move and
  mkdir do not add household activity noise.
* Mine uses the authenticated Person to resolve its root. The folder
  (`Members/<name>`, first the lower-cased handle) is recorded for the Person the
  first time and kept, so a rename does not move them to an empty folder
  (runtimedb 0093), and from 0094 at the moment the Person is created (0095 fills in anybody created between the two). A folder
  another Person holds is refused, not shared. The
  signed-in member is matched by the handle they carry now, never by an old
  one. A directory account with no Person keeps the folder its username
  gives, unless that folder is a Person's: recorded for one, or named after a
  handle one carries or once carried, ignoring case.
  Requests cannot select another Person. Nested operations preserve
  personal-space isolation.
* The page states that everyone with filedrop access can change shared content.
  This is a collaborative folder, not a per-folder/read-only permission system.
  URLs carry namespace and directory, with breadcrumbs and browser history.
  Dialogs and uploads retain the destination selected when the operation began.

## Mounted storage and NFS limits

`files_mount` is an optional Owner-configured existing mountpoint containing
`files_path`. Its rendered `requiredMount` setting protects browser and domestic
flat tools. When configured, the filesystem root must already exist on that
mount: no local replacement tree is created. Mount metadata and the pinned
mount descriptor's mount ID are compared so losing the mount during opening
cannot switch access to the underlying local directory. Write fences recheck
mount presence before publication. This is a guard for an externally managed
mount, not a new Store resource or a mount operation.

Mount presence is not reachability or write permission. Root-squashed exports
must permit actual create, chmod, write, link and delete under Keystone's mapped
identity. Personal-space privacy remains governed by the deployment's
export permissions; this ADR makes no further privacy promise.

Linux NFS rejects nonzero rename flags in
[`nfs_rename`](https://github.com/torvalds/linux/blob/master/fs/nfs/dir.c).
Consequently, atomic rename plus race-safe no-overwrite is unavailable there.
NFS uploads can use exclusive hard links; **rename/move returns an explicit 501
without changing the source** when the filesystem rejects atomic no-replace.
There is deliberately no check-then-rename fallback. Any future NFS copy/move or
cooperative-lock protocol needs a separate contract for other NAS writers and
crash recovery. Filesystems lacking hard links also refuse the new upload path.

A hard NFS mount can still block filesystem I/O. Wildstone neither changes
host mount options nor promises an HTTP timeout can cancel a kernel syscall.
Each deployment must validate its mount timeout policy separately.
Permission, unavailable/stale/timeout, collision, full-storage and cross-device
errors receive actionable responses without exposing host paths.

## Verification

Store and API tests, `console/scripts/filedrop-check.mjs` (Chromium, 1280px and
390px) and an opt-in NFS fixture test cover these rules.

## Proposed W047 companion extension: generated-artifact export

Pending acceptance of [ADR-W047](ADR-W047-native-agent-research-and-workspaces.md)
and its [v1 interface contract](../design/native-agent-capability-contract-v1.md);
no new export operation is claimed implemented here.

Keystone may publish an authorized committed workspace artifact directly into
the requesting Person's Mine, or into Shared with explicit permitted disclosure.
The new `artifact_export` tool uses current Person-folder mappings, confined
pinned-parent upload staging, exclusive no-overwrite publication, destination
limits, required-mount checks and execution fences. Another Person's Mine and
generic reads of existing Filedrop document bytes remain unavailable to Agents.

Copy is the default. Requested move copies, verifies/records destination
publication, then trashes the workspace source; retries use durable receipts.
No cross-store atomic rename or NFS check-then-rename fallback is introduced.
Everyone with Filedrop access can change Shared copies; workspace grants do
not impose new Shared ACLs. Existing Filedrop immediate deletion, no-trash
guarantee, flat domestic tools and opaque attachment downloads stand.
