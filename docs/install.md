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

`mint-join` prints the install command to run on the node with the
address, token and CA fingerprint already filled in — paste it as
given. While this repository is private it prints a second form as
well, and that is the one to use: the anonymous `curl` in the public
one-liner 404s on a private repo's release assets before the installer
ever runs.

```sh
# what mint-join prints, while the repo is private
gh release download --repo chickenrat17/wildstone-homelab --pattern install.sh
sudo GH_TOKEN=<pat> bash ./install.sh --role node \
  --keystone <keystone-host>:9440 \
  --join-token <token> \
  --ca-fingerprint <hex>
```

`--node-id` is optional and defaults to the node's short hostname; pass
`--node-id <name>` to choose it. If the command comes back with
`<host:9440>` in it, Keystone has no routable address to advertise
(`-hosts` is at its loopback default) — substitute the address the node
can reach it on, and consider setting `-hosts` so the next one is
filled in.

The join token is single-use and only read on first start; afterwards
the node authenticates with its own mTLS identity from
`/var/lib/wildstone/wardstone`. The CA fingerprint pins the join against
MITM — omit it only on a network you fully trust (trust-on-first-use).

Confirm it arrived:

```sh
wild node list
# NODE   STATE      AGENT         PROTOCOL  SINCE
# vm-1   connected  v0.1.0-alpha  minor 7   just now
```

`connected` is read from the live agent session, not from stored state,
so it is always the current answer. The Console's Nodes view shows the
same fleet.

### Installing interactively

Run it with no `--role` from a terminal and it asks instead of assuming
(ADR-W023):

```sh
sudo bash ./install.sh
```

It shows the licence, offers to install Docker if it is missing rather
than installing it silently, and offers to set up Tailscale — printing a
sign-in link to open in any browser, which is also the path to take if
you have no Tailscale account yet.

It then asks for the account you will log in with, which services you
want, and shows a summary before doing anything. Approving that summary
is the approval for every plan the install creates (ADR-W023 A2) — they
are recorded in the audit trail against your account.

From there it does the rest by itself: installs the control plane, pairs
this host as its own node, waits for the agent to connect, then installs
the ingress, identity, the dashboard and your chosen services. It
finishes by printing the address to open.

Answers are saved to `/var/lib/wildstone/install-answers` (root-only, no
credentials — the password is used and discarded) as they are given, so
an install that fails partway resumes without re-asking. Delete that
file to start the questions over.

Passing any `--role` keeps the old non-interactive behaviour exactly, so
upgrades, node joins and automation are unchanged — as is a run whose
stdin is not a terminal.

### Joining the host to a tailnet (optional)

Either role accepts `--tailscale-authkey`, which installs `tailscaled`
on the host and joins it to your tailnet:

```sh
sudo bash ./install.sh --role node ... \
  --tailscale-authkey tskey-auth-... \
  --tailscale-hostname media-1        # defaults to --node-id
```

### If the admin token is lost

The token printed at first start exists only in that one line of output.
If it is gone — scrubbed, never captured, or lost among tokens from
earlier installs on the same host — mint a new one offline:

```sh
sudo systemctl stop keystone
sudo keystone claim-code --data-dir /var/lib/wildstone/keystone
sudo systemctl start keystone
sudo cat /var/lib/wildstone/keystone/claim-code   # then delete it
```

This creates the `admin` principal if it does not exist and mints a
token either way. Existing principals and their tokens are untouched.
The authority check is possession of the data directory, i.e. root on
the host — the same proof of physical access join tokens already rely
on, so nothing new is trusted.

Note that `journalctl` outlives a reinstall, so grepping it for `wst_`
returns a token per install the host has ever had, oldest first (F66).
Prefer this command over the journal.

`TAILSCALE_AUTHKEY` in the environment works too. The key is passed
straight to `tailscale up` and never written to disk or printed — use a
pre-authorized, ideally ephemeral key. Tailscale SSH is deliberately
**not** enabled; turn it on yourself if you want it.

It also bind-mounts tailscaled's local API into Wildstone's managed
prefix, as `/srv/wildstone/tailscale/tailscaled.sock`, via a systemd
mount unit ordered after `tailscaled`. That is what lets the
`ingress-tailnet-ts` profile serve under a Tailscale-issued certificate:
Caddy fetches `*.ts.net` certificates from tailscaled and needs no
configuration at all to do it, but Wardstone bind-mounts host paths only
from its managed prefixes (ADR-W003 A1), and `/run/tailscale` is not
one.

The socket is **not moved**. `tailscale status` and every other command
keep working exactly as they did, because the CLI still finds it where
it has always been — the bind mount adds a second path to the same
socket rather than relocating it.

To use the tailnet ingress profile you also need HTTPS certificates
enabled for your tailnet (Tailscale admin console → DNS → HTTPS
Certificates). Without it, tailscaled has no certificate to hand over
and Caddy serves none, which surfaces as a TLS handshake failure rather
than as anything naming the setting.

Re-running the installer to upgrade leaves an existing join alone, so
this is safe on every run.

**Why the host and not a container.** Tailscale needs `/dev/net/tun`
and `NET_ADMIN`. The Wardstone operation surface carries neither, on
purpose — a closed surface with no way to grant a container host
network control is the property the whole design defends. So this is
host setup you own, exactly like Docker.

The consequence is that the tailnet join is **not Wildstone-managed**:
it is not in the Statefile, not reconciled, and not restored when you
rebuild a host from backups. [recovery.md](recovery.md) lists it among
the steps you redo by hand.

## 4. Optional: Console and Butler

- **Console**: nothing to do. The installer fetches
  `console_dist.tar.gz` from the release, verifies it against
  `checksums.txt` like every other artifact, unpacks it to
  `/usr/share/wildstone/console`, and points Keystone at it.

  It is a release asset rather than something built on the host because
  **the host needs no Node at all to serve static files**, and
  `apt-get install -y nodejs npm` on a clean Ubuntu Server pulls ~570
  packages, including a full C/C++ toolchain (`build-essential`, `gcc`,
  `g++`) and an X11/Vulkan graphics stack (`alacritty`,
  `mesa-vulkan-drivers`) — a large attack surface to add to a
  control-plane host for a file server. The Console's own dependencies
  were never the problem: `npm ci` installs 72 packages in about two
  seconds, and the build produces 160 KB.

  Installing a release older than the asset is not an error; the
  installer says so and serves the API alone.

  To run your own build instead — a branch, or a local change — pass
  `--console-dir`, which always wins over the release asset:

  ```sh
  # on your workstation, in a checkout of this repo
  cd console && npm ci && npm run build
  scp -r dist/ <keystone-host>:/srv/wildstone/console

  sudo bash ./install.sh --role keystone --console-dir /srv/wildstone/console
  ```

  The flag lands in `KEYSTONE_ARGS` in `/etc/wildstone/keystone.env`,
  which the unit's `ExecStart` expands. Re-running the installer to
  upgrade rewrites the unit but leaves that file alone, so the flag
  survives; edit the file (and `systemctl restart keystone`) to change
  it, or pass `--keystone-arg <flag>` for any other keystone flag. The
  Console is served beside `/api` on the same listener — same API, same
  tokens.
- **Household automation** (ADR-W024): `-household-timezone` is the
  IANA zone the household's rules keep for time-of-day and weekday
  conditions (default `Local`); `-automation-interval` is how often the
  engine ticks when no event has poked it (default 15s).
- **Home Assistant** (ADR-W024 H5): the `home-automation` package
  configures an existing Home Assistant to publish arrivals and
  departures. Mint its credential FIRST (`wild household publisher
  home-automation -node <node>`), then enable the capability with
  `intake_url` (the API address Home Assistant can reach — usually a
  tailnet address or a Route, not loopback) and `people` (its person
  entities, e.g. `person.mika, person.sam`), then include the rendered
  file from `configuration.yaml`:

  ```yaml
  homeassistant:
    packages: !include_dir_named /etc/wildstone/home-automation
  ```

- **Household actions** (ADR-W024 H4): `-ntfy-url` is the ntfy server
  the `notifications.notify` action publishes to; leaving it empty
  registers no executor, and a rule naming one is refused with a reason
  on the household stream. The ntfy access token comes only from
  `NTFY_TOKEN`, never a flag. `-household-reply-url` is the household
  event intake address a notification's reply buttons post to **as a
  phone can reach it** — not Keystone's loopback address, so this needs
  a Route or a tailnet address in front of the API. Leaving it empty
  means notifications carry no buttons, and a rule that asks for replies
  fails loudly rather than asking a question nobody can answer.
  `-household-reply-ttl` bounds how long a reply token stays valid
  (default 12h).
- **Butler** (ADR-W017): `-butler-provider anthropic|openai|ollama`,
  with `-butler-model`/`-butler-endpoint` as needed. API keys come ONLY
  from `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` environment variables —
  there is deliberately no key flag. At startup the provider runs a
  tool-reliability probe; a model that fails it runs read-only
  (QA-only). `-butler-provider off` (the default) leaves the ask
  endpoint answering 503; everything else, including the morning
  report, works regardless.

  A local Ollama needs no key at all, which makes it the shortest path
  to a working Butler:

  ```
  keystone \
    -butler-provider ollama \
    -butler-endpoint http://localhost:11434 \
    -butler-model llama3.1
  ```

  Whatever you choose, the Console's **Butler** screen shows what the
  flags resolved to — provider, model, endpoint, probe verdict, and the
  tools that session is offered — and when nothing is configured it says
  so and prints the flags above rather than just failing. Ask there; a
  session's proposals appear on **Plans** as `proposed`, and approving
  them is still a human verb.

## 4b. Optional: talk to the Butler from a chat app

The Butler answers on two surfaces (ADR-W026), and they are two
different jobs rather than one with a volume knob:

| | operator | household |
|---|---|---|
| Reads | the Statefile, plans, health, diagnosis | the household timeline |
| Writes | proposes a plan a human approves | one `domestic`-tier action |
| Cannot | approve or apply anything | see or touch the home lab at all |

Both are reachable from a chat app, and **each surface is its own bot
with its own credential**. That is not tidiness: one bot serving both is
one compromised phone away from being an infrastructure console.

Approving is never available over chat, for anyone. Ask the operator bot
to change something and it replies with a plan id and "nothing has
changed yet" — you approve in the Console or with `wild plan approve`,
where a Wildstone credential authenticates you. A chat account is
protected by the vendor's account recovery, which Wildstone does not
control.

Enabling one, using Telegram's household bot as the example:

```sh
# 1. Build the adapter image and push it (see "Picking a registry" below).
docker buildx build --platform linux/amd64 \
  -f cli/cmd/chat-adapter/Dockerfile \
  -t ghcr.io/<you>/chat-adapter:1.0.0 . --push
docker buildx imagetools inspect ghcr.io/<you>/chat-adapter:1.0.0   # prints the digest

# 2. Make the bot (Telegram: talk to @BotFather) and keep its token.

# 3. Mint the adapter's Keystone credential. The surface decides the
#    role: family -> concierge, admin -> valet. Both hold the `converse`
#    verb and nothing else, and the capability's name has to agree with
#    the surface you ask for.
wild chat adapter chat-telegram-family -node <node> -surface family

# 4. Set the bot token, then enable the capability with the image digest.
wild secret set chat-telegram-family-bot -node <node>
wild capability enable chat-telegram-family \
  -set node=<node> \
  -set keystone_url=http://<tailnet-ip>:9441 \
  -set adapter_image=<registry>/chat-adapter@sha256:<digest>

# 5. Introduce someone. Nobody can talk to the bot until you do.
wild chat code -surface family -identity <member> -transport telegram
```

Read the code out; they send it to the bot on its own, once. It works
once, expires, and can only ever create the binding it names. After that
`wild chat bindings` is the access list and `wild chat revoke <id>`
takes effect on that account's next message.

The adapter **dials out** — Telegram's and Matrix's APIs are polled — so
this adds no port, no Route and nothing at your edge. `chat-matrix-*` is
the same shape against a homeserver you can run yourself; the operator
variants are `chat-telegram-admin` and `chat-matrix-admin`, installed
the same way with `-surface admin` and a second bot.

### Picking a registry

Wildstone publishes no adapter image. The release pipeline builds
binaries, not images, so you build this one yourself — and where you put
it is constrained by one thing worth knowing before you choose.

**It must be pullable without credentials.** Keystone pulls the image
before every container start, a failed pull stops the step, and that
pull carries no registry auth. So:

- a **private** registry (private GHCR, a password-protected Docker Hub
  repo) does not work — the pull fails and the container never starts;
- **side-loading** with `docker load` on the node does not work either,
  because the pull still runs and still fails.

**A public repository under your own account is the straightforward
answer**, and it costs you nothing here. The image is one static binary
built from open source on a `scratch` base — no secrets are baked into
it, and every credential (bot token, Keystone token) arrives as an
environment variable at runtime from your secret store.

Two things to get right:

- **Match your node's architecture.** `docker build` produces your
  laptop's; if the node is a Pi, pass `--platform linux/arm64`. Building
  the wrong one fails at start with an exec-format error.
- **Pass the digest, not the tag.** `adapter_image` is digest-pinned
  like every image Wildstone runs (ADR-W003 A4.2); `docker buildx
  imagetools inspect` prints it.

If you would rather nothing were public, the options are a registry on
your tailnet — which needs each node's Docker daemon configured for it,
outside Wildstone's management — or adding registry auth to the agent's
pull, which is recorded as F94 and is a small feature rather than a
workaround.

Once a household bot is bound, a household rule can ask through it —
`chat-telegram-family.send` is a `domestic` action like `notifications.notify`
— and the member's reply lands back on the household timeline as
`household.answered`, correlated to the rule that asked. Recording an
answer involves no model, so that loop keeps working with no Butler
provider configured at all.

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
