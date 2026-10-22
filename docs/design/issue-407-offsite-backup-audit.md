# Issue #407 — off-site (S3-compatible) backup: repository audit

Audit of what exists today, what is missing, and the proposed epic shape
for old private issue 407.
Goal: a user-configurable S3-compatible endpoint for disaster recovery,
where the user chooses which data goes off-site.

## 1. What exists today

The E6 backup engine is real and exercised weekly by the exit drill.

| Area | Where | What it does |
|---|---|---|
| Resource kind | `keystone/resource/resource.go:839` `BackupPlanSpec` | `volumes`, `repository` (a **local volume name**), `passwordSecret` (`secret/<name>`), `schedule` (daily/weekly/monthly), `retention` (keepDaily/Weekly/Monthly) |
| Engine | `keystone/backup/backup.go` | Keystone timer (`Tick`, default 5m) runs restic as a one-shot, digest-pinned container: `init` → `backup` → `forget --prune`; monthly drill; restore-alongside |
| Runner | `keystone/backup/runner.go` | `PullImage` → `EnsureContainer` → poll `InspectContainer` → remove. Uses only generic closed-oneof ops |
| Reconciler | `keystone/reconcile/backupplan.go` | Declared-only; never dispatches an op |
| Ledger | `keystone/runtimedb/backupruns.go` | `backup_runs` rows (backup/drill/restore) |
| API | `keystone/api/handlers_backups.go` | `GET /api/v1/backups`, `POST …/{plan}/run`, `POST …/{plan}/restore` (step-up) |
| CLI | `cli/cmd/wild/plan.go:133` | `wild backup list \| run \| restore` |
| Console | `console/src/views/Backups.tsx` | Per-plan cards from the ledger; Run now / Restore alongside |
| Secrets | `wardstone/internal/agent/secrets.go`, ADR-W012 | Write-only node store; `secret/<name>` env values materialized at container create |
| Packages | `packages/*/package.yaml` | 31 of 65 ship a hard-coded `BackupPlan` template (e.g. `photos-immich:231`) |
| Runbook | `docs/recovery.md`, `scripts/exit-drill.sh` | Total-host-loss procedure, proven weekly in CI |

Plan risk: BackupPlan create/update = Low, delete = Medium
(`keystone/plan/plan.go:666`). Approving a BackupPlan is the standing
approval for all its runs.

## 2. Gaps relevant to #407

### Blocking an S3 target
- `RESTIC_REPOSITORY` is hard-coded to `/repo` (`backup.go:310`); the repo
  must be a local volume. No `s3:`/`b2:`/`rest:` URL can be expressed.
- `resticSpec` builds a fixed two-key env map — no place for
  `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` references.
- No resource models a remote target (endpoint, bucket, prefix, region,
  path-style, credentials). No S3/rclone/MinIO code anywhere.
- Run containers attach no explicit network; egress from the restic
  container to an external endpoint is **unverified**.

### Undermining trust in any off-site copy
- **Success = container finished**, not restic's exit code
  (`backup.go:21`). A failed upload would be recorded as succeeded.
  (Fixed by #413: the exit code was already on the wire since protocol
  minor 11; the engine never read it.)
- No `restic check`; the only verification is the local restore drill.
- **No DB consistency**: live Postgres/SQLite dirs (immich-pgdata,
  paperless-pgdata, …) are copied while running. No pre/post hooks.

### Blocking user selection
- No data classes/priorities; `policies.dataSensitivity` is unused by backups.
- No include/exclude; unit of backup is a whole Docker volume.
- Host bind-mount data (`/srv/wildstone/...` media, files, books) is never backed up.
- No API/CLI/UI to list or edit BackupPlans; the Console only shows plans
  that have run at least once (`Backups.tsx:63`).

### Coverage and small bugs (not #407, but found)
- 22 packages with volumes have no BackupPlan — notably **identity**.
- No lint rule requires a package with Volumes to declare a BackupPlan.
- Console restore toast says "approve it on Plans" but the API restores
  immediately (`Backups.tsx:124` vs `handlers_backups.go:51`).

### Principle tension
`docs/vision.md:80`: *"Data stays home. If a design forces household data
off-site, the design is wrong."* The feature must be **opt-in, never
forced**, and only **client-side-encrypted ciphertext** may leave home.
Recommend an ADR that states this explicitly.

## 3. Proposed architecture (recommendation)

```
live volumes ──backup──▶ local restic repo (volume)   ← fast restore path (unchanged)
                               │
                               └──restic copy──▶ BackupTarget (S3-compatible)
                                                 ← off-site DR copy, per-plan opt-in
```

- **New kind `BackupTarget`** — `type: s3`, `endpoint`, `bucket`, `prefix`,
  `region`, `pathStyle`, `accessKeySecret` / `secretKeySecret`
  (`secret/<name>` refs, `provided: true`), own `passwordSecret`.
  Provider-agnostic: AWS, B2, Wasabi, R2, MinIO all speak S3.
- **`BackupPlan` gains `offsite: { target: <name>, schedule, retention }`**
  — the user's selection is per plan (i.e. per app/dataset). Local backup
  stays the source; off-site is `restic copy` from local repo to target
  (off-site repo initialised with `--copy-chunker-params` to keep dedup).
- **Why copy, not direct-to-S3:** live data is read once; local restores
  stay fast; the off-site repo has its own key and retention; a broken
  endpoint never blocks local backups.
- **Risk:** adding/pointing a plan at an off-site target = **High**
  (data leaves home) → human approval + step-up. Removing = Medium.
- **Selection UX:** Console toggle per plan, grouped by data class
  (critical / important / rebuildable) declared by packages.

## 4. Proposed epic breakdown

| # | Slice | Why / scope |
|---|---|---|
| 0 | ADR: off-site backup | Principle carve-out, copy model, risk grading, key-recovery model |
| 1 | Truthful run results | Engine reads the exit code the protocol already reports (minor 11) and fails runs on non-zero restic exit. **Prerequisite** |
| 2 | `BackupTarget` kind + validation | Schema, secret refs, policy (no literals), plan risk High |
| 3 | Off-site copy in engine | `offsite` block on BackupPlan, `restic copy`, off-site retention, container egress |
| 4 | Verification | `restic check` (+ `--read-data-subset`) on both repos; off-site drill; freshness alerts in morning report |
| 5 | API/CLI/Console | List plans (incl. never-run), manage targets, "test connection", per-plan off-site toggle; fix restore toast |
| 6 | Data classes | Package-declared class per BackupPlan; select-by-class UX |
| 7 | DB consistency hooks | Pre-backup dump (pg_dump / sqlite `.backup`) for DB-backed packages |
| 8 | DR runbook + exit drill | Total-loss restore from S3 only; MinIO in CI |
| 9 | Hardening (later) | Object Lock / append-only keys, credential rotation, cost view |

Slices 1–3 + 5 give a minimal usable feature; 4 and 8 are needed before
calling it trustworthy.

## 5. Owner decisions (2026-09-28)

1. **Per-plan route** — `offsite-only` (straight to S3, for big NAS data)
   or `local-and-offsite` (local repo, then `restic copy`). A disk-space
   guard stops a local copy from filling its disk; the local repo may
   live on a NAS. (Revised from "always copy from local".)
2. **Separate password** for the off-site repo.
3. **Whole-site recovery** — Statefile, runtime DB and (sealed) secrets go
   off-site too, so a total loss restores "like nothing happened".
4. **Host folders selectable** — the user decides, e.g. hard-to-replace movies.

Captured in [ADR-W045](../decisions/ADR-W045-off-site-backup.md).
