# ADR-W045: Off-site backup to S3-compatible storage

- **Status:** proposed.
- **Issue:** [#407](https://github.com/chickenrat17/wildstone-homelab/issues/407).
  Audit: [`docs/design/issue-407-offsite-backup-audit.md`](../design/issue-407-offsite-backup-audit.md).
- **Builds on:** ADR-W011 A4 (backup engine), ADR-W012 (secrets path),
  ADR-W018 A2 (recovery contract), ADR-W033 D1 (household secrets).
- **Amends (if accepted):** ADR-W018 A2 item 3, ADR-W012 A1 — see A5.

## Context

The E6 engine backs selected Docker volumes into a **local** restic
repository volume. Getting that repository off the host is left to the
operator. A household that loses the host, NAS and local backups at once
(fire, theft, ransomware) loses everything not copied elsewhere by hand.

The owner wants: a user-configured S3-compatible endpoint; per-dataset
choice of what goes there, including host folders such as media; and a
total-loss restore that brings the whole site back "like nothing
happened".

Principle 1 (`docs/vision.md`) says *data stays home; a design that
forces household data off-site is wrong*. This ADR must therefore never
force data off-site.

## Decisions

### A1. Opt-in only; only ciphertext leaves home

- The default is unchanged: nothing leaves the house.
- Off-site is enabled only by an owner-approved plan (A7).
- Only restic repository objects are uploaded. restic encrypts on the
  node before upload; the provider sees ciphertext, object sizes and
  timing — never file names or contents.
- Keys never go to the provider (A5, A8).

### A2. New resource kind: `BackupTarget`

A provider-agnostic description of one S3-compatible bucket. AWS S3,
Backblaze B2, Wasabi, Cloudflare R2 and MinIO all fit.

```yaml
kind: BackupTarget
metadata: { name: offsite, node: node1 }
spec:
  type: s3                          # only value in v1
  endpoint: https://s3.us-west-004.backblazeb2.com
  bucket: wildstone-backups
  prefix: home                      # optional; one bucket, many installs
  region: us-west-004               # optional
  pathStyle: false                  # true for MinIO and most self-hosted
  accessKeySecret: secret/offsite-s3-access-key
  secretKeySecret: secret/offsite-s3-secret-key
```

- Credentials are `secret/<name>` references with `provided: true`
  (ADR-W012). Literal values are refused by validation and by policy.
- Each plan's off-site repository lives at
  `s3:<endpoint>/<bucket>/<prefix>/<plan>`.
- A target can be tested before use ("test connection": list/stat
  against the bucket) without writing data.

### A3. Each plan picks a route

Two routes. The owner picks one per BackupPlan.

| Route | Copies | Extra local space | Good for |
|---|---|---|---|
| `offsite-only` | S3 only | None (small cache) | Big data whose original already lives on a NAS: movies, media |
| `local-and-offsite` | Local repo, then S3 | About the size of the data | Small, critical app data: fast, offline restores |

```
offsite-only:       source ──backup──────────────────────────▶ BackupTarget
local-and-offsite:  source ──backup──▶ local repo ──restic copy──▶ BackupTarget
```

- `BackupPlan` gains an optional `offsite` block:

  ```yaml
  offsite:
    target: offsite
    route: local-and-offsite        # or offsite-only
    passwordSecret: secret/restic-photos-offsite   # separate from local
    schedule: daily
    retention: { keepDaily: 7, keepWeekly: 8, keepMonthly: 12 }
  ```

- **offsite-only** runs `restic backup` straight into the off-site repo.
  Files stream from the source (host or NAS) through the run container
  to S3; nothing but restic's cache is written locally. The plan's local
  `repository` is omitted.
- **local-and-offsite** runs the local backup, then `restic copy` to the
  off-site repo. Live data is read once. The off-site repo is created
  with `restic init --copy-chunker-params`, so both repos split files the
  same way and copies stay deduplicated.
- **Where the local repo lives** is the owner's choice: a host volume, or
  a NAS share (A3.2).
- **Separate password.** A leaked local password does not open the
  off-site copy, and the other way round.
- Off-site has its own schedule and retention. A broken endpoint fails
  only the off-site step; local backups keep running.

### A3.1. Disk-space guard

A local copy must never fill the disk it lives on. It is checked at
three points.

- **New agent op `DiskSpace{path}`** returns total and free bytes for the
  filesystem holding a managed path or volume (host disk or NAS mount).
  Today the agent only reports free space on the root disk.
- **Reserve.** Every local repo location keeps a reserve free: by default
  the larger of 10% of the disk or 20 GiB. The owner can change it per
  location, never to zero.

1. **Before approval.** The plan shows source size (`MeasureUsage`), the
   estimated local repo size, and free space at the repo location. The
   estimate is source size × 1.2 (a margin for kept older versions). If
   it would cut into the reserve, the plan is **refused** with a plain
   message and two suggestions: switch to `offsite-only`, or put the
   local repo on the NAS.
2. **Before every run.** Free space is checked again, because data
   grows. If the reserve would be crossed, the local step is **skipped**
   and the run backs up **directly off-site** for that run instead. The
   data stays protected, the ledger records `skipped-local: low disk`,
   and the morning report asks the owner to act.
3. **During a run.** The runner checks free space on every poll. If it
   reaches the reserve, the restic container is stopped and the run is
   marked failed. Stopping restic mid-backup is safe: the partial data
   is removed by the next prune.

- **Early warning.** The morning report shows each local repo's growth
  trend: "local copy of *movies* will reach the reserve in ~20 days".

### A3.2. NAS shares as sources and repo locations

- A NAS share is attached with the existing `EnsureMount` op (NFS/SMB),
  under the managed mount root (`/srv/wildstone/...`).
- A plan's `paths` may point into such a mount, so NAS data is read over
  the network and never stored on the host.
- A plan's local `repository` may be a path on such a mount, not only a
  Docker volume name.
- A run whose source or repo mount is missing fails loudly. It never
  backs up an empty mount point as "nothing changed", and never writes a
  repo onto the host disk underneath a missing mount.

### A4. The user chooses what goes off-site, including host folders

- **Per plan.** Off-site is switched on or off per BackupPlan, so per app
  or dataset.
- **Host and NAS folders.** `BackupPlanSpec` gains `paths: []string`
  next to `volumes`, so owner-authored plans can cover folders such as
  `/srv/wildstone/media/movies`, on the host or on a NAS mount (A3.2).
  Only paths under the Wardstone managed prefix are accepted — the agent
  already refuses other host mounts.
- **Owner-authored plans.** The user may create plans that no package
  ships ("my movie library"). Package plans stay as they are.
- **Excludes.** `exclude: []string` (restic `--exclude` patterns) per plan.
- **Data class.** Packages tag each plan `critical`, `important` or
  `rebuildable`. The Console groups plans by class, so choosing is easy;
  the class does not change engine behaviour.
- **Cost and space are visible before approval.** The plan shows source
  size (`MeasureUsage`), a rough monthly storage estimate, and the
  disk-space check (A3.1).

### A5. Whole-site recovery set

To restore "like nothing happened", everything that cannot be rebuilt
goes off-site, not only app data. A reserved system plan, `wildstone`,
covers:

| Item | How it is captured | On restore |
|---|---|---|
| App data | Each plan's own off-site repo | Restore alongside, as today |
| Statefile | `git bundle` of the Statefile repo | Cloned before Keystone starts |
| Runtime DB | SQLite online backup of `keystone.db` (consistent, not a live file copy) | Restored **in quarantine** (ADR-W018 A2 item 4, ADR-W030 A10) |
| Node secrets | Sealed secret bundle (below) | Re-imported to each node |
| Household secrets | Their store's own export, sealed the same way | Re-imported per ADR-W033 D1 |

- **CA, op-signing keys and node identity stay rebuilt**, as today. A new
  CA on rebirth is correct (ADR-W018 A2). Nodes re-join with fresh
  tokens; that is the one visible step in a total-loss restore.

**Sealed secret bundle (amends ADR-W012 A1, ADR-W018 A2 item 3).**
Today secret values never leave the node, and the operator re-enters
them from a password manager. For a full restore:

- At setup, the owner creates a **recovery key pair**. Only the public
  key is stored in the Statefile. The private key goes into the
  recovery kit (below) and is never stored on any Wildstone host.
- Wardstone encrypts its secret store to that public key and hands out
  only the sealed file. Keystone and the provider never see plaintext.
  With the private key offline, nothing on a running system can open it.
- The Console and CLI keep no reveal path; write-only still holds on a
  running system.
- This is the one decision here that changes an accepted ADR. It needs
  explicit owner acceptance.

**The recovery kit** — the only thing that must live outside Wildstone:

1. The endpoint, bucket and a **read-only** S3 key.
2. The off-site repository password(s).
3. The recovery private key.

Keep it in a password manager and on paper. Without it, the off-site
copy is cryptographically gone — by design.

### A6. A backup counts only when it is proven

- **Prerequisite:** the protocol gains a container exit-code arm, and a
  run whose restic exit code is non-zero is a failed run. This is the
  follow-up `keystone/backup/backup.go` already names.
- `restic check` runs on both repos on a schedule; off-site uses
  `--read-data-subset` to bound download cost.
- There is a monthly off-site restore drill into a scratch volume.
- The morning report flags a stale off-site copy, a failed check, and a
  sudden shrink in source size (possible ransomware or accidental delete).

### A7. Risk and policy

| Change | Risk |
|---|---|
| Create or edit a `BackupTarget` | High (points data outside the house) |
| Turn off-site on for a plan, or add paths to an off-site plan | High |
| Turn off-site off, or delete a target | Medium (coverage removed) |
| Off-site run, check, drill | Standing approval from the plan, as today |
| Restore from off-site | Existing restore rules (step-up; alongside only) |

- The Butler may propose any of these and approve none of them (principle 2).
- A policy rule refuses an off-site block whose target does not exist.

### A8. Credentials

- Off-site keys are separate from any other credentials, and scoped to
  one bucket and prefix.
- The node's key can write and list but should not be able to delete
  locked objects. v1 documents per-provider least-privilege setups.
  Object Lock / append-only retention is a follow-up (work package I9).
- Rotating a key means setting the secret again. The next run picks it
  up, because run containers are created fresh each time.

## Rejected alternatives

- **Local-first for every plan** — doubles the storage for big NAS data
  the owner cannot fit locally; replaced by the per-plan route (A3).
- **Sharing the local password** — one leak opens both copies.
- **rclone-syncing the local repo volume** — this also copies local
  corruption or deletion off-site, and there is no independent retention.
- **Keeping secrets manual-only** — this cannot meet "like nothing
  happened"; it stays possible for owners who skip the recovery key.

## Work packages

| # | Package | Depends on |
|---|---|---|
| I1 | Truthful run results (exit-code arm) | — |
| I2 | `BackupTarget` kind, validation, policy, risk grading | — |
| I3 | Off-site routes in the engine (`offsite` block, `offsite-only` backup, `restic copy`, egress) | I1, I2 |
| I4 | Disk-space guard: `DiskSpace` op, reserve, approval/run/in-run checks, growth warning | I1 |
| I5 | NAS shares as backup sources and local repo locations | I4 |
| I6 | Selection: `paths`, `exclude`, owner-authored plans, data class | I2, I5 |
| I7 | Verification: `restic check`, off-site drill, freshness and shrink alerts | I3 |
| I8 | API, CLI and Console: plans list, targets, test connection, route picker, space and cost | I2–I6 |
| I9 | Whole-site set: Statefile bundle, runtime DB snapshot, sealed secrets, recovery kit | I3 |
| I10 | DB consistency hooks for DB-backed packages (pg_dump, sqlite) | I1 |
| I11 | Hardening: Object Lock, key rotation guide, provider guides | I3 |
| I12 | Total-loss runbook and CI exit drill against MinIO | I7, I9 |
