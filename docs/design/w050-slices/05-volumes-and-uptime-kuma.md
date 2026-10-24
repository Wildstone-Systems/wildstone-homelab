# W050 slice 5: Longhorn volumes and Uptime Kuma singleton updates

> Tracked in #161. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Run a persistent singleton in the small proof with honest update downtime and safe rollback.

## Scope

- Map Volume to a retained Longhorn PVC with one replica on one node; singleton uses Recreate.
- Use declared startup/readiness probes (#39); explicitly document any interim TCP probe limitations.
- Observe rollout health and report failed update/outage. Only offer image rollback where schema/data backward compatibility is declared and tested; otherwise require validated restore alongside after #163.
- Expose volume capacity; keep PVC deletion a separate High-risk Plan. Metadata alone does not establish failover.

## Dependencies

#160; #179 accepted before deployment metadata drives the translator.

## Acceptance criteria

- [ ] Install, create a monitor, restart pod and reboot VM; monitor persists.
- [ ] Known-bad image terminates old pod under Recreate, reports outage/failure and supports a proven-compatible rollback Plan; no old-pod continuity assertion.
- [ ] An incompatible data/schema change refuses image-only rollback and requires the restore path.
- [ ] Package removal retains PVC until separately approved deletion.

## Non-goals

Backups implementation (#163), shared folders, replicated zero-downtime rollout.

## Size and decomposition

M; volume translation and singleton lifecycle.
