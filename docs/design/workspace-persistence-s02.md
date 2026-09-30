# Workspace persistence (S02)

This is the dormant runtime-database foundation for #474 under
[ADR-W047](../decisions/ADR-W047-native-agent-research-and-workspaces.md) and
[the v1 contract](native-agent-capability-contract-v1.md). No tool or HTTP
handler calls these accessors yet. Metadata persistence does not authorize
access or prove that bytes have been published.

## Migration and compatibility

`0097_agent_workspaces.sql` follows `0096_backup_verification.sql` on main
`7ffcc108817429bc53daada73dbf081529d6f114`. It creates purpose-specific
workspace tables, indexes and constraints in one ordinary migration
transaction, with foreign keys enabled. It does not rebuild existing tables,
backfill workspaces, seed tools, or modify existing Agent assignments.

The integration target is `feature/471-native-agent-capabilities`, refreshed
through `3c2c023c9aadcfb78eeda3bde591a1d652fc6921` (PR #513). The migration
number is unchanged because that feature-branch update adds no migration.
This is the first database slice of #471. Successors #490 and #497 must
refresh that feature branch and allocate the next unused migration number
after 0097;
0098 is not reserved here. Once merged, changes to this schema require a
new forward migration, not an edit to 0097. There is no downgrade migration.
Older binaries do not use these additive tables; that is not permission to
run an old writer once subsequent slices activate the capability.

## Ownership and persistence boundary

Workspace identity binds a stable Person ID and Agent database identity.
Artifact ownership and revision facts cannot be updated. Revisions retain
creating-task linkage, content hash, size, media type and privacy metadata.
Source edges are recorded before preparation and sealed with the revision's
publication intent. Sealing refuses workspace-source edges until their source
revisions have committed publications; tentative or aborted bytes cannot become
durable lineage. Receipts cannot be deleted while publication history is retained,
so exact retries preserve their original effect identity. These records are
trusted Keystone inputs, not model arguments. Selectors and receipts grant
no authority.

The resource resolver in S03 must recheck current Person/Agent access,
source restrictions and surface/class ceilings. Sharing remains a later
slice; this migration grants nobody access. No private content is exposed by
adding persistence accessors.

## Journal and accounting

The journal records `staging -> prepared -> bytes_published -> committed`,
with an explicit recovery abort. Only committed revision metadata is exposed
by the committed-read accessor. Artifact state is independently `pending`,
`committed` or `trashed`; retained committed history survives replacement and
trash. Foreign keys bind current/base/new revisions to the same artifact and
publication reservations to the same workspace and task.

The database commit operation checks the base revision and settles the
current revision, reservation and receipt in one SQLite transaction. A
receipt conflict rolls back the whole metadata change. Unresolved publication
intent pins its reservation. Retained revisions and held reservations count
against the owning Agent across Persons, including trashed history.

S05 must verify and durably synchronize confined immutable byte publication
before marking bytes published, coordinate execution fences, and recover
uncertain effects before aborting. These methods perform no filesystem I/O
and do not claim cross-store atomicity. S06 supplies reconciled storage
accounting and limits around actual writes; no storage usage is inferred from
filesystem paths here. S11 owns terminal-task scratch and trash cleanup;
there is no expiry worker or destructive cleanup in this change.

Aborting records the failed intent and releases its reservation; it preserves
the artifact identity, pending path and journal for recovery evidence. A retry
can publish a new revision against that same pending artifact. It does not
free the path for a fresh artifact or erase bytes. S05 must resolve uncertain
storage effects before releasing that charge. Later lifecycle work must add
an explicit coordinated purge path; deleting sealed provenance/journal rows
is intentionally refused by this foundation.

## Configurable quota integration gate (S06 / #479)

These dormant primitives enforce the initial 1 GiB Agent budget and 10 MiB
file ceiling. They do **not** consume workspace configuration or its revision.
In particular, `ReserveWorkspaceBytes` must not be presented as enforcement of
an Owner-selected quota: 0097 fixes both the reservation-size ceiling and the
aggregate admission trigger at 1 GiB. A smaller configured file limit is also
not enforced by the revision table's 10 MiB platform ceiling.

The configuration slice #476 / PR #515 accepts a positive per-Agent quota up
to 1 TiB and a file limit from one byte to 10 MiB (bounded by the quota).
W047 section 8 identifies defaults; #479 explicitly owns validating
Owner-configurable bounds and wiring them into staged publication. Neither
this persistence slice nor configuration readiness activates workspace tools.
The two seams are not yet a complete configurable-quota implementation.

Before activation, S06 must replace the fixed-default reservation constraints
in a forward migration and bind atomic admission to current trusted settings.
It must check the configured per-file limit as well as the platform ceiling,
coordinate configuration changes with reservation/commit, and preserve held
reservations plus retained history/trash in Agent-wide accounting. Do not
silently clamp a larger configured quota to 1 GiB, ignore smaller configured
limits, accept model-supplied limits, or remove the fail-closed default checks
without their replacement. Existing retained bytes must survive limit lowering.

Required integration cases include quota above 1 GiB (within the configured
maximum), a one-byte configured file limit, configuration changes between
reserve and commit, restart, and simultaneous writes across Person/visibility
scopes. Lowering a limit below retained/held usage must refuse further growth
without deleting artifacts or discarding unresolved recovery reservations.
S05 may develop against defaults, but S06 remains a prerequisite for exposing
writes that claim to obey workspace settings. A configuration revision alone
is not authorization and must not let an old reservation bypass current limits.
