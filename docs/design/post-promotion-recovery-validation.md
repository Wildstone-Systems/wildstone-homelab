# Post-promotion Keystone recovery validation

Status: repository validation for #287, completing the bounded #284–#287
sequence. This is not HA, automatic failover, household migration, or live
Kubernetes recovery acceptance.

## Code-backed state machine

The implementation composes existing restore, signed-journal, release,
promotion, coordinator, task, routine, workspace, and authorization state. It
does not add another recovery state machine.

| Transition | Required authority | Persistent change | Coordinator state | Execution and audit | Failure and idempotency |
| --- | --- | --- | --- | --- | --- |
| 1. Restore enters quarantine | Offline operator using the supported `RestoreBackup`/management restore path and a fresh target | Random restore generation, exact snapshot hash/time, reason, `quarantined=1`; stale observations and non-restorable mail material are removed | Preserved source holder/epoch; no process-local lease | All household execution and restored data surfaces are held; restore state is durable | Destination must not exist. A partial restore is removed or remains quarantined; restore is not an overwrite operation |
| 2. Verify independently signed journal | Fresh active Owner submits terminal evidence signed by the separately held journal custodian key | Immutable generation/snapshot/evidence reconciliation, verified head, source fence, custody, replay provenance and time | Preserved holder/epoch | Still quarantined. Revocation/deletion suffix is conservatively replayed and audited | Missing, stale, gapped, divergent, tampered, mismatched or unfenced evidence fails closed. Repeating the identical proof returns the original evidence result |
| 3. Complete conservative replay | Same verified reconciliation transaction/path | Post-snapshot revocations/deletions applied; abandoned model attempts get a new task epoch; tool/delivery attempts are fenced and Owner-held; replay checkpoint recorded | Preserved holder/epoch | No provider is invoked. Restored authority cannot be resurrected and uncertain effects cannot be blindly retried | Replay changes are monotonic and provenance-bound. Retrying sees fenced attempts and existing waits rather than adding dispatches |
| 4. Release quarantine | Fresh Console session resolving now to one admitted, active Owner | `released_at`/`released_by` for the exact generation and evidence | Preserved source holder/epoch | Data quarantine clears, but `CheckHouseholdExecution` still fails for lack of a process execution lease; request audit records the Owner | Every evidence condition is revalidated in one transaction. Identical release is idempotent; mismatched or incomplete state remains quarantined |
| 5. Promote restored coordinator | Fresh current Owner Console session; administrator bearer, Operator, Member, Agent, revoked/disabled Owner are insufficient | Immutable promotion audit and restore projection; lease compare-and-swap from `(old holder, N)` to `(replacement, N+1)` | Durable holder becomes replacement at exactly `N+1`; promoting handle has no execution lease | Old holder/epoch is fenced immediately. Promotion does not dispatch or recover work | Repeated promotion conflicts and cannot increment again. Missing release/evidence/source fence or a concurrent lease change aborts atomically |
| 6. Promoting process remains fenced | No additional decision; enforced by handle-local restart gate | None | Durable promoted lease exists, but the handle that wrote it has `restoreCoordinatorAcquisitionAllowed=false` | `AcquireCoordinator` and all execution checks fail; administrative API remains available | Repeated acquisition on that handle cannot turn promotion into execution authority |
| 7. Controlled stop/restart | Operator stops the singleton after promotion; no timeout or lease stealing | Process-local handles and OS lock close; durable lease is unchanged | No active process lease during the boundary | No work is eligible during shutdown | A crash before reopen leaves the promoted durable epoch fenced, ready for the same controlled retry |
| 8. Acquire exact promoted epoch | Newly opened database handle using the stable promoted deployment holder | Acquisition/heartbeat timestamps update; epoch does not increment | Only the exact promoted holder may re-acquire `N+1`; old or mismatched holders fail | Process-local execution lease exists, but coordinator-only subsystems are not wired yet | Acquisition is a same-holder restart, not election. A stale epoch is rejected by every later coordinator check |
| 9. Run startup recovery | The newly acquired process lease at the exact durable epoch | Workspace journals roll back conservatively; abandoned task attempts and claimed routine effects are fenced/reconciled; uncertain outcomes receive durable waits/receipts | Holder remains replacement at `N+1` | `RecoverWorkspaceWrites` then `RecoverCoordinatorStartup` complete before task, subscription, mailbox, delivery, reviewed-routine, forwarder, or Owner-review workers are wired | Recovery is idempotent. Any unexpected task/routine recovery error clears the handle's in-memory execution lease and fails startup; confirmed quarantine/household-stop deferral leaves the API up but disables coordination until another restart |
| 10. Wire only eligible work | Existing live authorization, Plan, capability, connection, Agent, task, stop, restore, and coordinator gates all agree | Normal work checkpoints only | Replacement holder at `N+1` | Completed receipts/routine effects remain terminal; model work may retry at its fenced task epoch; tool/delivery and claimed routine outcomes remain Owner-held; revoked/disabled authority stays denied | Duplicate receipts resolve to the immutable receipt and routine occurrences remain unique. No exactly-once external-side-effect claim is made without integration-specific idempotency |

Successful lease acquisition is therefore necessary but insufficient. The
actual startup order is:

1. open and migrate the runtime database;
2. acquire the stable deployment holder and durable epoch;
3. recover workspace publication metadata;
4. recover native attempts and claimed reviewed-routine effects through
   `RecoverCoordinatorStartup`;
5. start the heartbeat;
6. recover the Statefile publication journal and load policy/catalog state;
7. construct the API and execution gateways; and
8. only when the recovered `coordinating` flag remains true, wire native
   task/subscription/mailbox paths, reviewed routines, outbound forwarding,
   and Owner-review notification work.

The ordinary deterministic automation loop exists in a non-coordinator
process so recovery APIs remain serviceable, but its store and external action
boundaries call the household execution fence. Coordinator-only reviewed
routines are not wired at all without startup eligibility.

## Deterministic repository evidence

`TestPostPromotionRecoveryAndExecutionFencing` creates a valid parsed
Automation through the supported draft, exact Plan snapshot, and Owner
approval APIs. It also creates a current Owner, a credential revoked after the
snapshot, a completed native effect with immutable receipt, interrupted model
and tool/delivery attempts, one confirmed routine effect, one claimed routine
effect, and the pre-recovery coordinator lease. It uses a local SQLite file,
fixed timestamps, Ed25519 journal evidence, and checkpoint APIs only. It makes
no model, provider, shell, network, or household call.

The test performs restore, signed reconciliation, release, authenticated
store-level promotion, handle close, database reopen, exact promoted-holder
acquisition, workspace recovery, and the production startup execution
recovery method. Existing API tests independently prove that the promotion
endpoint admits only a fresh current Owner session and records request audit.

The fixture proves:

- restart before reconciliation, release, or promotion remains restore-fenced;
- promotion without restart remains process-fenced;
- stale/mismatched holders and the prior epoch cannot acquire or pass checks;
- repeated promotion cannot increment the epoch;
- completed task receipts and confirmed routine effects are not duplicated;
- late results from fenced attempts and stale routine completion are rejected;
- model attempts are fenced for retry while tool/delivery and claimed routine
  outcomes remain uncertain for Owner resolution;
- post-snapshot credential revocation survives replay; and
- repeated startup recovery is idempotent.

`TestFailedCoordinatorStartupRecoveryRevokesProcessExecutionAuthority` injects
a deterministic SQLite failure while reconciling a claimed routine effect.
Startup returns an error, clears the handle's in-memory execution lease, and
leaves the claim unchanged for a later safe retry.

## Limits and isolated Kubernetes gate

Repository tests exercise real close/reopen semantics, SQLite process locks,
the durable promoted epoch, and the exact startup recovery method. They do not
prove Kubernetes Deployment termination, PVC attachment, or image/chart
packaging. Read-only cluster preflight additionally found that Kubernetes Pod
replacement changes the default Pod hostname, while Keystone includes that
hostname in its coordinator holder. Chart 0.2.1 therefore binds the singleton
Pod hostname to the chart's Kubernetes fullname. The hostname is not an
independent value: it is the same DNS-label-safe identity used by the
Deployment and Service, so Kubernetes ownership also prevents two releases in
one namespace from silently selecting the same identity. Chart tests prove
that replacement renders preserve the restored identity and that source and
restored releases have distinct identities.

An isolated K3s proof remains useful for operational acceptance, but is a
separate Owner approval gate rather than a prerequisite for the repository
safety contract.

The proposed live target must use a new namespace and synthetic claims; it
must not reuse or mutate retained `wildstone-mgmt-270`,
`wildstone-mgmt-271`, or `wildstone-mgmt-277` resources. Exact image digest,
chart version, resource limits, storage limits, credentials, journal evidence,
shutdown proof, retention, and cleanup commands must be recorded from the
approved PR head immediately before execution.

### Proposed isolated K3s exercise (not authorized)

Read-only inspection on 2026-11-03 found one Ready `dev-services` node on K3s
`v1.37.1+k3s1`, the default `local-path` StorageClass with `Delete` reclaim
policy and no expansion, and all four Flux controllers Ready in
`flux-system`. The retained `wildstone-mgmt-270`, `wildstone-mgmt-271`, and
`wildstone-mgmt-277` namespaces exist and are excluded.

After separate Owner approval, use only:

- namespace `wildstone-mgmt-287`, labeled to enforce, audit, and warn
  `restricted:v1.37`;
- direct Helm releases `ws287-source` and `ws287-restored`, chart
  `charts/wildstone-management` version `0.2.1`, rendered from the approved
  #287 PR head. Set `fullnameOverride` to the respective release name; this
  produces distinct stable Pod hostnames `ws287-source` and
  `ws287-restored`. The releases must never have active Pods concurrently;
- a locally built `wildstone-management:issue287` image imported into K3s and
  pinned in values as
  `wildstone-management@sha256:66a4234beadb2596e88dced9743bde2c95304d5528a6807933c4675b6abdb124`.
  This `linux/amd64` image was built from the validated #287 executable tree
  as version `0.0.0-issue287` and passed `scripts/test-management-image.sh`.
  Rebuild and replace this digest if executable inputs change before approval;
  a tag or the chart's zero-digest sentinel is never accepted;
- source PVCs `ws287-source-runtime`, `ws287-source-uploads`, and
  `ws287-source-workspaces`, and
  restore-target PVCs `ws287-restored-runtime`, `ws287-restored-uploads`, and
  `ws287-restored-workspaces`, plus evidence PVC `ws287-recovery-set`. Request
  `2Gi` for each runtime claim and `1Gi` for every other claim (9Gi total).
  Set every new test PVC retention annotation before installation. After
  binding, resolve each claim's exact `spec.volumeName`, verify that every
  resolved PV is bound to a `wildstone-mgmt-287/ws287-*` claim, and patch only
  those resolved PVs to `Retain` before loading data. Never select or patch PVs
  by StorageClass, provisioner, or an unbounded label query;
- the chart defaults of one replica, `Recreate`, request `100m` CPU/`256Mi`
  memory, limit `1` CPU/`1Gi` memory, and `128Mi` bounded `/tmp`; no LoadBalancer,
  ingress, provider secret, service-account token, or external endpoint;
- ConfigMaps `ws287-source-config` and `ws287-restored-config`, Secrets
  `ws287-source-secrets`, `ws287-restored-secrets`, and
  `ws287-owner-credentials`, journal trust ConfigMap
  `ws287-journal-public`, and offline host directories
  `ws287-recovery-set` and `ws287-bootstrap-keys` with mode 0700. All identities,
  keys and Statefile content are generated for this exercise; the journal
  private key remains only in the separate bootstrap directory;
- one current synthetic Owner session, one revoked credential, a valid
  reviewed inert routine, completed native/routine receipts, interrupted
  model and tool/delivery attempts, one claimed routine effect, and a known
  source coordinator holder/epoch.

Sequence:

1. Install `ws287-source` on `ws287-source-runtime`; record Pod UID, stable
   hostname, image ID, chart digest, PVC/PV bindings, holder/epoch, fixture
   receipt counts, and synthetic credential hashes.
2. Scale the source Deployment to zero, wait for Pod deletion and volume
   detachment, then delete that Deployment by exact name and record the
   terminal journal source fence. Direct Helm ownership has no reconciler that
   recreates the deleted Deployment. A Lease timeout or NotReady condition is
   insufficient, and no later source Helm action is permitted.
3. Build the signed terminal journal evidence in the offline directory, run
   management preflight, and restore the snapshot into
   `ws287-restored-runtime` while no replacement Deployment exists.
4. Install `ws287-restored` with hostname `ws287-restored` and start one Pod.
   Prove restart before reconciliation, release, and
   promotion remains execution-fenced. Submit evidence and release through
   fresh Owner sessions, then promote to the stable replacement holder.
5. Without restarting, attempt a synthetic queued task/routine tick and prove
   the promoting Pod stays fenced. Delete only that replacement Pod and let the
   same singleton Deployment create one new Pod over the restored PVC; verify
   that its Pod UID changes while its hostname remains `ws287-restored`.
6. Prove the new process acquires exactly the promoted epoch and startup
   reconciliation completes before readiness. Attempt stale old-holder/epoch
   task completion, routine completion, outbound mailbox/delivery read/write,
   and old execution-lease use; each must be rejected without changing receipt
   or dispatch counts.
7. Verify completed effects remain single, interrupted model work is only
   retry-eligible at its new task epoch, uncertain tool/delivery/routine effects
   remain Owner-held, the revoked credential stays denied, and disabled
   principals/capabilities cannot execute.

Retain namespace manifests, sanitized logs, database projections, image/chart
digests, and PVC/PV names for review. Do not delete anything during the proof.
After acceptance, request a separate cleanup approval; export the sanitized
evidence first, scale the Deployment to zero, verify Pod/volume detachment,
then remove only `wildstone-mgmt-287` resources by exact name. Restore any
`Retain` PV policy only as part of that approved cleanup. Never issue a broad
namespace/PVC prune and never touch the retained namespaces.
