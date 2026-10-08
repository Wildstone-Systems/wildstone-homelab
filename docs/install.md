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

To upgrade a node that has already joined, run
`sudo bash ./install.sh --role node [--version vX.Y.Z]` — no token is
needed while the node's identity is on disk, and the node id, Keystone
address and CA pin are reused from `/etc/wildstone/wardstoned.env`.

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
oneshot service (`wildstone-tailscale-socket.service`) ordered after
`tailscaled`. That is what lets the `ingress-tailnet-ts` profile serve under a Tailscale-issued certificate:
Caddy fetches `*.ts.net` certificates from tailscaled and needs no
configuration at all to do it, but Wardstone bind-mounts host paths only
from its managed prefixes (ADR-W003 A1), and `/run/tailscale` is not
one.

The socket is **not moved**. `tailscale status` and every other command
keep working exactly as they did, because the CLI still finds it where
it has always been — the bind mount adds a second path to the same
socket rather than relocating it. Because it is a mount, deleting the
prefix needs care: see "Removing the managed prefix" in
[uninstall.md](uninstall.md).

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
| Slack | bot token (`xoxb-…`) and app-level token (`xapp-…`) | api.slack.com/apps. Turn on Socket Mode; the bot needs `chat:write`, `im:history`, `files:read` and `files:write` (to hear voice clips and answer with one) and the `message.im` event, and the app token needs `connections:write`. |
| Matrix | nothing, on the household's own homeserver; otherwise a homeserver address and access token | with `matrix-synapse` installed the bot is made there as an application service (ADR-W033 B7.2). For another homeserver, an ordinary account there, used as the bot; keep its token-bearing client session active. Either way, use a new unencrypted room; the adapter accepts room invitations but does not implement Matrix E2EE. |

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

## 6. Home automation hardware

Read this before plugging radios in. The Console's Home automation
screen (choose → readiness → setup → status) checks what it can see on
the machine; this section is what it cannot see. To prove a setup on
real hardware, follow [the hardware runbook](../testing/home-automation/README.md).

### What Wildstone runs, and what it does not

Wildstone runs **Home Assistant Container** (the `home-assistant`
package) next to the servers Home Assistant OS would run as add-ons,
each as its own package: `mqtt-mosquitto` (MQTT broker),
`zigbee-zigbee2mqtt`, `zwave-zwave-js-ui`, `matter-matterjs` (Matter
server) and `thread-otbr` (OpenThread Border Router). If you already run
Home Assistant elsewhere, choose *Connect existing Home Assistant* in
the Console instead of *Run it here*; Matter and Thread are then not
offered, because their servers only answer on the same machine as Home
Assistant (127.0.0.1).

**Home Assistant OS is not what this is.** HAOS is an operating system
that owns the whole machine; Wildstone is a guest on a Linux host you
own. The boundary, plainly:

| | Home Assistant OS | Wildstone |
|---|---|---|
| The host (kernel, sysctls, firewall, BlueZ, USB) | HAOS tunes it for you | **You** prepare it. Wildstone's readiness check reports what is missing and never changes it |
| Add-ons and the Supervisor | Yes | No Supervisor and no add-on store. The servers above are Wildstone packages; anything else is a package or nothing |
| Backups | Home Assistant's own, including add-ons | Wildstone's restic plans, one per package ([recovery.md](recovery.md#home-automation-controllers)). Home Assistant's own backup page covers only Home Assistant's `/config`, not the controllers |
| Updates | Supervisor | A new package version, through a plan you approve |
| Upstream support | Full | Home Assistant Container is supported upstream. The Matter server and OTBR images are supported upstream on HAOS only; on a general host they are "as-is" |

### Supported machines

- **CPU:** 64-bit x86 (`amd64`) or 64-bit ARM (`arm64`, such as a
  Raspberry Pi 4 or 5 with a **64-bit** OS). Wildstone's own binaries
  are built for those two only, and Home Assistant and the Matter server
  publish no 32-bit image. A 32-bit Raspberry Pi OS does not work.
- **OS:** a current Debian or Ubuntu with systemd and Docker. IPv6 must
  be enabled: Matter and Thread need it.
- **A VM is fine.** Pass each USB radio through to the VM as a USB
  device (Proxmox: VM → Hardware → Add → USB Device → *Use USB
  Vendor/Device ID*), and bridge the VM onto the household LAN, not
  behind NAT: mDNS, IPv6 router advertisements and Thread routes must
  reach it.

### One radio per protocol

- **One coordinator per Zigbee network, one controller per Z-Wave
  network, one RCP per Thread border router.** Two sticks are two
  networks with two sets of keys.
- **One protocol per radio.** A stick runs Zigbee firmware **or**
  OpenThread RCP firmware, not both. Multiprotocol (Zigbee and Thread
  on one stick) is not supported: upstream has deprecated it, and it
  shares one radio's time between two meshes. A Home Assistant Connect
  ZBT-1 or a Sonoff ZBDongle-E can be either; buy two if you want both.
- Wildstone refuses a second package that claims a radio another package
  already owns, whichever path names it (`/dev/serial/by-id/...` or
  `/dev/ttyUSB0`), before any plan exists.

### Name radios by their by-id path

Always give a package the radio's `/dev/serial/by-id/...` name, never
`/dev/ttyUSB0` or `/dev/ttyACM0`. The numbered names follow plug order
and can swap on reboot; the by-id name contains the stick's model and
serial number and stays put. List them with `ls -l /dev/serial/by-id/`,
or read the node's radio inventory (the Console shows it; the API is
`GET /api/v1/nodes/<node>/probes/radios`). A replacement stick has a
different serial number, so a different by-id name: replacing a stick
is always a param change too.

### USB placement and interference

Zigbee, Thread, Wi-Fi and Bluetooth all share 2.4 GHz, and USB 3 ports,
USB 3 drives and SSD enclosures radiate noise right across it.

- Put every stick on a **USB 2.0 extension cable** (about 1 m), away
  from the computer's case, USB 3 ports and disks. Do not plug a stick
  straight into the machine, and especially not into a USB 3 port
  beside a USB 3 drive.
- Keep the sticks apart from each other and from Wi-Fi access points
  (about 1 m is a good start).
- In a VM, the stick still sits next to the physical host: the same
  advice applies to the host's ports.
- Metal cases, racks and cupboards block radio. A stick on an extension
  outside the cabinet often fixes a "flaky mesh".

### Channel planning

Zigbee and Thread both use IEEE 802.15.4 channels 11 to 26; 2.4 GHz
Wi-Fi uses channels 1, 6 and 11. They overlap roughly like this:

| Wi-Fi channel | Overlaps 802.15.4 channels | Clear 802.15.4 channels nearby |
|---|---|---|
| 1 | 11 – 14 | 15 |
| 6 | 16 – 19 | 20 |
| 11 | 21 – 24 | 25, 26 |

- Find the Wi-Fi channels in use (yours and the neighbours') first, and
  fix your access points on 1, 6 or 11 rather than "auto".
- Give Zigbee and Thread **different** clear channels: for example
  Zigbee **15** and Thread **25** with Wi-Fi on 6 and 11, or Zigbee 25
  and Thread 15 with Wi-Fi on 1 and 6. Channel 26 is weaker on some
  devices and missing on a few; avoid it for Zigbee.
- Choose before pairing. Zigbee2MQTT fixes its channel in its
  `configuration.yaml` on first start, and changing it later can mean
  re-pairing devices. Thread's channel is part of the dataset Home
  Assistant creates for the border router.

### Network coordinators

`zigbee-zigbee2mqtt` also accepts a network coordinator
(`coordinator_url: tcp://<address>:<port>`, such as an SLZB-06 or an
ESPHome serial bridge) instead of a USB stick.

- **For:** it can sit in the middle of the house on PoE, far from the
  server and its USB noise, and the server can be a VM anywhere.
- **Against:** the serial link now depends on the network. A dropped
  link or a switch reboot disconnects Zigbee2MQTT until it reconnects;
  the TCP serial port has no password, so keep it on a network only
  your own devices reach; and its firmware is one more thing to update.
- Z-Wave and Thread are USB-only here. `zwave-zwave-js-ui` takes a
  stick on the machine; `thread-otbr` refuses a network RCP because the
  added latency breaks Thread's radio timing.

### Host preparation for Thread and Matter

The readiness check (Console, or
`GET /api/v1/nodes/<node>/probes/home-automation`) reports these;
Wildstone never sets them. For **Thread**, on the machine with the RCP
(replace `<NIC>` with the LAN interface from `ip route show default`):

```sh
printf 'net.ipv6.conf.all.forwarding = 1\nnet.ipv6.conf.<NIC>.accept_ra = 2\nnet.ipv6.conf.<NIC>.accept_ra_rt_info_max_plen = 64\n' \
  | sudo tee /etc/sysctl.d/60-wildstone-otbr.conf
sudo sysctl --system
sudo modprobe ip6table_filter
echo ip6table_filter | sudo tee /etc/modules-load.d/wildstone-otbr.conf
```

`accept_ra = 2` keeps the machine's own IPv6 address and default route
once forwarding is on (with forwarding on, the default `1` ignores
router advertisements), and `accept_ra_rt_info_max_plen = 64` accepts
the routes to Thread networks that other border routers advertise.
`ip6table_filter` is needed for the border router's firewall (the
default, `firewall=on`). For **Matter**, allow UDP 5353 (mDNS) and UDP
5540 in from the LAN, and keep any stateful firewall's UDP timeout
longer than your devices' report interval.

### Thread credentials on phones

To commission a Matter-over-Thread device with a phone onto *this*
Thread network, the phone needs its credentials. Once Home Assistant
has the OpenThread Border Router integration and that network is
preferred (Settings → Thread), use the companion app's *Sync Thread
credentials* (in the app's own settings; where it sits differs between
iOS, Android and app versions), then commission from Settings → Devices
& services → Add device → Matter in the app. Without the sync, the
phone may put the device on an Apple or Google Thread network instead.
Sync again after you restore or change the network.

### Migrating from an existing Home Assistant

Move one thing at a time, and **never run the old and the new copy of
a controller at once**: two servers with the same keys fight over the
same devices. The pattern for each piece is the one in
[recovery.md](recovery.md#restore-order-for-a-replacement-node):
install the package, disable it (its volume stays), copy the old data
into the volume, move the stick across, enable it.

1. **Take a full backup of the old installation** and keep it until
   everything works. On HAOS, download a full backup from Settings →
   System → Backups; it is a tar file with one archive per add-on.
2. **Broker.** Install `mqtt-mosquitto`. There is nothing to copy: it
   issues new passwords, and every Wildstone consumer is given its own.
   Home Assistant's is re-entered in step 7.
3. **Zigbee2MQTT.** Copy the old data directory (`configuration.yaml`,
   `database.db`, `coordinator_backup.json`, `state.json`) into
   `zigbee2mqtt-data`, move the stick, and enable with the stick's by-id
   path and the right `coordinator_adapter`. The MQTT settings in the
   old `configuration.yaml` are overridden by Wildstone's environment.
   Coming from **ZHA**, there is no conversion: either keep ZHA (set
   `home-assistant`'s `serial_device` to the stick) or re-pair the
   devices in Zigbee2MQTT.
4. **Z-Wave.** From Z-Wave JS UI, copy its whole `store` into
   `zwave-js-ui-store`. From the official Z-Wave JS add-on, copy its
   S0 and S2 keys (add-on configuration) into the new Z-Wave JS UI
   before including anything; the procedure is in the
   `zwave-zwave-js-ui` package header. Move the stick: the network is on
   it.
5. **Thread.** Copy the OTBR add-on's Thread settings (its
   `/data/thread`) into `otbr-data`, or let Home Assistant hand its
   preferred dataset to the new, empty border router (step 7). Remove
   the old border router from Home Assistant once the new one is
   preferred. The RCP stick needs OpenThread RCP firmware either way.
6. **Matter.** Copy the old Matter server's storage into
   `matter-data` and `chown -R 1000:1000` it. The Matter.js server is
   upstream's drop-in replacement for the Python Matter Server; confirm
   afterwards that the `compressed_fabric_id` is the same as before. If
   it is not, stop: the devices are on the old fabric.
7. **Home Assistant last.** Copy the old `/config` (from a HAOS backup,
   the `homeassistant.tar.gz` inside it) into `home-assistant-config`
   and enable `home-assistant`. Then, in Home Assistant, point each
   integration at its new server: MQTT at the broker's LAN address with
   user `home-assistant` and the password
   `docker exec home-assistant printenv WILDSTONE_MQTT_PASSWORD` shows;
   Z-Wave JS at `ws://<listen_addr>:3000`; Matter at
   `ws://127.0.0.1:5580/ws`; OpenThread Border Router at
   `http://127.0.0.1:8081`. Remove what only made sense under HAOS
   (the Supervisor integration, add-on update entities).
   If the Wildstone URL answers 400 "not set-up for reverse proxies",
   the copied config does not trust the proxy: stop Home Assistant and
   make `trusted_proxies` in `/config/.storage/http` (or the `http:`
   block of `configuration.yaml`, on an older config) include
   `172.16.0.0/12` and `127.0.0.1/32`, with `use_x_forwarded_for: true`.
8. **Turn the old installation off** for good, and keep its backup.
