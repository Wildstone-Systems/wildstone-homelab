<div align="center">

# Wildstone HomeLab

**Your home server, run by an experienced sysadmin that never sleeps.**

A self-hosted control plane with desired-state reconciliation,
capability-driven deployment, and a constrained AI operator — the Butler.

[![CI](https://github.com/Wildstone-Systems/wildstone-homelab/actions/workflows/ci.yml/badge.svg)](https://github.com/Wildstone-Systems/wildstone-homelab/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Wildstone-Systems/wildstone-homelab?include_prereleases&label=release)](https://github.com/Wildstone-Systems/wildstone-homelab/releases)
[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)
![Status: Alpha](https://img.shields.io/badge/status-alpha-orange.svg)

[Install](#quick-start) · [Features](#features) · [Docs](docs/README.md) · [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md)

</div>

---

> Less like Portainer. More like hiring an experienced systems administrator.

## Why Wildstone?

The modern home is full of other people's computers. Wildstone is the
counter-move: the home lab as **household infrastructure**, coordinated by
a Butler with scoped tools and explicit authority.

- **You stay in charge** — infrastructure changes are proposed as Plans and need your approval.
- **Local first** — runs on your hardware, under your control.
- **Safe by design** — no arbitrary shell on the wire; every action is typed and checked.

Read the full story in [docs/vision.md](docs/vision.md).

## Features

- 🧠 **The Butler** — an AI operator with scoped tools and explicit permissions.
- 🔁 **Desired-state reconciliation** — describe what you want; Wildstone keeps it that way.
- 📦 **75+ ready-made packages** — Jellyfin, Immich, Home Assistant, Vaultwarden, Paperless, Ollama, Pi-hole and more ([browse](packages/)).
- ✅ **Plans & policy** — every infrastructure change is reviewed before it runs.
- 💾 **Backup & recovery** — local and off-site (S3) backups with a tested [recovery runbook](docs/recovery.md).
- 🖥️ **Console + CLI** — a web UI and the `wild` CLI, both on the same public API.
- 🌐 **Multi-node** — enroll extra Linux machines; optional Tailscale networking.

## Quick start

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

Manual `--role` installs, joining more machines, pinning a version and
air-gapped installs are covered in [docs/install.md](docs/install.md).

## Project status

> [!WARNING]
> **Alpha software.** [`v0.1.13-alpha`](https://github.com/Wildstone-Systems/wildstone-homelab/releases/tag/v0.1.13-alpha)
> is the first public release. It installs and runs, but it is not yet
> production-ready. Back up anything you can't afford to lose.

- Some capabilities are well tested; others are still experimental.
- Not every package has passed live install and restore testing.
- Upgrades between alpha releases may need manual steps.
- Kubernetes placement and automatic failover are **not** shipped yet — see the
  proposed [ADR-W051](docs/decisions/ADR-W051-failure-domains-and-recovery.md)
  on top of the accepted [ADR-W050](docs/decisions/ADR-W050-multi-node-runtime-kubernetes-engine-host-providers-editions.md).

Track progress in the [alpha tracker](https://github.com/Wildstone-Systems/wildstone-homelab/issues/13)
and the [documentation review](docs/design/documentation-refresh-2026-10-22.md).

## How it works

| Component | Role |
|---|---|
| **Keystone** | Control plane: API, Reconciler, Capability Engine, Policy Engine, Statefile, Butler |
| **Wardstone** | Privileged host agent (`wardstoned`) that runs workloads through a typed boundary |
| **Statefile** | Git-backed record of your desired state |
| **Butler** | AI operator that proposes Plans and performs permitted household actions |
| **Console** | Web UI (TypeScript/React) |
| **`wild`** | Command-line client |

New here? Start with [docs/concepts.md](docs/concepts.md) and the
[glossary](docs/glossary.md).

## Documentation

| Guide | What's inside |
|---|---|
| [Vision](docs/vision.md) | Why this exists |
| [Concepts](docs/concepts.md) | The mental model |
| [Install](docs/install.md) | Installing from a release |
| [Recovery](docs/recovery.md) | Recovery contract and total-host-loss runbook |
| [Threat model](docs/threat-model.md) | Trust boundaries and residual risks |
| [Releasing](docs/releasing.md) | Cutting a release |
| [Decisions](docs/decisions/) | W-series ADRs |
| [Implementation scope](docs/02-implementation-scope.md) | Epic scope and planning guidance |
| [App Management Contract 0.1](docs/design/application-management-contract-v0.1.md) | Proposed operational contract ([comparison](docs/design/application-management-contract-v0.1-comparison.md)) |
| [Full index](docs/README.md) | Everything, with authority order |

## Ground rules

These are enforced from commit one:

1. **No secrets in this repository, ever** — including test fixtures. CI runs a secret scanner on every push.
2. **No `Exec(string)` in the Wardstone protocol.** "No arbitrary shell" is a property of the wire format, not a policy hope.
3. **Console, CLI and Butler use the same public API.** No private UI-only or AI-only endpoints.
4. **Infrastructure changes use Plans.** Agent, household and opt-in host actions keep their own scoped checks; no Agent may approve its own infrastructure Plan.

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

| Path | Contents |
|---|---|
| `proto/` | gRPC + resource schemas — the contract everything else consumes |
| `wardstone/` | Privileged host agent |
| `keystone/` | Control plane |
| `cli/` | `wild` CLI |
| `console/` | Web UI |
| `packages/` | Capability packages and the package toolchain |
| `scripts/install.sh` | Release installer, setup and node enrollment |
| `installer/` | Installer background docs |
| `testing/` | e2e harness, scrubbed fixtures, restore-drill runner |
| `docs/` | Product docs and ADRs |

</details>

## Contributing

Issues and small PRs are welcome; for large PRs, please open an issue to
talk first. See [CONTRIBUTING.md](CONTRIBUTING.md).

Found a vulnerability? Please report it privately — see [SECURITY.md](SECURITY.md).

## License

[AGPL-3.0](LICENSE). If you run a modified Wildstone as a service for
others, you owe them your source — that's the point.

The Wildstone name is not covered by the code license; don't ship a fork
under the same name.
