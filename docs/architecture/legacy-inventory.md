# Legacy reference and reuse inventory

Status: **preparatory inventory for #350**, inspected at `1ef9d7c`. It
classifies the repository for the in-place replacement decided in #338.
It authorizes **no deletion and no change to running systems**.
Classification and proposed gates are preliminary historical notes, not
an implementation order or authorization to retire code. Build and
validate the replacement first. Consult specific rows for reuse only
when the corresponding new implementation needs them.

`tools/architecture/check_boundaries.py` (CI job `architecture
boundaries`) keeps this file honest:

- every top-level entry in the repository must have a row here;
- every path listed here must still exist.

So a PR that adds a top-level directory, or deletes a listed path, must
edit this file in the same PR.

## What the words mean

| Disposition | Meaning |
|---|---|
| **Retain** | Likely useful in the final repository; confirm during consolidation. |
| **Adapt** | Potentially reusable logic, pins or tests. Inspect as needed during implementation; extract or move with independent tests, without creating legacy runtime dependencies. |
| **Retire** | Candidate for eventual cleanup **after** the standard new platform passes end-to-end validation. No deletion is scheduled now. |

The final column ("Gate before the change") records the original audit's
**hypothetical** evidence thresholds. They are not active deletion gates;
no deletion follows automatically from completing #341–#348.

"Evidence" is the existing test or proof that shows the code works. It
was **not rerun** for this inventory unless noted. #340 holds the
detailed storage audit behind the `deploy/` and `scripts/engine-*` rows.

## Top-level entries

| Path | Disposition | Why / evidence | Owner | Gate before the change |
|---|---|---|---|---|
| `.github` | Retain | Workflows, rulesets, templates. Per-workflow rows below. | #350 | — |
| `.dockerignore` | Retire | Only covers legacy image builds (`console/dist`, `runtimes/`). | retirement PR | Last legacy Docker build removed. |
| `.gitignore` | Retain | Repo hygiene. Prune legacy entries when their paths go. | — | — |
| `.gitleaksignore` | Retain | Secret-scan allowlist. Remove entries for deleted paths in the same deletion PR; never widen it to make a PR pass. | — | — |
| `.ignore` | Retain | Search config for the optional Graft code graph. | — | — |
| `.mcp.json` | Retain | Optional contributor tooling (Graft); not runtime. | — | — |
| `CONTRIBUTING.md` | Retain | Updated in #350 to point new work at `docs/architecture/`. | #350 | — |
| `LICENSE` | Retain | AGPL-3.0. | — | — |
| `README.md` | Retain | Updated in #350: new path is authoritative; legacy labelled. Rewrite the quick start when the new installer ships. | #350, #341–#345 | Installer switch (see gates). |
| `SECURITY.md` | Retain | Reporting policy applies to both trees. | — | — |
| `charts` | Retire | `wildstone-management` packages Keystone+Console+native runtime (#269). The pinned-Helm test pattern is adapted, not the chart. | retirement PR | Keystone gate. |
| `cli` | Retire | `wild` imports `keystone` (`cli/go.mod`). The new CLI is written fresh under `bootstrap/`. `cli/cmd/agent-bridge`, `chat-adapter` belong to #339. | #341, #343 | New CLI covers create/join/app install; #339 decision for agent commands. |
| `console` | Retire | Legacy React Console; replaced by Homarr (#345). | #345 | Homarr + identity on LAN proven (#345). |
| `deploy` | Adapt (part) | K3s/Longhorn Ansible is the best reusable code. Per-file rows below. | #341 | — |
| `docs` | Retain | ADRs stay as history. `docs/architecture/` is authoritative for new work. | #350 | — |
| `go.work` | Retire | Workspace of legacy modules only. New Go modules must **not** be added (checked by CI). | retirement PR | Last legacy Go module deleted. |
| `go.work.sum` | Retire | Goes with `go.work`. | retirement PR | Same as `go.work`. |
| `installer` | Retire | Placeholder README pointing at `scripts/install.sh`. `bootstrap/` replaces it. | #341 | `bootstrap/` exists (delete in that PR). |
| `keystone` | Retire | 1,373 files; control plane, Plans, Statefile, publisher, backup/recovery engines. Sub-rows below list what is worth re-deriving. | #341–#347 | Keystone gate. |
| `packages` | Retire | Legacy Docker app catalog. Per-app data/OIDC knowledge is reference input for #345/#346, re-derived as upstream Helm values. | #343, #346 | Catalog gate. |
| `packaging` | Retire | Management image + `wardstoned` unit. `host-logging/` row below. | retirement PR | Keystone + Wardstone gates. |
| `proto` | Retire | gRPC contract between Keystone, Wardstone and `wild`. | retirement PR | Keystone + Wardstone gates. |
| `release` | Retire | `management-policy.json` for the management release (#316). | retirement PR | Keystone gate. |
| `runtimes` | Retire | Native agent + DDGS worker; evaluation moved to #339. | #339 | #339 decision. |
| `scripts` | Adapt (part) | Mixed: legacy installer and drills, plus reusable checks. Per-file rows below. | #341, #347 | — |
| `testing` | Adapt (part) | QEMU harness reusable; installer/Keystone tests retire. Rows below. | #341, #348 | — |
| `tools` | Retain | New: `tools/architecture/` boundary checker (#350). | #350 | — |
| `wallpapers` | Retire | Console theme images only. | #345 | Console gate. |
| `wardstone` | Retire | Typed host daemon + `wild-playbooks` signer. No persistent host daemon in the new design (#338). | #341, #347, #348 | Wardstone gate. |

## `deploy/` — the reusable K3s/Longhorn code

Evidence for the K3s + Longhorn files: `testing/e2e` `TestEngine` in a
disposable QEMU Ubuntu 24.04 guest — Ready node, rerun `changed=0`,
tamper refusal, 7/7 storage checks (#207, run 40449781118, cited in
#340). That was a single node and Longhorn applied from a vendored
manifest, not the new create/join installer.

| Path | Disposition | Why / evidence | Owner | Gate before the change |
|---|---|---|---|---|
| `deploy/ansible/prereqs.yml` | Adapt | open-iscsi, nfs-common, `iscsi_tcp`, iscsid, scoped multipath exclusion. Tighten "Debian or Ubuntu" to Ubuntu 24.04 + kernel/package qualification. | #341 | Moved into `platform/` with a QEMU test. |
| `deploy/ansible/k3s-server.yml` | Adapt | Checksum-verified K3s download, idempotent install. Drop `/etc/wildstone` Wardstone paths and the VIP branch. | #341 | Same. |
| `deploy/ansible/templates/k3s-config.yaml.j2` | Adapt | Keep `secrets-encryption`, `protect-kernel-defaults`, `disable-force-detach-on-timeout`. **Change:** it disables Traefik, which the standard profile needs. | #341 | Same. |
| `deploy/ansible/templates/k3s.service.j2` | Adapt | systemd unit for the pinned binary. | #341 | Same. |
| `deploy/ansible/templates/pod-security.yaml.j2` | Adapt | Cluster Pod Security defaults (K3s hardening guide). | #341 | Same. |
| `deploy/ansible/templates/namespaces.yaml.j2` | Adapt | restricted-by-default namespaces pattern. Rename away from `wildstone-system` legacy roles. | #341, #343 | Same. |
| `deploy/ansible/engine-storage.yml` | Adapt | Hash-checked manifest, image pull by digest, manager/CSI readiness. #341 requires **direct Helm** Longhorn, so keep the pin-and-verify pattern, replace the `kubectl apply` of a vendored manifest. | #341 | Longhorn via pinned Helm passes the storage checks. |
| `deploy/ansible/vars/engine-versions.yml` | Adapt | Longhorn 1.13.0 image digests, K3s pin approach. Re-qualify versions for Ubuntu 24.04 (#341); do not reuse as authority. | #341 | Same. |
| `deploy/ansible/vendor` | Retire | Vendored Longhorn manifest; superseded by a pinned Helm chart. | #341 | Same. |
| `deploy/ansible/templates/longhorn.yaml.j2` | Retire | Image-rewrite template for the vendored manifest. | #341 | Same. |
| `deploy/ansible/templates/longhorn-storageclass.yaml.j2` | Adapt | Retain/expansion/WaitForFirstConsumer is right. **Change:** replica count = min(all nodes, 3) is wrong; use explicit `r1` on one eligible node (#340 §4.2). | #341, #348 | Same. |
| `deploy/ansible/ansible.cfg` | Adapt | "Only this config applies" pattern. Comment names Wardstone; rewrite. | #341 | Same. |
| `deploy/ansible/engine-addons.yml` | Retire | Applies the Keystone authority record, leader fence and runtime identities. | #341 | K3s path works without it. |
| `deploy/ansible/templates/authority.yaml.j2` | Retire | Keystone authority record. | #341 | Same. |
| `deploy/ansible/templates/fence-policy.yaml.j2` | Retire | Admission fence for Wardstone leaders. | #341 | Same. |
| `deploy/ansible/templates/runtime-rbac.yaml.j2` | Retire | RBAC for `wardstone-<node>` and `keystone`. | #341 | Same. |
| `deploy/ansible/engine-runtime-tokens.yml` | Retire | Runtime tokens for Wardstone/Keystone. | #341 | Same. |
| `deploy/ansible/files` | Retire | Token rotation for those identities. | #341 | Same. |
| `deploy/ansible/templates/wildstone-runtime-token@.service.j2` | Retire | Same. | #341 | Same. |
| `deploy/ansible/templates/wildstone-runtime-token@.timer.j2` | Retire | Same. | #341 | Same. |
| `deploy/ansible/engine-vip.yml` | Retire | kube-vip is not needed for one server. Re-evaluate a stable API endpoint in #349. | #349 | — |
| `deploy/ansible/templates/kube-vip.yaml.j2` | Retire | Same; #159 evidence did not prove failover. | #349 | — |
| `deploy/ansible/k3s-upgrade.yml` | Retire | Signed Wardstone run-playbook entry. Upgrade becomes a bootstrap command later. | #341 | — |
| `deploy/ansible/os-update.yml` | Retire | Same; OS updates are Ubuntu's (unattended-upgrades). | — | Wardstone gate. |
| `deploy/ansible/wardstone-update.yml` | Retire | Updates `wardstoned`. | — | Wardstone gate. |
| `deploy/ansible/runplaybooks.catalog` | Retire | Wardstone signed-playbook catalog. | — | Wardstone gate. |
| `deploy/systemd` | Retire | `keystone.service`, `wardstoned.service`. | — | Keystone + Wardstone gates. |

## `scripts/`

| Path | Disposition | Why / evidence | Owner | Gate before the change |
|---|---|---|---|---|
| `scripts/engine-storage-check.sh` | Adapt | Proves RWOP binding, data across Pod replacement, second-writer refusal, Retain after PVC delete (#207). Becomes a `platform/` storage test; add reboot and missing-mount cases (#341). | #341 | Moved with a QEMU test. |
| `scripts/engine-resource-sample.py` | Adapt | Host CPU/RAM/disk sampler; CI smoke in `ci.yml`. Drop the Docker baseline label. | #341 | Same. |
| `scripts/disk-guard.sh` | Adapt | Root headroom + required-mount refusal; offline tests in `scripts/test-disk-guard.sh` (CI). Fits #341 "missing disk/mount refusal" and #347 backup guards. | #341, #347 | Same. |
| `scripts/test-disk-guard.sh` | Adapt | Tests for the above; move together. | #341, #347 | Same. |
| `scripts/engine-try.sh` | Retire | Runs the Wardstone-signed set beside Docker. Replaced by `bootstrap` create. | #341 | New create path has a QEMU test. |
| `scripts/engine-fence-check.sh` | Retire | Tests the Wardstone leader fence. | #341 | Same. |
| `scripts/install.sh` | Retire | The **published** Docker/systemd installer. Do not change it until the new installer ships. | #341–#345 | Installer switch. |
| `scripts/uninstall.sh` | Retire | Uninstalls the above. | #341–#345 | Installer switch. |
| `scripts/exit-drill.sh` | Retire | Keystone+Wardstone total-host-loss drill (ADR-W018). Its *outcome* becomes a #347 gate. | #347 | #347 isolated site-loss drill passes. |
| `scripts/offsite-exit-drill.sh` | Retire | S3-only variant of the above. | #347 | Same. |
| `scripts/object-lock-drill.sh` | Adapt | Off-site object-lock retention check; uses only disposable MinIO containers (no Keystone/Wardstone references at `1ef9d7c`); reusable as a #347 R2/S3 validation. | #347 | Re-reviewed when moved. |
| `scripts/build-native-agent.sh` | Retire | Native agent package. | #339 | #339 decision. |
| `scripts/build-ddgs-worker.sh` | Retire | DDGS worker image. | #339 | #339 decision. |
| `scripts/management-release.py` | Retire | Management release metadata (#316). The signed-release *pattern* informs the bootstrap release in #341. | #341 | Keystone gate. |
| `scripts/test_management_release.py` | Retire | Tests the above. | — | Keystone gate. |
| `scripts/verify-management-release.sh` | Retire | Same. | — | Keystone gate. |
| `scripts/test-management-chart.sh` | Retire | Pinned-Helm lint/template test for the management chart; copy the pinned-Helm idea for `platform/` charts. | #341 | Keystone gate. |
| `scripts/test-management-image.sh` | Retire | Management image test. | — | Keystone gate. |
| `scripts/console-coverage.py` | Retire | Console coverage report. | — | Console gate. |
| `scripts/docx-to-md.py` | Retire | One-off doc conversion. | — | Any time. |

## `testing/`

| Path | Disposition | Why / evidence | Owner | Gate before the change |
|---|---|---|---|---|
| `testing/e2e/local.go` | Adapt | QEMU provider for disposable Ubuntu 24.04 guests (ADR-W002); module has no Wildstone imports. Basis for #341 and #348 VM tests. | #341, #348 | Moved with a passing VM test. |
| `testing/e2e/seed.go` | Adapt | cloud-init seed ISO for those guests. | #341 | Same. |
| `testing/e2e/provider.go` | Adapt | Provider interface. | #341 | Same. |
| `testing/e2e/proxmox.go` | Retire | Proxmox is a migration source, not a target (#338). | — | Any time after the move. |
| `testing/e2e/engine_test.go` | Retire | Drives `engine-try.sh` with `wild-playbooks`. | #341 | New VM test replaces it. |
| `testing/e2e/engine-guest.sh` | Retire | Same. | #341 | Same. |
| `testing/e2e/e2e_test.go` | Retire | Legacy installer smoke. | — | Installer switch. |
| `testing/e2e/installer_owner_test.go` | Retire | Legacy installer. | — | Installer switch. |
| `testing/e2e/installer_rerun_owner_test.go` | Retire | Legacy installer rerun; the *idea* (rerun is non-destructive) is a #341 acceptance test. | #341 | Installer switch. |
| `testing/e2e/installer_calendar_resume_test.go` | Retire | Legacy installer. | — | Installer switch. |
| `testing/e2e/adrreconciliation_test.go` | Retire | Legacy ADR/audit structure tests. | — | Keystone gate. |
| `testing/e2e/auditcurrent_test.go` | Retire | Same. | — | Keystone gate. |
| `testing/e2e/auditregister_test.go` | Retire | Same. | — | Keystone gate. |
| `testing/fixtures/authelia` | Adapt | Scrubbed Authelia config reference for #345 Helm values. | #345 | Re-derived in `catalog/` or `platform/`. |
| `testing/fixtures/lldap` | Adapt | LLDAP reference for #345. | #345 | Same. |
| `testing/fixtures/immich` | Adapt | Immich OIDC reference for #346. | #346 | Same. |
| `testing/fixtures/jellyfin` | Adapt | Jellyfin LDAP reference for #346. | #346 | Same. |
| `testing/fixtures/caddy` | Retire | Traefik replaces Caddy. | — | Keystone gate. |
| `testing/fixtures/gluetun` | Retire | Not in the standard profile. | — | Keystone gate. |
| `testing/fixtures/mail` | Retire | Mail monitoring belongs to #339. | #339 | #339 decision. |
| `testing/fixtures/routines` | Retire | Legacy automation; #280 owns the replacement. | #280 | — |
| `testing/fixtures/home-automation` | Adapt | Reference node for #346 radio/affinity tests. | #346 | Same. |
| `testing/home-automation` | Adapt | Real-hardware radio runbook; `vm-check.sh` redacts serials. Basis for #346 node-affine Home Assistant proof. | #346 | Same. |

## `keystone/` — outcomes to re-derive, not code to import

These packages are **candidates for later retirement**, but each holds
tested behavior or requirements that may be useful to the new platform.
Use the rows as a reference, not deletion instructions.

| Path | Disposition | Outcome to preserve / evidence | Owner | Gate before the change |
|---|---|---|---|---|
| `keystone/capability/helm.go` | Retire | Audiobookshelf Helm projection: singleton Recreate, existing claims, missing-input refusal (#258 live proof). Re-derive as catalog values. | #343 | ABS install via Flux proven (#343). |
| `keystone/capability/testdata/audiobookshelf-helm.yaml` | Retire | Reference values for the above. | #343 | Same. |
| `keystone/backup` | Retire | Restic scheduling, retention/check, off-site copy, mount/disk guards, pg_dump + SQLite `.backup` hooks (unit + `database_e2e_test.go`). These are #347 requirements. | #347 | #347 drill passes. |
| `keystone/recovery` | Retire | Management-independent restore (#276, #287). Outcome only: restore without the live control plane. | #347 | Same. |
| `keystone/localgit` | Retire | Local Git bundle/serving. Replaced by Forgejo (#342). | #342 | Forgejo backup/restore proven. |
| `keystone/publish` | Retire | Owner Plan → local Git publisher (#263–#266). Replaced by CLI commit to Forgejo + Flux. | #343 | Same as #343 gate. |
| `keystone/deployment` | Retire | SOPS/age encrypted secret projection (#324, #327, #331). **Must not regress:** ciphertext-only in Git, off-host age key, scoped decryption. Contract: `docs/design/flux-secret-custody.md`. | #343, #347 | Flux SOPS decryption + key custody proven. |
| `keystone/statefile` | Retire | Legacy state store. Git/Flux replaces it. | — | Keystone gate. |

## `packaging/`

| Path | Disposition | Why / evidence | Owner | Gate before the change |
|---|---|---|---|---|
| `packaging/host-logging` | Adapt | journald limits and the disk-guard timer/service are host hygiene the new bootstrap still needs. | #341 | Moved with disk guard. |

## Legacy CI (`.github/`)

Legacy jobs stay **required and running** until the code they test is
deleted. This keeps every existing security check in force.

| Path | Disposition | Why | Owner | Gate before the change |
|---|---|---|---|---|
| `.github/workflows/architecture.yml` | Retain | New, independent CI for the new tree (#350). | #350 | — |
| `.github/workflows/ci.yml` | Retire (part) | `secret scan (gitleaks)` is **retained**: move it into `architecture.yml` before `ci.yml` is deleted. Other jobs retire with their code. | retirement PRs | Per job: its code is gone. |
| `.github/workflows/e2e.yml` | Adapt | Manual QEMU jobs; the pattern (KVM, Ubuntu 24.04 cloud image) is reused for #341. `engine-local` retires with `engine_test.go`. | #341 | New VM job exists. |
| `.github/workflows/exit-drill.yml` | Retire | Legacy total-host-loss drill on every PR. | #347 | #347 drill passes. |
| `.github/workflows/release.yml` | Retire | Builds Keystone/Wardstone/`wild`/Console assets. Replaced by a bootstrap release. | #341 | Installer switch. |
| `.github/rulesets/main.json` | Retain | Remove a required check **in the same PR** that deletes its job, never before. | retirement PRs | — |
| `.github/dependabot.yml` | Retain | Drop legacy directories as they are deleted; add new ones. | — | — |

## Consolidation and retirement (deferred)

**No legacy deletion is authorized or scheduled by this inventory.**
First complete and validate the working standard platform: fresh
bare-metal bootstrap, Longhorn state retention, Forgejo/Flux,
Rancher/identity/Homarr, curated apps, backup and independent site-loss
recovery, and worker/replica expansion. Verify that none of these paths
depend on legacy services.

Then create a separate consolidation task based on the **actual** new
codebase. Audit which files, fixtures, tests, CI protections and
release assets remain in use, and delete unused legacy components in
focused reviewed PRs. The candidate gate column above is historical
context, not a binding sequence. Move necessary security checks,
including gitleaks, before removing their current workflows.
Multi-server control-plane HA (#349) is not a prerequisite for cleanup.
