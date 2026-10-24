# W050 slice 1: Wardstone runtime seam + package `availability` (#151)

## Implementation review amendment — 2026-10-24

Preserve PR #177 as per-node Docker encapsulation with unchanged behavior. Its Docker-shaped result types are not the Kubernetes workload contract and the future backend need not implement Docker primitives. PR #178 is informational only and requires #179 capability/policy review before merge; if already merged, resolve through a compatible follow-up before runtime use. Do not treat array order, absence of volumes, or host networking as proof of replica safety, selected policy, or exact hardware identity. Acceptance of W050 does not schedule further runtime work.

This supersedes conflicting scope, ordering and acceptance language below. See [revised proof gates](../w050-migration-plan.md#implementation-review-and-proof-gates--2026-10-24). No runtime work is authorized by this amendment.


> Additions to existing issue #151 (posted as a comment there). This file is the source draft; the issue is the live copy.

## Goal

Put all Docker Engine calls in Wardstone behind one interface and let packages declare `availability`. No behaviour change.

## User-visible outcome

None. Package pages may show "Can move to another machine: yes/no" later; not in this slice.

## Scope (additions to #151 in **bold**)

- `wardstone/internal/agent/`: interface `runtimeBackend` with the 13 Docker client methods (`docker.go:172-498`) **and the raw `DELETE` at `ops.go:708`**. Callers: `ops.go`, `seed.go:75`, `diskspace.go:30`.
- **Say plainly:** this seam is per node. The cluster-wide seam is slice 2.
- `keystone/pkgspec/pkgspec.go`: optional `availability: [pinned|failover|replicated|offload]`, first value is the default.
- `keystone/pkgtool/lint.go`: **defaults and checks:**
  - a device mapping or `networkMode: host` → must be `pinned`;
  - any Volume mounted read-write → `replicated` refused;
  - no Volume and no host path → may offer `replicated`.
- Set the field on all 73 packages using the survey in the plan (§5).

## Non-goals

Kubernetes code; placement; Console changes.

## Dependencies

None (can run beside slice 0).

## Acceptance criteria

- [ ] #151's list.
- [ ] `grep -n 'a\.docker\.' wardstone/internal/agent/*.go` matches only the Docker backend file.
- [ ] Lint tests: each value; `replicated` + RW volume refused; device without `pinned` refused.
- [ ] Every package declares `availability` (lint fails otherwise).

## Risks

Low. Refactor touches the most security-sensitive op (`ensureContainer`); keep the pre-check order (`ops.go:535-640`) intact.

## Size

**M.** Mechanical refactor plus one field and 73 small package edits.

## ADR amendment

No (ADR-W005 package format gains a field; note it in the format doc).
