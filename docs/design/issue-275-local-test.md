# Issue 275: local-stack pilot

Branch: `issue-275-shared-file-manager`.

This implements the updated **Shared and Mine file manager slice**, not the wider Store
resource/orchestration work. Photos remains flat. The text editor is
excluded. See [ADR-W037](../decisions/ADR-W037-shared-file-tree.md).

## Build and deployment

From the checkout, build the embedded Files page into Keystone:

```sh
go build -ldflags '-X main.version=dev-issue275' -o /tmp/keystone-issue275 ./keystone/cmd/keystone
/tmp/keystone-issue275 -version
```

The portal HTML is embedded in this binary; a Console asset deployment is not
needed for the Files page. The package definition adds the optional
`files_mount` field. Reapply the updated local `filedrop-wildstone` package
through the normal reviewed Plan flow if enabling that setting. Merely replacing
the Keystone binary does not update the installed package's desired state.

Before replacing the live binary, preserve its current copy and confirm the
unit's ExecStart (`systemctl cat keystone`). The usual installer path is
`/usr/local/bin/keystone`. Deploy and restart only when ready for the pilot;
keep the saved binary to restore and restart if the pilot fails. This checkout's
implementation work does not change the running service or the host mount.

For mounted storage, set `files_mount` to the **actual mountpoint**, and
`files_path` to an existing writable folder at or beneath it. Do not use a normal
subdirectory as the mountpoint. Keep the deployment's existing `RequiresMountsFor`
and NFS mount policy until verified. The new guard detects missing mounts;
it does not mount the export or make hard NFS I/O cancellable.

## Browser checks

Run these checks with disposable content in both Shared and Mine. Every member
with access can modify Shared; Mine is scoped to the signed-in member.

1. Create a folder and a nested folder. Refresh inside it; use breadcrumbs and
   browser back/forward. The directory should persist in the URL.
2. Upload using both picker and drag/drop. Download and compare the bytes.
   Upload the same name again: the new tree API must refuse with no overwrite.
3. On a supported local filesystem, rename a file and folder and move them
   within the selected namespace. A destination collision must preserve both entries. Moving a
   folder into itself or crossing a filesystem must fail.
4. On NFS, expect rename/move to report unsupported safe atomic operations and
   leave the source unchanged. This is a known limitation, not an untested
   assumption that NFS supports renameat2 flags.
5. Delete a file and an empty folder. A nonempty folder must remain intact with
   a useful refusal. There is no recycle bin.
6. Check at 390px width: controls remain usable, long names do not widen the page,
   folder downloads are disabled, and the shared-access notice is visible.
7. Verify Photos remains flat. Sign in as a second member and verify they cannot
   list, download, rename, move or delete the first member’s nested Mine files.
8. Verify access without filedrop membership and cross-origin mutation requests
   fail. Operator audit/events should name the actor and operation; household
   activity must not reveal filenames.
9. With disposable mounted storage and the guard configured, test the absent
   mount state under an operator-controlled maintenance procedure. Requests must
   report unavailable and never create the `files_path` tree on local storage.
   Never unmount a live household export just to run this check.
10. Validate permission/stale-handle/server-unavailable behavior on the pilot
    export. A hard mount may block, so assess the actual mount timeout policy.

## Automated checks

```sh
go test -race ./keystone/portal ./keystone/api ./keystone/domestic ./keystone/pkgtool
cd console
npm ci
npx playwright install chromium
node scripts/filedrop-check.mjs
npm run build
```

The browser test uses fixture responses; the Go API tests exercise real storage
and authorization. The browser supports `PLAYWRIGHT_CHROMIUM_EXECUTABLE` for an
existing Chromium binary and `FILEDROP_SCREENSHOT_DIR` for screenshots.

The NFS integration test is opt-in and creates/removes only a temporary subtree:

```sh
WILDSTONE_TEST_NFS_ROOT=/path/to/disposable/writable/nfs-fixture \
  go test -v ./keystone/portal -run '^TestSharedTreeNFSIntegration$'
```

Run it with Keystone's effective service identity and the same export options,
including root squash. The fixture must be an already mounted NFS filesystem.
Passing local filesystem tests does not validate the export's UID mapping,
permissions, hard-link support or outage behavior.
