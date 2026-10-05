# Real-hardware run 1: Proxmox VM, 2026-10-01 to 10-02

The first run of this folder's [runbook](../README.md) against epic
#431, on a real VM with real radios. It is partial: sections 0 to 2.4
are done, and 2.5 to 6 remain. Step numbers match the runbook.

**Redacted:**
- The LAN address is shown as `192.0.2.10`.
- The node and tailnet names are generic.
- The Zigbee PAN IDs, the Thread network name and the Matter compressed
  fabric ID are recorded locally only.

Key and dataset *fingerprints* (short sha256 prefixes) are kept, as the
runbook allows; section 5 compares against them.

## Setup

| | |
|---|---|
| Branch under test | `claude/dreamy-einstein-9c5ory` at `7a8a266`, plus the fixes in #528 as they landed |
| Machine | Proxmox VM, Ubuntu 26.04, kernel 7.0.0-31, Docker 29.8.1. One NIC (`ens18`, configured by systemd-networkd through netplan), on the LAN with IPv6 from router advertisements. Keystone and the node agent run on the same VM. |
| Ingress | tailnet (`ingress-tailnet-ts`): routes are ports on the node's tailnet name, not `hass.<domain>` subdomains |
| Zigbee | Sonoff Zigbee 3.0 USB Dongle Plus V2: EmberZNet 7.4.4 (EZSP 13) at 115200, `coordinator_adapter=ember` |
| Thread | Sonoff Dongle Lite MG21: `SL-OPENTHREAD/2.4.4.0` RCP at 460800, `flow_control=none` |
| Z-Wave | none (section 2.2 skipped) |
| Both sticks | CP210x, `10c4:ea60`, so the inventory reports `kind: unknown` for both, as the runbook expects |

Firmware was identified by sending each stick one EZSP RST frame and
one Spinel `PROP_VALUE_GET NCP_VERSION` frame. Nothing was written to
either stick.

## Fixes made during the run (#528)

| Step | Problem | Fix |
|---|---|---|
| 2.1.3 | Zigbee2MQTT 2.x stops at its onboarding page on every fresh install, because `configuration.yaml` never exists when configuration is env-only. The stick is never opened and MQTT never connected. **Main is affected too.** | `zigbee-zigbee2mqtt` 2.0.3 sets `Z2M_ONBOARD_NO_SERVER=1` |
| 2.1.9 (new) | With both sticks replugged in the other order, the stored `/dev/ttyUSBn` belonged to the Thread RCP. The drift repair would have run `docker start` on Zigbee2MQTT against it, and so would Docker's restart policy after a reboot. | The agent records the requested by-id paths in a `dev.wildstone.device-sources` label, re-creates instead of starting when one now resolves elsewhere, and reports it as drift |
| 1.7.6 | On tailnet installs the route gate only probed Caddy's front door (502 or a 302 to sign-in counted as up), so a route change applied against a stopped host service, dropping its auth | A `host/<port>` upstream is also probed on the node |
| 2.3.5 | The OTBR preflight's `accept_ra` warning told networkd hosts to set the sysctl to 2, which fights networkd | `thread-otbr` 1.0.1: for `accept_ra=0` the warning names netplan `accept-ra: true` |

All of these were verified live on this VM after the fix, except the
OTBR warning text, which is covered by a fixture test.

## Findings filed as issues

- **#529:** rotating an MQTT secret re-creates the ingress proxy, which
  mounts all of `/etc/wildstone`. That takes every web route down
  briefly, and the ingress can read the broker's hash file.
- **#530:** `wild plan show` renders a device's `/dev/serial/by-id/…`
  source as `[REDACTED]`, so whoever reviews the plan cannot see which
  stick is mapped. `usb.serial` in the radio inventory is redacted too.
- **#531:**
  - A failed plan carries no reason.
  - Drift-repair plans list no steps.
  - A stopped container reads "nothing is wrong with this resource".
- **#532:**
  - MQTT shows `backup_missing`, because there's no BackupPlan for
    `mosquitto-data`.
  - Bluetooth reads "Installed and healthy" with no proxy verified.
  - Whether the front-door gate should fail on a 5xx.

## Results

PASS / FAIL / SKIP / OPEN (not yet run). Plan numbers are this VM's.

### Automated

- `go vet` and `go test` pass in keystone, wardstone, cli and proto
  (61 packages).
- Console: typecheck, build, render, contrast and handoff checks pass.
  `secrets-check` and `ui-check` were not run: there's no Playwright
  browser or live Console address on the VM.
- shellcheck v0.9.0 on `testing/home-automation/*.sh` and `scripts/*.sh`:
  pass.

### 0. Before you start

| Step | Result | Notes |
|---|---|---|
| 0.2.5 | PASS | 2 of 3 sticks (no Z-Wave) |
| 0.4 / 0.7.2 | SKIP | no snapshot was taken before the upgrade |
| 0.5 | PASS | built with local Go/Node toolchains instead of `build-dist.sh` (same steps) |
| 0.7.1 | PASS | connected, minor 26 |
| 0.7.3 | PASS | |

### 1. Software checks

| Step | Result | Notes |
|---|---|---|
| 1.1.1–1.1.2 | PASS | sorted, stable |
| 1.1.3 | OPEN | |
| 1.1.4 | NOTE | by-id paths and ids intact; `usb.serial` is `[REDACTED]` (#530) |
| 1.1.5 | PASS | in the journal and the audit log |
| 1.2.1–1.2.4, 1.2.8 | PASS | 1.2.2's filter needed `quota` (runbook fixed) |
| 1.3.1–1.3.3 | PASS | 1.3.3 compared the newest plan id (runbook fixed) |
| 1.4.1–1.4.4 | PASS | listener on `192.0.2.10:1883` only; three `$7$101` users; `600 root`; anonymous refused (tested from the VM) |
| 1.5.2 | PASS* | refusal text exact. *HA then needed a drift-repair approval, not "no new plan" (#531) |
| 1.5.3–1.5.5, 1.5.7, 1.5.8 | PASS | HA through the tailnet route; spoofed `X-Forwarded-For` → 400; `/dev/sda` refused |
| 1.6.1–1.6.4, 1.6.6 | PASS | 1.6.5 SKIP (no older node) |
| 1.7.2–1.7.4, 1.7.7, 1.7.9 | PASS | no Caddy re-create in 1.7.2: the `hass` route had already added the alias |
| 1.7.6 | FAIL → PASS | failed at first (plan-81 applied); passed after the #528 fix (plan-116 "health gate failed, rolled back") |

### 2. One radio at a time

| Step | Result | Notes |
|---|---|---|
| 2.1.2 | PASS | no literal password, no by-id in env; the plan *display* redacts the device (#530) |
| 2.1.3 | FAIL → PASS | onboarding page; passed after Zigbee2MQTT 2.0.3 |
| 2.1.4–2.1.6 | PASS | bulb paired via Permit join, controllable in HA. Zigbee key fingerprint `f74dcca4909155ed`, channel 11 |
| 2.1.7 | PASS* | Zigbee2MQTT reconnected. *The ingress was also re-created (#529) |
| 2.1.9 (new) | FAIL → PASS | swapped sticks; passed after the agent fix: re-created on the new node and "zigbee-herdsman started (resumed)" with the bulb |
| 2.2 | SKIP | no Z-Wave stick |
| 2.3.1–2.3.3 | PASS | the preflight refused with fix commands; restarting, not "Up" |
| 2.3.4 | PASS | `eth9` and `/dev/ttyACM0` refused; unplug SKIP |
| 2.3.5 | PASS* | *networkd host: netplan `accept-ra: true` instead of the `accept_ra` sysctl (runbook and preflight fixed) |
| 2.3.6–2.3.7 | PASS | `host [IPC_LOCK NET_ADMIN] false`; REST on 127.0.0.1 only; leader; dataset fingerprint `7d1d3973e7c0f83b`, channel 15; meshcop advertised |
| 2.3.8 | PASS / OPEN | dataset kept across two re-creates; `docker restart` and stop/start chain check OPEN |
| 2.3.9 | PASS | dataset kept; the backup after re-enable used the existing repository |
| 2.3.10–2.3.12 | OPEN | |
| 2.4.1–2.4.3 | PASS | the controller uses ephemeral UDP ports, not 5540 (runbook fixed) |
| 2.4.4 | PASS | Matter-over-Wi-Fi device commissioned **after a factory reset** and controllable |
| 2.4.5 | PASS | fabric id recorded locally |
| 2.4.6 | PASS / SKIP | reconnected by itself after `docker restart`; re-create SKIP (one NIC) |
| 2.4.7 | PASS* | reconnected and re-subscribed without re-commissioning. *Fabric id recheck OPEN |
| 2.5 | OPEN | the Thread device needs a factory reset; nothing reached the server yet |
| 2.6 | OPEN | |

### 3 to 6

Not started.

## Notes for the next run

- **Reset second-hand Matter devices first.** A Matter device last
  paired to another (now gone) controller must be factory-reset. Until
  then, nothing reaches the Matter server: the phone fails during its
  own Bluetooth/Wi-Fi step.
- **Never add the sticks in HA directly.** Adding a stick through ZHA
  or a USB path in Home Assistant fails ("failed to connect"). That is
  by design: the HA container has no radio access.
- **Pause autodeploy on a test box.** The box's autodeploy timer
  installed a new main over the branch build mid-run. Main's Keystone
  could not parse the branch statefile (the `home-assistant` container's
  new fields), so it stopped at the ingress step, with the main node
  agent (minor 22) installed. Nothing was disturbed, and reinstalling
  the branch build recovered it, but going back to main while a branch
  statefile is live is not safe.
- **One flaky USB moment.** The MG21 had one `cp210x … -71` USB
  disconnect when its port was first opened under passthrough. It
  re-enumerated at once and did not recur.
- **Another Thread network on the LAN.** Another Thread border router
  on the same LAN advertised its own, separate network. HA's preferred
  network stayed this one.
