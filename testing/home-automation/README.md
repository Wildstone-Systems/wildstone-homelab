# Home automation: real-hardware test runbook

This is the hands-on test for Wildstone's home-automation packages on a
real machine with real radios (#453, part of #431). Nothing here runs in
CI: it needs a VM on your LAN, three USB sticks and a phone. It is
written so you can follow it without reading any Wildstone source code.

**What you will prove:** each package installs and works with its real
radio; the Console's guided setup works end to end; everything survives
restarts and reboots; each controller's backup really brings its
network back; and Wildstone refuses to hand one stick to two programs.

**Earlier runs:** [results/](results/) (redacted reports, one per run).

**How long:** plan two or three sessions. Sections 0 and 1 take an
evening; section 2 another; sections 3 to 6 a third. Section 2.2 step
10 needs the machine left running overnight.

## How to read this

- Each step has a number, like **2.3.4**. Section 7 is a checklist of
  those numbers. Fill it in as you go and paste it back at the end.
- **On the VM:** run the commands in a terminal on the test VM (SSH in).
- **On the Proxmox host:** run them in the Proxmox shell (web UI → your
  node → Shell).
- **From your laptop:** another computer on the same home network.
- **In the browser** / **In Home Assistant** / **On your phone:** click
  through what is described.
- **Expect:** what you should see. If you see something else, note it
  next to the step number (copy the exact text), mark the step FAIL, and
  carry on unless the step says to stop.
- **Paste back:** what to copy into your report for that step.
- Steps marked **(changes the VM)** change something on the machine on
  purpose. Steps marked **(optional)** can be skipped: mark them SKIP.
- `sudo ./vm-check.sh ...` is a helper in this folder that only
  **reads**. It never changes anything. It hides USB serial numbers and
  MAC addresses so its output is safe to paste back. It still shows your
  private LAN addresses: fine to send in our chat, but replace them
  (with 192.0.2.x) before posting anything in a public GitHub issue.
- Never paste a password, token, network key or the contents of
  `settings.json`, `configuration.yaml` or a Thread dataset. The helper
  prints *hashes* of keys (a short fingerprint like `0532f3f87b7d91b7`):
  those are safe and are what we compare.

Words used below:

| Word | Meaning |
|---|---|
| **by-id path** | The stable name of a USB stick, like `/dev/serial/by-id/usb-Vendor_Model_SERIAL-if00-port0`. It never changes when you reboot or replug. |
| **plan** | Every change Wildstone makes is first written up as a plan that you approve (Console → Plans, or `wild plan approve <id>`). |
| **capability / package** | A service Wildstone installs, like `zigbee-zigbee2mqtt`. "Enable" installs it, "disable" removes it but **keeps its data**. |
| **coordinator / controller / RCP** | The USB stick that runs a Zigbee network / a Z-Wave network / a Thread border router. |

---

## 0. Before you start

### 0.1 What you need

- The test VM: Debian 12 or Ubuntu 24.04, 64-bit, with Wildstone
  already installed from an earlier release (Keystone and the node agent
  on the same VM, as the interactive installer sets it up), Docker
  running, and the VM's network **bridged** onto your home LAN (Proxmox
  `vmbr0`), not behind NAT.
- Your three sticks: a **Zigbee coordinator**, a **Thread RCP** (a stick
  you will flash, or have flashed, with OpenThread RCP firmware) and a
  **Z-Wave stick**. A USB 2.0 extension cable for each if you have them.
- At least one Zigbee device to pair (a bulb or a button), one Z-Wave
  device (ideally one that pairs securely, like a lock or a recent
  switch), one **Matter-over-Wi-Fi** device (such as a smart plug with a
  Matter logo that says Wi-Fi) and one **Matter-over-Thread** device
  (says Thread on the box).
- A phone with the **Home Assistant companion app** installed, Bluetooth
  on, on the home Wi-Fi.
- A laptop on the home network (for the "From your laptop" steps).
- Optional: an ESP32 board (for the Bluetooth proxy test in 2.6). You do
  **not** need a Bluetooth adapter on the VM.
- Your Wildstone admin token (starts `wst_`). If it is lost, see
  "If the admin token is lost" in [docs/install.md](../../docs/install.md).

### 0.2 Pass the three sticks through to the VM

**On the Proxmox host** (plug all three sticks into the Proxmox machine
first, each on its extension cable, away from the case and from USB 3
ports and disks):

1. Run:

   ```sh
   lsusb
   ```

   **Expect:** one line per stick, like
   `Bus 001 Device 005: ID 10c4:ea60 Silicon Labs CP210x UART Bridge`.
   The `10c4:ea60` part is the stick's vendor:product ID. Write down which
   line is which stick (unplug one and run `lsusb` again if unsure).

2. In the Proxmox web UI: select the VM → **Hardware** → **Add** →
   **USB Device**. Choose **Use USB Vendor/Device ID** and pick the first
   stick. Repeat for the other two.

   **Two sticks with the same ID?** (Common: many sticks use the same
   Silicon Labs or CH340 chip.) Then for those two choose **Use USB
   Port** instead, and pick the port each is plugged into. Do not move
   them to other ports afterwards.

   Command-line alternative (replace `<VMID>` and the IDs/ports):

   ```sh
   qm set <VMID> -usb0 host=10c4:ea60     # by vendor:product ID
   qm set <VMID> -usb1 host=1-1.2         # by port (bus-port, from: lsusb -t)
   ```

3. If the sticks do not show up in the VM within a minute, shut the VM
   down from Proxmox and start it again (a reboot inside the VM is not
   always enough).

4. Proxmox firewall: if the VM's network device has **Firewall** ticked
   (VM → Hardware → Network Device), multicast (mDNS) may be dropped.
   For this test, untick it or make sure the VM's firewall rules allow
   UDP 5353 and IPv6.

**On the VM:**

5. **0.2.5** Check the sticks arrived:

   ```sh
   ls -l /dev/serial/by-id/
   ```

   **Expect:** three entries, each pointing to `../../ttyUSB0`,
   `../../ttyACM0` or similar. These by-id names are what every step
   below uses.

   **Paste back:** `sudo ./vm-check.sh radios` (run it from this folder
   once you have the repository in step 0.3).

### 0.3 Build this branch on the VM

Wildstone releases are built by GitHub when a version is tagged. To test
work that is not released yet, you build the same files on the VM and
point the installer at them. The helper `build-dist.sh` does it with
Docker only (no Go or Node to install).

**On the VM** (as your normal user, not root):

1. **0.3.1** Get the code. The repository is private, so use the GitHub
   CLI (`sudo apt install gh`, then `gh auth login`) or a token:

   ```sh
   cd ~
   gh repo clone Wildstone-Systems/wildstone-homelab wildstone-homelab   # first time only
   cd ~/wildstone-homelab
   git fetch origin
   git checkout claude/dreamy-einstein-9c5ory        # the branch under test
   git pull
   git log --oneline -1
   ```

   **Paste back:** the `git log` line (the commit you are testing).

2. **0.3.2** Build:

   ```sh
   cd ~/wildstone-homelab
   testing/home-automation/build-dist.sh
   ```

   It takes 5 to 10 minutes the first time (it downloads the Go and Node
   images). **Expect** it to finish with
   `[build-dist] done: /home/<you>/wildstone-homelab/dist (v0.0.0-test-<commit>, linux/amd64)`
   and a list including `keystone_linux_amd64.tar.gz`,
   `wardstoned_linux_amd64.tar.gz`, `wild_linux_amd64.tar.gz`,
   `console_dist.tar.gz`, `packages.tar.gz` and `checksums.txt`.

### 0.4 Record what is running before the upgrade

So we can prove the upgrade does not restart your existing services
(#432 step 1).

```sh
sudo docker ps --format '{{.Names}}' | sort | while read -r c; do
  sudo docker inspect -f '{{.Name}} {{.Id}} {{.HostConfig.NetworkMode}} {{index .Config.Labels "dev.wildstone.spec-hash"}}' "$c"
done > ~/before-upgrade.txt
wc -l ~/before-upgrade.txt
```

### 0.5 Upgrade the node agent, then Keystone (changes the VM)

The node agent (`wardstoned`) goes first: the new Keystone expects the
new agent's features (protocol minors 23 to 27: host networking, the
host-gateway alias, the radio inventory, the readiness probe, and the
`/run/dbus` mount).

```sh
cd ~/wildstone-homelab/dist
# 1. the node agent, keeping its existing join (address, name, CA pin)
sudo bash -c '. /etc/wildstone/wardstoned.env && bash ./install.sh --role node \
  --artifact-dir "$PWD" --skip-docker \
  --keystone "$KEYSTONE_ADDR" --node-id "$NODE_ID" --ca-fingerprint "$CA_FINGERPRINT" \
  --join-token "${JOIN_TOKEN:-already-joined}"'
# 2. Keystone, the CLI, the Console and the package catalog
sudo bash ./install.sh --role keystone --artifact-dir "$PWD"
```

The join token is only read the very first time a node starts; an
already-joined node keeps its own identity, so the old (used) token or
the placeholder `already-joined` is fine here.

**Expect:** each run ends with a line like
`wardstoned.service enabled, (re)started, and healthy after 5s` /
`keystone.service enabled, (re)started, and healthy after 5s`, and
`using local artifact .../keystone_linux_amd64.tar.gz` lines (proof it
used your build, not a download).

### 0.6 Set up your test shell

Install the tools the steps use (changes the VM):

```sh
sudo apt update
sudo apt install -y jq curl python3 usbutils mosquitto-clients avahi-utils pipx
```

Then create a small file with the values every step uses. Paste the
block as it is, then open the file (`nano ~/ha-test.env`) and replace
the three parts in `<...>`:

```sh
cat > ~/ha-test.env <<'EOF'
export WILD_TOKEN='<your wst_ admin token>'
export NODE='<the NODE name from: wild node list>'
export DOMAIN='<your services domain: the part after "hass." in your Home Assistant link>'
export ADMIN_DOMAIN="$DOMAIN"   # change it if your admin services use a different domain
export NIC="$(ip route show default | awk '{print $5; exit}')"
export LAN_IP="$(ip -4 -o addr show "$NIC" | awk '{print $4}' | cut -d/ -f1 | head -1)"
export K=http://127.0.0.1:9441
api()     { curl -sS -H "Authorization: Bearer $WILD_TOKEN" "$K$1" | jq .; }
apipost() { curl -sS -H "Authorization: Bearer $WILD_TOKEN" -H 'Content-Type: application/json' -d "$2" "$K$1" | jq .; }
check()   { sudo --preserve-env=WILD_TOKEN,NODE ~/wildstone-homelab/testing/home-automation/vm-check.sh "$@"; }
ready()   { curl -sS -H "Authorization: Bearer $WILD_TOKEN" "$K/api/v1/nodes/$NODE/probes/home-automation" \
              | jq -r '.checks[] | [.check, .state, .subject, (.value // ""), (.detail // "")] | @tsv'; }
EOF
chmod 600 ~/ha-test.env
. ~/ha-test.env
echo "node=$NODE nic=$NIC lan_ip=$LAN_IP"
```

Run `. ~/ha-test.env` again after editing it and in every new terminal,
and check the `echo` line shows your node, interface and address. From now on,
`check <section>` runs the read-only helper, and `ready` prints the
readiness checks as a table (check, state, what it looked at, value).

Also add the three by-id paths (copy them from `ls /dev/serial/by-id/`)
to the end of `~/ha-test.env`, then re-run `. ~/ha-test.env`:

```sh
export ZIGBEE=/dev/serial/by-id/<your Zigbee stick>
export ZWAVE=/dev/serial/by-id/<your Z-Wave stick>
export THREAD=/dev/serial/by-id/<your Thread stick>
```

### 0.7 Check the upgrade

1. **0.7.1** `wild node list`

   **Expect:** your node `connected`, AGENT `v0.0.0-test-<commit>`,
   PROTOCOL `minor 27`.

2. **0.7.2** No churn (#432 step 1): run the command from 0.4 again into
   `~/after-upgrade.txt`, then `diff ~/before-upgrade.txt ~/after-upgrade.txt`.

   **Expect:** no difference for any container that existed before
   (same ID and spec-hash: nothing was re-created by the upgrade), and
   every container's network is still what it was (not `host`). A
   container that is new or re-created is a FAIL unless its own package
   changed version in this branch; note which.

3. **0.7.3** `check host radios`

   **Paste back:** the whole output.

### 0.8 (optional) Run the container tests that need a real network

Three automated tests could not run where they were written (no IPv6,
or the registry was blocked). They need Go on the VM:

```sh
cd ~ && curl -fsSLO https://go.dev/dl/go1.25.1.linux-amd64.tar.gz
mkdir -p ~/go-sdk && tar -C ~/go-sdk -xzf go1.25.1.linux-amd64.tar.gz
export PATH="$HOME/go-sdk/go/bin:$PATH"
cd ~/wildstone-homelab/keystone
sudo env PATH="$PATH" WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run 'TestSecuredMosquitto' -v -count=1
sudo env PATH="$PATH" WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run 'TestMatterServerServesHomeAssistant' -v -count=1
```

1. **0.8.1** Mosquitto: **Expect** `PASS` (anonymous refused, wrong
   password refused, the right password accepted from host network and
   from the bridge, one client's password refused for another).
2. **0.8.2** Matter: **Expect** `PASS`, and the log must **not** say
   "NOT the pinned image" or "test shim". Then
   `sudo docker image inspect ghcr.io/matter-js/matterjs-server@sha256:54232d0d3e7dff5a54759469d2753399270412b4c30c55b31750a4595e4cb236 --format '{{json .Config.Entrypoint}} {{.Config.User}} {{json .Config.Healthcheck}}'`
   should show `["node","--enable-source-maps",".../MatterServer.js"]`,
   `1000:1000` and a healthcheck.
3. **0.8.3** (optional, advanced) Thread with a simulated radio: build
   OpenThread's simulated RCP and run the dataset test:

   ```sh
   sudo apt install -y cmake ninja-build build-essential git
   cd ~ && git clone --depth 1 https://github.com/openthread/openthread && cd openthread
   git submodule update --init --depth 1 third_party/mbedtls/repo && (cd third_party/mbedtls/repo && git submodule update --init --depth 1)
   mkdir b && cd b && cmake -GNinja -DOT_PLATFORM=simulation -DOT_COMPILE_WARNING_AS_ERROR=OFF -DOT_APP_CLI=OFF -DOT_APP_NCP=OFF -DOT_APP_RCP=ON -DOT_FTD=OFF -DOT_MTD=OFF -DOT_RCP=ON -DCMAKE_EXE_LINKER_FLAGS=-static -DCMAKE_BUILD_TYPE=Release .. && ninja ot-rcp
   cd ~/wildstone-homelab/keystone
   sudo env PATH="$PATH" WILDSTONE_DOCKER_TESTS=1 WILDSTONE_OTBR_RCP_SIM="$HOME/openthread/b/examples/apps/ncp/ot-rcp" go test ./pkgtool/ -run 'TestOTBR' -v -count=1
   ```

   **Expect:** `TestOTBRRestartKeepsTheThreadDataset` PASS. It has never
   run before: paste back any failure exactly.

**Paste back:** the last 20 lines of each test run.

---

## 1. Software checks (no radio needed yet)

The sticks stay plugged in; nothing uses them until section 2.

### 1.1 The radio inventory (#434)

1. **1.1.1** `api /api/v1/nodes/$NODE/probes/radios`

   **Expect:** `"platformSupported": true` and one entry per stick. For
   each: `devPath` like `/dev/ttyUSB0` or `/dev/ttyACM0`; `byIdPaths`
   matching `ls -l /dev/serial/by-id/`; `usb.vendorId`/`productId`
   matching `lsusb`; a `driver` (`cp210x`, `ch341`, `ftdi_sio` or
   `cdc_acm`); an `id` starting `serial-by-id:`. `kind` is `zigbee`,
   `zwave` or `thread` where the stick is recognised, `unknown` for a
   generic USB-serial chip (expected for many sticks). (On a VM with
   no sticks passed through, the same call returns `"radios": []`.)
2. **1.1.2** Run it twice. **Expect:** the same order both times (sorted by `id`).
3. **1.1.3** Unplug one stick and plug it into a **different** USB port on
   the Proxmox host (only if it was passed through by Vendor/Device ID;
   skip for "by port" sticks). Run 1.1.1 again. **Expect:** its `id` is
   unchanged; `usb.port` changes; `kernelName` may change.
4. **1.1.4** Look for `[REDACTED]` in the output. **Expect:** none. If a
   long mixed-case by-id name comes back as `[REDACTED]`, that is a bug
   (the API's secret-hiding rule is matching it): note which field.
5. **1.1.5** `sudo journalctl -u wardstoned --since -10min | grep -c 'arm=ListRadios'` and
   `sudo grep -c '"ListRadios"' /var/lib/wildstone/keystone/audit.log`.
   **Expect:** both above 0.
6. **1.1.6** (optional, needs a second node with an older agent) The
   same call against an agent below protocol minor 25 answers 400
   "unknown operation — agent too old"; the readiness probe (1.2) needs
   minor 26, and Home Assistant's `bluetooth=host-bluez` minor 27.

**Paste back:** `check api` (the radios part) and any FAILs.

### 1.2 The readiness probe (#436)

Endpoint: `api /api/v1/nodes/$NODE/probes/home-automation` (full
JSON), or `ready` for the checks as a table.

1. **1.2.1** Baseline. **Expect:** `interfaces` lists only your LAN NIC
   (`defaultRoute: true`), not `docker0` or `veth*`; `ipv6` ready (value
   `0`); `interface_ipv6` ready with the NIC's IPv6 addresses (a
   link-local `fe80::` one counts); `ipv6_forwarding` matches
   `sysctl net.ipv6.conf.all.forwarding` (`1` ready, `0` missing);
   `multicast` ready; `dbus_system_bus` ready; `tun` ready
   (`char 10:200`); `bluetooth_adapter` missing (no adapter: expected).
   `bluez_service` is ready if BlueZ is installed and running, else
   missing.
2. **1.2.2** The probe never changes anything:

   ```sh
   sudo sysctl -a 2>/dev/null | sort > /tmp/sysctl-before
   api /api/v1/nodes/$NODE/probes/home-automation > /dev/null
   sudo sysctl -a 2>/dev/null | sort > /tmp/sysctl-after
   diff /tmp/sysctl-before /tmp/sysctl-after | grep -v -E 'random|entropy|dentry|inode|file-nr|ns_last_pid|uuid|stat|quota' | grep -v -E '^(---|[0-9,]+c[0-9,]+)$' | head
   ```

   **Expect:** no lines (only ever-changing counters, which the grep hides).
3. **1.2.3** (changes the VM, then undoes it) IPv6 off:

   ```sh
   sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1
   ready | grep ipv6
   sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0
   ```

   **Expect:** `ipv6` missing and `interface_ipv6` missing
   (`disable_ipv6=1`). After the last line, both ready again (re-run
   the probe to confirm; the NIC may need a few seconds to get its IPv6
   address back).
4. **1.2.4** (changes the VM, then undoes it) Multicast off:
   `sudo ip link set $NIC multicast off`, `ready | grep multicast`, then
   `sudo ip link set $NIC multicast on`. **Expect:** `multicast` missing,
   with the interface flags as its value; ready again after.
5. **1.2.5** (optional; only if BlueZ is installed) `sudo systemctl stop bluetooth`,
   probe, `sudo systemctl start bluetooth`. **Expect:** `bluez_service`
   missing ("installed but not running"). Without BlueZ installed it
   already says "not installed" (`load=not-found`).
6. **1.2.6** (optional) `tun`: on a kernel where `tun` is a module and
   nothing uses it, `sudo modprobe -r tun` makes `tun` missing;
   `sudo modprobe tun` restores it. Skip if `modprobe -r` refuses.
7. **1.2.7** (optional) Booting with the kernel argument `ipv6.disable=1`
   makes `ipv6`, `ipv6_forwarding` and `interface_ipv6` `unsupported`.
   Only if you are comfortable editing GRUB; otherwise SKIP.
8. **1.2.8** `sudo journalctl -u wardstoned --since -15min | grep -c 'arm=HomeAutomationReadiness'`.
   **Expect:** above 0.

**Paste back:** the baseline probe output (1.2.1) and any FAILs.

### 1.3 The home-automation status API (#446)

1. **1.3.1** `api "/api/v1/home-automation?topology=managed&select=zigbee,matter"`

   **Expect:** `nodes[0].readiness.probe` = `ok`; `radioInventory` lists
   your sticks with `suitableFor`; `mqtt` = `ready_to_propose` (Zigbee
   needs it); `zigbee` blocked (`dependency_not_ready`) until the broker
   exists.
2. **1.3.2** (changes the VM, then undoes it) `sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1`,
   repeat with `select=matter`. **Expect:** matter blocked with
   `ipv6_missing` and a manual-step instruction. Undo with `=0`, repeat:
   the blocker clears.
3. **1.3.3** (changes the VM, then undoes it) Agent offline: note the
   newest plan first (`api /api/v1/plans | jq -r '.[0].id'`; the list
   stops at 50, so its length proves nothing), then
   `sudo systemctl stop wardstoned`, repeat 1.3.1.
   **Expect:** `node_offline` blockers; the newest plan is still the same
   one (nothing was proposed). Then `sudo systemctl start wardstoned`.

### 1.4 The MQTT broker (#438)

1. **1.4.1** Refused addresses (nothing is proposed):

   ```sh
   for a in 0.0.0.0 127.0.0.1 8.8.8.8; do
     wild capability enable mqtt-mosquitto -set node=$NODE -set listen_addr=$a -dry-run 2>&1 | tail -2
   done
   ```

   **Expect:** each refused with a reason (every-interface, loopback, not private).
2. **1.4.2** (changes the VM) Install:

   ```sh
   wild capability enable mqtt-mosquitto -set node=$NODE -set listen_addr=$LAN_IP
   wild plan list | head -5
   wild plan show <the new plan id>
   ```

   **Expect** in the plan: three generated Secrets (`mqtt-zigbee`,
   `mqtt-cameras`, `mqtt-home-assistant`), ConfigFiles `passwd` and
   `mosquitto.conf`, a Container publishing `<LAN_IP>:1883:1883`. No
   password anywhere in the text. Approve it: `wild plan approve <id>`.
3. **1.4.3** On the VM:

   ```sh
   sudo awk -F: '{print $1, substr($2,1,7)}' /etc/wildstone/mqtt/passwd
   sudo stat -c '%a %U' /etc/wildstone/mqtt/passwd
   ss -ltn | grep 1883
   sudo docker logs mosquitto 2>&1 | tail -5
   ```

   **Expect:** three users each with `$7$101`; mode `600 root`; the
   listener on `<LAN_IP>:1883` only (not `0.0.0.0`); Mosquitto started
   (a warning about running as root is expected).
4. **1.4.4** **From your laptop** (install `mosquitto-clients` there):
   `mosquitto_pub -h <LAN_IP> -t test -m x`. **Expect:** "not
   authorised" or connection refused (no anonymous access).
5. **1.4.5** (optional, needs Frigate) Frigate with MQTT: install
   `cameras-frigate` 2.0.0 with `mqtt_enabled=false` first (it installs
   with no broker; `/etc/wildstone/cameras/config.yml` says
   `mqtt: enabled: false`), then `mqtt_enabled=true` on this node. **Expect:**
   the config has host `<LAN_IP>`, port 1883, user `cameras`, password
   `{FRIGATE_MQTT_PASSWORD}`; Frigate's log shows MQTT connected; and
   `mosquitto_sub -h <LAN_IP> -u home-assistant -P "$(sudo docker exec home-assistant printenv WILDSTONE_MQTT_PASSWORD)" -t 'frigate/#' -C 1 -v`
   (once Home Assistant exists, 1.5) prints `frigate/available online`.
   With a second node: Frigate with `mqtt_enabled=true` there is
   refused naming both nodes and `mqtt-cameras`; with
   `mqtt_enabled=false` it installs.

### 1.5 Home Assistant on the host network (#440)

1. **1.5.1** (optional) Upgrade from 1.x: only if Home Assistant 1.x
   is installed on this VM. Create a user and one automation in it
   first. Upgrade with `wild capability enable home-assistant -set node=$NODE -set public_domain=$DOMAIN`.
   **Expect:** the plan replaces Container/home-assistant (network
   change) and changes Route/hass to `host/8123`; SeedFile steps are
   no-ops. After approving: the same user and automation;
   `sudo docker inspect home-assistant -f '{{.HostConfig.NetworkMode}}'` = `host`;
   `wild backup run home-assistant` succeeds.
2. **1.5.2** (changes the VM) Port conflict, on a VM where Home
   Assistant is **not** yet running: in a second terminal run
   `python3 -m http.server 8123`. Then propose the install:

   ```sh
   wild capability enable home-assistant -set node=$NODE -set public_domain=$DOMAIN -set mqtt_enabled=true
   ```

   and approve it. **Expect:** the Container step fails with
   "already listens on 8123 (Route hass; already listening on 0.0.0.0)".
   (On an upgrade from 1.x, the old container is still running:
   `sudo docker ps`.) Stop the `http.server` (Ctrl-C). **Expect:** within
   a minute or two the next reconcile installs Home Assistant without a
   new plan (`wild status`).
3. **1.5.3** Open Home Assistant through Wildstone: the Home Assistant
   tile on the Wildstone dashboard (normally `https://hass.<DOMAIN>`).
   **Expect:** the onboarding page (create your user) or the login, and
   **not** "400: Bad Request" or "not set-up for reverse proxies".
   Create your Home Assistant user.
4. **1.5.4** Proxy trust: `check identity` (Home Assistant part).
   **Expect:** on a fresh install `trusted_proxies: ['172.16.0.0/12', '127.0.0.1/32']`;
   on an upgraded one the older, broader list. If your Docker uses
   address pools outside 172.16.0.0/12, 1.5.3 gives a 400: note it.
5. **1.5.5** **From your laptop:** open `http://<LAN_IP>:8123`.
   **Expect:** it answers (plain HTTP, same login). Then
   `curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-For: 1.2.3.4' http://<LAN_IP>:8123/`
   **Expect:** `400` on a fresh install (the LAN is not trusted to set
   that header).
6. **1.5.6** Discovery: if you have a Chromecast, Sonos, smart TV or
   printer, **In Home Assistant** Settings → Devices & services. **Expect:**
   it shows under "Discovered" within a few minutes.
7. **1.5.7** MQTT details:

   ```sh
   sudo docker exec home-assistant printenv | grep WILDSTONE_MQTT | sed 's/PASSWORD=.*/PASSWORD=(hidden)/'
   ```

   **Expect:** `WILDSTONE_MQTT_HOST=<LAN_IP>`, `WILDSTONE_MQTT_PORT=1883`,
   `WILDSTONE_MQTT_USERNAME=home-assistant`, and a password line. Read
   the password itself with
   `sudo docker exec home-assistant printenv WILDSTONE_MQTT_PASSWORD`
   (do not paste it anywhere). **In Home Assistant:** Settings →
   Devices & services → Add integration → MQTT, with those values.
   **Expect:** it connects. Try once with a wrong password first:
   **Expect** it is refused.
8. **1.5.8** Refusals (nothing is proposed):

   ```sh
   wild capability enable home-assistant -set node=$NODE -set public_domain=$DOMAIN -set serial_device=/dev/sda -dry-run 2>&1 | tail -2
   ```

   **Expect:** refused (not a radio). With a second node only (optional):
   `-set node=<other node> -set mqtt_enabled=true` is refused naming both
   nodes and `mqtt-home-assistant`.

**Paste back:** `check containers` (Home Assistant and Mosquitto parts) and any FAILs.

### 1.6 Host networking for any container (#432)

A throw-away test container (`whoami`, a tiny web server) moved onto the
host network and back. Create its definition:

```sh
mkdir -p ~/ha-test && cd ~/ha-test
cat > nettest.yaml <<EOF
apiVersion: wildstone.dev/v1
kind: Container
metadata: { name: nettest, owner: capability/nettest, node: $NODE }
spec:
  image: traefik/whoami@sha256:200689790a0a0ea48ca45992e0450bc26ccab5307375b41c84dfc4f2475937ab
  command: ["--port", "18080"]
  restart: unless-stopped
EOF
propose() { apipost /api/v1/plans "$(jq -n --arg s "$1" --arg d "$(cat "$2")" '{summary: $s, put: [$d]}')"; }
```

1. **1.6.1** (changes the VM) `propose "nettest on the bridge" nettest.yaml`, approve it.
   **Expect:** `sudo docker inspect nettest -f '{{.HostConfig.NetworkMode}}'`
   is not `host`, and it is attached to the `wildstone` network.
2. **1.6.2** (changes the VM) Add the line `  networkMode: host` under
   `spec:` in `nettest.yaml`, propose "nettest on host" again.
   **Expect:** the plan shows a Medium update "(host network)". Approve.
   `sudo docker inspect nettest -f '{{.HostConfig.NetworkMode}} {{json .NetworkSettings.Networks}}'`
   shows `host` and only `host`. **From your laptop:**
   `curl http://<LAN_IP>:18080` answers with whoami's text.
   `sudo journalctl -u wardstoned --since -5min | grep EnsureContainer | grep -c network_mode=host` is above 0.
3. **1.6.3** Add `  ports: ["18081:80"]` too and propose. **Expect:**
   refused (`spec.ports` with host networking); `nettest` keeps running
   with the same ID.
4. **1.6.4** (changes the VM) Remove both lines, propose, approve.
   **Expect:** back on the `wildstone` network.
5. **1.6.5** (optional) An agent older than minor 23 refuses
   `networkMode: host` with a message naming "protocol minor 23", the
   node and "upgrade wardstoned". Only testable with a second, older
   node: SKIP otherwise.
6. **1.6.6** Clean up: `apipost /api/v1/plans '{"summary":"remove nettest","delete":[{"kind":"Container","name":"nettest"}]}'`, approve.

### 1.7 Routes to services on the host (#433)

1. **1.7.1** In a second terminal: `python3 -m http.server 18123 --bind 0.0.0.0`.
2. **1.7.2** (changes the VM) A Route to it:

   ```sh
   cd ~/ha-test
   cat > hostroute.yaml <<EOF
   apiVersion: wildstone.dev/v1
   kind: Route
   metadata: { name: hostroute, owner: capability/nettest, node: $NODE }
   spec:
     host: hostroute.$ADMIN_DOMAIN
     upstream: host/18123
     auth: authelia_oidc
     classification: admin_tailscale
   EOF
   propose "route to a host service" hostroute.yaml
   ```

   Approve. **Expect:** the plan re-creates the ingress Caddy container
   once (and the dashboard container, if it is on this node).
3. **1.7.3** Checks:

   ```sh
   sudo docker ps --format '{{.Names}}' | grep -i caddy
   sudo docker inspect <the caddy container> --format '{{.HostConfig.ExtraHosts}}'
   sudo grep -r 'host.docker.internal:18123' /etc/wildstone/ 2>/dev/null | head -3
   ```

   **Expect:** `host.docker.internal:host-gateway` in ExtraHosts; a
   `reverse_proxy host.docker.internal:18123` line; no `172.x` address
   in the route file.
4. **1.7.4** Browse `https://hostroute.<ADMIN_DOMAIN>` (after the household
   sign-in). **Expect:** the Python directory listing. `wild status`
   later shows nothing left to do (converged).
5. **1.7.5** (optional, needs the dashboard) The dashboard card for a
   host-network package (Home Assistant) shows a green dot.
6. **1.7.6** Stop the Python server (Ctrl-C) and change something
   harmless in the Route (for example `auth: none`), propose, approve.
   **Expect:** the apply fails its gate (the probe of
   `http://127.0.0.1:18123/` fails).
7. **1.7.7** Bad upstreams are refused: try `upstream: host/0`,
   `upstream: host/localhost:80` and `upstream: host:8123`. **Expect:**
   each refused on `spec.upstream`.
8. **1.7.8** Firewall note: if Caddy answers 502 while the gate passes,
   the host firewall is dropping traffic from Docker's bridge to the
   port (or the service listens on 127.0.0.1 only). Note it if you see it.
9. **1.7.9** Clean up: delete the Route (`{"kind":"Route","name":"hostroute"}`) as in 1.6.6.

---

## 2. One radio at a time

### 2.1 Zigbee (#439, #440)

Needs 1.4 (broker) and 1.5 (Home Assistant with `mqtt_enabled=true`).
Pick the adapter type for your stick: `zstack` (Texas Instruments:
Sonoff ZBDongle-P, SLZB-06), `ember` (Silicon Labs: Sonoff ZBDongle-E,
SLZB-06M, Home Assistant Connect ZBT-1 with Zigbee firmware), `deconz`
(ConBee).

1. **2.1.1** (optional) Upgrade from 1.x: only if Zigbee2MQTT 1.x is
   installed. Before upgrading note
   `sudo docker volume inspect zigbee2mqtt-data -f '{{.CreatedAt}}'` and
   `check identity` (Zigbee part). Upgrade with the same `usb_device`
   and adapter. **Expect:** the plan has only the Container update (no
   Volume create/delete); afterwards the same volume creation time, the
   same network-key fingerprint and PAN ID, paired devices still in the
   Zigbee2MQTT page.
2. **2.1.2** (changes the VM) Install:

   ```sh
   wild capability enable zigbee-zigbee2mqtt -set node=$NODE -set admin_domain=$ADMIN_DOMAIN \
     -set usb_device=$ZIGBEE -set coordinator_adapter=<zstack|ember|deconz>
   ```

   **Expect:** the plan does **not** contain a literal password or your
   stick's by-id name in any env value. Approve it.
3. **2.1.3** Checks:

   ```sh
   sudo docker logs zigbee2mqtt 2>&1 | grep -i -E 'mqtt|error' | tail -5
   sudo docker inspect zigbee2mqtt --format '{{json .HostConfig.Devices}} {{index .Config.Labels "dev.wildstone.device-sources"}}'
   sudo docker inspect zigbee2mqtt --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -E 'MQTT_(SERVER|USER)|SERIAL'
   ```

   **Expect:** "Connected to MQTT server" and no "Not authorized"; the
   device mapping from the node your by-id path points at today (the
   `/dev/ttyUSBn` in `ls -l /dev/serial/by-id/`: the agent hands Docker
   the resolved node) to `/dev/zigbee`, and the by-id path itself in the
   `dev.wildstone.device-sources` label;
   `ZIGBEE2MQTT_CONFIG_MQTT_SERVER=mqtt://<LAN_IP>:1883`, `USER=zigbee`,
   `SERIAL_PORT=/dev/zigbee`.
4. **2.1.4** No container-name addresses: `sudo grep -r 'mosquitto:1883' /etc/wildstone/ ; echo exit=$?`.
   **Expect:** no matches (`exit=1`).
5. **2.1.5** Open `https://zigbee.<ADMIN_DOMAIN>` (household sign-in first).
   Click **Permit join**, pair your Zigbee device. **Expect:** it appears
   in Zigbee2MQTT and, within a minute, in Home Assistant (Settings →
   Devices & services → MQTT) and responds to control.
6. **2.1.6** `check identity`. **Paste back:** the Zigbee part
   (fingerprint, PAN ID, channel). Keep this output: section 5 compares
   against it.
7. **2.1.7** (changes the VM) Rotate the Zigbee broker password:
   `wild secret rotate mqtt-zigbee -node $NODE` (or Console → Secrets →
   mqtt-zigbee → Rotate). **Expect:** the password file is rewritten,
   `mosquitto` and `zigbee2mqtt` are re-created, Zigbee2MQTT reconnects
   (2.1.3's log line again), the device still responds in Home Assistant.
8. **2.1.8** (optional, needs a second node) Installing Zigbee2MQTT on
   another node is refused naming both nodes and `mqtt-zigbee`.
9. **2.1.9** (changes the VM; best with two sticks, before 2.3) Swapped
   sticks. In Proxmox unplug **both** sticks, plug the **Zigbee** stick
   back first, wait 5 seconds, then the other one. `ls -l /dev/serial/by-id/`
   should show the ttyUSB numbers swapped. Zigbee2MQTT stops ("Adapter
   disconnected"); approve the drift repair Wildstone proposes.
   **Expect:** `zigbee2mqtt` is re-created (2.1.3's `docker inspect` now
   shows the Zigbee stick's new node), it logs
   `zigbee-herdsman started (resumed)` with your devices, and the device
   responds again. It must never be started against the other stick's
   node.

### 2.2 Z-Wave (#444)

1. **2.2.1** (changes the VM) Install:

   ```sh
   wild capability enable zwave-zwave-js-ui -set node=$NODE -set admin_domain=$ADMIN_DOMAIN \
     -set usb_device=$ZWAVE -set listen_addr=$LAN_IP
   ```

   **Expect:** no 32-character hex values and no key names in the plan's
   env. Approve.
2. **2.2.2** Checks:

   ```sh
   sudo docker exec zwave-js-ui ls -l /dev/zwave
   sudo docker logs zwave-js-ui 2>&1 | grep -E 'wildstone: generated|MQTT is disabled|external Z-Wave settings' | head
   ss -ltn | grep -E ':3000|:8091'
   ```

   **Expect:** `/dev/zwave` exists; the log says "wildstone: generated
   securityKeys..." **once**, "MQTT is disabled" and "Loaded external
   Z-Wave settings"; port 3000 on `<LAN_IP>` only, 8091 not listed.
3. **2.2.3** Open `https://zwave.<ADMIN_DOMAIN>`. **Expect:** the household
   sign-in first, then Z-Wave JS UI. Settings shows the WS server as
   managed/enabled. Set your RF region (Settings → Z-Wave) if asked.
4. **2.2.4** **In Home Assistant:** Add integration → Z-Wave JS → URL
   `ws://<LAN_IP>:3000`. **Expect:** it connects and shows the
   controller. Include your Z-Wave device (securely, S2, if it offers
   it). **Expect:** it works from Home Assistant.
5. **2.2.5** `check identity`. **Paste back:** the Z-Wave part
   (key fingerprints). Keep it for section 5.
6. **2.2.6** (changes the VM) `sudo docker restart zwave-js-ui`, then
   change the port: `wild capability enable zwave-zwave-js-ui ... -set ws_port=3001`
   (same params as 2.2.1 plus this), approve. Update Home Assistant's
   Z-Wave JS URL to port 3001 (Settings → Devices & services → Z-Wave JS
   → Reconfigure). **Expect:** it reconnects, the S2 device still works,
   and `check identity` shows the **same** key fingerprints. Change it
   back to 3000 the same way.
7. **2.2.7** (changes the VM) `wild capability disable zwave-zwave-js-ui`,
   approve. **Expect:** the container and Route are gone;
   `sudo docker volume ls | grep zwave-js-ui-store` still lists the
   volume. Enable it again (2.2.1's command), approve. **Expect:** the
   same key fingerprints, the same network (the device still works), and
   no new "generated securityKeys" line in the log.
8. **2.2.8** Next day: `check identity` shows an `NVM_*.bin` NVM backup
   (made at 02:30), and `wild backup list` shows a `zwave` backup run
   `succeeded`.

### 2.3 Thread border router (#443)

Needs Home Assistant (1.5) on this same VM.

**Step A: firmware.** The Thread stick must run **OpenThread RCP**
firmware, not Zigbee firmware and not "multiprotocol".

1. **2.3.1** Check what it runs (nothing else must be using the stick):

   ```sh
   pipx run universal-silabs-flasher --device $THREAD probe
   ```

   (For Silicon Labs sticks: ZBT-1, ZBT-2, Sonoff ZBDongle-E.)
   **Expect:** the application type `spinel` (that is Thread RCP). If it
   says `ezsp` (Zigbee) or `cpc` (multiprotocol), flash OpenThread RCP:
   for a ZBT-1/ZBT-2 use the web flasher at
   toolbox.openhomefoundation.org in Chrome and choose "Thread"; for a
   ZBDongle-E use a community OpenThread RCP build. An nRF52840 dongle
   takes Nordic's `ot-rcp`. Note your stick's serial speed and flow
   control: ZBT-1/ZBT-2 RCP is 460800 with hardware flow control (the
   package defaults); most ZBDongle-E RCP builds are 460800 with
   `flow_control=none`; an nRF52840 dongle ignores the speed and needs
   `flow_control=none`.

   **Paste back:** the probe line and which settings you used.

**Step B: install with the host NOT ready yet** (it must refuse clearly).

2. **2.3.2** `ready`.
   **Expect:** `ipv6`, `interface_ipv6`, `multicast`, `tun` ready;
   `ipv6_forwarding` **missing** (value 0) on a normal Debian/Ubuntu.
   Leave it that way for now.
3. **2.3.3** (changes the VM) Install:

   ```sh
   wild capability enable thread-otbr -set node=$NODE -set radio_device=$THREAD -set backbone_interface=$NIC
   # add -set flow_control=none (or another -set baud_rate=...) if 2.3.1 said so
   ```

   **Expect** in the plan: Container/otbr (host network), ConfigFile
   `before_server`, two Volumes, a Secret, a BackupPlan, and no Route.
   Approve. Then:

   ```sh
   sudo docker logs otbr 2>&1 | grep wildstone-otbr | tail -5
   sudo docker inspect otbr -f '{{.State.Status}} {{.RestartCount}}'
   sudo docker exec otbr pgrep otbr-agent; echo exit=$?
   ```

   **Expect:** `FAIL [ipv6_forwarding] net.ipv6.conf.all.forwarding is 0 ... Fix on the node: ...`
   then `not starting otbr-agent`; the container restarting or exited
   with a growing restart count; `pgrep` fails. It must **not** sit "Up"
   doing nothing.
4. **2.3.4** Three quick refusals, one try each:
   - `backbone_interface=eth9` (re-run 2.3.3's command with that): **Expect**
     the log says `FAIL [backbone_interface] ... (it has: ...)`. Put it back.
   - `radio_device=/dev/ttyACM0` with `-dry-run`: **Expect** refused (not a by-id path).
   - Unplug the Thread stick (Proxmox: remove the USB device from the
     VM's hardware, or pull it), then change any param and approve:
     **Expect** the step refused with `device "/dev/serial/by-id/..." does not exist on this node`.
     Plug it back.

**Step C: prepare the host** (changes the VM; order matters: `accept_ra`
before forwarding, so the VM keeps its own IPv6 address and route):

5. **2.3.5**

   ```sh
   printf 'net.ipv6.conf.all.forwarding = 1\nnet.ipv6.conf.%s.accept_ra = 2\nnet.ipv6.conf.%s.accept_ra_rt_info_max_plen = 64\n' "$NIC" "$NIC" \
     | sudo tee /etc/sysctl.d/60-wildstone-otbr.conf
   sudo sysctl --system > /dev/null
   sudo modprobe ip6table_filter
   echo ip6table_filter | sudo tee /etc/modules-load.d/wildstone-otbr.conf
   ```

   **If `networkctl status $NIC` says the interface is configured by
   systemd-networkd** (Ubuntu with netplan, for one): leave out the two
   `accept_ra` lines. networkd handles router advertisements itself and
   keeps the kernel's `accept_ra` at 0; tell it to keep accepting them
   with forwarding on instead, then check the IPv6 default route survives:

   ```sh
   printf 'network:\n  version: 2\n  ethernets:\n    %s:\n      accept-ra: true\n' "$NIC" | sudo tee /etc/netplan/60-wildstone-otbr.yaml
   sudo chmod 600 /etc/netplan/60-wildstone-otbr.yaml
   sudo netplan generate && sudo netplan apply
   ip -6 route show default
   ```

   (The `ethernets` key must match the one in your existing netplan
   file; the preflight's `WARN [accept_ra]` for `accept_ra is 0` then
   names this case and can be ignored once the route is there.)

   (Only if you will try `nat64=on` in 2.3.12: also
   `echo 'net.ipv4.ip_forward = 1' | sudo tee -a /etc/sysctl.d/60-wildstone-otbr.conf && sudo sysctl --system`.)

   **Expect:** the probe now shows `ipv6_forwarding` ready, and within
   Docker's restart back-off the log shows
   `wildstone-otbr preflight: ok (backbone <NIC>, radio /dev/ttyRCP)` then
   `Starting thread border agent otbr-agent`, and no `WARN [accept_ra]`
   (on a networkd host, the `accept_ra is 0` form of it).
   Without `ip6table_filter` it would say `FAIL [ip6tables]`.

**Step D: healthy checks.**

6. **2.3.6**

   ```sh
   sudo docker inspect otbr -f '{{.HostConfig.NetworkMode}} {{.HostConfig.CapAdd}} {{.HostConfig.Privileged}} {{json .HostConfig.Devices}}'
   sudo docker exec otbr ot-ctl rcp version
   sudo docker exec otbr ot-ctl state
   ip -6 addr show wpan0
   ss -ltn | grep 8081
   curl -s http://127.0.0.1:8081/node/state; echo
   ss -ltn | grep ':80 '
   sudo ip6tables-legacy -S FORWARD | grep OTBR_FORWARD_INGRESS
   sudo ipset list -n | grep otbr
   ```

   **Expect:** `host [IPC_LOCK NET_ADMIN] false` and devices = your
   by-id path → `/dev/ttyRCP` plus `/dev/net/tun`; an RCP version line
   (if you see "Failed to communicate with RCP" or timeouts, re-install
   with the other `flow_control` or `baud_rate` and note which worked);
   state `disabled` (no network yet); `wpan0` exists; 8081 on
   `127.0.0.1` only; `"disabled"`; no port-80 listener from the border
   router; the firewall chain and ipsets present.
   **From your laptop:** `curl -m3 http://<LAN_IP>:8081/node/state`
   **Expect:** connection refused.
7. **2.3.7** **In Home Assistant:** Add integration → "Open Thread
   Border Router" → URL `http://127.0.0.1:8081` (not the LAN address:
   the border router only answers on the VM itself). **Expect:** it
   adds; Home Assistant creates a Thread network (or, under Settings →
   Thread, make this border router's network the preferred one). Then:

   ```sh
   sudo docker exec otbr ot-ctl state         # expect: leader
   check identity                              # the thread part: dataset fingerprint, channel
   ```

   **From your laptop:** `avahi-browse -rt _meshcop._udp` lists the
   border router. **Paste back:** the Thread part of `check identity`.
8. **2.3.8** (changes the VM) Keep the network across restarts:
   `sudo docker restart otbr`; then re-create it by changing a param
   (`-set firewall=off`, approve, then back to `on`, approve). After
   each: **Expect** the same dataset fingerprint, `ot-ctl state` back to
   `leader` (or `router`) within about 2 minutes, the same network under
   Home Assistant → Settings → Thread, and (once 2.5 is done) the Thread
   device still responding. `sudo docker stop otbr` should remove the
   firewall chain (step 2.3.6's `ip6tables-legacy` line prints nothing);
   start it again with `sudo docker start otbr`.
9. **2.3.9** (changes the VM) `wild capability disable thread-otbr`,
   approve. **Expect:** the container is gone,
   `sudo docker volume ls | grep otbr-data` still lists the volume.
   Enable again (2.3.3's command), approve. **Expect:** the same
   dataset fingerprint.
10. **2.3.10** Next day: `wild backup list` shows a `thread` backup run
    `succeeded` (class critical).
11. **2.3.11** Docker's IPv6 forwarding rule (for a later fix):
    `sudo ip6tables -S FORWARD | head -1`. If it says `-P FORWARD DROP`,
    check whether a Thread device can reach a LAN IPv6 address (for
    example Home Assistant's Thread device page shows it reachable). If
    it cannot, try
    `sudo ip6tables -I DOCKER-USER -i wpan0 -j ACCEPT; sudo ip6tables -I DOCKER-USER -o wpan0 -j ACCEPT`
    and report whether that was needed.
12. **2.3.12** (optional) `nat64=on`: re-install with `-set nat64=on`
    (needs `net.ipv4.ip_forward = 1` from 2.3.5). Then restart it twice
    and run `sudo iptables-legacy -t nat -S POSTROUTING | grep -c 0x1001`
    after each. **Expect** (known upstream behaviour) the count grows by
    one per restart. Report the numbers, then set `nat64=off` again.

### 2.4 Matter server, and Matter over Wi-Fi (#442)

1. **2.4.1** Install (changes the VM):

   ```sh
   for i in lo 'eth0;x'; do wild capability enable matter-matterjs -set node=$NODE -set primary_interface="$i" -dry-run 2>&1 | tail -1; done
   wild capability enable matter-matterjs -set node=$NODE -set primary_interface=$NIC
   ```

   **Expect:** the two dry runs refused. The real plan: Secret
   `restic-matter`, Volumes `matter-data` and `matter-backup-repo`,
   Container `matter-server` (host network, no ports, read-only root,
   no-new-privileges), BackupPlan `matter` (critical), no Route. Approve.
2. **2.4.2** Checks:

   ```sh
   sudo docker logs matter-server 2>&1 | grep -i 'listening on'
   ss -ltn | grep 5580
   curl -s http://127.0.0.1:5580/health; echo
   ss -lun | grep -E ':5353'
   sudo docker inspect -f '{{.State.Health.Status}}' matter-server
   sudo docker exec matter-server ls -ln /data
   ```

   **Expect:** `http://127.0.0.1:5580` only; 5580 on `127.0.0.1` only;
   health JSON with `"version":"1.4.0"`; mDNS (5353) on all interfaces
   (Matter's own UDP is on ephemeral ports, not 5540: 5540 is where
   Matter *devices* listen, and this is a controller); `healthy`; `/data` owned by
   `1000`. **From your laptop:** `curl -m3 http://<LAN_IP>:5580/health`
   **Expect:** connection refused.
3. **2.4.3** **In Home Assistant:** Add integration → Matter → untick
   the add-on option if asked → `ws://127.0.0.1:5580/ws`. **Expect:** it
   sets up.
4. **2.4.4** Commission your **Matter-over-Wi-Fi** device: **On your
   phone**, Home Assistant app → Settings → Devices & services → Add
   device → Add Matter device, scan its QR code. **Expect:** it joins
   your Wi-Fi and appears in Home Assistant; it responds to control.
   Optional: the device page's "Ping" works only if
   `sysctl net.ipv4.ping_group_range` covers 1000 (Debian/Ubuntu default
   does).
5. **2.4.5** `check identity` → **Paste back:** the Matter part
   (`compressed_fabric_id`).
6. **2.4.6** (changes the VM) `sudo docker restart matter-server`; then
   re-create it by changing `primary_interface` (to the same NIC is not
   a change; if you have only one NIC, skip the re-create). **Expect:**
   the same `compressed_fabric_id`; the device still works with no
   re-commissioning.
7. **2.4.7** (changes the VM) `wild capability disable matter-matterjs`,
   approve; `sudo docker volume ls | grep matter-data` still lists it.
   Enable again, approve. **Expect:** the same fabric id; the device
   still there and working.
8. **2.4.8** (optional) The Matter server's own dashboard is not
   published anywhere. To look at it, **from your laptop**:
   `ssh -L 5580:127.0.0.1:5580 <you>@<LAN_IP>` and browse
   `http://localhost:5580/`. **Expect:** the node list with your
   devices.
9. **2.4.9** (optional, needs a second node) A Home Assistant on another
   node that is wired to this Matter server is refused at install,
   naming both nodes (the server only answers on 127.0.0.1).

### 2.5 Matter over Thread (#442, #443)

Needs 2.3 and 2.4.

1. **2.5.1** **On your phone:** Home Assistant app → Settings →
   Companion app → Troubleshooting → **Sync Thread credentials** (the
   exact place differs between iOS, Android and app versions). **Expect:**
   it says the credentials were shared or are already on the phone.
2. **2.5.2** Commission the **Matter-over-Thread** device the same way
   as 2.4.4. **Expect:** it joins (this can take a minute or two),
   appears in Home Assistant, responds, and shows under Settings →
   Thread as part of **your** network (not an Apple or Google one).
3. **2.5.3** (changes the VM) `sudo docker restart otbr`, wait 2
   minutes. **Expect:** the device responds again without re-pairing.
4. **2.5.4** Reachability: on the device's page in Home Assistant, it
   should not be "unavailable" after 15 to 30 minutes of idling. If it
   drops repeatedly, note it (sleepy devices behind a firewall that
   forgets UDP sessions; see 2.3.11).

### 2.6 (optional) Bluetooth (#445, #434, #436)

You do not need this for the core test.

**Proxy mode (the default, needs an ESP32):**

1. **2.6.1** `sudo docker inspect home-assistant --format '{{json .HostConfig.Binds}} {{json .HostConfig.CapAdd}}'`.
   **Expect:** only the config volume, and `null` capabilities.
2. **2.6.2** Flash the ESP32 from esphome.io/projects ("Bluetooth
   Proxy") in Chrome and join it to your Wi-Fi. **Expect:** Home
   Assistant shows it under Settings → Devices & services → Discovered;
   add it; a Bluetooth sensor nearby appears in the Bluetooth
   integration.

**Host-adapter mode (only if you pass a USB Bluetooth adapter to the VM):**

3. **2.6.3** With the adapter passed through: `api /api/v1/nodes/$NODE/probes/radios`
   shows an entry `kind: "bluetooth"`, `kernelName: "hci0"`,
   `driver: "btusb"`, no `devPath`; the readiness probe shows
   `bluetooth_adapter` ready on `hci0`; after `sudo rfkill block bluetooth`
   it shows missing (`rfkill soft`). `sudo rfkill unblock bluetooth` after.
4. **2.6.4** Without BlueZ (`systemctl status bluetooth` says not found),
   try `-set bluetooth=host-bluez` on Home Assistant (same params as
   1.5.2 plus this). **Expect:** the Container step refused with
   "bluez_service missing (bluetooth.service)" and "before the existing
   container is touched"; Home Assistant keeps running (same container
   ID). With the adapter rfkill-blocked: refused with
   "bluetooth_adapter missing (hci0): hci0 is soft-blocked by rfkill".
5. **2.6.5** `sudo apt install bluez && sudo systemctl enable --now bluetooth && sudo rfkill unblock bluetooth`,
   apply again. **Expect:** it converges; `docker inspect` shows binds
   `home-assistant-config:/config` and `/run/dbus:/run/dbus:ro`,
   capabilities `NET_ADMIN NET_RAW`, no devices, not privileged; Home
   Assistant's Bluetooth integration shows the adapter and finds a BLE
   device nearby. Paste back any log line like "Operation not permitted".
6. **2.6.6** Switch back to `proxy`, then `off`. **Expect:** the
   container is replaced each time; users, automations and history stay.

---

## 3. The Console's guided setup, end to end (#447, #450)

This repeats the install the way a household would do it: through the
Console. First remove what sections 1 and 2 installed. **Removing keeps
every volume**, so your devices stay paired: this also proves that.

### 3.0 Start from "not installed" (changes the VM)

1. **3.0.1** Save the fingerprints: `check identity > ~/identity-before-console.txt`.
2. **3.0.2** Disable, approving each plan:

   ```sh
   for p in home-assistant zigbee-zigbee2mqtt zwave-zwave-js-ui thread-otbr matter-matterjs mqtt-mosquitto; do
     wild capability disable $p
   done
   wild plan list | head -10      # approve each: wild plan approve <id>
   ```

   **Expect:** after approval, none of those containers in `sudo docker ps`;
   every volume still in `sudo docker volume ls`.

### 3.1 Preview through the API (#447)

1. **3.1.1**

   ```sh
   apipost /api/v1/home-automation/preview '{"topology":"managed","select":["zigbee","zwave","matter","thread"]}' \
     | jq '{next: .next | {id, action, params}, steps: [.sequence[] | {id, action, status}]}'
   ```

   **Expect:** `next` is `mqtt:install_package:mqtt-mosquitto` with
   params `node=<NODE>` and `listen_addr=<LAN_IP>`, and a sequence of
   the remaining packages. Any step that says `plan_refused`: paste back
   its reason exactly. Nothing is created by a preview.

### 3.2 Choose and check (browser)

1. **3.2.1** Console → **Home automation** → **Run it here** → choose
   your node → tick **Zigbee**, **Z-Wave**, **Matter over Wi-Fi** and
   **Matter over Thread** → Bluetooth: **Bluetooth proxies** (or *Not
   needed*) → **Continue**.
2. **3.2.2** The readiness page. **Expect:** every network check green
   (IPv6, IPv6 on the interface, multicast, IPv6 forwarding, TUN); a
   note that the Thread radio's firmware cannot be checked by Wildstone
   (with instructions); a radio picker per protocol listing your sticks.
   Pick each stick. A stick already picked for one protocol should not
   be offered for another.
3. **3.2.3** **Continue to setup**.

### 3.3 Propose, approve, watch (browser)

1. **3.3.1** On the first step (MQTT broker) click **Propose this
   plan**. **Expect:** "Plan <id> is filed" and "Waiting for your
   approval". There is **no** approve button on this page, only "Open
   Plans to review and approve".
2. **3.3.2** Proposing the same step again through the API files
   nothing:

   ```sh
   apipost /api/v1/home-automation/propose '{"topology":"managed","select":["zigbee","zwave","matter","thread"],"expectStep":"mqtt:install_package:mqtt-mosquitto"}' | head -20
   ```

   **Expect:** an error (409) and still only one plan in Console → Plans.
3. **3.3.3** **Reload the browser tab.** **Expect:** it comes back at
   the same step with the same plan id.
4. **3.3.4** Approve the plan in Console → Plans, then go back to the
   tab. **Expect**, without reloading: "Applying", then "Applied,
   waiting to see it running", then "Installed", and only then the next
   step (Home Assistant, with MQTT on).
5. **3.3.5** While a step shows "Applying" or "Applied, waiting to see
   it running", restart Keystone: `sudo systemctl restart keystone`.
   Then run 3.1.1 again. **Expect:** the same `next.id` as before the
   restart; the page carries on by itself.

### 3.4 Interruptions (browser and VM)

1. **3.4.1** (changes the VM) At a step showing "Applied, waiting to
   see it running": close the Console tab and **reboot the VM**
   (`sudo reboot`). Wait for it to come back, reopen the Console.
   **Expect:** it resumes from where it was and moves on once the
   container is seen healthy.
2. **3.4.2** Open a second browser tab at the same step and click
   **Propose this plan** there first. Then click it in the first tab.
   **Expect** in the first tab: "Nothing was proposed: the next step
   changed", and only one new plan in Console → Plans.
3. **3.4.3** Before the Zigbee step, unplug the Zigbee stick (or remove
   it from the VM's USB devices in Proxmox). **Expect:** the page says
   it is "no longer a free Zigbee radio" with **Choose a radio**, which
   goes back to the picker; your other choices are kept. Also through
   the API, with the stick still unplugged:

   ```sh
   apipost /api/v1/home-automation/propose '{"topology":"managed","select":["zigbee","zwave","matter","thread"],"radios":{"zigbee":"<the zigbee id from 1.1.1>"},"expectStep":"<the current next.id: run 3.1.1 again to see it>"}' | jq '.rejected'
   ```

   **Expect:** `rejected[0].code` = `radio_stale` (409). Plug it back,
   choose it again.

### 3.5 Finish (browser and Home Assistant)

1. **3.5.1** Carry on until every package is installed. For the Zigbee,
   Z-Wave and Thread steps, open the plan before approving. **Expect:**
   the `usb_device` / `radio_device` param is the stick's
   `/dev/serial/by-id/...` path, filled in for you.
2. **3.5.2** The Home Assistant checklist. **Expect:** exact
   instructions: the MQTT broker's host and port, the Z-Wave URL, the
   Matter URL `ws://127.0.0.1:5580/ws`, the OTBR URL
   `http://127.0.0.1:8081`, and the phone's Thread credentials step.
   Home Assistant already has these integrations from section 2; check
   each still connects (Settings → Devices & services: no "Failed to set
   up"). For MQTT, the password is the same as before unless you rotated
   it. If an integration needs its details again, follow the checklist.
3. **3.5.3** Tick each **I have done this in Home Assistant**.
   **Expect:** each shows "You confirmed" (not "Verified"); "Home
   automation is set up" appears only when all are ticked; unticking one
   takes it away again.
4. **3.5.4** The same through the API:

   ```sh
   apipost /api/v1/home-automation/preview '{"topology":"managed","select":["zigbee","zwave","matter","thread"]}' | jq '{complete, next: .next.id, external: [.externalSteps[] | "\(.component)/\(.id)"]}'
   ```

   **Expect:** `complete: false` and `next` an `external_step:...`;
   calling it again gives the same `next` and files no plan. Then send
   every id in that `external` list as `confirmedSteps`, for example:

   ```sh
   apipost /api/v1/home-automation/preview '{"topology":"managed","select":["zigbee","zwave","matter","thread"],
     "confirmedSteps":["zigbee/ha_mqtt_integration","zwave/ha_zwave_integration","matter/ha_matter_integration","thread/ha_otbr_integration","thread/thread_credentials"]}' | jq '{complete, next: .next.id}'
   ```

   (use exactly the ids the first call listed). **Expect:** `complete: true`.
5. **3.5.5** `check identity > ~/identity-after-console.txt` then
   `diff ~/identity-before-console.txt ~/identity-after-console.txt`.
   **Expect:** the same Zigbee network-key fingerprint, PAN ID and
   channel; the same Z-Wave key fingerprints; the same Thread dataset
   fingerprint; the same Matter fabric id. Every device from section 2
   works without re-pairing.

**Paste back:** the diff (empty is good) and any FAILs.

---

## 4. Restarts and reboots

1. **4.1** (changes the VM) Restart every container:
   `for c in mosquitto zigbee2mqtt zwave-js-ui otbr matter-server home-assistant; do sudo docker restart $c; done`.
   Wait 3 minutes. **Expect:** `check containers` shows all running
   (healthy where there is a health check); each test device responds
   in Home Assistant; `check identity` unchanged.
2. **4.2** (changes the VM) Reboot the VM: `sudo reboot`. After it is
   back (give it 5 minutes):
   - `check host`: **Expect** `60-wildstone-otbr.conf` still applied
     (forwarding 1, accept_ra 2), `ip6table_filter: loaded`.
   - `check radios`: **Expect** the same three by-id names.
   - `check containers`: **Expect** all running; `wpan0` exists.
   - `sudo docker exec otbr ot-ctl state`: **Expect** `leader` within
     about 2 minutes.
   - In Home Assistant: every integration connected; every device
     responds; the Thread device too.
   - `check identity`: **Expect** unchanged.
3. **4.3** (optional) Reboot the Proxmox host itself. **Expect:** the
   VM starts with the sticks attached (by-id names unchanged) and
   everything in 4.2 holds.
4. **4.4** (changes the VM) The node agent restarting does not touch
   the containers: note `sudo docker ps -q | sort | md5sum`, run
   `sudo systemctl restart wardstoned`, wait a minute, compare.
   **Expect:** the same.

**Paste back:** anything that did not come back by itself.

---

## 5. Backup and restore drill, one controller at a time

The proof that a backup brings each network back. Order: Thread,
Matter, Zigbee, Z-Wave, Home Assistant (the same order a real recovery
uses; see [docs/recovery.md](../../docs/recovery.md#home-automation-controllers)).

### 5.1 Canaries and fingerprints (changes the VM)

A canary is a small file you put in each volume. If it comes back from
the backup, the backup really holds that volume.

```sh
stamp=$(date -u +%Y%m%dT%H%M%SZ)
for v in otbr-data matter-data zigbee2mqtt-data zwave-js-ui-store home-assistant-config; do
  d=$(sudo docker volume inspect -f '{{.Mountpoint}}' $v)
  echo "canary-$v-$stamp" | sudo tee "$d/.wildstone-canary" > /dev/null
  sudo chown --reference="$d" "$d/.wildstone-canary"
done
check canary identity > ~/identity-before-drill.txt
cat ~/identity-before-drill.txt
```

**Expect:** every volume shows `live <volume>: canary-<volume>-<time>`.

### 5.2 Back up now

```sh
for p in thread matter zigbee zwave home-assistant; do wild backup run $p; done
check canary | tail -15
```

**5.2** **Expect:** five `backup` runs, each `succeeded`.

### 5.3 Restore alongside and compare

Restoring "alongside" writes the backup into a new volume
`<plan>-restore` and leaves the live one alone.

```sh
for p in thread matter zigbee zwave home-assistant; do
  sudo docker volume rm $p-restore 2>/dev/null   # only an old one from an earlier drill
  wild backup restore $p
done
check canary restored
```

**5.3** **Expect:** for every plan, `restored <plan>-restore: canary-...`
equal to the live canary; `restore` runs `succeeded`; in the
"restored copies" part, the Zigbee network-key fingerprint, Z-Wave key
fingerprints and Home Assistant users/integrations equal the live ones;
the restored `otbr-data` holds the same file names as the live one.

### 5.4 Put a restored copy in place, for real (changes the VM)

Do this for one controller at a time, in the order below. Each time:
disable it, keep a safety copy of the live data, replace the live data
with the restored copy, enable it, check its identity. **Never run two
copies of a controller at once.**

Set these for the controller you are on:

| Controller | `P` (plan) | `V` (volume) | `PKG` (package) | Re-enable with |
|---|---|---|---|---|
| Thread | `thread` | `otbr-data` | `thread-otbr` | 2.3.3's command |
| Matter | `matter` | `matter-data` | `matter-matterjs` | 2.4.1's last command |
| Zigbee | `zigbee` | `zigbee2mqtt-data` | `zigbee-zigbee2mqtt` | 2.1.2's command |
| Z-Wave | `zwave` | `zwave-js-ui-store` | `zwave-zwave-js-ui` | 2.2.1's command |
| Home Assistant | `home-assistant` | `home-assistant-config` | `home-assistant` | 1.5.2's command |

```sh
P=thread V=otbr-data PKG=thread-otbr          # change for each controller
wild capability disable $PKG                   # approve it; wait until the container is gone:
sudo docker ps --format '{{.Names}}' | grep -E 'otbr|matter-server|zigbee2mqtt|zwave-js-ui|home-assistant'
# safety copy of the live data
sudo docker volume create $V-safety
sudo docker run --rm -v $V:/from:ro -v $V-safety:/to alpine:3.20 cp -a /from/. /to/
# replace the live data with the restored copy
sudo docker run --rm -v $P-restore:/from:ro -v $V:/to alpine:3.20 \
  sh -c "find /to -mindepth 1 -delete && cp -a /from/backup/$V/. /to/"
# now re-enable it with its usual command (table above), and approve
```

Then:

1. **5.4.1 Thread:** **Expect** `ot-ctl state` back to `leader` within
   about 2 minutes, the same dataset fingerprint as
   `~/identity-before-drill.txt`, the Thread device responds.
2. **5.4.2 Matter:** **Expect** the same `compressed_fabric_id`, both
   Matter devices respond with no re-commissioning.
3. **5.4.3 Zigbee:** **Expect** the same network-key fingerprint, PAN
   ID and channel; the Zigbee device responds without re-pairing.
4. **5.4.4 Z-Wave:** **Expect** the same key fingerprints; the secure
   (S2) device still works; no "generated securityKeys" log line.
5. **5.4.5 Home Assistant:** **Expect** the same users, integrations and
   automations; every integration connects. (If it complains that its
   history database is damaged, it moves it aside and starts a new one:
   note it, it is not a FAIL.)
6. **5.4.6** `check canary identity` and compare with
   `~/identity-before-drill.txt`. **Expect:** identical fingerprints.
   **Paste back:** the comparison (the fingerprints only).
7. **5.4.7** Clean up, only when everything works:

   ```sh
   for v in otbr-data matter-data zigbee2mqtt-data zwave-js-ui-store home-assistant-config; do sudo docker volume rm $v-safety; done
   for p in thread matter zigbee zwave home-assistant; do sudo docker volume rm $p-restore; done
   ```

If something does not come back, **stop**, and put the safety copy back
the same way (disable, copy from `$V-safety` into `$V`, enable). Report
exactly what you saw.

### 5.5 (optional, advanced) A clean replacement node

The full recovery from [docs/recovery.md](../../docs/recovery.md) onto a
second, empty VM. Only after the whole-site recovery contract is set up
(the reserved `wildstone` plan, the recovery kit and off-site copies of
the five plans). In short: shut the first VM down for good (never run
both); on the new VM prepare the host first (Docker, the
`60-wildstone-otbr.conf` and `ip6table_filter` from 2.3.5) **with the
sticks not passed through**; restore Keystone, re-join the node under
the same name, import its secrets; let it rebuild (the Zigbee, Z-Wave
and Thread containers are refused because their sticks are absent:
that is intended); then restore and move into place in the order of
5.4, passing each stick through just before re-enabling its package.
**Expect:** the same fingerprints as `~/identity-before-drill.txt` and
every device working without re-pairing.

---

## 6. One stick, one owner (#435)

With Zigbee2MQTT installed on `$ZIGBEE` (section 2 or 3):

1. **6.1** The same stick by its other name. Find the plain name:
   `readlink -f $ZIGBEE` (for example `/dev/ttyUSB0`). Then:

   ```sh
   wild capability enable zwave-zwave-js-ui -set node=$NODE -set admin_domain=$ADMIN_DOMAIN \
     -set usb_device=$(readlink -f $ZIGBEE) -set listen_addr=$LAN_IP
   ```

   **Expect:** refused (409) with "duplicate physical-device ownership
   ... on node <NODE>, radio <id> is claimed by Container/zigbee2mqtt
   (package zigbee-zigbee2mqtt) via <by-id path> and Container/zwave-js-ui
   (package zwave-zwave-js-ui) via /dev/ttyUSB0"; `wild plan list` shows
   no new plan.
2. **6.2** The same by-id path in another package:
   `wild capability enable thread-otbr -set node=$NODE -set radio_device=$ZIGBEE -set backbone_interface=$NIC`.
   **Expect:** refused before a plan exists, naming Container/zigbee2mqtt.
   The other way round, Zigbee asking for the Z-Wave stick:
   `wild capability enable zigbee-zigbee2mqtt -set node=$NODE -set admin_domain=$ADMIN_DOMAIN -set usb_device=$ZWAVE`.
   **Expect:** refused, naming Container/zwave-js-ui.
3. **6.3** Home Assistant cannot take a stick another package owns:
   `wild capability enable home-assistant -set node=$NODE -set public_domain=$DOMAIN -set mqtt_enabled=true -set serial_device=$ZIGBEE`.
   **Expect:** refused, naming Container/zigbee2mqtt.
4. **6.4** In the Console: Capabilities (or Services) → Z-Wave →
   enable, choosing the Zigbee stick's path. **Expect:** the preview
   shows the same refusal and no plan is filed.
5. **6.5** (optional, if a Butler is set up) Ask it to "install
   zwave-zwave-js-ui with usb_device <the Zigbee stick's /dev/ttyUSB path>".
   **Expect:** its proposal is refused the same way; no plan.
6. **6.6** (changes the VM) With the agent stopped
   (`sudo systemctl stop wardstoned`): repeat 6.1 with the plain
   `/dev/ttyUSB...` path. **Expect:** this time it is **accepted** (with
   the agent offline Wildstone can only compare exact paths): reject the
   plan at once (`wild plan reject <id>`). Repeat with the identical
   by-id path (`-set usb_device=$ZIGBEE`): **Expect** refused. Then
   `sudo systemctl start wardstoned`.
7. **6.7** (optional) Two containers sharing `/dev/dri/renderD128`
   (Jellyfin and Frigate) on one node are accepted; the same by-id path
   on two **different** nodes is accepted. Only if you have those.

---

## 7. Results checklist

Copy this block, fill in each line with PASS, FAIL or SKIP and a short
note for every FAIL (the exact message you saw), and paste it back
together with:

- the output of `check all` (taken at the end),
- the `git log --oneline -1` line from 0.3.1,
- every "Paste back" item above.

```text
Commit tested:
Sticks (vendor/model only, no serial):  Zigbee=      Z-Wave=      Thread=
Thread RCP settings (baud / flow control):

0.2.5  sticks visible by-id in the VM            :
0.3.2  build-dist.sh finished                    :
0.5    agent and Keystone upgraded               :
0.7.1  node at protocol minor 27                 :
0.7.2  no container churn from the upgrade       :
0.8.1  TestSecuredMosquitto (optional)           :
0.8.2  TestMatterServerServesHomeAssistant (opt) :
0.8.3  TestOTBR with simulated RCP (optional)    :

1.1.1  radio inventory lists the three sticks    :
1.1.2  stable order                              :
1.1.3  id survives a port change                 :
1.1.4  no [REDACTED] in by-id names              :
1.1.5  ListRadios audited                        :
1.1.6  old agent answers 400 (optional)          :
1.2.1  readiness baseline                        :
1.2.2  probe changes no sysctl                   :
1.2.3  IPv6 off detected                         :
1.2.4  multicast off detected                    :
1.2.5  BlueZ stopped detected (optional)         :
1.2.6  tun missing detected (optional)           :
1.2.7  ipv6.disable=1 unsupported (optional)     :
1.2.8  HomeAutomationReadiness audited           :
1.3.1  status API: mqtt ready, zigbee blocked    :
1.3.2  status API: matter blocked without IPv6   :
1.3.3  status API: node offline, no plan         :
1.4.1  broker refuses bad addresses              :
1.4.2  broker plan contents                      :
1.4.3  passwd hashed, 0600, LAN-only listener    :
1.4.4  anonymous MQTT refused from laptop        :
1.4.5  Frigate MQTT (optional)                   :
1.5.1  HA 1.x upgrade keeps data (optional)      :
1.5.2  port 8123 conflict refused, then converges:
1.5.3  HA through the Wildstone URL (no 400)     :
1.5.4  trusted_proxies as expected               :
1.5.5  direct LAN port; forged XFF gets 400      :
1.5.6  LAN discovery works                       :
1.5.7  MQTT env; integration connects; bad pw no :
1.5.8  /dev/sda refused                          :
1.6.1  bridge container on wildstone network     :
1.6.2  host network, reachable on LAN            :
1.6.3  host network + ports refused              :
1.6.4  back to bridge                            :
1.6.5  old agent refuses host network (optional) :
1.7.2  host route re-creates Caddy once          :
1.7.3  host-gateway alias, no 172.x              :
1.7.4  host route serves the page                :
1.7.5  dashboard dot for host service (optional) :
1.7.6  gate fails when service stopped           :
1.7.7  bad upstreams refused                     :

2.1.1  Zigbee 1.x upgrade keeps network (opt)    :
2.1.2  Zigbee plan: no password, no by-id in env :
2.1.3  Zigbee MQTT connected, /dev/zigbee        :
2.1.4  no mosquitto:1883 anywhere                :
2.1.5  device paired, in HA, controllable        :
2.1.7  mqtt-zigbee rotation reconnects           :
2.1.8  other-node Zigbee refused (optional)      :
2.1.9  swapped sticks: re-created on the new node :
2.2.1  Z-Wave plan: no keys                      :
2.2.2  /dev/zwave, keys generated once, :3000 LAN:
2.2.3  UI behind sign-in                         :
2.2.4  HA Z-Wave JS connects; S2 device works    :
2.2.6  restart and port change keep keys         :
2.2.7  disable/enable keeps keys and network     :
2.2.8  nightly NVM backup and restic run         :
2.3.1  RCP firmware is spinel                    :
2.3.2  ipv6_forwarding missing before prep       :
2.3.3  OTBR refuses clearly, does not sit Up     :
2.3.4  bad backbone / ttyACM / unplugged refused :
2.3.5  host prep makes preflight pass            :
2.3.6  OTBR healthy checks                       :
2.3.7  HA OTBR integration, leader, meshcop      :
2.3.8  restart/re-create keep the dataset        :
2.3.9  disable/enable keeps the dataset          :
2.3.10 nightly thread backup                     :
2.3.11 DOCKER-USER rule needed? (yes/no)         :
2.3.12 nat64 rule growth (optional)              :
2.4.1  Matter refuses bad interface; plan        :
2.4.2  WebSocket loopback only, healthy, uid 1000:
2.4.3  HA Matter integration                     :
2.4.4  Matter-over-Wi-Fi device commissioned     :
2.4.6  restart/re-create keep the fabric         :
2.4.7  disable/enable keeps the fabric           :
2.4.8  Matter dashboard over ssh (optional)      :
2.4.9  remote HA refused for Matter (optional)   :
2.5.1  Thread credentials synced to phone        :
2.5.2  Matter-over-Thread device on our network  :
2.5.3  survives OTBR restart                     :
2.5.4  stays reachable when idle                 :
2.6.x  Bluetooth (optional)                      :

3.0.2  everything disabled, volumes kept         :
3.1.1  preview: next is the broker               :
3.2.2  readiness page all green, radio pickers   :
3.3.1  plan filed, no approve button             :
3.3.2  second propose 409, one plan              :
3.3.3  reload resumes same plan                  :
3.3.4  applying -> applied -> installed, no F5   :
3.3.5  Keystone restart keeps next.id            :
3.4.1  VM reboot mid-step resumes                :
3.4.2  second tab: "the next step changed"       :
3.4.3  unplugged stick: Choose a radio; 409 stale:
3.5.1  by-id params filled in                    :
3.5.2  HA checklist instructions correct         :
3.5.3  "You confirmed"; set up only when all     :
3.5.4  API confirmedSteps -> complete: true      :
3.5.5  identities unchanged after re-install     :

4.1    container restarts                        :
4.2    VM reboot                                 :
4.3    Proxmox host reboot (optional)            :
4.4    agent restart leaves containers alone     :

5.1    canaries written                          :
5.2    five backups succeeded                    :
5.3    restored canaries and fingerprints match  :
5.4.1  Thread restored in place                  :
5.4.2  Matter restored in place                  :
5.4.3  Zigbee restored in place                  :
5.4.4  Z-Wave restored in place                  :
5.4.5  Home Assistant restored in place          :
5.4.6  fingerprints identical after the drill    :
5.5    clean replacement node (optional)         :

6.1    tty alias of a claimed stick refused      :
6.2    same by-id in another package refused     :
6.3    HA serial_device on a claimed stick refused:
6.4    Console preview refuses                   :
6.5    Butler proposal refused (optional)        :
6.6    agent offline: tty accepted, by-id refused:
6.7    shared GPU / other node accepted (opt)    :
```

## Putting the VM back

Everything here is a test install, but the host changes stay until you
undo them:

- Thread host prep: `sudo rm /etc/sysctl.d/60-wildstone-otbr.conf /etc/modules-load.d/wildstone-otbr.conf && sudo sysctl --system`
  (forwarding stays on until a reboot).
- Tools: `sudo apt remove mosquitto-clients avahi-utils pipx` if you do not want them.
- The test packages: `wild capability disable <package>` keeps their
  data; delete a volume (`sudo docker volume rm <name>`) only when you
  are sure you no longer need that network.
- `~/ha-test.env` holds your admin token: `rm ~/ha-test.env` when done.
- The build output: `rm -rf ~/wildstone-homelab/dist`.
