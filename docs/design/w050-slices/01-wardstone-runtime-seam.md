# W050 slice 1: completed Docker encapsulation and capability parser/lint implementation

> Tracked in #151. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Preserve the completed Docker refactor and implement release 1 of the accepted deployment capability contract without changing runtime behavior.

## Scope

- #177 merged as 97628db: runtimeBackend encapsulates Docker calls, including raw removal; preserve pre-check ordering and host/runtime separation.
- Docker-shaped result types remain internal to the Docker contract; Kubernetes need not emulate them.
- G2 passed: owner accepted #179 in merged #182. The [accepted contract](../package-deployment-capabilities-contract.md), including §9 decisions, is the schema and validation authority.
- #178 is open and no longer a draft. Its rework adds strict parser support for optional devices/deployment fields and opt-in capability lint, with no catalog edits. Review implementation against release 1 of the contract.
- Follow the contract's staged compatibility rollout: catalog declarations come in a later release after supported parsers can read them. No scheduler, placement policy or replica behavior is introduced.

## Dependencies

Docker encapsulation is complete. Capability design G2 is complete in #182; #178 implements that accepted contract and does not wait for a new capability decision. #158 may proceed independently when scheduled.

## Acceptance criteria

- [x] #177 merged; preserve its Docker gofmt, vet, race-test, e2e and exit-drill evidence.
- [x] Owner accepted #179 in #182, including §9 decisions.
- [ ] #178 parser and opt-in lint tests implement release 1 of the accepted contract.
- [ ] No catalog package/lockfile edits in the release 1 parser PR; staged catalog adoption follows the contract.
- [ ] Docker behavior and workload counts remain unchanged; no runtime consumes the new metadata.

## Non-goals

Catalog-wide declarations, Kubernetes runtime implementation, placement behavior and Console policy changes.

## Size and decomposition

M; Docker encapsulation complete, design gate passed, release 1 parser/lint implementation under review in #178.
