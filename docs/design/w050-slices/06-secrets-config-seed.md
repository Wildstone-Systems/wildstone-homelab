# W050 slice 6: Cluster secrets, configuration and seed files

> Tracked in #162. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Deliver secret/config/seed data without Keystone receiving secret values or losing secret identity on leader change.

## Scope

- Define canonical secret source, leader access, versions, export/import and datastore encryption-key recovery; missing local state must never regenerate an existing value.
- Implement typed cluster secret/config operations with scoped credentials; secretKeyRef, ConfigMap/Secret materialization and digest-triggered restart.
- Restart on rotation follows the workload strategy: singleton Recreate includes outage; replicated workloads may roll only when declared safe.
- SeedFile uses fixed Go init image, O_EXCL semantics and observed completion; user edits survive reconcile.
- Use a selected proof service or isolated fixture for secret coverage. Grafana is an optional integration fixture, not a fourth service migration in #129.

## Dependencies

#161; owner acceptance of ADR-W012 custody amendment and #180 secret continuity contract before implementation. #180 fault evidence follows this slice, avoiding a dependency cycle.

## Acceptance criteria

- [ ] Keystone receives existence/version only; audit logs contain no secret values.
- [ ] Rotation preserves declared versions and performs the applicable restart strategy; login works for the chosen fixture.
- [ ] Seed is written once and user edits survive reconcile/restart.
- [ ] Fresh import preserves values/versions and required key recovery; #180 tests the same secret across leader change.
- [ ] Empty local store cannot replace an existing secret; unavailability yields an explicit blocked state.

## Non-goals

Identity relocation, extra proof-service migrations, universal zero-downtime rotation.

## Size and decomposition

L; custody review, typed operations and seeder image.
