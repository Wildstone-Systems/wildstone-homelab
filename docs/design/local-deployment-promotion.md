# Local deployment promotion: journal, Git and approval fence

Status: local journal/ref foundation plus mandatory final approval fence under #257; not a completed deployment feature. Governing direction: amended [W050](../decisions/ADR-W050-multi-node-runtime-kubernetes-engine-host-providers-editions.md), #262. No production caller, public mutation endpoint, Git listener or live deployment is enabled here.

## Why a separate repository

The accepted Statefile contains product/household history. A Flux reader must not receive that entire object database merely to read generated deployment files. Use a separate, installation-owned bare repository containing only one integration instance's generated configuration. It lives outside the public source checkout, with no remote and private filesystem permissions. The existing product publisher and accepted Statefile semantics remain unchanged.

`statefile.OpenLocalDeployment` initializes/checks this dedicated repository and refuses source-checkout ancestry, symlinked paths, foreign repositories without its marker, group/world-accessible repository directories, remotes and alternate/shallow object storage. Git subprocesses strip inherited Git environment overrides and global/system configuration. No network Git operation is implemented. Initialization installs no software and starts no service; bootstrap must provide Git first.

This initial mapping accepts only `source.yaml`, `release.yaml` and `kustomization.yaml`, with a 1 MiB bound. It constructs a complete tree with Git plumbing, without staging working files or using hooks. It does not validate the meaning of arbitrary bytes: the approved dispatcher must generate and revalidate #256's projection, never accept arbitrary API-provided files.

## Promotion and interruption

The low-level publisher extends `keystone/publish` and `runtimedb`, not the external API. It uses the existing coordinator/execution check and accepted-Statefile revision, plus a mandatory projection-gate seam and a final exact-Plan authority fence. With the gate absent, it refuses work. Production proposal/approval/dispatch integration is still absent; a callback cannot substitute for the final DB fence.

1. Freeze files and bind repository identity, operation ID, Plan ID, accepted revision, reviewed digest, expected deployment revision and exact output bytes.
2. Recheck execution authority/accepted revision and the required gate.
3. Build a candidate commit from the complete output and retain it under a private ref.
4. Insert an immutable `deployment_promotions` journal row. Retried operation IDs must match the same binding. Concurrent builders use the row winner's candidate, not a newly regenerated commit.
5. Verify the recorded candidate's tree; recheck authority and the gate.
6. Inside an immediate runtime-DB write transaction, recheck the exact approved Plan, current accepted revision, active approving Owner and coordinator/execution authority, then compare-and-swap `refs/heads/deploy` from the exact expected revision to the journal candidate.
7. Record `promoted`. This means local ref promotion, not Flux reconciliation or application readiness.

A crash before step 6 leaves the old served ref. Replaying the same reviewed operation resumes the recorded candidate. A crash after step 6 but before journal acknowledgment recognizes that exact candidate at the served ref and completes acknowledgment. A different owner ref update is a conflict, not permission to rewind/overwrite it. Lost candidate objects fail closed rather than reconstructing history from mutable files. Retained candidates need a separately reviewed retention policy before pruning is introduced.

The journal never changes `statefile_head` or marks an application healthy. The caller must supply the same operation after restart; automatic enumeration/dispatch of pending approved operations remains part of the public Plan wiring follow-up.

## Final approval fence

`DeploymentPublisher.Promote` always calls `DB.WithDeploymentApproval` for the final ref effect; a permissive projection callback cannot bypass it. The standalone Plan document binds the complete publication fingerprint and accepted source revision to a stable Owner identity, admission generation and role-change audit sequence. It accepts only approved/applying Plans resolved by that current active Owner. Mixed/audit/unknown documents, changed output/repository, superseded Plans, moved accepted revisions, restore quarantine, household stop and a fenced coordinator cannot reach the ref effect. Re-admission or demotion followed by re-promotion does not revive an old approval. A handle rename conservatively requires a fresh approval.

The existing immediate SQLite writer transaction spans the final authority reads and bounded local Git head/CAS operations. Other DB handles attempting revocation, product publication, Plan transition, stop or coordinator takeover serialize with it. If their write commits first, promotion is refused; if the Git effect enters first, they wait for that bounded effect. This is an ordering guarantee, not a promise to undo an already-started Git update. The effect performs no DB calls, network access, rendering or application work.

Git and SQLite still are not one transaction. After a ref update, DB acknowledgment can fail or the process can stop. Exact approved retry recognizes the journal candidate; it never rewinds an owner change. If authority was revoked after the ref advanced, another effect is refused and the journal may remain prepared. The future status/resume integration must report that observed ref honestly without treating acknowledgment as permission for new work or automatically rolling it back.

This fence does not implement Plan creation, fresh interactive Owner approval, instance inventory, encrypted-secret version revalidation or automatic dispatch. Those remain required before enabling a production caller. It checks a stored exact binding; the dispatcher must derive and revalidate that binding from installation-owned inputs, rather than trusting API-supplied digests or files.

## Source visibility and transport

Prepared refs use `refs/wildstone/` and are hidden with Git's transfer settings. Arbitrary object-ID wants are disabled. A real upload-pack advertisement test checks that private refs/candidates are not advertised before promotion and that the served deployment ref is advertised afterward. This is not yet an adversarial network transport/authentication proof.

The [local Git delivery slice](local-git-source.md) renders established OpenSSH/Git upload-pack against a separate promoted-only delivery mirror, with a fixed repository and forced read-only command. Hiding prepared refs in the private promotion repository is not a complete Git object-confidentiality boundary, so the network reader must never receive that object database. Keep it local/private and supervised independently of Keystone. Verify authentication, pod reachability, write/arbitrary-shell refusal, hidden object access and operation while Keystone is down. Do not write a custom Git protocol server or require Gitea.

Repository permissions and endpoint identity must be designed together. This foundation uses private owner-only storage; granting a serving identity access requires explicit deployment design, not relaxing permissions or exposing the repository generally. No host account, authorized_keys, SSH daemon, firewall or existing service configuration is changed here.

## Remaining #257 boundaries

- Wire exact review material to the existing Owner Plan proposal/approval/publication path; reject audit-only records, unrelated Plans, cross-instance changes and agent apply. Verify current encrypted-secret configuration revision and artifact identity, not just a caller-provided digest.
- Integrate the mandatory final fence with production proposal/review/dispatch; add API authorization, secret-redaction and full lifecycle/resume tests. The bounded DB/ref interleavings are covered here.
- Resume approved pending operations automatically with truthful status; do not treat journal presence as approval.
- Implement and test the local read-only Flux source endpoint and its bootstrap configuration. Do not bootstrap the endpoint through a configuration source it needs in order to start.
- #258 observes exact served/source/controller revisions and application readiness on the existing development VM after explicit non-disruptive scheduling.

S3/Hetzner backup integration remains deferred until local operation works. Neither endpoint is the active deployment source. Only synthetic configuration is used in repository tests; public GitHub receives code/docs/fixtures, never installation state.

## Verification

Real temporary bare repositories test prepared/ref-advanced interruption, exact retry, conflicting edits, altered review/files, missing gate, stale source revision, lost authority, concurrent publishers, source-checkout/remote refusal and inherited Git environment redirection. No system Git remote or host service is changed. Existing Statefile/publication suites must stay green; migration 0113 adds a separate journal, not a new approval table.


Authority tests use separate DB handles to race revocation, accepted-revision changes, superseding Plans, household stop and coordinator takeover against the effect. Publisher tests now use real approved Plan records and verify final-check refusal after a permissive callback, cross-repository/output conflicts, and exact approved crash/retry. No API mutation route, serving endpoint, development VM service or runtime activation is introduced.
