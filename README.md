# Wildstone HomeLab

The control plane for a self-hosted home server: opinionated infrastructure,
desired-state reconciliation, capability-driven deployment, and a constrained
AI operator (the Butler).

> Less like Portainer. More like hiring an experienced systems administrator.

## Status

Pre-alpha — Phase 0 (architecture spikes). Nothing here is usable yet.

Architecture, scope, and decision documents currently live in the reference
deployment repository:
[`chickenrat17/homelab-config`](https://github.com/chickenrat17/homelab-config)
under `docs/architecture/wildstone/` — start with `01-architecture-proposal.md`
and `02-implementation-scope.md`.

## Layout

| Path | Contents |
|---|---|
| `proto/` | gRPC + resource schemas — the contract everything else consumes; versioned first |
| `wardstone/` | Privileged host agent (`wardstoned`): the only component that touches the host |
| `keystone/` | Control plane: API, Reconciler, Capability Engine, Policy Engine, Statefile, Butler |
| `cli/` | `wild` — the CLI client |
| `console/` | Web UI (TypeScript/React) |
| `packages/` | Capability packages (data) and the package toolchain |
| `installer/` | Bootstrap installer and preflight checks |
| `testing/` | e2e harness, scrubbed fixtures, restore-drill runner |
| `docs/` | Product docs and Wildstone ADRs (as they migrate from the reference repo) |

## Component vocabulary

**Wardstone** (host agent) · **Keystone** (control plane) · **Reconciler** ·
**Capability Engine** · **Policy Engine** · **Butler** (AI operator) ·
**Console** (UI) · **`wild`** (CLI) · **Statefile** (git-backed desired state).

## Ground rules (enforced from commit one)

1. **No secrets in this repository, ever** — including in test fixtures.
   Fixtures are scrubbed to placeholder values; CI runs a secret scanner on
   every push.
2. **No `Exec(string)` in the Wardstone protocol.** "No arbitrary shell" is a
   property of the wire format, not a policy hope.
3. **Console, CLI, and Butler use the same public API.** No private
   UI-only or AI-only endpoints.
4. **Every intentional change is a Plan.** No unmediated write path.

## Building

Go 1.25+. `go build ./...` from `wardstone/`, `keystone/`, or `cli/`
(workspace via `go.work` at the root). Real functionality lands with spike S1.
