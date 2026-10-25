# W050 slice 13: Catalog migration waves after safety proof

> Tracked in #169. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Expand catalog support only after the small deployment and early fault proof are accepted.

## Scope

- Split waves into bounded package children when scheduled; initial 1–3 proof services do not authorize catalog migration.
- Wave A: easy packages from plan survey, with reviewed capability declarations, probes and limits.
- Wave B: database/shared-folder/LAN-port/host-binary packages; require consistency hooks and supported storage/image paths.
- Wave C: remaining infrastructure/host-bound packages after their dedicated support.
- Per package: parameter-aware capability lint, translation/admission test, install/readiness/front-door/remove and backup/restore when stateful.

## Dependencies

Owner accepts #129, #179 and #180; #166 actual Keystone recovery, #167 host-bound support and #168 household tooling complete. Wave B also needs D4 shared-storage and O11 image decisions.

## Acceptance criteria

- [ ] Owner records gate acceptance and migration support before first catalog-wave implementation.
- [ ] Each claimed backend passes translation/admission and package lifecycle; stateful packages pass consistent backup/restore.
- [ ] Rotating VM coverage plus owner evidence for real devices/NAS; unsupported variants remain explicitly unsupported.
- [ ] Failure revises the affected package child, or #129/#180 when the architecture assumption failed; downstream rollout stays paused.

## Non-goals

First multi-node experiment, mass live migration, replica/offload feature implementation.

## Size and decomposition

3 × L tracker; split waves and packages.
