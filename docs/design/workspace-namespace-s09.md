# Workspace namespace foundation (#482, S09 part 1)

This slice adds dormant trusted DB primitives. It does not register tools,
perform filesystem operations, authorize callers, or complete S09. The governing
contract is [native-agent-capability-contract-v1](native-agent-capability-contract-v1.md).

## Metadata and invariants

`0103_workspace_namespace.sql` follows the existing complete migration filenames;
neither historical migration chain is renumbered. Existing live artifact and open
write ancestors receive independent random directory IDs and generation tokens.
New nested artifact paths, staged writes, moves and mkdir metadata also mint any
missing logical parents atomically. These tokens describe logical identity, not
filesystem readiness. Removed directory rows remain history; recreation mints
both a new identity and token. Root has no removable directory row.

`workspace_namespace_intents` journals immutable workspace/task/operation/key,
argument hash, source/destination, expected file revision or directory token, and
the selected directory identity. Prepare reserves overlapping paths. Pending
intents exclude conflicting writes/publications and moves, including descendants
and ancestors. Literal prefix comparisons do not treat `%` or `_` as patterns.

SQLite's immediate writer transactions serialize collision admission, revision
checks, empty-directory checks and metadata settlement. Files, directories,
unfinished write sessions and pending publication intents participate in those
checks. Pending files and child directories make rmdir nonempty. Trashed files
retain their immutable revision history but do not occupy the live namespace.
Committed move changes only the artifact path; ID, revision, provenance, retained
bytes and quota accounting remain unchanged. Destinations require an allowlisted
inert extension; a rename does not reinterpret the retained revision's media type.

Prepare and commit independently check the coordinator execution fence and task
ownership of the private workspace. Exact settled retries return the original
receipt, after those checks, and never mutate a recreated directory. Reusing a key
with changed arguments or retrying an aborted intent conflicts. The committed
intent ID is the durable receipt. Abort only releases metadata reservation; it
does not undo a filesystem effect. Startup recovery can enumerate prepared
intents; restart does not implicitly release their reservations.

The exported methods are trusted persistence seams, as with existing publication
primitives. Their caller must independently resolve current grants, task privacy,
Person lifecycle, Agent/Connection admission, tool assignment and QA-only mode.
`CommitWorkspaceNamespace` must be called only after confined physical operations
and durability were verified. The physical and metadata steps are not one atomic
transaction. SQL trigger errors from existing write primitives are kept as refused
operations; gateway integration must map namespace conflicts to its typed result.

Every new TEXT column has an explicit `NotAPerson` verdict in personref's catalog:
directory and workspace IDs, generation selectors, paths, operation/state enums,
effect keys, request digests and timestamps. Person identity is inherited solely
through the existing workspace and task foreign keys.

## Remaining review slices (keep #482 open)

1. **This foundation:** durable logical directories, intent reservations,
   collision/publication guards, fresh-install/populated-upgrade/restart tests,
   concurrent moves and metadata rollback faults.
2. **Physical operations and recovery:** pin validated Linux parent descriptors,
   confined mkdir/atomic empty rmdir/no-overwrite same-scope move, durable identity
   reconciliation, fsync and mount/confinement fault tests. Reconcile prepared
   intents before abort or metadata commit. Fail closed on unsupported storage.
   Account for logical parents minted by legacy nested staged writes.
3. **Gateway integration after #481:** independently grantable mkdir/rmdir/move
   schemas, current resource/provenance authorization, QA-only refusal, execution
   fences, body-free audit and complete tool receipts/list DirectoryRefs. Map DB
   namespace conflicts without exposing SQL or paths. Rebase onto reviewed staged
   tools rather than importing unfinished gateway internals.

Local evidence targets `TestWorkspace*` and personref completeness tests plus
focused vet, gofmt and diff checks. Linux exact-head CI must run race coverage and
the immutable migration inventories; Windows checkouts have unrelated historical
SQL newline fingerprint differences. No filesystem success is claimed by these
metadata-only tests.
