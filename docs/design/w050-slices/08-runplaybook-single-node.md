# W050 slice 8: host updates on one node (`RunPlaybook`)

> Draft issue body. Plan: §6.1.

## Goal

OS, k3s and Wardstone updates on a single-node k3s install run as Plan steps through `RunPlaybook`, with the downtime stated before approval.

## User-visible outcome

- Console: "Update this machine" Plan: "Apps will be down for about N minutes; choose when."
- Node page shows OS, kernel, k3s and Wardstone versions after the update.

## Scope

- Proto `RunPlaybook(name, version, typed params)` with `OsUpdateParams`, `K3sUpgradeParams`, `WardstoneUpdateParams`.
- Wardstone: verify manifest signature, run `ansible-playbook -c local` with a fixed argv; stream bounded progress; reboot handling (report, reboot, reconnect, report versions).
- Playbooks `os-update.yml`, `k3s-upgrade.yml`, `wardstone-update.yml`.
- Keystone Plan step kind "host update"; risk High; one-node downtime text (ADR-W050 §2.5).
- Write the #129 recommendation (slices 3–8 results) for owner review.

## Non-goals

Cordon/drain and multi-node ordering (slice 14).

## Dependencies

Slice 3.

## Acceptance criteria

- [ ] e2e: OS update Plan → reboot → node back → apps ready → versions reported.
- [ ] k3s upgrade to a pinned newer patch version succeeds; a bad version fails before replacing the binary.
- [ ] Unsigned or unknown playbook name refused with `FORBIDDEN`; params outside the typed message impossible by construction.
- [ ] Butler can propose but never approve the Plan.
- [ ] #129 recommendation document committed.

## Risks

A failed reboot leaves the only node down; document hardware power/BIOS prerequisites (W051 §4.4).

## Size

**M.** One op, three playbooks, one Plan step type.

## ADR amendment

No (ADR-W050 §2.5/§2.7). Optional W050 §2.7 wording on typed params.
