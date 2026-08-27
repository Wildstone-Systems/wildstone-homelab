# Recovery runbook

This is the operator's copy of the recovery contract the weekly exit
drill executes (`scripts/exit-drill.sh`, ADR-W018). If the drill is
green, this document is known to work end-to-end.

## The contract: three things must survive the host

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

Everything else — the runtime DB, the CA and op-signing keys, node
identities, containers, the data volumes themselves — is rebuilt.

## Losing a host: the restore procedure

Mirrors the drill phase-for-phase.

### 1. Fresh Keystone over the restored Statefile

```sh
sudo ./install.sh --role keystone          # fresh install on the new host
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
sudo ./install.sh --role node --keystone <host>:9440 \
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

- `wild backup list` — the restore run is in the ledger, `succeeded`.
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
