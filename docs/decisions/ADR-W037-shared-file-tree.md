# ADR-W037: File trees in Shared and Mine

Status: proposed for local-stack pilot (#275)

## Context and scope

[Issue #275's updated implementation slice](https://github.com/chickenrat17/wildstone-homelab/issues/275#issuecomment-5783022779)
allows a native file manager inside the existing Shared namespace. This slice
adds that tree and, at the owner’s request, the same operations in Mine. Photos remains flat. A Store resource, mount
orchestration, capacity cards, bulk package binding, remote backup repositories,
read-only grants (#278), destination grants (#281), and a text editor are
separate work. The recent-files widget and domestic agent tools retain their
flat view; no recursive filename feed is added to the agent. This does not claim to complete the wider attached-storage issue.

The earlier #197 prohibition on browsing must no longer be used to reject this
explicitly collaborative Shared tree. The Owner still chooses the host root.
A member may select only a bounded relative path beneath Shared or their own identity-resolved Mine root. The existing
namespace identity and os.Root confinement remain the filesystem boundary;
there is no API for a host path or another person's personal namespace.

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
* Operator events identify the member, operation and relative path. Household
  events contain no filename, including in their event ID. Rename, move and
  mkdir do not add household activity noise.
* Mine uses the authenticated member to resolve its root; requests cannot select
  another member. Nested operations preserve the existing personal-space isolation.
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
identity. Personal-space privacy remains governed by the pre-existing deployment's
export permissions; this change does not introduce a new privacy promise.

Linux NFS rejects nonzero rename flags in
[`nfs_rename`](https://github.com/torvalds/linux/blob/master/fs/nfs/dir.c).
Consequently, atomic rename plus race-safe no-overwrite is unavailable there.
NFS uploads can use exclusive hard links; **rename/move returns an explicit 501
without changing the source** when the filesystem rejects atomic no-replace.
There is deliberately no check-then-rename fallback. Any future NFS copy/move or
cooperative-lock protocol needs a separate contract for other NAS writers and
crash recovery. Filesystems lacking hard links also refuse the new upload path.

A hard NFS mount can still block filesystem I/O. This slice neither changes
host mount options nor promises an HTTP timeout can cancel a kernel syscall.
The pilot must validate the deployment's mount timeout policy separately.
Permission, unavailable/stale/timeout, collision, full-storage and cross-device
errors receive actionable responses without exposing host paths.

## Verification

Store tests cover validation, nested operations, symlinks and replacement races,
FIFOs, concurrent collisions, late authority refusal, stream cancellation and
absent mounts. API tests cover all path-taking endpoints, portal auth/CSRF,
structured bodies, attachment headers, auditing and household-event privacy.
`console/scripts/filedrop-check.mjs` exercises the actual page in Chromium at
1280px and 390px with deterministic API fixtures. An opt-in NFS test checks an
existing disposable fixture under the service's actual effective identity.
