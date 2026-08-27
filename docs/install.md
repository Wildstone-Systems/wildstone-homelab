# Installing Wildstone

Wildstone is two binaries plus a CLI:

- **keystone** — the control plane: agent gateway (`:9440`, mTLS),
  public HTTP API (`127.0.0.1:9441` by default), Statefile, runtime DB,
  reconciler, backup engine, Butler host.
- **wardstoned** — the node agent: the only thing that touches Docker,
  through a closed operation surface (ADR-W003). One per node.
- **wild** — the operator CLI, talking to the same public API as the
  Console and the Butler (ground rule: there is ONE API).

## Prerequisites

- A Linux host for Keystone (no Docker required there).
- Docker on every node that will run workloads.
- Releases are published from `v*` tags with `checksums.txt`
  (ADR-W014); the installer verifies sha256 before installing.

## 1. Install Keystone

```sh
curl -fsSLO https://github.com/chickenrat17/wildstone-homelab/releases/latest/download/install.sh
sudo ./install.sh --role keystone
```

This installs `/usr/local/bin/keystone`, the `keystone.service` unit,
and starts it. First start prints two things you must record:

```sh
journalctl -u keystone | grep -A1 'first start'   # one-time admin API token (wst_...)
journalctl -u keystone | grep 'CA fingerprint'    # pin for node joins
```

The admin token is shown exactly once — the database stores only its
hash (ADR-W007 A3). Put it in your password manager.

## 2. Set up `wild`

```sh
export WILD_TOKEN=wst_...       # the bootstrap token, or a minted one
wild status                     # talks to http://127.0.0.1:9441 by default; -api overrides
```

Mint scoped tokens for daily use rather than living on the bootstrap
admin token: `POST /api/v1/principals` + `/tokens` (admin only), or via
the Console's Principals view.

## 3. Join a node

On the Keystone host, mint a single-use join token (it comes with the
CA fingerprint):

```sh
wild node mint-join
```

On the node:

```sh
sudo ./install.sh --role node \
  --keystone <keystone-host>:9440 \
  --node-id <name> \
  --join-token <token> \
  --ca-fingerprint <hex>
```

The join token is single-use and only read on first start; afterwards
the node authenticates with its own mTLS identity from
`/etc/wildstone/wardstone`. The CA fingerprint pins the join against
MITM — omit it only on a network you fully trust (trust-on-first-use).

## 4. Optional: Console and Butler

- **Console**: build `console/` (`npm ci && npm run build`) and start
  keystone with `-console-dir <path-to-dist>`. It is served beside
  `/api` on the same listener — same API, same tokens.
- **Butler** (ADR-W017): `-butler-provider anthropic|openai|ollama`,
  with `-butler-model`/`-butler-endpoint` as needed. API keys come ONLY
  from `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` environment variables —
  there is deliberately no key flag. At startup the provider runs a
  tool-reliability probe; a model that fails it runs read-only
  (QA-only). `-butler-provider off` (the default) leaves the ask
  endpoint answering 503; everything else, including the morning
  report, works regardless.

## 5. First workload

Everything changes through a plan: propose (via a package, the Console,
the Butler, or `POST /api/v1/plans`), review, approve
(`wild plan approve <id>`), and the reconciler converges the node.
Nothing you propose is applied until a human approves it.

Set up backups before you rely on any of it — see
[recovery.md](recovery.md) for what must exist before a host dies.
