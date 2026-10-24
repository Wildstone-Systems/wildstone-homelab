# W050 slice 12: live-install migration tooling

> Draft issue body. Plan: §6.10.

## Goal

An existing Docker household can move one package at a time to k3s, without data loss, with rollback.

## User-visible outcome

- Package page: "Move to the new engine". A Plan lists: back up, stop, copy data, start, switch address, keep old data 14 days.
- "Move back" works during those 14 days.

## Scope

- Plan template with the steps in plan §6.10.
- Copy Job: reads `/var/lib/docker/volumes/<owned>/_data` read-only (admission exemption scoped to that Job and owned volume names), writes the PVC, compares size and checksums.
- Hold and delete: old volume kept; deletion is a separate High-risk Plan.
- Rollback Plan: scale k3s to 0, start Docker containers, revert Route.
- Update `docs/migration.md` with the W050 path; keep the two-masters rule.

## Non-goals

Bulk "move everything" button (later, built from this).

## Dependencies

Slices 7 and 9.

## Acceptance criteria

- [ ] e2e: mealie and the Postgres fixture moved and moved back; data identical both ways.
- [ ] A copy mismatch stops the Plan before the address switch.
- [ ] Docker volume untouched until the delete Plan.

## Risks

Large volumes (Immich uploads) take hours; Plan must show estimated time and keep the app on Docker until the copy finishes.

## Size

**L.** One flow, but it touches real household data.

## ADR amendment

No; update `docs/migration.md`.
