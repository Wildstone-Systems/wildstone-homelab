# W050 slice 12: live-install migration tooling

## Implementation review amendment — 2026-10-24

Split a bounded proof child EARLY after #163: migrate the 1–3 services selected in #129 behind Docker ingress; preserve new writes on reverse migration and inject copy/cutover failures. This proof does not depend on full identity migration #165. The full household UI, identity/ingress migration and general migration tooling retain later dependencies. Source services must be stopped/quiesced during the final consistent copy; estimate and disclose outage. A pre-copy can reduce it but needs a final synchronized copy. Checksums alone do not prove database/application consistency.

This supersedes conflicting scope, ordering and acceptance language below. See [revised proof gates](../w050-migration-plan.md#implementation-review-and-proof-gates--2026-10-24). No runtime work is authorized by this amendment.


> Tracked in #168 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

An existing Docker household can move one package at a time to k3s, without data loss, with rollback.

## User-visible outcome

- Package page: "Move to the new engine". A Plan lists: back up, stop, copy data, start, switch address, keep old data 14 days.
- "Move back" works during those 14 days.

## Scope

- Plan template with the steps in plan §6.10.
- Copy Job: reads `/var/lib/docker/volumes/<owned>/_data` read-only (admission exemption scoped to that Job and owned volume names), writes the PVC, compares size and checksums.
- Hold and delete: old volume kept; deletion is a separate High-risk Plan.
- Rollback Plan that keeps post-cutover writes: back up k3s copy → scale k3s to 0 → reverse-copy PVC into the held Docker volume (frozen copy kept as `<volume>-premigration` until done) → checksum → start Docker → revert Route. Never start Docker on the frozen pre-migration data.
- Update `docs/migration.md` with the W050 path; keep the two-masters rule.

## Non-goals

Bulk "move everything" button (later, built from this).

## Dependencies

Slices 7 and 9.

## Acceptance criteria

- [ ] e2e: mealie and the Postgres fixture moved, **new data written on k3s**, then moved back; the post-cutover data is present on Docker.
- [ ] A failed reverse copy leaves k3s stopped, the Route unchanged and both data copies intact.
- [ ] A copy mismatch stops the Plan before the address switch.
- [ ] Docker volume untouched until the delete Plan.

## Risks

Large volumes (Immich uploads) take hours; Plan must show estimated time and keep the app on Docker until the copy finishes.

## Size

**L.** One flow, but it touches real household data.

## ADR amendment

No; update `docs/migration.md`.
