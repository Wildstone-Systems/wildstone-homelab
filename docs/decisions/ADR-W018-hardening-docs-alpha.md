# ADR-W018: Hardening, docs, alpha release (E13)

- **Status:** accepted; A2 amended by ADR-W033; A2 item 3 amended by
  ADR-W045 A5 (secrets recovered from a sealed bundle); constrained by
  ADR-W030 (Butler runtime state and restore quarantine)
- **Governed by:** ADR-W033 where they overlap.
- **Relates to:** ADR-W002 (e2e boundary), ADR-W011 (backup engine),
  ADR-W012 (secrets path), ADR-W014 (installer), implementation scope E13
- **Audience:** decision record AND implementation spec for E13.

## Constraints from ADR-W030

- Durable Butler runtime state (task/step ledgers, conversations and
  selected memory, grants and immediate denials, routines, access settings,
  delivery state and work traces) joins the recovery contract. Its off-host
  copy publishes a measured recovery point; replication lag is possible
  data loss. See [`docs/recovery.md`](../recovery.md).
- A restore starts Butler in quarantine: no retrieval, dispatch, ingestion
  or proactive delivery until current authority and later
  deletions/revocations are reconciled (ADR-W030 A10).
- The exit drill grows a restore/revocation case: restore an older snapshot
  holding access and content revoked afterward, prove it stays
  unavailable, and prove uncertain pending actions do not resume.
  _Code note: `scripts/exit-drill.sh` does not run this case yet; quarantine
  is covered by unit tests in `keystone/runtimedb` and `keystone/api`._

---

# Part A — Decisions (binding)

### A1. The exit drill is a script over real binaries, run weekly in CI

The scope's exit drill — "total-host-loss restore, unattended, on the
e2e harness" — runs as `scripts/exit-drill.sh`: an unattended script
that builds the real `keystone`, `wardstoned`, and `wild` binaries and
drives them against the runner's real Docker daemon and the pinned
restic image. No mocks anywhere in the drill path: real join, real
plan proposal/approval over the public API, real reconcile creating
real Docker volumes, a real restic backup, real host loss (processes
killed, data dirs and the data volume destroyed), and a real
restore-alongside from the surviving repository.

Hosted runners provide Docker but not nested-VM comfort, so the drill's
"host" is the process+state boundary, not a VM: everything Keystone and
Wardstone persist is destroyed except the two things the recovery doc
(A3) tells operators to keep off-host — the restic repository and a
replica of the Statefile git repo. A VM-boundary variant on the tailnet
runner (ADR-W002's proxmox provider) can wrap the same script later;
the script is the drill either way.

`.github/workflows/exit-drill.yml` runs it weekly (cron) and on
`workflow_dispatch`. A drill failure is a release blocker: the alpha
promise is "you can lose the host and keep your data", and this is the
only executable proof of it.

### A2. What must survive, verbatim (the recovery contract)

The local-repository drill and the S3-only total-loss drill pin the recovery
contract the docs teach. In the total-loss case the host, NAS, Statefile
replicas and local restic repositories may all be gone; the off-host recovery
kit and S3 repositories are the root of recovery:

1. **The off-site restic repositories** (per BackupPlan) — encrypted
   application data. A local repository or NAS copy is useful but is not a
   required survivor.
2. **The reserved `wildstone` repository** — a Statefile Git bundle, a
   consistent runtime-DB snapshot, the sealed household-vault key, and a
   recovery-key-sealed secret bundle for each node (ADR-W045 A5).
3. **The recovery kit, held outside Wildstone** — S3 endpoint and
   bucket/prefix, a read-only recovery credential, all off-site repository
   passwords, and the offline recovery private key. This replaces the old
   assumption that a separate Statefile replica and manually re-entered node
   secrets must survive, while retaining those as valid fallback inputs.
4. **A deletion/revocation journal source newer than the snapshot** — restore
   cannot infer changes that happened after its recovery point.
5. **The runtime DB inside the whole-site set** — it is not rebuildable. It
   is not rebuildable. It holds Persons and their bindings, conversations
   and memory, task ledgers, Plans and approved routine versions,
   grants and denials, soft-deleted records awaiting Owner purge, and
   the forgetting journal (ADR-W033 C10, E5, F6), together with
   structured work traces, feedback labels and evaluator/configuration
   versions under the same source-derived permissions and retention.
   Its measured recovery point is published; replication lag is possible
   data loss, not proof of zero loss.

Everything else — CA and op-signing keys, node identity, containers,
volumes — is rebuilt from those inputs. A new CA on rebirth is correct
behavior, not a defect: nodes re-join with fresh join tokens.

**Restore never resurrects what was forgotten or revoked.** Forgetting
survives restore (ADR-W033 C10). A restored runtime DB starts
quarantined: memory retrieval, task and routine dispatch, ingestion and
proactive delivery stay blocked until later authority and deletion
information is reconciled. Where current permission cannot be
established, data stays unavailable and pending actions do not resume;
a restored trace cannot expose content whose source was later deleted
or revoked. The drill proves this by restoring an older snapshot that
predates a deletion and a revocation.

_Changed by ADR-W033 §C10, §E5, §F6: the runtime DB now holds household state that cannot be rebuilt. Changed by ADR-W045 A5/A6: the sealed whole-site set and recovery kit make S3 sufficient after total local loss._

`scripts/offsite-exit-drill.sh`, run by the weekly exit-drill workflow, is the
executable proof: it destroys the Statefile, runtime DB, node identity, live
volume and all local repositories; restores the control plane from the kit;
keeps the runtime DB quarantined; imports sealed node secrets; approves the
High-risk target drift; and verifies application canary bytes restored from
S3 alone.

### A3. Docs are part of the deliverable, refreshed against as-built

`docs/install.md` (both roles, from the released installer),
`docs/concepts.md` (resources, plans, capabilities, the Butler's
ceiling), `docs/recovery.md` (the A2 contract as a runbook, mirroring
the drill step-for-step), and `docs/threat-model.md` (refreshed against
what was actually built: closed op surface, plan-mediated writes,
encoder-level redaction, envelope hardening, RBAC, rate limits). Docs
state what the code does today, citing the ADR that decided it — no
aspirational features.

### A4. Dogfood migration is a runbook with a hard cutover rule

`docs/migration.md` captures the dogfood parallel-VM migration:
hand-managed and Wildstone-managed workloads may coexist (the drift
classifier's never-delete-foreign rule protects foreign resources), but
**ingress and identity are singular**. Apps cut over per-capability via
DNS; identity migrates exactly once (LLDAP export/import, old directory
retired same day); ingress ownership transfers atomically per hostname,
never incrementally. Friction found during the migration is filed in
`docs/friction-log.md` and burned down before alpha exit.

# Part B — Work packages

- **H1** — A1/A2: `scripts/exit-drill.sh` + weekly workflow + CI lint
  coverage for all of `scripts/`.
- **H2** — A3: install/concepts/recovery/threat-model docs.
- **H3** — A4: migration runbook + friction log.
