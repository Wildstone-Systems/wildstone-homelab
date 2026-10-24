# Recovery runbook

Reviewed 2026-10-22 against `60df9dcbc1daa99d43f7692eb83f7d72f7070956`.
The local and S3-only CI drills (`scripts/exit-drill.sh` and
`scripts/offsite-exit-drill.sh`, `.github/workflows/exit-drill.yml`) exercise
defined recovery fixtures on pull requests, weekly and on demand. A passing
run is evidence for that revision and fixture, not proof that every installed
application, external mount, radio or hypervisor configuration can recover.

## Choose the failure being recovered

| Failure | First response | Evidence of recovery |
| --- | --- | --- |
| One application unavailable | Capture status/logs and dependency/storage state; use the existing approved action path for any restart | Application readiness and real client operation, not just a running container |
| Suspected database corruption | Preserve original bytes and stop blind retries; select a compatible backup and restore alongside | Database/application consistency, supported version, login and data checks before cutover |
| Workload guest/node lost | Rebuild prerequisites, rejoin and restore the affected recovery group | Storage, identity and network bindings plus application checks |
| Management lost | Use the whole-site kit, quarantine and fenced recovery below | One active coordinator; reconciled revocations; restored application access |
| Physical host/storage lost | Recover onto surviving/replacement infrastructure with off-host backups and keys | External monitoring plus management and workload validation |

The current installer does not rebuild Proxmox itself or configure guest
autostart. Keep hypervisor, storage and network prerequisites separately.
See [disk/recovery guardrails](operations/disk-and-recovery.md) and proposed
[ADR-W051](decisions/ADR-W051-failure-domains-and-recovery.md).
No new automatic repair or destructive-reset authority is granted here.

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

### Durable Agent state and restore quarantine

ADR-W030 adds recoverable Butler runtime state: task/step ledgers,
conversations and selected memory, bindings and identity-link projections,
grants and immediate denials, routines/procedures, access settings,
delivery state, structured work traces/feedback labels, and evaluator/config
versions. Its encrypted off-host backup or replica must publish a
measured recovery point; replication lag is possible data loss, not proof of
zero loss. Node secrets covered by the whole-site sealed bundle can be imported on fresh nodes. Personal credentials and other excluded values must be re-entered through their supported flows; retain the offline kit and password-manager records.

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

#### Mailbox monitors after a restore

A Person's mailbox monitor (epic #326) is built to come back closed, not
open. What a restore does to it, and what you do afterwards:

- **Before anything else, the copy is quarantined.** `RestoreBackup`
  enters quarantine first; then it abandons every scheduled run the lost
  machine had in flight (their checkpoints did not move, so the first run
  after release re-reads that mail and the fingerprints dedupe it),
  revokes every open review (their tokens could not be presented anyway:
  they derive from the excerpt key, which is in no backup), deletes every
  sealed excerpt and scrubs every mail answer from the work queue.
- **While quarantined nothing reads, offers or writes.** No scheduled
  read is dispatched, no adapter can claim, no review is delivered, and
  the review reconciler does not look at a calendar or settle an
  interrupted write until the quarantine is released.
- **The replay re-fences.** A background authorization, a Personal
  credential or a whole connection revoked after the snapshot is revoked
  again; an offboarded Person's mailbox state (candidates, reviews,
  effects, dedupe state, checkpoints) is erased again. Then
  `wild restore reconcile` proves every fenced credential copy erased on
  its node and erases any `pc.<connection>.` name a node holds that the
  snapshot never heard of; anything unproven leaves the connection
  `degraded` (`erasure_unverified`) and visible.
- **Adapter placements are re-filed, not applied.** The replay re-files
  each Personal connection's adapter render, so an adapter the replayed
  authority no longer allows is proposed for removal. Approve it (or the
  rendering that matches); the reconciler applies nothing while
  quarantined.
- **People re-enter their own credentials.** Personal credentials are
  not in the recovery set (Wardstone leaves `pc.` names out of the node
  bundle). After release, each Person whose monitor should run opens
  *Watch this mailbox*, enters their app password again, and authorizes;
  an Owner approves the render Plan. Their earlier candidates that were
  still unresolved show without an excerpt; nothing is asked twice.

What is never in a backup: the excerpt key file
(`<DataDir>/mail-excerpts.key`), any Personal credential value or node
secret name's value, any mail answer, and any full message body, subject
or sender (`runtimedb.BackupTo` scrubs answers and excerpts from the copy
and vacuums it; `keystone/runtimedb/mailoffboarding_test.go` and the
capture checks in `keystone/api/mail_e2e_test.go` inspect the bytes).

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

The off-site engine includes scheduled integrity checks and alongside restore
drills (`keystone/backup/offsite.go`, with regression coverage in
`offsite_test.go`). Inspect actual ledger results; these are not a substitute
for application-level restore validation or evidence that every Console
workflow is complete.

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

## Legacy/manual reconstruction from a Statefile replica

This narrower procedure reconstructs declared resources from a surviving
Statefile and application repositories. It does **not** recover the complete
runtime DB, grants, Connections or durable Agent state. Prefer the whole-site
kit procedure above for a current installation; do not treat this legacy path
as an equivalent total-site restore. Preserve the old management state and
fence the previous coordinator before rebuilding.

**Host prerequisites are not reconstructed by Statefile replay.** Declared
resources can be rebuilt, but durable management state and excluded secrets
need their own recovery path. Anything underneath Wildstone is also yours to redo:

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

#### Restoring the household's notes

The `knowledge` plan backs up a host folder (default
`/srv/wildstone/knowledge`), not a volume. Its snapshot appears under
`backup/` in `knowledge-restore`, with the folder's full path. Stop
`silverbullet` if it is installed, copy that tree over the live folder,
`chown -R` it back to the folder's owner, then start `silverbullet`
again. `.history` comes back with the notes: it holds every version an
agent's update replaced (ADR-W046 A5).

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

## Home automation controllers

A home-automation controller is not an ordinary app. Each one holds the
keys its paired devices trust, and a controller that comes back with a
different key is, to every device in the house, a stranger. Getting the
data back is not enough: it has to come back **before** the controller
first starts on an empty volume, onto the **same** kind of radio, with
**only one copy** running. This section is the inventory and the order.
`keystone/pkgtool/controller_backup_test.go` holds the packages to the
table below, and [testing/home-automation](../testing/home-automation/README.md)
is the drill that proves it on real hardware.

### What each controller keeps

| Package (plan) | Volume, path in container | Identity-bearing state in the volume | Also on the radio (NVM) | Wildstone secrets | Lost with no backup |
|---|---|---|---|---|---|
| `zigbee-zigbee2mqtt` (`zigbee`, critical) | `zigbee2mqtt-data`, `/app/data` | `configuration.yaml`: `network_key`, `pan_id`, `ext_pan_id`, `channel`. `database.db`: every pairing. `coordinator_backup.json`: Zigbee2MQTT's own copy of the coordinator's network. `state.json`. | The coordinator stores the same network (key, PAN ids, channel, frame counter) and its child table. | `restic-zigbee`; `mqtt-zigbee` (on the broker's node) | Re-pair every Zigbee device. |
| `zwave-zwave-js-ui` (`zwave`, critical) | `zwave-js-ui-store`, `/usr/src/app/store` | `settings.json` → `zwave.securityKeys` (S0, three S2 keys) and `securityKeysLongRange`. `nodes.json`, the driver cache, and `backups/nvm/NVM_*.bin` (nightly, keeps 7). | The stick **is** the network: home ID, node table, routes. The keys are **not** on the stick. | `restic-zwave` | Stick intact: plain devices keep working; every secure (S0/S2) device, every lock, must be excluded and re-included. |
| `thread-otbr` (`thread`, critical) | `otbr-data`, `/var/lib/thread` | The active and pending operational datasets (network key, PAN ID, extended PAN ID, channel, mesh-local prefix, PSKc) and frame counters. | Nothing. An RCP is a radio without memory; the host keeps the network. | `restic-thread` | Re-commission every Thread device, unless Home Assistant still holds the dataset (see below). |
| `matter-matterjs` (`matter`, critical) | `matter-data`, `/data` | The fabric: root CA key, operational certificates, fabric ID, every node's ID, credentials and ACLs. | No radio. Matter-over-Thread devices also need the Thread network above. | `restic-matter` | Factory-reset and re-commission every Matter device. |
| `home-assistant` (`home-assistant`, important) | `home-assistant-config`, `/config` | `.storage/`: users and logins, config entries (including the MQTT password you typed in, and the Z-Wave, Matter and OTBR URLs), `thread.datasets` (Home Assistant's copy of Thread credentials), ESPHome device keys. YAML automations, scripts, scenes. `home-assistant_v2.db` (history). | None. `bluetooth: host-bluez` pairing keys live in the **host's** BlueZ (`/var/lib/bluetooth`), which Wildstone does not back up. | `restic-home-assistant`; `mqtt-home-assistant` | Rebuild automations, dashboards and users. No device is paired to Home Assistant itself. |
| `mqtt-mosquitto` (no plan) | `mosquitto-data`, `/mosquitto/data` | Nothing identity-bearing: retained messages, which Zigbee2MQTT and Home Assistant republish. | — | `mqtt-zigbee`, `mqtt-cameras`, `mqtt-home-assistant`, rendered into the password file. | Nothing, if the secrets survive. |

Two copies live outside these volumes and are worth knowing about:

- **Home Assistant's copy of the Thread dataset.** When Home Assistant
  adds the OpenThread Border Router integration it records the dataset
  under Settings → Thread. If `otbr-data` is lost but Home Assistant is
  restored, Home Assistant can give that dataset to a fresh border router
  that has none (re-adding the OpenThread Border Router integration
  applies Home Assistant's preferred dataset to an empty router). Treat
  this as the second copy, not the plan: restore `otbr-data`, and check
  this path in the drill before relying on it.
- **Phones.** The Home Assistant companion app can copy the Thread
  credentials onto the phone (*Sync Thread credentials*, under the
  companion app's own settings; the menu differs between iOS, Android
  and app versions). That lets the phone commission Matter-over-Thread
  devices onto **this** network instead of an Apple or Google one. It is
  a copy for commissioning, not a backup: you cannot restore a border
  router from a phone. Sync again after restoring or changing the
  network.

**The broker's passwords.** The three `mqtt-*` secrets are generated on
the broker's node and written into Mosquitto's password file from there.
Every Wildstone consumer gets its password by reference and follows a
rotation by itself. **Home Assistant does not**: its MQTT integration
holds the password you typed in. If `mqtt-home-assistant` is rotated,
or regenerated because it was not restored, update it in Home Assistant
(Settings → Devices & services → MQTT → Reconfigure) with the value
`docker exec home-assistant printenv WILDSTONE_MQTT_PASSWORD` shows.

### What survives what

| Event | Zigbee | Z-Wave | Thread | Matter | Home Assistant |
|---|---|---|---|---|---|
| Restart, image upgrade, a param change that re-creates the container | Kept | Kept | Kept | Kept | Kept |
| Disable then enable the capability | Kept: the volume stays (ADR-W004 A4) | Kept | Kept | Kept | Kept |
| Volume restored from a backup, same radio | Back to the snapshot. Devices paired since are lost and re-pair. | Keys back; the stick still has every node. Nodes included since are on the stick but unknown to the cache: re-interview them. | Back to the snapshot. The router re-attaches; allow a few minutes for routes. | Back to the snapshot. Devices commissioned since must be reset and re-commissioned. | Back to the snapshot. |
| Radio replaced, volume intact | Same chip family (`zstack` → `zstack`, `ember` → `ember`): Zigbee2MQTT writes `coordinator_backup.json` onto the new coordinator at start; check its log. Some devices may still need re-pairing. Across families: not supported here. | Restore the newest `backups/nvm/NVM_*.bin` onto the new stick (Z-Wave JS UI → Settings → NVM Management → Restore). The keys come from the store. | Flash OpenThread RCP firmware, set `radio_device` to the new stick's by-id path. Nothing to restore on the radio. | — | — |
| Volume lost, radio intact | **Do not start Zigbee2MQTT on an empty volume with the old coordinator plugged in**: it generates a new key and may form a new network on the stick over the old one. Unplug the coordinator until the volume is restored. | The store is regenerated with **new** keys. Restoring the store later puts the old keys back. | The router starts with no network. Restore, or give it Home Assistant's dataset (above). | A new, empty fabric is created. Restoring the volume over it (with the server stopped) brings the old one back, as long as nothing was commissioned into the new one. | Onboarding starts again. Do not onboard; restore. |
| Both lost | Re-pair everything. | Re-include everything. | Re-commission everything. | Re-commission everything. | Rebuild. |

Each new stick has a different `/dev/serial/by-id/` name: the name
contains the stick's serial number. Replacing a radio is always a param
change (`usb_device` or `radio_device`) as well.

**Only one copy at a time.** Never run two Zigbee2MQTTs with the same
configuration, two border routers from one `otbr-data`, or two Matter
servers from one fabric: they answer for the same identity and fight
over it. Before restoring onto a new machine, make sure the old one is
off or its controller is disabled.

### Backups that can outlive the node

The four critical plans write to a repository **volume on the same
node**, and their restic passwords are **generated on that node**: you
never saw them. A dead disk takes the volume, the repository and the
password together. For a household that depends on these devices:

1. set up the whole-site contract above (the reserved `wildstone` plan
   and the recovery kit), which seals every node's secrets, restic
   passwords included, so a new node can import them; and
2. give the critical plans an off-site route (`local-and-offsite`, see
   [Off-site copies](#off-site-copies-s3-compatible-storage)).

Without both, a backup of these controllers protects against mistakes on
a node that still works, and nothing else.

### Restore order for a replacement node

Follow [Losing a host](#losing-a-host-the-restore-procedure) (or [Losing
a single node](#losing-a-single-node-host-survives)), with these
additions. The order matters because a controller that starts on an
empty volume makes a new identity, and with a radio attached it may
write that identity to the radio.

1. **Before re-joining the node, prepare the host.** None of this is
   restored, because it is host setup the agent cannot do:
   Docker; the tailnet; for Thread, IPv6 forwarding with `accept_ra = 2`
   on the LAN interface and the `ip6table_filter` module (see
   [install.md](install.md#6-home-automation-hardware)); for
   `bluetooth: host-bluez`, BlueZ installed and running.
2. **Leave every radio unplugged** (or not passed through to the VM).
   When reconcile rebuilds the shape, the agent refuses the Zigbee,
   Z-Wave and Thread containers with `device "/dev/serial/by-id/..."
   does not exist on this node`. That refusal is what you want: those
   controllers cannot start on empty volumes.
3. **Re-join, import secrets, let reconcile rebuild** (steps 2 to 4 of
   the host procedure). The broker comes back by itself; it has nothing
   to restore. Matter and Home Assistant start on empty volumes. Do not
   commission anything into Matter and do not onboard Home Assistant.
4. **Restore each controller, in this order:** `thread`, `matter`,
   `zigbee`, `zwave`, `home-assistant`. Thread comes before Matter
   because Matter-over-Thread devices are reached through it; Home
   Assistant comes last so that when it starts, every server it
   connects to already has its old identity. For each plan:

   ```sh
   wild backup restore <plan>               # or: -from offsite
   ```

   check the restored copy (the identity checks in the drill), then move
   it into place with the capability **disabled**, so nothing is running
   on the volume while it is replaced:

   ```sh
   wild capability disable <package>        # approve the plan; the volume stays
   sudo docker run --rm -v <plan>-restore:/from:ro -v <volume>:/to alpine:3.20 \
     sh -c 'find /to -mindepth 1 -delete && cp -a /from/backup/<volume>/. /to/'
   # plug the radio in now, if this controller has one
   wild capability enable <package> -set ...   # the same params as before; approve
   ```

   `cp -a` keeps file ownership, which matters for `matter-data` (owned
   by UID 1000). If a stick is new, its by-id name is new: pass it in
   the enable's params.
5. **Verify each identity**, not just that the container is healthy:
   Zigbee devices respond without re-pairing, a lock still reports, the
   Thread dataset hash and Matter `compressed_fabric_id` match what you
   recorded, Home Assistant shows the same users and automations.
   Home Assistant's history database is copied while it is being
   written; if Home Assistant reports it corrupt, it moves it aside and
   starts a new one, and only history is lost.
6. **Delete the `<plan>-restore` volumes** once you are satisfied
   (`sudo docker volume rm <plan>-restore`), so the next restore starts
   from an empty volume instead of mixing two snapshots.

## Drill cadence

- **Weekly, automated:** the `exit drill` workflow runs the full
  total-host-loss cycle in CI. A red run is a release blocker — treat
  it as "recovery is currently broken", not as a flaky test.
- **Monthly, per plan:** the backup engine's built-in restore drill
  (`DrillEvery`, ADR-W011 A4) restores each plan's latest snapshot
  alongside and records the result in the ledger; the morning report
  surfaces failures.
- **Quarterly, and after replacing a radio:** the home-automation
  controller drill in
  [testing/home-automation](../testing/home-automation/README.md)
  (section 5): a canary in each controller's volume, a backup, a
  restore alongside, and the identity checks. The monthly drill proves
  the bytes come back; only this one proves the devices still answer.
