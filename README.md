# Wildstone HomeLab

The control plane for a self-hosted home server: opinionated infrastructure,
desired-state reconciliation, capability-driven deployment, and a constrained
AI operator (the Butler).

> Less like Portainer. More like hiring an experienced systems administrator.

## Why

The modern home is full of other people's computers. Wildstone is the
counter-move: the home lab as household infrastructure, coordinated by a
Butler whose loyalty is enforced by construction — it can read and
propose, and structurally cannot act without human approval. Data stays
home; power stays with the household. Read [docs/vision.md](docs/vision.md).

## Status

**Alpha.** [`v0.1.13-alpha`](https://github.com/Wildstone-Systems/wildstone-homelab/releases/tag/v0.1.13-alpha)
is the first public release. The MVP epics (E1–E13) are implemented:
privileged agent, control plane, plan-mediated reconciliation, policy
engine, signed capability packages, backups with a weekly
total-host-loss exit drill in CI, secrets, installer, CLI, web console,
and the Butler (sessions, diagnostics, injection-hardening red-team
suite).

It installs and runs, but expect rough edges and frequent changes.
Package quality varies — some capabilities are well tested, others are
still experimental — and upgrades between alpha releases may need
manual steps. Back up anything you can't afford to lose. What remains,
chiefly moving a real household onto it, is tracked in the
[alpha tracker](https://github.com/Wildstone-Systems/wildstone-homelab/issues/13).

## Install

On a Linux host, as one line:

```sh
curl -fsSLO https://github.com/Wildstone-Systems/wildstone-homelab/releases/latest/download/install.sh && sudo bash install.sh --role keystone
```

The installer verifies every download against the release's
`checksums.txt`, asks you to accept the AGPL-3.0 licence, and walks you
through setup. [docs/install.md](docs/install.md) covers joining more
machines, pinning a version, and air-gapped installs.

## Documentation

- [docs/vision.md](docs/vision.md) — why this exists.
- [docs/concepts.md](docs/concepts.md) — the mental model.
- [docs/install.md](docs/install.md) — installing (from a release).
- [docs/releasing.md](docs/releasing.md) — cutting a release.
- [docs/recovery.md](docs/recovery.md) — the recovery contract and
  total-host-loss runbook, proven weekly by `scripts/exit-drill.sh`.
- [docs/threat-model.md](docs/threat-model.md) — the threat model
  against as-built.
- [docs/02-implementation-scope.md](docs/02-implementation-scope.md) —
  the epic/milestone scope; [docs/decisions/](docs/decisions/) — the
  W-series ADRs.

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

Go 1.26+. `go build ./...` from `wardstone/`, `keystone/`, or `cli/`
(workspace via `go.work` at the root); `npm ci && npm run build` in
`console/`. The full verification loop is what CI runs: `gofmt -l`,
`go vet ./...`, `go test -race ./...` per module.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) (short version: issues and small
PRs welcome; large PRs — talk first) and [SECURITY.md](SECURITY.md)
(private disclosure, please).

## License

[AGPL-3.0](LICENSE). If you run a modified Wildstone as a service for
others, you owe them your source — that's the point.

The Wildstone name is not covered by the code license; don't ship a
fork under the same name.
