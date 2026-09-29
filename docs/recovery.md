# Recovery runbook

This is the operator's copy of the recovery contract the weekly exit
drill executes (`scripts/exit-drill.sh`, ADR-W018). If the drill is
green, this document is known to work end-to-end.

## The whole-site contract

Keep these off the host, always. The reserved `BackupPlan/wildstone`
packages the control-plane artifacts described in item 2; the recovery kit
in item 4 contains what a fresh install needs to unlock and locate them:

1. **The restic repository** for every BackupPlan — the data. Put the
   repository volume on storage that outlives the host (NAS export,
   separate disk, synced elsewhere).
2. **The `wildstone` recovery repository** — a Git bundle of the Statefile,
   a consistent SQLite online snapshot, a sealed household-vault key, and a
   recovery-key-sealed secret bundle for every node.
3. **A current deletion/revocation journal source** newer than the snapshot,
   so restore quarantine can be reconciled truthfully.
4. **The recovery kit** — endpoint, bucket/prefix, read-only bucket key,
   every off-site restic password, and the offline recovery private key.

The CA, operation-signing key and node identities are deliberately rebuilt.
Nodes re-join; their restored values are immediately sealed under each fresh
node's new local store key.

### Create the recovery key, plan and kit

Generate the key pair once. Put only the `.pub` value in the reserved plan;
move the private file off every Wildstone host.

```sh
wild recovery keygen ./wildstone-recovery.key
```

The reserved plan must be named `wildstone`, use `offsite-only`, and have the
single source `/srv/wildstone/recovery`. Its `recoveryPublicKey` is the
contents of `wildstone-recovery.key.pub`. The engine rebuilds that source
atomically immediately before each run; do not put files there by hand.

```yaml
apiVersion: wildstone.dev/v1
kind: BackupPlan
metadata: { name: wildstone, owner: system, node: node1 }
spec:
  paths: [/srv/wildstone/recovery]
  class: critical
  recoveryPublicKey: WSR-PUBLIC-1-...
  offsite:
    target: offsite
    route: offsite-only
    passwordSecret: secret/restic-wildstone-offsite
    schedule: daily
    retention: { keepDaily: 7, keepWeekly: 8, keepMonthly: 12 }
```

Create the password-manager-friendly kit without placing secret values in
the process argument list:

```sh
export WILD_RECOVERY_READ_KEY='<read-only-secret-access-key>'
export WILD_RECOVERY_REPOSITORY_PASSWORDS='{"wildstone":"...","photos":"..."}'
wild recovery kit -out recovery-kit.json \
  -endpoint https://s3.example -bucket household-backups \
  -access-key-id '<read-only-access-key-id>' \
  -private-key ./wildstone-recovery.key
unset WILD_RECOVERY_READ_KEY WILD_RECOVERY_REPOSITORY_PASSWORDS
```

Import that JSON into a password manager, make a paper copy, then remove the
local private-key and kit files. A recovery key that remains on the host
defeats the sealed-bundle boundary.

### Restore after total loss

This path assumes the worst useful case: the Keystone host, every node,
the NAS, every local restic repository, and every ordinary Statefile replica
are gone. Do not recreate an empty Statefile by hand. The surviving recovery
kit locates the encrypted `wildstone` repository in S3; that repository is
the source of the Statefile, runtime database and sealed secrets.

Before starting, provision a fresh Linux host with Docker and copy only the
recovery kit onto it. Confirm the S3 endpoint in the kit is reachable from a
Docker container (not merely from the host). Then:

1. Run `keystone recover -kit ...` below. It downloads the latest whole-site
   snapshot from S3, verifies its manifest byte-for-byte, restores the
   Statefile and household vault key, and installs the runtime database in
   restore quarantine. A failed verification leaves the destination unusable;
   do not bypass it.
2. While Keystone is stopped, mint a short-lived recovery credential with
   `keystone claim-code -data-dir <dir> -name recovery -ttl 1h`, read it once,
   and remove the claim-code file. Then start fresh Keystone and Wardstone
   installations. Re-use the node IDs in the restored Statefile, but expect a
   new CA and new node identities.
3. Import each node's sealed bundle from `recovery-import`. This restores the
   S3 keys and restic passwords without revealing them through Keystone.
4. Approve the `system/drift` Plan for the restored `BackupTarget` and
   off-site routes. It is High risk by design: recovery does not silently
   authorize a fresh machine to send data to an external bucket.
5. Restore every application plan with `-from offsite`, verify the restored
   data alongside the empty live volumes, and only then move it into place.
6. Bring the deletion/revocation journal current, replay it, and release
   quarantine. Until that succeeds, Butler retrieval, dispatch, ingestion and
   proactive delivery remain stopped.

On a fresh host with Docker available, the kit is sufficient to download the
latest `wildstone` snapshot with the pinned restic image, verify every
artifact, and install the set before Keystone starts:

```sh
keystone recover -kit ./recovery-kit.json \
  -data-dir /var/lib/wildstone/keystone
```

If the set was restored separately, use `-set <directory> -private-key
<file>` instead. Neither path writes the recovery private key into the data
directory.

Before starting Keystone, use `keystone claim-code` as described above; then
start it and re-join the rebuilt nodes. For each node, use the sealed artifact listed under its ID in
`/var/lib/wildstone/keystone/recovery-import/manifest.json`'s `nodeBundles`
map. A kit-based restore preserves these encrypted artifacts in that directory
before removing its temporary download:

```sh
wild recovery import-node <node-id> \
  -bundle /var/lib/wildstone/keystone/recovery-import/node-....sealed \
  -kit ./recovery-kit.json
```

For a separately restored set, `-private-key <file>` may be used instead of
`-kit`. The two options are mutually exclusive.

Restore each application plan from off-site, confirm the deletion journal is
current, then replay and release quarantine:

```sh
wild backup restore <plan> -from offsite
wild restore status
wild restore reconcile --release
```

The executable proof is `scripts/offsite-exit-drill.sh`. CI creates a real
S3-compatible Versity gateway, writes a canary, backs up both the application
and the reserved whole-site set, deletes all local state and repositories,
and performs precisely the sequence above. The final comparison is on the
canary bytes restored from S3; retaining a local repository cannot make that
drill pass.

### The fourth recovery input before durable Butler ships

ADR-W030 adds recoverable Butler runtime state: task/step ledgers,
conversations and selected memory, bindings and identity-link projections,
temporary grants and immediate denials, routines/procedures, access settings,
delivery state, structured work traces/feedback labels, and evaluator/config
versions. Its encrypted off-host backup or replica must publish a
measured recovery point; replication lag is possible data loss, not proof of
zero loss. Secrets remain separately re-entered from the password manager.

Restoring that state starts Butler in quarantine. No memory retrieval, task or
routine dispatch, ingestion, or proactive delivery occurs until current
authority and deletion/revocation information is reconciled. If an old backup
cannot prove that access or retained content is still valid, the affected data
stays unavailable and requires fresh authorization and, where appropriate,
reindexing. Administrative acknowledgement alone cannot recreate missing
deletions.

The expanded exit drill must restore an older snapshot containing access and
content that were revoked/deleted afterward, prove they remain unavailable,
include an offboarded member with old bindings/connections/private indexes, and
prove uncertain pending actions do not resume. Restore the Butler state after
fresh Keystone starts and before any Butler-facing surface leaves quarantine.

### How the quarantine is actually released

Restoring a Butler-state backup puts the runtime database in quarantine. That
is the default, not something an operator switches on: nothing Butler-facing
runs — no retrieval, no task dispatch, no ingestion, no proactive delivery —
until reconciliation has happened.

Releasing it takes two things, and the order is the point.

1. **Assert the deletion journal is current.** Deletions and revocations are
   journalled separately from the data they concern, so they can be replayed
   over restored state. A journal restored from the same old snapshot is
   itself incomplete, so somebody has to state that it has been brought up to
   date from a source newer than the snapshot. This is a precondition.
2. **Replay it.** Every journalled deletion and revocation is re-applied: a
   forgotten memory stays forgotten, an offboarded member stays denied, a
   revoked link stays revoked, a de-indexed source stays unsearchable. This is
   the evidence.

An administrative acknowledgement alone is not evidence that missing deletions
were recovered, and the schema refuses a release that has only the assertion.
Work whose outcome was unknown when the snapshot was taken stays paused: a
restored coordinator that resumes it would be retrying an action that may
already have happened.

    wild restore status                 # is this database quarantined
    wild restore reconcile --release    # assert, replay, and release

V1 has one active coordinator. A standby restore may be prepared, but promotion
is an explicit fenced recovery action: verify the previous Keystone cannot
dispatch, restore/reconcile authority state in quarantine, then release the new
coordinator. Do not run both schedulers against replicated state.

## Declaring a BackupPlan

None of the below is worth anything without a BackupPlan to restore
from. Three fields are guessable in exactly the wrong way, so here is a
complete one:

- `repository` is a **volume name**, not a filesystem path. The
  repository volume must exist as its own `Volume` resource — put it on
  storage that outlives the host.
- `passwordSecret` is a **reference** of the form `secret/<name>`, never
  a literal password. Set the value separately with `wild secret set`;
  Keystone never stores a readable copy.
- `schedule` is one of `daily`, `weekly`, `monthly` — not a cron
  expression.

```sh
printf '%s' '<repo-password>' | wild secret set photos-restic -node node1

cat > /tmp/photos-repo.yaml <<'EOF'
apiVersion: wildstone.dev/v1
kind: Volume
metadata:
  name: photos-repo
  owner: capability/photos
  node: node1
spec:
  name: photos-repo
EOF

cat > /tmp/photos-backup.yaml <<'EOF'
apiVersion: wildstone.dev/v1
kind: BackupPlan
metadata:
  name: photos
  owner: capability/photos
  node: node1
spec:
  volumes: [photos-data]        # the source volumes, mounted read-only
  repository: photos-repo       # a volume name, not a path
  passwordSecret: secret/photos-restic
  schedule: daily               # daily | weekly | monthly
  retention:
    keepDaily: 7
    keepWeekly: 4
    keepMonthly: 6
EOF

curl -sS -X POST http://127.0.0.1:9441/api/v1/plans \
  -H "Authorization: Bearer $WILD_TOKEN" \
  -d "$(jq -n --arg r "$(cat /tmp/photos-repo.yaml)" \
             --arg b "$(cat /tmp/photos-backup.yaml)" \
        '{summary: "back up photos daily", put: [$r, $b]}')"

wild plan approve <id>
wild backup run photos        # don't wait for the schedule to find out
wild backup list              # the run is in the ledger, succeeded
```

Get a field wrong and the API says which one, indexed to the document
that carried it — `put[1]: ... line 3: field repositoryVolume not found
in type resource.BackupPlanSpec`.

### Consistent database hooks

Never put a live PostgreSQL data directory or SQLite file in a snapshot and
call it a database backup. A plan may declare typed `preBackup` hooks. The
engine constructs the one-shot container itself; the plan cannot provide an
image, environment variable, command, or shell.

```yaml
spec:
  volumes: [photos, photos-db-backup]
  preBackup:
    - kind: pg_dump
      container: photos-postgres
      database: photos
      outputVolume: photos-db-backup
```

`pg_dump` reuses the declared PostgreSQL container's pinned image, username,
password reference, managed network or Unix socket. `sqlite` uses Wildstone's
pinned sqlite3 image and names the absolute database path inside the declared
container. Every output volume must also be covered by the plan. A failed hook
fails the run before restic reads any source.

## Off-site copies (S3-compatible storage)

A BackupPlan can also send its data to an S3-compatible bucket — AWS S3,
Cloudflare R2, Backblaze B2, Wasabi, or an S3 server of your own — so a
fire or theft that takes the host, the NAS and the local copies
together still leaves one copy (ADR-W045). It is off until you turn it
on, and only encrypted data leaves the house: restic encrypts on the
node, and the provider never sees file names or contents.

Each plan picks a **route**:

- `offsite-only` — straight to the bucket, nothing stored locally. For
  big data whose original already lives on a NAS, like a movie library.
- `local-and-offsite` — the local repository as usual, then its
  snapshots are copied to the bucket. Fast local restores, plus the
  off-site copy.

The off-site copy has its **own password**, separate from the local one.

For provider-specific least-privilege policies, Object Lock, credential
rotation and current cost examples, see
[Off-site backup provider hardening](backup-provider-hardening.md).

```sh
# The bucket's key (limit it to this one bucket), and one password per plan.
printf '%s' '<access-key-id>'     | wild secret set offsite-s3-access-key -node node1
printf '%s' '<secret-access-key>' | wild secret set offsite-s3-secret-key -node node1
printf '%s' '<offsite-password>'  | wild secret set restic-photos-offsite -node node1
```

The target — where the bucket is:

```yaml
apiVersion: wildstone.dev/v1
kind: BackupTarget
metadata:
  name: offsite
  owner: owner
  node: node1
spec:
  type: s3
  endpoint: https://<account-id>.r2.cloudflarestorage.com  # AWS: https://s3.<region>.amazonaws.com
  region: auto                                               # AWS: e.g. us-east-1
  bucket: wildstone-backups
  prefix: home                     # optional folder inside the bucket
  accessKeySecret: secret/offsite-s3-access-key
  secretKeySecret: secret/offsite-s3-secret-key
```

Then an `offsite` block on the plan (the photos plan above):

```yaml
spec:
  volumes: [photos-data]
  repository: photos-repo
  passwordSecret: secret/photos-restic
  schedule: daily
  retention: {keepDaily: 7, keepWeekly: 4, keepMonthly: 6}
  offsite:
    target: offsite
    route: local-and-offsite
    passwordSecret: secret/restic-photos-offsite   # never the local one
    schedule: weekly
    retention: {keepWeekly: 8, keepMonthly: 12}
```

For `offsite-only`, leave out `repository`, `passwordSecret`, `schedule`
and `retention` at the top level: the `offsite` block carries them.

Propose both documents in one plan and approve it. Pointing data
off-site is **High** risk, so it always needs your approval. `wild
backup run <plan>` runs the local backup and then the off-site copy;
`wild backup list` shows them as `backup` and `offsite` rows.

An S3 server on your home network over plain `http://` needs
`insecureHTTP: true`; most self-hosted servers also need
`pathStyle: true`.

**Put these in your password manager**, or the off-site copy is
cryptographically gone with the house: the endpoint and bucket, the
bucket key, and every off-site password.

Not yet built (tracked under #407): the monthly restore drill and
integrity checks for the off-site copy, and Console screens.

### NAS shares and folders

A plan can back up folders on the node, not only volumes, and keep its
local copy on a NAS so it never uses the host's own disk.

Declare the share once. Wildstone mounts it under `/srv/wildstone`
(always `nosuid,nodev`) and records it in `/etc/fstab` so it is back
after a reboot; if it drops, the next reconcile mounts it again. NFS only
for now — an SMB share needs a password, which fstab would hold in
plain text.

```yaml
apiVersion: wildstone.dev/v1
kind: NetworkMount
metadata: {name: media, owner: owner, node: node1}
spec:
  type: nfs4
  source: nas.example.internal:/volume1/media
  target: /srv/wildstone/nas/media
  options: [soft]          # optional, e.g. ro, vers=4.1
```

Then name folders in `paths`, and a folder as the `repository`:

```yaml
spec:
  paths: ["/srv/wildstone/nas/media/My Movies"]   # spaces are fine
  offsite: {target: offsite, route: offsite-only, passwordSecret: secret/restic-movies-offsite, schedule: weekly}
```

```yaml
spec:
  volumes: [photos-data]
  repository: /srv/wildstone/nas/backups/photos    # the local copy lives on the NAS
  passwordSecret: secret/photos-restic
  schedule: daily
```

Changing a share's `source` or `options` rewrites its fstab entry, but
the share already mounted stays until the next reboot or remount —
Wildstone has no unmount. Until then the change shows as not applied,
and backups refuse a share mounted from a different source.

**If the NAS is down, the backup does not run.** Before every run
Wildstone checks that each folder under a declared share is really on
that share. Otherwise it would be backing up the empty folder
underneath — and after enough of those, retention would prune the real
snapshots away. The run fails and says which share is missing.

**Leave things out** with `exclude` — restic patterns, one per entry. A
pattern without a leading `/` matches anywhere; an absolute one must sit
inside one of the plan's `paths`:

```yaml
spec:
  paths: ["/srv/wildstone/nas/media/My Movies"]
  exclude:
    - "*.nfo"                                        # anywhere
    - "**/Samples"                                   # any folder called Samples
    - "/srv/wildstone/nas/media/My Movies/Extras"    # this one folder
```

On a plan that goes off-site, removing an exclude is High risk: it sends
more out of the house. Adding one is not.

Excludes also hold for what is already stored locally: every snapshot is
tagged with the plan's sources and excludes, and a `local-and-offsite`
copy takes only snapshots made under the current ones. Snapshots taken
before you added an exclude stay home.

**Say how much it matters** with `class: critical` (irreplaceable),
`important` (painful to rebuild) or `rebuildable`. Every package's plan
already carries one; it only groups plans when you choose what goes
off-site, and changes nothing about how they run.

A plan's `volumes`, and its repository when that is a volume, must be
declared `Volume` resources on the same node: a typo would otherwise back
up an empty volume Docker created on the spot, and report success.

A snapshot keeps the folder's path, so a restore of
`/srv/wildstone/nas/media/My Movies` lands in
`<plan>-restore/backup/srv/wildstone/nas/media/My Movies`.

### Disk space

A local copy never fills the disk it lives on. Each repository's disk
keeps a **reserve**: by default the larger of 10% and 20 GiB (a quarter
of a disk smaller than 80 GiB). Set your own per plan with
`reserve: 50GiB` or `reserve: 15%`; it cannot be zero.

- **When you propose a plan**, Wildstone sizes the sources and refuses a
  local copy that would not fit, suggesting `offsite-only` or a NAS
  repository instead.
- **Before every run** it checks again, because data grows. If the copy
  would cross the reserve, the local copy is skipped: a plan with an
  off-site copy backs up straight to it for that run, so the data stays
  protected; one without fails and says why. `wild backup list` shows the
  skipped run and the reason.
- **During a run** it re-checks every 30 seconds and stops restic at the
  reserve.
- A disk that is already past its reserve (common on a well-used host)
  still runs backups that add little, but a run may then use at most
  half of what is left.

## Losing a host: the restore procedure

Mirrors the drill phase-for-phase.

**Host-level setup is not restored.** Everything Wildstone manages is
rebuilt from the Statefile and the restic repository, but anything that
lives on the host *underneath* Wildstone is yours to redo:

- **Docker** on every node — the installer puts it back for you.
- **The tailnet join**, if the host was on one. Not in the Statefile,
  not reconciled, so re-run the installer with
  `--tailscale-authkey` (see [install.md](install.md)). Do this
  **before** the node join if the node reaches Keystone over the
  tailnet, or the join has no route.

Neither is a gap in the recovery contract: both are host network
control, which the node agent deliberately cannot grant itself.

### 1. Fresh Keystone over the restored Statefile

```sh
sudo bash ./install.sh --role keystone          # fresh install on the new host
sudo systemctl stop keystone
sudo git clone <statefile-replica> /var/lib/wildstone/keystone/statefile
sudo systemctl start keystone
journalctl -u keystone | grep -A1 'first start'   # NEW admin token
journalctl -u keystone | grep 'CA fingerprint'    # NEW CA — this is correct
```

A new CA on rebirth is expected behavior, not a defect: node
identities are per-install. Record the new token and fingerprint.

### 2. Re-join every node

For each node (also freshly installed, or wiped):

```sh
wild node mint-join                        # on/against the new keystone
sudo bash ./install.sh --role node --keystone <host>:9440 \
  --node-id <same-node-id> --join-token <t> --ca-fingerprint <hex>
```

Use the **same node ids** the Statefile references, or its resources
have no node to land on.

### 3. Re-enter secrets from your vault

For every `secret/<name>` referenced by the Statefile (container env,
BackupPlan `passwordSecret`):

```sh
printf '%s' '<value>' | wild secret set <name> -node <node-id>
```

The restic repository password is the critical one — without it the
backups are cryptographically gone.

### 4. Let reconcile rebuild the shape

The reconciler reads the restored Statefile and recreates volumes and
containers on the re-joined nodes (creates are Low-risk and repair
unattended).

**If you use off-site copies**, the rebuild includes your BackupTarget
and off-site plans, which are High-risk: nothing is rebuilt until you
approve it. Find the drift-repair plan with `wild plan list` and
approve it once — that single approval is you confirming that backups
may go off-site again from the new host. Watch `wild status` / the Console until observed matches
desired. Volumes come back **empty** — that's the next step.

### 5. Restore data alongside, then move it

```sh
wild backup restore <plan>        # latest snapshot -> volume <plan>-restore
```

If the local repository did not survive, restore from the off-site copy
instead (an `offsite-only` plan always restores from there):

```sh
wild backup restore <plan> -from offsite
```

Restore-alongside is deliberate (ADR-W011): nothing overwrites live
data unattended. Verify the restored tree (the snapshot's paths appear
under `backup/<volume>/...`), stop the consuming containers, move the
data into the live volume, start them again. Repeat per plan.

#### Restoring package databases

Database dumps are ordinary files inside the restore volume. Restore them
only after the package has recreated a fresh database service, and keep the
application container stopped until the import finishes:

| Package / plan | Restored dump | Import |
|---|---|---|
| Immich / `photos` | `backup/immich-db-backup/immich-postgres.dump` | Recreate database `immich`; run `pg_restore --clean --if-exists --no-owner --dbname=immich` with the matching Immich PostgreSQL image. |
| Paperless / `documents` | `backup/paperless-db-backup/paperless-postgres.dump` | Recreate database `paperless`; run `pg_restore --clean --if-exists --no-owner --dbname=paperless` with the package PostgreSQL image. |
| Synapse / `matrix` | `backup/matrix-db-backup/matrix-postgres.dump` | With the private PostgreSQL socket mounted, recreate `synapse` and run `pg_restore --clean --if-exists --no-owner --dbname=synapse`. |
| Gitea / `code` | `backup/gitea-db-backup/gitea.sqlite3` | Stop Gitea, replace `/data/gitea/gitea.db` with the dump, remove stale `-wal`/`-shm` files, preserve ownership, then start Gitea. |
| Mealie / `recipes` | `backup/mealie-db-backup/mealie.sqlite3` | Stop Mealie, replace `/app/data/mealie.db`, remove stale `-wal`/`-shm` files, preserve ownership, then start Mealie. |
| Roundcube + Davis / `mail` | `backup/mail-db-backup/{roundcube,davis}.sqlite3` | Stop both apps; replace `/var/roundcube/db/sqlite.db` and `/data/davis.db` respectively, remove stale sidecars, preserve ownership, then restart. |

For PostgreSQL, feed the dump file to `pg_restore` as a single file—not to
`psql`, because the hook writes PostgreSQL custom format. Run the package's
normal health check and sign in before deleting the alongside restore volume.

### 6. Verify

- `wild backup list` — the restore run is in the ledger, `succeeded`,
  with `runKind: restore` (the scheduled rehearsal of the same
  mechanism records as `drill`).
- `wild status` — everything healthy.
- `GET /api/v1/reports/morning` — no unhealthy resources, backups ok.

## Losing a single node (host survives)

Skip steps 1: keystone and the Statefile are intact. Re-join the node
under the same id (step 2), re-enter that node's secrets (step 3), let
reconcile rebuild (step 4), restore data (step 5).

## Drill cadence

- **Weekly, automated:** the `exit drill` workflow runs the full
  total-host-loss cycle in CI. A red run is a release blocker — treat
  it as "recovery is currently broken", not as a flaky test.
- **Monthly, per plan:** the backup engine's built-in restore drill
  (`DrillEvery`, ADR-W011 A4) restores each plan's latest snapshot
  alongside and records the result in the ledger; the morning report
  surfaces failures.
