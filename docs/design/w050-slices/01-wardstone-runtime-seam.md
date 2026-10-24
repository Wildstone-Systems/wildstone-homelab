# W050 slice 1: Wardstone runtime seam + package `availability` (#151)

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
