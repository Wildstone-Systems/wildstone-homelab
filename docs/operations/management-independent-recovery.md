# Management-independent Keystone recovery

Status: repository contract for #276 plus the repository-only signed journal
reconciliation, quarantine-release, promotion, and post-promotion restart
contracts from #284–#287. See the
[post-promotion validation](../design/post-promotion-recovery-validation.md).
The isolated
Kubernetes exercise below requires separate Owner approval. This is not
household migration, off-host acceptance, live quarantine-release acceptance,
full Flux-owned management acceptance, or HA. The narrower, accepted #317
[isolated Flux reconciliation proof](isolated-flux-keystone-proof.md) records
what was established without widening those claims.

## Why this is a separate recovery path

`keystone recover` and the `wildstone-recovery-set-v1` whole-site set remain
valid for their documented total-loss model: restore the canonical Statefile,
quarantined runtime database, household vault key and sealed node bundles,
then create a fresh CA and operation-signing identity and rejoin nodes. That
path deliberately does **not** restore the same management identity. It also
does not contain the private local deployment Git history.

Recovering the same Helm-managed Keystone installation therefore requires a
larger, quiesced consistency group and the distinct fail-closed preflight:

```sh
keystone recover preflight-management \
  -set /offline/restored-management-set \
  -keys /offline/separately-held-bootstrap-keys
```

The command is local and API-independent. It writes only temporary validation
copies. It does not contact Kubernetes, start Keystone, change a Git ref,
release restore quarantine, promote a coordinator, or grant approval.

After preflight, install the verified runtime group into a new chart 0.2
private data path while Keystone remains stopped:

```sh
keystone recover restore-management \
  -set /offline/restored-management-set \
  -keys /offline/separately-held-bootstrap-keys \
  -data-dir /mnt/new-runtime-claim/keystone
```

This command repeats the complete preflight, refuses an existing target,
restores into a mode-0700 sibling staging directory, and places the SQLite
database that will actually start in restore quarantine before atomically
publishing the target directory. It preserves the CA, operation-signing keys,
join-token records and household credential key byte-for-byte. It neither
starts Keystone nor releases quarantine or coordinator fencing. Do not replace
this command with a raw database copy.

## Recovery dependency and data inventory

The private recovery set is an encrypted backup artifact, never public source
content. `management-manifest.json` binds every file by SHA-256, the capture
time, immutable image digest, accepted Statefile revision, promoted deployment
revision, source/target chart versions and storage layout. It also records the
Owner-approved quiesce and original-writer fence. A boolean in a manifest is
not authorization by itself: the Owner retains the reviewed capture record.

### Mandatory same-installation data

| Material | Why it is mandatory | Current source/validation |
| --- | --- | --- |
| `statefile.bundle` | Canonical product intent and all publication refs | `statefile.Bundle`; preflight restores the bundle and requires the runtime DB's exact accepted commit |
| `keystone.db` | Principals/tokens, authorization and revocation, audit, Plans, publication journals, task/routine/subscription checkpoints and receipts, restore journal, coordinator holder/epoch | `runtimedb.BackupTo`; preflight restores a copy through `RestoreBackup` and proves quarantine and coordinator state |
| `ca.crt`, `ca.key` | Existing Wardstone client identity trust | Preflight parses the CA and proves the certificate/private-key pair |
| `op-signing.key`, `op-signing.pub` | Existing operation signature trust | Preflight proves the Ed25519 pair |
| `tokens.json` | Minted/used node join-token continuity | Preflight uses the Gateway startup decoder and rejects malformed token identifiers, null records and missing/invalid expirations; no fresh replacement is accepted |
| `household-secrets.key` | Decrypts household-scoped credentials in the runtime DB | Exact file must accompany the matching DB; missing/wrong keys remain fatal at Keystone startup |
| `management-values.yaml` and immutable image digest | Reconstructs the reviewed chart deployment | Private installation values; no plaintext secret values |
| `deployment-config.json`, `local-git-source.json`, `deployment.sops.yaml` | Restores private placement, local source configuration and encrypted deployment input | Private bootstrap/configuration, outside both public and deployment Git |
| `promotion.bundle`, `delivery.bundle` | Approved local deployment history and the exact ref Flux read | Preflight clones both independently and requires `refs/heads/deploy` at the recorded promoted commit |
| SOPS/age private key, local Git SSH host key, Flux source-client private key | Decryption and continuity of the independent Git endpoint | Required from a **separate key directory**, not from the encrypted data repository alone |
| `recovery-journal.pub` plus separately retained custodian private key | Pins the independent authority that signs terminal deletion/revocation coverage | Public key is required from the separate key directory and copied into the restored private runtime; private key never enters the runtime claim or snapshot |
| Shared-upload and private-workspace captures when enabled | Preserves configured bindings and bytes that are not in the runtime claim | Manifest makes each conditional artifact mandatory; claim name alone is not data |

The recovery set must also retain the reviewed namespace/release names,
ServiceAccount and ConfigMap/Secret references, StorageClass and claim binding
inventory, local Git service account/permissions, known-host fingerprint,
approved source revision, chart artifact identity and management image digest.
Those values are configuration and evidence, not permission to mutate a
cluster.

### Optional application data

Provider cache files, transient Console sessions, `/tmp`, generated serving
leaf certificates and `mail-excerpts.key` are not required to identify the
same management installation. The mail excerpt key is intentionally excluded
from backups; restore scrubs sealed excerpts and requires reauthorization.
`provider-keys.json` and `policy-exemptions.yaml` are conditional management
data: if present/configured, set their manifest `Required` fields and capture
them; preflight then refuses their absence. Personal credentials excluded by
the existing backup contract must be re-entered by their owners.

Application PVCs, NAS libraries, identity-directory databases, certificate
issuer state and external provider state keep their own application-consistent
recovery groups. They are not silently folded into the Keystone SQLite copy.

### External prerequisites

An Owner still needs a supported Linux/K3s host, Kubernetes API access,
container artifacts (or an independent local cache), Git/OpenSSH/systemd,
storage provisioner, network bindings, time and DNS as applicable. Flux,
source-controller and the local read-only Git endpoint must operate without
Keystone. SOPS/age decryption, identity/Traefik/TLS and backup executor custody
remain governed by #162, #165 and #163 respectively.

## Capture contract

1. Record explicit Owner approval for the named installation, source/target,
   immutable image/chart revisions and capture destination.
2. Stop Flux reconciliation for the management release using the selected
   approved mechanism, then scale/stop the sole Keystone writer. Do not merely
   trust a Lease timeout. Prove no Keystone process has the runtime claim open.
3. Capture the runtime DB with `BackupTo` while the installation is quiesced,
   bundle the Statefile and both Git repositories, and copy identity/key/config
   files plus enabled upload/workspace groups. Capture one consistency point;
   do not combine files from independent times.
4. Hash every artifact into `management-manifest.json`. Record chart 0.1 as
   `claim-root` or chart 0.2 as `private-keystone-child`; record target 0.2.0.
5. Store the encrypted set independently of the management claim/host. Store
   recovery repository credentials and SOPS/Git bootstrap private keys in the
   separate offline/password-manager custody path. Test access from the
   proposed recovery target.
   Separately retain terminal signed deletion/revocation evidence as specified
   by the [#284 reconciliation contract](../design/recovery-journal-reconciliation.md).
   The SQLite snapshot cannot prove that its own embedded journal is current.
6. Run the preflight from an installed binary built from the reviewed source.
   Any missing file, changed hash, mismatched key pair, absent Git revision,
   Statefile/DB disagreement, missing coordinator state, incomplete binding,
   or absent approval/fence/quiesce record is a refusal, not a reason to start
   an empty Keystone.

This slice validates already captured synthetic material; it does not add a
scheduler or backup engine. #163 must select and integrate the established
executor that performs the real quiesced capture.

## Independent recovery sequence

All Kubernetes-changing steps below are gated on a separate reviewed Owner
approval. The original Keystone API is never used.

1. From the independent copy, verify the manifest and run
   `keystone recover preflight-management`. Preserve the source set read-only.
2. Confirm the old release/Pod is stopped or physically/storage fenced. If its
   status cannot be proven, stop: a new writer is unsafe.
3. Recreate the private local promotion and delivery repositories from their
   bundles into new, empty 0700 paths. Verify the promoted commit. Restore the
   root-controlled OpenSSH configuration and host key; restore the scoped Flux
   client Secret and pinned known-host key through privileged bootstrap. Start
   the read-only Git endpoint and mirror independently of Keystone.
4. Bootstrap Flux from separately held, approved bootstrap configuration. The
   management GitRepository/Kustomization/HelmRelease must be isolated from the
   existing prune-enabled root until #166's Flux-owned-management gate is
   approved. Flux remains the reconciler; do not overlap it with direct Helm.
5. Create new target claims. Mount the fresh runtime claim in an offline helper
   environment and run `keystone recover restore-management` with `-data-dir`
   naming the not-yet-existing chart 0.2 `keystone/` child. The command accepts
   a declared chart 0.1 `claim-root` source but always installs the normalized
   private-child target and quarantines the actual target database. Never point
   chart 0.2 at the old nonempty root and never copy `keystone.db` directly.
   Set the resulting child ownership to UID/GID 65532 without changing its
   mode-0700 protection. Restore enabled uploads/workspaces to their distinct
   claims. No missing claim may fall back to an image directory or `emptyDir`.
6. Before any normal Deployment exists, restore private ConfigMap/Secret
   references and SOPS decryption material through the approved bootstrap.
   Render the exact chart 0.2.0 singleton/Recreate Deployment using the bound
   image digest. Verify one runtime claim and no second writer.
7. Start the restored Keystone in quarantine. It must load the preserved CA,
   operation-signing pair, household key, principals/tokens/revocations,
   accepted Statefile, task/routine receipts and old coordinator lease. Any
   first-start bootstrap token, new CA/signing key, empty Statefile/DB, or
   automatic coordinator takeover fails the exercise.
8. Use the existing host-local, stopped-service `keystone claim-code` only if
   no preserved Owner credential is usable. It mints a short-lived recovery
   credential in the preserved DB and is audited; remove the output file after
   use. `keystone owner` remains the root-equivalent identity lockout path.
9. With the original writer independently fenced, create or retrieve the
   terminal signed deletion/revocation evidence held outside the snapshot.
   Authenticate through a fresh current Owner Console session and submit the
   evidence to `POST /api/v1/restore/reconcile`. Exact snapshot identity,
   journal prefix/sequence/hash continuity, terminal coverage, preserved-key
   signature and custody metadata must verify. The result remains quarantined.
   If no independent post-snapshot evidence exists, stop: the snapshot and an
   Owner acknowledgement cannot prove their own freshness. Do not combine
   reconciliation with release.
10. From a fresh current Owner Console session, explicitly send
    `{"release":true}` to `POST /api/v1/restore/release`. Keystone revalidates
    the generation-bound immutable audit, exact snapshot, preserved journal
    trust root, terminal source fence and completed replay. The result releases
    data quarantine but retains the old coordinator holder/epoch and all
    execution fencing. See the
    [#285 release contract](../design/restore-quarantine-release.md).
11. While the replacement is serving reads but dispatch is fenced, the Owner
    calls `POST /api/v1/coordinator/promote`. The endpoint revalidates the
    released generation-bound signed evidence and source fence, records the
    immutable old/new holder and epoch, and leaves the current process fenced.
    Operators, administrator bearers, Agents and revoked/disabled Owners must
    be refused. See the
    [#286 promotion contract](../design/restored-coordinator-promotion.md).
    Restart the singleton under #287 so it acquires the new epoch and performs
    interrupted-attempt/routine recovery. Verify the old epoch cannot commit.
12. Verify preserved login/API credential behavior, revoked credential denial,
    Statefile head/resources, local deploy ref and Flux-observed revision,
    completed/uncertain execution history, receipt idempotency, uploads and
    private workspace markers. Only then approve relinquishment of the old
    fenced target; cleanup is a separate decision.

## Proposed development-VM proof (not authorized yet)

Read-only inspection on 2026-11-02 found K3s/Kubernetes
`v1.37.1+k3s1`, default `local-path` storage with `Delete` reclaim policy,
Flux controllers ready in `flux-system`, and retained direct-Helm namespaces
`wildstone-mgmt-270` and `wildstone-mgmt-271`. Those retained resources are
out of scope and must not be reused, modified or cleaned up.

After Owner approval, use exactly:

- namespace `wildstone-mgmt-276`, enforcing/auditing/warning
  `restricted:v1.37`;
- release `ws276-management`, chart `wildstone-management` 0.2.0 from the
  approved PR commit. The repository proof built commit `77bd5a70` as version
  `0.0.0-issue276`, smoke-tested it, and recorded the local-only digest
  `sha256:192aa76cc043065576c1bb74b2ee35907267dd7a7731689c5ead3721c33716c2`.
  Rebuild the final approved PR head if it changes and record/use that exact
  imported digest, never a tag;
- new `local-path` PVCs `ws276-runtime`, `ws276-uploads` and
  `ws276-workspaces`, plus separate target PVCs `ws276-restored-runtime`,
  `ws276-restored-uploads` and `ws276-restored-workspaces`;
- synthetic Statefile resource, admin and Owner credentials, one revoked API
  credential, valid approved deterministic routine, completed receipt,
  uncertain claimed effect, coordinator lease, upload marker and private
  workspace marker. No real provider/model/household effect;
- a private host directory outside the source checkout and all PVC backing
  paths for the recovery set, and a second 0700 directory for synthetic
  SOPS/Git keys. Generate all values for the exercise and retain only hashes
  in the issue/PR;
- isolated host paths/repositories and a dedicated local Git endpoint/source
  identity for `ws276`; do not reuse the household source or existing root
  Kustomization. A separately reviewed non-pruning management Flux path is
  required before claiming Flux-owned management.

Procedure: install the synthetic source singleton, create/verify the fixture,
quiesce and capture it, stop the original Deployment and prove no process has
the runtime claim, then make its API unavailable by leaving the original
scaled to zero. Run host-side preflight, then run offline `restore-management`
with the fresh restored runtime claim mounted and its new `keystone/` child as
`-data-dir`; restore only into the `restored` claims. Initiate the restored
release with standard `k3s kubectl`, Helm/Flux
and local Git tools; do not call the original API. Keep the restored runtime
quarantined and non-coordinating until identity/data/revocation/history checks
pass, then use an authenticated current Owner for journal release and explicit
coordinator promotion, restart once, and prove stale-epoch fencing.

Failure cases must separately remove one copied artifact, one offline key and
one claim reference; tamper with a hash/key pair; present chart-0.1 root data
directly to chart 0.2; omit Owner approval; and attempt simultaneous writers.
Every case must fail before dispatch or replacement initialization. Restore
the fixture from the immutable source between cases rather than repairing it
in place.

Retain the new namespace, claims, local image and private synthetic recovery
material until Owner review, unless the approval explicitly selects cleanup.
Any later cleanup names only `wildstone-mgmt-276` and its dedicated host
paths. The existing #270/#271 resources remain untouched.

This is a same-node synthetic restore because both `local-path` PVs and the
independent host directories still share one VM and physical disk. Genuine
off-host evidence requires a different host/storage failure domain, an
independently reachable encrypted backup, separate keys, artifact availability
and a rebuilt local Git/Flux bootstrap. Do not report the same-node proof as
off-host recovery.

## Remaining decisions and gates

- #162: final SOPS/age custody, rotation and recovery mechanism, including
  which bootstrap Secret owner supplies the Flux decryption key.
- #163: established scheduler/executor and atomic capture of runtime, Git,
  optional claims and keys; actual off-host restore execution.
- #165: identity-directory, Traefik, certificate/internal-CA and DNS recovery.
- #166: #317 accepted an isolated source/Kustomization/HelmRelease, independent
  source operation while Keystone was down, Pod replacement, continuity, and
  fail-closed input checks. A reversible source revision change/rollback and
  full management recovery acceptance remain separately gated.
- #168: data-preserving chart 0.1-to-0.2 ownership migration and rollback with
  new writes retained.
- #180: HA. This contract remains one SQLite writer and does not authorize a
  replica-count shortcut.

No cutover, release adoption, household migration, public routing, host reboot,
fault injection, retained-resource cleanup or off-host claim follows from this
document.
