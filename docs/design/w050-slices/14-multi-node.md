# W050 slice 14: Production multi-node onboarding and rolling host updates

> Tracked in #170. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Productize proven join, placement/failover and rolling host updates for households.

## Scope

- Split production onboarding/placement/failover and rolling host updates into bounded children, reusing #180 harness.
- Join delivers k3s secret after mTLS enrollment; verify SQLite-to-etcd conversion and server roles on the declared topology.
- Expose actual protection: quorum, healthy storage replicas, eligible capacity, independent failure domains and dependency single points. Node count alone never shows 'any one machine can fail'.
- Tune failover from measured detection, detach/reattach and readiness time; choose replica counts with evidence.
- Cordon/drain, signed update, wait Ready plus storage rebuild, uncordon; stop on error, server-first k3s upgrade and safe leader handover.
- External observer and power-restoration readiness tests include documented topology limitations.

## Dependencies

#164, #166, #169 and accepted #180 proof; ADR-W051 accepted; owner-operated disposable test environment under ADR-W002. #180 never depends on this slice or catalog waves.

## Acceptance criteria

- [ ] Second/third-node Console join and declared role conversion work.
- [ ] Power loss, partition and returning-old-node tests preserve one writer and meet measured service/data objectives.
- [ ] Rolling host update and leader handover resume exact approved steps without stale replay; interruption matches workload policy.
- [ ] UI protection status follows observed quorum/storage/capacity/dependencies, not node count.
- [ ] Owner accepts production topology evidence; failures return to #170 child or #180 safety contract.

## Non-goals

Initial architectural proof, replicas/workers, Talos.

## Size and decomposition

XL tracker; split onboarding/failover and rolling updates.
