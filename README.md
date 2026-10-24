# Wildstone HomeLab

The control plane for a self-hosted home server: opinionated infrastructure,
desired-state reconciliation, capability-driven deployment, and a constrained
AI operator (the Butler).

> Less like Portainer. More like hiring an experienced systems administrator.

## Why

The modern home is full of other people's computers. Wildstone is the
counter-move: the home lab as household infrastructure, coordinated by a
Butler with scoped tools and explicit authority. Infrastructure proposals
require human approval; permitted household actions use their own checked
grants and confirmation rules. Local operation and household control are
central to the design. Read [docs/vision.md](docs/vision.md).

## Status

**Pre-alpha.** The repository contains the control plane and host agent,
declarative packages, policy and Plan handling, installer, Console/CLI,
household and Agent services, and local/off-site backup and recovery code.
Implementation does not establish production readiness or that every package
has passed live installation and restore testing. See the
[alpha tracker](https://github.com/Wildstone-Systems/wildstone-homelab/issues/13)
and [documentation review](docs/design/documentation-refresh-2026-10-22.md).

### Deployment today and proposed direction

Today the installer installs Linux systemd services. Workload nodes need
Docker; Keystone and workload services can share a host, and additional
Linux nodes can enroll separately. On Proxmox, use a Linux guest for the
existing installation path; this is not a Proxmox-host installer.

[ADR-W051](docs/decisions/ADR-W051-proxmox-first-deployment-and-recovery.md)
proposes a simple Proxmox-first experience with independent management and
application recovery groups. VM versus LXC placement and guest provisioning
remain under review. Native LXC management and automatic cluster failover are
not shipped features. No migration or feature removal is authorized by the draft.

## Documentation

- [docs/vision.md](docs/vision.md) — why this exists.
- [docs/concepts.md](docs/concepts.md) — the mental model.
- [docs/install.md](docs/install.md) — installing (from a release).
- [docs/recovery.md](docs/recovery.md) — the recovery contract and
  total-host-loss runbook, with scoped local and S3 CI drills.
- [docs/threat-model.md](docs/threat-model.md) — selected trust boundaries, implementation evidence and residual risks.
- [docs/02-implementation-scope.md](docs/02-implementation-scope.md) —
  historical epic scope and current planning guidance; [docs/decisions/](docs/decisions/) — the
  W-series ADRs.
- [docs/glossary.md](docs/glossary.md) — shared vocabulary, with proposed terms marked.
- [Application Management Contract 0.1](docs/design/application-management-contract-v0.1.md) — proposed operational contract and [package comparison](docs/design/application-management-contract-v0.1-comparison.md).
- [docs/README.md](docs/README.md) — full documentation index and authority order.

## Layout

| Path | Contents |
|---|---|
| `proto/` | gRPC + resource schemas — the contract everything else consumes; versioned first |
| `wardstone/` | Privileged host agent (`wardstoned`): typed workload execution boundary |
| `keystone/` | Control plane: API, Reconciler, Capability Engine, Policy Engine, Statefile, Butler |
| `cli/` | `wild` — the CLI client |
| `console/` | Web UI (TypeScript/React) |
| `packages/` | Capability packages (data) and the package toolchain |
| `scripts/install.sh` | Release installer, interactive setup and node enrollment |
| `installer/` | Installer background documentation |
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
4. **Infrastructure desired-state changes use Plans.** Agent, household and opt-in host actions retain their own scoped authorization and confirmation checks; no Agent may approve its own infrastructure Plan.

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
