# W050 slice 15: Replicated workloads and optional worker components

> Tracked in #171. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Support safe multiple replicas and separately measured worker offload.

## Scope

- Replicated workload child: Service, replicas, RollingUpdate, PDB and anti-affinity under reviewed policy.
- Worker/offload child: distinct main and worker components; evaluate Jellyfin and Immich ML with measurements, without making offload an exclusive placement mode.
- Caddy replicas require proven shared certificate storage; conservative singleton otherwise.

## Dependencies

#170 and accepted #179 contract; application-specific safety declarations.

## Acceptance criteria

- [ ] Two-copy safe app updates without failed requests under a defined load probe; drain/PDB preserves required capacity.
- [ ] Tests reject unsupported replica counts or incompatible state sharing.
- [ ] Worker report demonstrates utility before defaults change; placement and recovery remain independently declared.

## Non-goals

Autoscaling or an unreviewed capability schema.

## Size and decomposition

L tracker; separate replica rollout from worker experiments.
