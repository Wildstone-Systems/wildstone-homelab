# W050 slice 2: Keystone backend seam and provisional typed cluster operations

> Tracked in #158. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Separate diff/risk/approval from backend translation in a Docker-only refactor with unchanged Plans.

## Scope

- Extract ensureNodePrereqs, specHash, containerEnsureOp, health gates, ingressaddr, aliases, host-port rendering and restart translation into the Docker translator; preserve operation ordering.
- Introduce node/cluster target types and an initially empty cluster group; add FakeCluster recorder without a real client.
- Draft typed workload/job/config/secret/host-update operation contracts in w050-leader-ops.md. Reserve numbers only after checking existing allocations; no generated cluster arms here.
- Keep wire semantics provisional: review #180 authority, stale/in-flight operations, revision binding and idempotency before implementing each operation; refine from #129 proof. Do not require Kubernetes to return Docker types.

## Dependencies

Merged #177 completes the required Docker encapsulation; owner schedules #158. May run in parallel with #179; does not wait for #129 or #180 experiments.

## Acceptance criteria

- [ ] Existing reconcile, API, backup and loop tests preserve byte-identical Plans and Docker behavior.
- [ ] Only the Docker translator emits Docker-shaped operations; an ordering test protects prereqs/secrets sequencing.
- [ ] Design review confirms typed requests without raw YAML, commands or free Ansible variables; final wire contract is not implied by this refactor.
- [ ] Docker e2e and exit drill pass.

## Non-goals

Cluster client, runtime capability consumption, final wire-contract freeze or multi-node deployment.

## Size and decomposition

L; split mechanical extraction from operation design if needed.
