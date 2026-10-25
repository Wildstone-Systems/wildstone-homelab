# W050 multi-node authority, storage, secret and recovery contract

- **Issue:** [#180](https://github.com/Wildstone-Systems/wildstone-homelab/issues/180), the G4 gate in the [W050 plan](w050-migration-plan.md#revised-execution-gates)
- **Status:** **proposed.** Owner decisions are in §8. Nothing here authorizes a deployment, a credential or runtime code.
- **Evidence base:** `main` after #185, 2026-10-25.
- **Answers:** the open questions in [`w050-leader-ops.md`](w050-leader-ops.md) §5 (1, 2 and the secret row) and ADR-W050 A1's authority, Lease, admission, secret, storage and recovery bullets.
- **Amends on acceptance:** ADR-W012 (secret custody on a cluster), ADR-W050 (A2), ADR-W051 (recovery path).

With one machine, one Wardstone does everything, and a stale write can't happen. With several machines there's a **leader**: one Wardstone that talks to Kubernetes for everyone. Leaders change when a machine dies or the network splits. This contract says how Wildstone stays safe while that happens:
- an old leader can't overwrite a newer change;
- two copies of an app never write the same data;
- secrets survive a leader change;
- the house can be recovered even when Keystone itself won't start.

Every rule here is something #180's disposable three-machine test has to prove (§7). None of it is a promise until it does.

## 1. Who holds which authority

| Authority | Held by | Can do | Can never do |
|---|---|---|---|
| **Desired state and approval** | Keystone only (ADR-W007, ADR-W008) | Accept revisions, approve Plans, issue leader epochs (§2) | Talk to Kubernetes directly about apps |
| **Runtime reconciliation** | The current leader's Wardstone, with a **least-privilege runtime ServiceAccount** | Apply and delete Wildstone-owned objects in `wildstone-apps`, from typed operations only | Change RBAC, admission policies or the authority record; register package grants; touch other namespaces |
| **Bootstrap and break-glass** | The person at a server node's root shell, via the k3s admin kubeconfig | Install, repair and restore the cluster | Nothing stops them, which is exactly why it is never loaded by Keystone or the leader (§8 Q4) |

**Admission protects against mistakes, not against an administrator.** The admission policies check that runtime output is well-formed and owned. Someone holding the admin kubeconfig can change those policies, so admission is never described as a security boundary against them.

## 2. Leadership and stale writes

**Why a Lease isn't enough:** Kubernetes leader election tells a Wardstone it is *probably* the leader. Its own docs say it doesn't stop an old leader that hasn't noticed yet. So Wildstone adds a **fencing token** that the Kubernetes API server itself checks.

1. **Keystone issues the epoch.** When a Wardstone wins the Lease, it tells Keystone. Keystone records `leader = <node>, epoch = N+1` in its runtime DB in one transaction. It writes the same two values to an **authority ConfigMap** in `wildstone-system`: the epoch, and the leader's runtime identity. Only Keystone's own narrow ServiceAccount can update that ConfigMap; the runtime ServiceAccounts can only read it.
   - **Each node has its own runtime ServiceAccount** (`wardstone-<node>`), so the API server can tell which Wardstone sent a request.
2. **Keystone sends operations only to the current leader,** each stamped with `leader_epoch = N`, `accepted_revision` and `plan_id`/`step_digest` (leader ops §1.3).
3. **The leader stamps every object it writes** with annotations `wildstone.dev/epoch: N` and `wildstone.dev/revision: <commit>`.
4. **The API server refuses stale writes.** A ValidatingAdmissionPolicy reads the authority ConfigMap as its parameter and checks every request on a Wildstone-owned object:
   - **Who:** the request's user (`request.userInfo`) must be the current leader's runtime identity. A partitioned old leader on another node is refused for every verb, deletes included, by the API server itself, not by its own judgement.
   - **Create and update:** the object's `wildstone.dev/epoch` must equal the current epoch. This also refuses stale work from an *earlier term of the same node*, which has the same identity.
   - **Delete is two steps,** because admission sees only the stored object on a delete (`oldObject`), not the requester's epoch:
     1. the leader first **stamps** the object with an update that sets `wildstone.dev/delete-epoch: N` (fenced like any update);
     2. it then deletes with a resourceVersion precondition equal to that stamp's version.

     The policy allows a delete only when the stored `delete-epoch` equals the current epoch. A new leader can therefore delete an object last written in an older epoch: it stamps it first, in its own epoch. An old term can't produce a stamp in the current epoch.
5. **On losing the Lease, or on any epoch rejection,** the leader cancels all in-flight work and stops writing.

*To prove in #180:* a ValidatingAdmissionPolicy with a ConfigMap parameter really does refuse a partitioned old leader's creates, updates and deletes, and a same-node stale term's writes. The new leader must still be able to stamp and delete an untouched object from the previous epoch. If any of these fails, this section is revised before any cluster operation ships.

## 3. In-flight work and replays

- **After a leader change, Keystone re-observes; it doesn't resume blindly** (§8 Q5). Steps that were in flight are marked "unknown". The new leader reports the real state, and the reconciler plans from that, under the same accepted revision and the same approval rules (ADR-W008 A2a). A step that is now already done is a no-op.
- **Replays are harmless by construction.** Server-side apply of the same typed request gives the same objects, and `op_id` identifies a retry.
- **A delete carries preconditions.** `DeleteWorkload` sends the object's UID as seen when the Plan was made. The leader checks it during the stamp step (§2.4), then deletes with the stamp's resourceVersion as the precondition. Kubernetes refuses the delete if the object has since been replaced or changed. That answers leader-ops question 2.
- **The accepted revision is checked at both ends.** Keystone sends only operations from the current accepted revision. The leader refuses an operation whose revision is older than the newest it has already applied for that object, which it reads from the revision annotation.

## 4. Secrets on a cluster

Today a secret's value lives only in Wardstone's encrypted node store, and Keystone never sees it (ADR-W012 A1). The sealed off-site bundle is the recovery copy (ADR-W045 A5). On a cluster, the leader moves, so values can't stay in one node's store.

**Recommended custody (§8 Q2):**
- **The canonical value is a Kubernetes Secret** in `wildstone-apps`, with k3s secrets-encryption at rest turned on.
- **The leader generates it.** A generated secret is created by the leader's Wardstone, never by Keystone, and is written straight into the Secret. Keystone still never sees a value.
- **Keystone records only metadata:** name, version, generated-or-provided, and the epoch that created it.
- **Never regenerate.** If Keystone's record says a secret exists, at version V, and the Secret is missing or empty, that's an **error and an incident**, not a reason to generate a new one. A new leader with an empty local store therefore can't silently replace every password.
- **Provided secrets** (the household types them) go from the authenticated request straight to the leader over mTLS and into the Secret, as with node fan-out today (ADR-W012, ADR-W030 constraints).
- **Recovery:**
  - the sealed off-site bundle grows to include the cluster Secrets;
  - the k3s encryption key goes into the off-host recovery kit (§6);
  - restoring the cluster without both means the secrets are gone. The Console says so up front.
- **Household and personal secrets** (ADR-W033) are unchanged: still ciphertext in Keystone under its own key, never in Kubernetes.

## 5. Storage: one writer, always

- **One-copy components with data** run with `replicas: 1`, `strategy: Recreate` and a `ReadWriteOncePod` claim, so two pods can't both have the volume.
- **No forced deletes, by Wildstone or by Kubernetes.** Wildstone never force-deletes a pod or force-detaches a volume to make failover happen. Kubernetes warns that force deletion can leave the old process running, and Longhorn won't safely move a volume still attached to an unreachable node.
  - **Kubernetes' own timeout is turned off.** By default, the controller manager force-detaches a volume when a pod has failed to delete for six minutes on an unhealthy node, and its docs warn this can corrupt data. The engine bootstrap sets `disable-force-detach-on-timeout` on k3s's controller manager, so the owner gate below can't be bypassed by a timer.
  - **Longhorn's node-down pod deletion is left at "do nothing",** so Longhorn doesn't delete the stuck pod on its own either.
- **Failover of a stateful app after a machine is lost** (§8 Q3):
  - **Recommended default:** the Console asks the owner to confirm the machine is really off (*"Is node-2 powered off or unplugged?"*). Only then does the leader remove the lost node's claim on the volume, using Kubernetes' out-of-service taint, and start the app elsewhere.
  - **Opt-in later:** automatic failover, once #180 shows the storage refuses a second writer even when the old machine comes back.
- **Stateless apps** (`copies: many`, nothing written) fail over automatically, because there's nothing for two copies to fight over.
- **Downtime is measured, not promised.** #180 records the real time from machine loss to the app serving again. The Console reports that number, not a theoretical one.
- **Disk pressure is tested, not assumed.** Space is reserved for the OS, Kubernetes and storage. #180 fills a disk on purpose and checks that management survives.

## 6. Recovery without Keystone

- **`wild-recover`** is a host binary on every server node. It works with Keystone stopped, corrupt or unschedulable, and can:
  - run diagnostics;
  - apply the fixed `management` workload (the bootstrap path, W050 plan §6.3);
  - restore Keystone's data from a backup.
- **The off-host recovery kit**, kept somewhere other than the machines, holds:
  - the Statefile bundle and the runtime DB backup;
  - the sealed secret bundle and the k3s encryption key;
  - a k3s etcd snapshot;
  - the steps to rebuild from bare machines.
- **Two cases are kept apart:**
  - the cluster API works but Keystone doesn't: use `wild-recover`;
  - the cluster API or storage is gone: rebuild offline from the kit.

  A proxy that forwards to Keystone isn't a recovery path, because it needs the thing that's broken.
- **Actual Keystone recovery is required before household migration is offered** (G5). A test fixture passing isn't enough.

## 7. What #180's test must show

On three disposable VMs, with the 1–3 services from #129 (tools-it-tools, Uptime Kuma, optionally Mealie or Postgres) plus a management fixture:

1. **Stale leader:** partition the leader while it's still powered. A new leader takes over. The old one's creates, updates and deletes are refused by the API server (§2.4). The new leader can still stamp and delete an untouched object from the previous epoch.
2. **Mid-apply leader loss:** kill the leader during an `ApplyWorkload`. The new leader re-observes and converges with no duplicate or lost step (§3).
3. **Single writer:** power off a node running Uptime Kuma. No second copy starts until the owner confirms. Then bring the old node back and show it can't write. Repeat with the node **powered on but partitioned for longer than the six-minute force-detach timeout**: the volume must stay attached to the old node until the owner confirms.
4. **Secrets:** change leaders, and every secret is still present at the same version. With the store emptied on purpose, Wildstone raises an incident and doesn't regenerate.
5. **Recovery:** stop Keystone and recover it with `wild-recover`. Then destroy the cluster and rebuild from the off-host kit.
6. **Pressure:** a full disk on one node leaves management running.
7. **Evidence:** versions, topology, measured outage and data loss, stored with the issue. The owner records pass, revise or reject.

Three VMs on one host prove software behaviour, not independent power or hardware.

## 8. Decisions for the owner

1. **Fencing:** use a Keystone-issued epoch, checked by a ValidatingAdmissionPolicy against an authority ConfigMap (§2)? **Recommended: yes.** It's the only option where the API server, not the old leader, refuses the stale write.
2. **Secret custody:** Kubernetes Secrets, encrypted at rest, generated by the leader, metadata only in Keystone, and never regenerated when missing (§4)? **Recommended: yes.** The alternative, keeping per-node stores and copying values between them, leaves values on every machine that was ever leader.
3. **Stateful failover:** owner confirms the lost machine is off before a stateful app moves (§5)? **Recommended: yes for now,** with automatic failover as a later opt-in once #180 proves it safe.
4. **Admin kubeconfig:** kept root-only on server nodes and in the off-host kit, and never loaded by Keystone or the leader? **Recommended: yes.**
5. **After a leader change:** re-observe and re-plan, rather than resume the old step list (§3)? **Recommended: re-observe.** It's slower by one tick and can't act on a stale picture.
