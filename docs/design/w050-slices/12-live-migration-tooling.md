# W050 slice 12: Bounded migration proof and household migration tooling

> Tracked in #168. Source draft; the issue is the live execution copy. Post-alpha; owner scheduling of bounded work remains required. See the [execution gates](../w050-migration-plan.md#revised-execution-gates).

## Goal

Migrate selected services with consistent data and rollback preserving post-cutover writes.

## Scope

- Create a bounded early child for the 1–3 services in #129 behind Docker ingress; do not move identity to test the basic path.
- Plan steps: backup/verify, stop or quiesce writers, final consistent copy, start/validate, switch Route, retain source 14 days; record estimated and measured outage.
- Scoped copy Job reads owned Docker volume path and writes PVC; compare bytes/ownership and application consistency. Optional pre-copy requires final synchronization.
- Rollback backs up k3s, stops writers, preserves pre-migration copy, reverse-copies new writes into held Docker volume, validates app and then restores route. Never start frozen stale data.
- General UI and household cutover follow later; old volume deletion is separate High-risk Plan. Reverse copy is an explicit approved mutation, not the retention/deletion step.

## Dependencies

Early proof: #163 plus selected service support (#160–#162); no #165 dependency. Household tooling: accepted #129/#180, #165 and actual management recovery #166.

## Acceptance criteria

- [ ] Selected proof services migrate, receive new writes, and move back with those writes present; owner records result under #129.
- [ ] Copy mismatch stops before Route switch; reverse-copy failure leaves both copies recoverable and no second writer.
- [ ] Final-copy downtime is measured; large-volume estimate does not promise uninterrupted Docker service during stopped-writer copy.
- [ ] General household tooling is offered only after core identity/management recovery gates pass; deletion remains separately approved.

## Non-goals

Bulk migration or general household UI as a prerequisite for the early proof.

## Size and decomposition

L tracker; early proof and later product tooling are separate children.
