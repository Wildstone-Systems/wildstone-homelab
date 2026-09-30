# Confined workspace bytes

ADR-W047 / issue #477 implements filesystem primitives only. `New` takes a
validated current workspace Settings snapshot and trusted stable Person/Agent
IDs. SHA-256 directory components isolate owning namespaces without storing
raw IDs. Sharing, current authority, quota reservations, task sessions, object
references and committed visibility remain the caller's responsibility. There
are no registered tools, HTTP routes, database changes or automatic grants.

`ValidatePath` reuses ADR-W037's validator without cleaning input. `Stage`
accepts only `.md`, `.txt`, `.json`, `.csv` logical paths and bounded UTF-8 text
(no NUL). JSON/CSV/Markdown are inert bytes, not parsed or executed. The logical
path belongs to metadata; it is never used as a filesystem object key. A
random 128-bit object ID and SHA-256 digest/size describe each completed stage.
Empty text is supported. Invalid input has no filesystem effects.

Linux `openat2` refuses symlinks in every component and mount crossings below
the configured root. Required-mount opening preserves the existing kernel
mount-ID checks and never creates a missing root. Operations pin directory and
file descriptors, validate regular-file type without blocking on FIFOs, and
reopen/check current storage immediately before linking and after the effect. Configured root opens
also use atomic kernel no-symlink resolution, closing the earlier
check-then-open window. Missing storage remains an actionable refusal.

Stages use `O_TMPFILE`, descriptor chmod to `0400`, file fsync and an exclusive
hard link from the pinned descriptor. There is no named partial object on a
failed write. `Publish` reopens/hash-checks a stage and links its pinned inode
into `revisions`, then fsyncs the destination directory. It requires an
execution fence before work and immediately before the effect; the caller
checks current authority/configuration and prepared journal intent there.
Existing destination files, symlinks and directories are never overwritten.
Both stage and revision links remain for recovery. `Read` returns a bounded
copy only after verifying regular type, UTF-8, size and digest.

`Object` is a byte reference, not an artifact identity, a committed revision,
or a bearer grant. Callers journal its full tuple and only disclose objects
whose metadata is committed and currently authorized. Retry collisions remain
explicit; the future publication coordinator reconciles the exact object.
If a link succeeds but directory fsync or the post-link storage check fails,
`ErrUncertain` means recovery is
required. Stage returns the object even with that error, allowing its identity
to be recorded. Never retry with a fresh ID or delete prior bytes to recover.
Lifecycle removal, reservations and cross-store export are separate slices.

Supported storage must provide Linux `openat2`, `O_TMPFILE`, procfs descriptor
links, exclusive hard links and directory fsync. Unavailable guarantees fail
visibly; there is no check-then-rename fallback (including on NFS). Non-Linux
byte operations fail closed. Readiness alone does not certify these I/O
features, capacity or reachability. This does not protect against a host
administrator modifying inodes directly; hashes detect changed bytes on read.

Verification: `go test -race ./workspace/...` covers paths, inert formats,
missing roots/mounts, descriptor identity, symlink/special-file refusal,
source/parent substitutions, actual nested mount refusal, concurrent collisions,
restart, tampering, execution fences, I/O faults and uncertain durability.
Existing `workspace` tests inject kernel mount snapshots for loss/replacement
without mount privileges. Portal tests retain the original W037 contract.
