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

It then asks for the account you will log in with, the household's time
zone, which services you want, and shows a summary before doing anything.
The time zone matters more than it looks: servers usually run on UTC, and
your agents read "3pm" and "tomorrow" in whatever zone the household keeps.
You can change it later on the Console's Household screen, and an agent
that hears it is wrong will propose the right one for you to approve. Approving that summary
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

  **Signing in** (ADR-W033 A4.1). Behind the tailnet, the Console asks
  Authelia who you are and when you last signed in. Open it from the
  **Console** button at the top of the household dashboard. Only Owners
  see that button. A family sign-in lasts 30 days (the identity
  package's `session_days`). The Console is stricter: it sends you back
  through Authelia once your sign-in is 12 hours old
  (`-console-entry-window`). Drastic actions ask you to confirm your
  password if you signed in more than 15 minutes ago
  (`-console-stepup-window`). That covers removing or offboarding a
  person, deleting an Agent, restoring a backup, taking away service
  access, rotating a node secret and minting any credential. Bearer tokens (`wild`, the installer,
  recovery from the host) are not affected. The installer's own token
  lasts two hours and is deleted when the install finishes.
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
- **Household lists** (`lists-wildstone`, installed with the homepage):
  shopping, chores, packing — shared by the whole household. Agents
  read and change them through `lists.*` household actions (adding,
  ticking and removing items act at once; deleting a whole list asks the
  person first), and the dashboard's Lists card and the `/lists/` page
  show them to members in `lists_users` (the page can also delete a
  whole list, after asking). Nothing runs for it: Keystone keeps the lists in its
  runtime database, so they are backed up with the runtime state
  ([recovery.md](recovery.md)), not with the Statefile.
- **Butler** (ADR-W017): `-butler-provider anthropic|openai|ollama`,
  with `-butler-model`/`-butler-endpoint` as needed. There is
  deliberately no key flag: a hosted provider's key is either entered in
  the Console (Butlers → Model providers; write-only, kept root-only on
  this machine — ADR-W017 A10) or set in keystone's environment, which
  wins — `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`, or
  `OPENAI_COMPATIBLE_API_KEY`. At startup the provider runs a
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

## 4b. Optional: talk to an Agent from a chat app

A **connection** lets one person talk to one of your Agents from
Telegram, Discord, Slack or Matrix (ADR-W033 B7, amended B7.1). Each
connection is a bot of its own: one Agent can have several, and a bot
answers for exactly one Agent. Who a bot may admit follows the Agent, not
the bot: an owner agent's bots admit Owners only.

Nothing needs building or publishing. Every release ships the chat
adapter, and the installer puts it on each machine at
`/srv/wildstone/bin/chat-adapter`. A bot runs it in Google's public
distroless image, pinned by digest, with that directory mounted
read-only, so there is no image of Wildstone's to host and no registry
credential to give a node.

**From the Console: Chat.** One screen lists each Agent's bots, connects
people, shows who is bound and lets you revoke an account or remove a
bot. To connect someone, pick the app, the Agent and the person. If the
Agent already has a bot on that app they are simply invited to it: one
bot serves many people, and a token already running another bot is
refused, because two bots polling one token steal each other's messages.
For a new bot, paste its credential:

| App | What you paste | Where it comes from |
|---|---|---|
| Telegram | bot token | @BotFather → /newbot |
| Discord | bot token | discord.com/developers → Bot → Reset Token. Turn on **Message Content Intent** there too, or the bot receives messages with their text removed. |
| Slack | bot token (`xoxb-…`) and app-level token (`xapp-…`) | api.slack.com/apps. Turn on Socket Mode; the bot needs `chat:write`, `im:history`, `files:read` (for voice clips) and the `message.im` event, and the app token needs `connections:write`. |
| Matrix | homeserver address and access token | an ordinary account on your homeserver, used as the bot. Keep its token-bearing client session active and use a new unencrypted room; the adapter accepts room invitations but does not implement Matrix E2EE. |

Keystone then does the rest:
1. It gives the bot its own Keystone credential, which holds the
   `converse` verb and nothing else.
2. It stores your credential on the machine.
3. It installs the bot as its own instance (`chat-telegram-joan`,
   `chat-telegram-joan-2`, …).
4. The Agent tells the person there's a connection waiting.

Creating a connection mints a credential, so the Console asks you to
confirm your password if you signed in more than 15 minutes ago.

**The person collects their own key.** The Agent's message appears on
their household dashboard, or in the Console for an owner agent. There
they press **Get my key**, and a single-use key is made for them there
and then. On Telegram it is a link that opens the bot with the key
already in it. Elsewhere it is a short code they send the bot in a direct
message. The first account to send it becomes theirs on that bot. The key
is never stored in readable form, and the Owner who set up the
connection never sees it.

Approving is never available over chat, for anyone. Ask an owner agent
to change something and it replies with a plan id and "nothing has
changed yet". You approve in the Console or with `wild plan approve`,
where a Wildstone credential authenticates you. A chat account is
protected by the vendor's account recovery, which Wildstone does not
control.

Revoking an account on the Chat screen (or `wild chat revoke <id>`)
takes effect on its next message. **Remove…** beside a bot uninstalls
it, revokes the Agent's address and the bot's own credential, and
withdraws its open invites. Removing a bot asks you to confirm your
password if you signed in more than 15 minutes ago.

The adapter **dials out**: Telegram and Matrix are polled, and Discord
and Slack hold a WebSocket open. So this adds no port, no Route and
nothing at your edge.

**A bot says nothing to people it doesn't know**: no greeting, no "wrong
code", nothing. Keystone records that an unknown account tried, without
what it said. **In a group or server channel, a bot answers only when
addressed**: mention it (`@joan_bot`) or reply to one of its messages.
Everything else said there never reaches Keystone (ADR-W033 B9.1).

Once a person is bound, a household rule can ask through the bot, since
`send` is a `domestic` action like `notifications.notify`. Their reply
lands back on the household timeline as `household.answered`, correlated
to the rule that asked. Recording an answer involves no model, so that
loop keeps working with no Butler provider configured at all.

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
