# Recovery runbook

This is the operator's copy of the recovery contract the weekly exit
drill executes (`scripts/exit-drill.sh`, ADR-W018). If the drill is
green, this document is known to work end-to-end.

## The current contract: three things must survive the host

Keep these off the host, always:

1. **The restic repository** for every BackupPlan — the data. Put the
   repository volume on storage that outlives the host (NAS export,
   separate disk, synced elsewhere).
2. **A replica of the Statefile git repo** — the desired state. It is
   a plain git repository at `<data-dir>/statefile`; add a remote and
   push it on a schedule, or back the directory up.
3. **Your secret values, in your password manager.** Wildstone
   deliberately never stores a readable copy (ADR-W012: write-only
   path). If a value exists only on the node, it dies with the node.

In the current release, everything else — including the runtime DB, the CA and
op-signing keys, node identities, containers, and data volumes — is rebuilt.
Durable Butler features from ADR-W030 must not ship under that three-input
contract.

### The fourth recovery input before durable Butler ships

ADR-W030 adds recoverable Butler runtime state: task/step ledgers,
conversations and selected memory, bindings and identity-link projections,
temporary grants and immediate denials, routines/procedures, access settings,
and delivery state. Its encrypted off-host backup or replica must publish a
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
and prove uncertain pending actions do not resume. When implemented, restore
the Butler state after fresh Keystone starts and before any Butler-facing
surface leaves quarantine. Until that drill passes, the steps below describe
only the current three-input release contract.

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
unattended). Watch `wild status` / the Console until observed matches
desired. Volumes come back **empty** — that's the next step.

### 5. Restore data alongside, then move it

```sh
wild backup restore <plan>        # latest snapshot -> volume <plan>-restore
```

Restore-alongside is deliberate (ADR-W011): nothing overwrites live
data unattended. Verify the restored tree (the snapshot's paths appear
under `backup/<volume>/...`), stop the consuming containers, move the
data into the live volume, start them again. Repeat per plan.

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
