# W050 slice 17: Talos edition (much later)

> Tracked in #173 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

A bare-metal appliance edition: boot a Wildstone Talos image, pair with a code on screen, join.

## User-visible outcome

"Install from USB" option; one OS install instead of OS plus Wildstone.

## Scope

- `TalosHost` provider in Wardstone (Talos API instead of Ansible).
- Image build, pairing code (maintenance mode accepts config unauthenticated on the LAN, so pairing is mandatory).
- A/B OS upgrades as `RunPlaybook`-equivalent Plan steps.
- Separate CI and exit drill.

## Non-goals

Mixed editions in one cluster.

## Dependencies

Slice 16.

## Acceptance criteria

- [ ] Fresh Talos node installs, joins, runs the e2e suite and the exit drill.
- [ ] Edition switch = backup → reinstall → restore, proven by the exit drill.

## Risks

Second edition doubles maintenance.

## Size

**XL.** New OS path and provider.

## ADR amendment

New ADR for the Talos provider and pairing.
