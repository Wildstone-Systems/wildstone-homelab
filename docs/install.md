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
sudo bash ./install.sh --role keystone
```

**Private repo?** An anonymous download cannot reach a private
repository's release assets. Pass a GitHub token and the installer
switches to the authenticated release-asset API:

```sh
sudo GH_TOKEN=github_pat_... bash ./install.sh --role keystone
# or: sudo bash ./install.sh --role keystone --token github_pat_...
```

The token needs only **contents: read** on this repository (a
fine-grained PAT scoped to it is enough). `WILDSTONE_TOKEN` and
`GITHUB_TOKEN` are also read. The `curl` above that fetches
`install.sh` is itself anonymous, so while the repo is private get the
script with an authenticated client instead:
`gh release download <tag> -R chickenrat17/wildstone-homelab -p install.sh`.

**Air-gapped host?** Download the release assets (`*_linux_*.tar.gz` +
`checksums.txt` + `install.sh`) elsewhere with `gh release download`,
copy them into one directory on the host, and run `install.sh` from
that directory — it detects the local `checksums.txt`, skips the
download, and still verifies every sha256. `--artifact-dir <dir>`
points at the assets explicitly if they live elsewhere.

This installs `/usr/local/bin/keystone`, the `keystone.service` unit,
and starts it. First start prints two things you must record:

```sh
journalctl -u keystone -o cat | grep -A1 'first start'   # one-time admin API token (wst_...)
journalctl -u keystone -o cat | grep 'CA fingerprint'    # pin for node joins
```

(`-o cat` drops the journald prefix so the values paste cleanly.)

The database stores only the token's hash (ADR-W007 A3), so Keystone
can never show it again — put it in your password manager now. It is
*printed once* but **not stored once**: it stays in the journal in
plaintext, readable by root and the `adm` group. Once it is safely in
your password manager, scrub it:

```sh
journalctl --rotate && journalctl --vacuum-time=1s   # drops the whole keystone journal
```

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
sudo bash ./install.sh --role node \
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

A worked hand-written proposal — a volume plus a container on node
`node1`. Two validation rules bite everyone the first time:

- Every resource **must** carry `metadata.owner` (convention:
  `capability/<name>`, or `system`). Packages get it injected by the
  capability engine; hand-written proposals have to set it themselves
  or validation rejects them.
- Container images **must** be digest-pinned (`repo@sha256:<64 hex>`);
  tags are refused (ADR-W003). Resolve a tag once with:
  `docker pull nginx:1.27 && docker image inspect --format '{{index .RepoDigests 0}}' nginx:1.27`

```sh
cat > /tmp/hello-volume.yaml <<'EOF'
apiVersion: wildstone.dev/v1
kind: Volume
metadata:
  name: hello-data
  owner: capability/hello
  node: node1
spec:
  name: hello-data
EOF

cat > /tmp/hello-container.yaml <<'EOF'
apiVersion: wildstone.dev/v1
kind: Container
metadata:
  name: hello
  owner: capability/hello
  node: node1
spec:
  image: nginx@sha256:<64-hex-digest-from-the-resolve-step>
  mounts:
    - source: hello-data
      target: /usr/share/nginx/html
  restart: unless-stopped
EOF

curl -sS -X POST http://127.0.0.1:9441/api/v1/plans \
  -H "Authorization: Bearer $WILD_TOKEN" \
  -d "$(jq -n --arg v "$(cat /tmp/hello-volume.yaml)" \
             --arg c "$(cat /tmp/hello-container.yaml)" \
        '{summary: "first workload: hello", put: [$v, $c]}')"

wild plan list                # find the proposed plan's id
wild plan approve <id>        # human approval is the only write gate
wild status                   # watch the reconciler converge it
```

The reconciler pulls the image itself (`PullImage` before
`EnsureContainer`) — nodes never need a manual `docker pull`.

Set up backups before you rely on any of it — see
[recovery.md](recovery.md) for what must exist before a host dies.
