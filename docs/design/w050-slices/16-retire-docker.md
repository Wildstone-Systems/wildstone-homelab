# W050 slice 16: Docker retirement after supported upgrade and recovery proof

> Tracked in #172. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Remove Docker implementation only after supported households can migrate and recover.

## Scope

- Remove Docker-only client/translator/network/address paths after compatibility evidence.
- Deprecate then reserve proto numbers; remove Docker installer/CI legs only when no supported path relies on them.
- Retain bounded migration tooling for supported upgrade window; document last Docker-capable release and replacement restore.

## Dependencies

#166, #168, #169, #170 and owner-accepted #180; explicit deprecation/upgrade decision and at least one warning release.

## Acceptance criteria

- [ ] Supported single/multi-node topology evidence accepted, including actual management and package replacement restore.
- [ ] Existing Docker household upgrade/migration/rollback proven; refuse incompatible upgrade with clear remediation.
- [ ] No Docker references outside supported migration path; k3s e2e and exit drill pass.
- [ ] Owner accepts retirement/deprecation decision; revise returns to affected #168/#170/#172 child and retains Docker support.

## Non-goals

Talos or deletion based only on catalog translation success.

## Size and decomposition

M; deletion follows evidence, not just code coverage.
