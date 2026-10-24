# W050 slice 11: host-bound packages (devices, labels, `pinned`, host networking)

> Draft issue body. Plan: §5.2, §6.5, §6.7.

## Goal

Packages that need a radio, GPU, D-Bus or the host network run on k3s on the machine that has the hardware, with every host grant visible and admission-checked.

## User-visible outcome

- Home Assistant, Zigbee2MQTT, Z-Wave JS UI, OTBR, Matter and Frigate work on the new engine.
- Node page lists "Zigbee stick found", "Intel GPU found".

## Scope

- Wardstone writes node labels from `ListRadios` and `HomeAutomationReadiness` (`has-zigbee`, `has-zwave`, `has-thread`, `has-igpu`, `has-coral`, `has-bluez`).
- Device plugins configured from the `hostaccess.go:20-31` allowlist.
- Translator: `devices` → device resources + node selector; `networkMode: host` → `hostNetwork` in `wildstone-host`; `/run/dbus` → read-only `hostPath`; capabilities from the allowlist.
- Admission exemption bound to signed package digests (#38).
- `hostport.go` and `hostbluez.go` checks run against the pinned node.
- `host/<port>` upstream → Service for the host-network pod.
- Packages: `home-assistant` (#83, #87), `zigbee-zigbee2mqtt`, `zwave-zwave-js-ui`, `thread-otbr`, `matter-matterjs`, `cameras-frigate`, `vpn-gluetun`.

## Non-goals

Moving a pinned package between machines automatically (it never moves).

## Dependencies

Slice 6 (SeedFiles for Home Assistant).

## Acceptance criteria

- [ ] k3d admission tests: host network refused without exemption; allowed with a signed digest.
- [ ] QEMU e2e with an emulated USB serial device (`-device usb-serial`) **(inferred feasible)**: Zigbee2MQTT pod lands on the labeled node.
- [ ] Home Assistant LAN discovery and BlueZ checked on owner hardware (evidence record).
- [ ] Unplugging the stick: pod Pending with reason shown in Console.

## Risks

Real radios are untestable in hosted CI; owner hardware runs required.

## Size

**XL.** Device plugins, labels, admission exemptions and seven packages with real hardware.

## ADR amendment

No.
