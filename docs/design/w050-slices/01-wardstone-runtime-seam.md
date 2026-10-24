# W050 slice 1: Wardstone Docker encapsulation and package capability review

> Tracked in #151. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Isolate Docker calls without changing behavior; review deployment metadata separately.

## Scope

- #177 merged as 97628db: runtimeBackend wraps Docker calls, including raw removal; preserve pre-check ordering and host/runtime separation.
- Docker-shaped result types remain internal to the Docker implementation contract; Kubernetes need not emulate them.
- Keep #178 a draft until #179 specifies capabilities versus selected policy, conservative defaults and parameter-dependent placement. Do not define the replacement schema in this slice.
- Packages without workloads need no placement metadata. Catalog edits and lint follow #179, not blanket defaults from volume/device heuristics.

## Dependencies

None for the completed encapsulation. Draft #178 requires owner acceptance of #179 before merge; #158 may proceed independently when scheduled.

## Acceptance criteria

- [ ] Completed Docker refactor retains gofmt, vet, race-test, e2e and exit-drill evidence from #177.
- [ ] Owner accepts #179 and #178 reflects it before merge; metadata remains informational until a separately tested runtime consumes it.
- [ ] Lint tests cover reviewed parameter variants; array order and absence of volumes never establish production replica safety.

## Non-goals

Kubernetes implementation, placement behavior and Console changes.

## Size and decomposition

M; Docker portion complete, metadata portion pending #179.
