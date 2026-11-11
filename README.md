<div align="center">

# Wildstone HomeLab

**A household platform for your home server — built on tools you can trust.**

Guided setup, a curated app experience, approved changes and a
constrained AI operator (the Butler), on top of established open-source
infrastructure.

[![CI](https://github.com/Wildstone-Systems/wildstone-homelab/actions/workflows/ci.yml/badge.svg)](https://github.com/Wildstone-Systems/wildstone-homelab/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Wildstone-Systems/wildstone-homelab?include_prereleases&label=release)](https://github.com/Wildstone-Systems/wildstone-homelab/releases)
[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)
![Status: Alpha](https://img.shields.io/badge/status-alpha-orange.svg)

[Status](#project-status) · [Install](#quick-start) · [Docs](docs/README.md) · [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md)

</div>

---

> Less like Portainer. More like hiring an experienced systems administrator.

> [!IMPORTANT]
> **The architecture is being replaced in place.** New development follows
> [docs/architecture/](docs/architecture/README.md): Ubuntu 24.04 + K3s,
> Longhorn, Forgejo, Flux, Rancher, Authelia/LLDAP and Homarr (#338).
> Keystone, Wardstone, the Console, `wild`, `packages/` and the Docker
> installer below are the **legacy architecture**: non-production, frozen
> and retained as reference while the new platform is built and validated.
> The [inventory](docs/architecture/legacy-inventory.md) is for selective reuse,
> not early retirement.
> The published installer keeps working until its replacement ships.

## Why Wildstone?

The modern home is full of other people's computers. Wildstone is the
counter-move: the home lab as **household infrastructure**.

- **You stay in charge** — infrastructure changes are proposed as Plans and need your approval. Agents can propose, never approve.
- **Local first** — your settings, secrets and deployment config stay on your machines. No GitHub account, cloud service or telemetry required.
- **No reinvented wheels** — proven tools do the heavy lifting; Wildstone adds the household experience on top.
- **Safe by design** — no arbitrary shell; every action is typed and permission-checked.

Read the full story in [docs/vision.md](docs/vision.md).

## How it works

Wildstone is the **product layer**. Established tools do the infrastructure work.

| Job | Done by |
|---|---|
| Household intent, approvals, integrations, audit | **Wildstone** (Keystone, Console, CLI, Butler) |
| Running and scheduling apps | Kubernetes (K3s) |
| Deploying approved config | Flux + Helm (upstream charts) |
| Web access / ingress | Traefik |
| Sign-in | Authelia + LLDAP |
| Deployment secrets | SOPS / age |
| Backups | Restic + app-native tools |
| Narrow host tasks | Wardstone (typed host agent) |

**The flow, in plain terms:**

1. You (or the Butler) propose a change.
2. You review and approve the Plan.
3. Wildstone writes the approved config to a **local** Git repo on your server.
4. Flux picks it up and Kubernetes runs it.
5. Wildstone shows you what actually happened.

Details: [ADR-W050](docs/decisions/ADR-W050-multi-node-runtime-kubernetes-engine-host-providers-editions.md) ·
[migration plan](docs/design/w050-migration-plan.md) ·
[local deployment](docs/design/approved-local-deployment.md).

## Project status

> [!WARNING]
> **Alpha, and mid-transition.** The project is moving from its own Docker-based
> runtime to the Kubernetes/Flux/Helm platform above. Expect breaking changes.
> Back up anything you can't afford to lose.

| Area | State |
|---|---|
| Linux/systemd + Docker installer | ✅ Available today ([`v0.1.13-alpha`](https://github.com/Wildstone-Systems/wildstone-homelab/releases/tag/v0.1.13-alpha) was the first public release) |
| Plans, approvals, Console/CLI, Butler | ✅ Working, actively evolving |
| Local & off-site backup and recovery | 🧪 Implemented, being re-validated for the new platform |
| K3s + Flux deployment path | 🚧 In progress — single-node proofs, not yet the default |
| Apps as upstream Helm charts | 🚧 Being added in curated waves |
| Multi-node, HA, automatic failover | 📋 Planned ([ADR-W051](docs/decisions/ADR-W051-failure-domains-and-recovery.md), proposed) |
| Self-upgrade of the management release | 📋 Planned — release metadata is published, upgrades not enabled |

Follow along in the [alpha tracker](https://github.com/Wildstone-Systems/wildstone-homelab/issues/13)
and [open pull requests](https://github.com/Wildstone-Systems/wildstone-homelab/pulls).

## Quick start

> [!NOTE]
> This installs the **current** Linux/systemd + Docker path, not the new
> Kubernetes platform.

On a Linux host, run:

```bash
curl -fsSLO https://github.com/Wildstone-Systems/wildstone-homelab/releases/latest/download/install.sh && sudo bash install.sh
```

The installer:

- Verifies every download against the release's `checksums.txt`
- Asks you to accept the AGPL-3.0 licence
- Walks you through Docker, optional Tailscale, your account, time zone and services

**Requirements**

- Linux with systemd
- Docker on workload nodes
- On Proxmox: install inside a Linux guest (not on the Proxmox host)

Manual installs, extra nodes, version pinning and more:
[docs/install.md](docs/install.md).

## Documentation

| Guide | What's inside |
|---|---|
| [Architecture](docs/architecture/README.md) | **New architecture: boundaries and build–validate–retire strategy** |
| [Legacy inventory](docs/architecture/legacy-inventory.md) | Reference for selective reuse and post-validation consolidation |
| [Vision](docs/vision.md) | Why this exists |
| [Concepts](docs/concepts.md) | The mental model |
| [Install](docs/install.md) | Installing the current release |
| [Recovery](docs/recovery.md) | Recovery contract and total-host-loss runbook |
| [Threat model](docs/threat-model.md) | Trust boundaries and residual risks |
| [Decisions](docs/decisions/) | W-series ADRs (start with W050) |
| [Releasing](docs/releasing.md) · [Release contract](docs/design/management-release-contract.md) | How releases are cut and signed |
| [Glossary](docs/glossary.md) | Shared vocabulary |
| [Full index](docs/README.md) | Everything, with authority order |

## Ground rules

1. **No secrets in this repository, ever** — including test fixtures. CI runs a secret scanner on every push.
2. **No arbitrary shell in Wardstone.** "No arbitrary shell" is a property of the wire format, not a policy hope.
3. **Console, CLI and Butler use the same public API.** No private UI-only or AI-only endpoints.
4. **Infrastructure changes use Plans.** No Agent may approve its own Plan.
5. **Your deployment config stays local.** Never pushed to GitHub or any external service by default.

## Building from source

Requires **Go 1.26+** and **Node.js** (for the Console).

```bash
# Go modules (workspace via go.work)
cd keystone && go build ./...   # also: wardstone/, cli/
```

```bash
# Web Console
cd console && npm ci && npm run build
```

CI runs `gofmt -l`, `go vet ./...` and `go test -race ./...` per module.

<details>
<summary><strong>Repository layout</strong></summary>

Everything below the first two rows is legacy architecture.

| Path | Contents |
|---|---|
| `bootstrap/`, `platform/`, `catalog/`, `recovery/` | **New architecture** — created by #341, #343, #347 ([contract](docs/architecture/README.md)) |
| `tools/architecture/` | Boundary checker for the new tree |
| `proto/` | gRPC + resource schemas — the shared contract |
| `keystone/` | Control plane: API, Plans, policy, Statefile, Butler |
| `wardstone/` | Typed host agent |
| `cli/` | `wild` CLI |
| `console/` | Web UI (TypeScript/React) |
| `charts/` | Helm chart for the Wildstone management release |
| `packages/` | App packages (legacy catalog, migrating to upstream charts) |
| `runtimes/` | Agent/worker runtimes |
| `scripts/` | Installer and release tooling |
| `testing/` | e2e harness, scrubbed fixtures, restore drills |
| `docs/` | Product docs and ADRs |

</details>

## Contributing

Issues and small PRs are welcome; for large PRs, please open an issue first.
See [CONTRIBUTING.md](CONTRIBUTING.md).

Found a vulnerability? Please report it privately — see [SECURITY.md](SECURITY.md).

## License

[AGPL-3.0](LICENSE). If you run a modified Wildstone as a service for
others, you owe them your source — that's the point.

The Wildstone name is not covered by the code license; don't ship a fork
under the same name.
