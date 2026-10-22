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

**Pre-alpha.** The MVP epics (E1–E13) are implemented: privileged agent,
control plane, plan-mediated reconciliation, policy engine, signed
capability packages, backups with a weekly total-host-loss exit drill in
CI, secrets, installer, CLI, web console, and the Butler (sessions,
diagnostics, injection-hardening red-team suite). What remains before an
alpha tag: the dogfood migration of a real household and the first
release-pipeline exercise — tracked in the
[road-to-alpha issue](https://github.com/chickenrat17/wildstone-homelab/issues/11).

Not ready to run your home yet. Watch the repo if you want to be there
when it is.

## Documentation

- [docs/vision.md](docs/vision.md) — why this exists.
- [docs/concepts.md](docs/concepts.md) — the mental model.
- [docs/install.md](docs/install.md) — installing (from a release).
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
