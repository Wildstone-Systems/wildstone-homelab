# Home automation: hardware-free validation matrix (#452)

The managed home-automation stack (MQTT, Home Assistant, Zigbee through a
network coordinator, Z-Wave, Matter, Thread, Bluetooth) is validated in
two tiers. Neither needs a radio, a LAN, multicast or IPv6 on the machine
running it.

## Tier 1: end to end, in ordinary CI

`reference-node.json` is the node every scenario starts from: the
`HomeAutomationReadiness` and `ListRadios` answers (protojson) of a
current agent on a prepared Debian/Ubuntu machine with a Z-Wave stick, a
Thread RCP and a Bluetooth adapter. `keystone/internal/fakenode` serves
it: an in-memory fleet that answers the Wardstone protocol as an agent at
a given protocol minor would, enforcing the agent-side rules (digest-pinned
images, devices present, secrets present, host mode without ports, unknown
ops below the minor that introduced them).

The tests drive the real control plane against it: the shipped catalog
(`packages/`), the orchestration API, the capability pipeline, plan
approval and the reconcile loop.

| Test (keystone/api) | What it proves |
|---|---|
| `TestHomeAutomationE2EReferenceStackFromEmpty` | Empty Statefile → base layer → the six packages, one approved plan at a time, in dependency order; params filled from the node; container shapes; Keystone restart mid-sequence; external steps; complete. Then: Keystone restart, host reboot, agent disconnect, a crashed service (drift plan), and a node lost mid-apply while switching Bluetooth to host-bluez. |
| `TestHomeAutomationE2EProtocolSkew` | A rolling agent upgrade 21 → 26: host networking (22) and host Routes (23) and the /run/dbus mount (26) refused by the reconciler at apply; the radio inventory (24) and readiness probe (25) refused by the aggregate before any plan; services untouched; each upgrade continues the setup. |
| `TestHomeAutomationE2EBrokenPrerequisites` | IPv6 off blocks Matter and Thread; IPv6 forwarding off blocks Thread (with the sysctl's evidence and fix), and Thread forced through the capability endpoint fails its health gate; D-Bus missing blocks host-bluez, and forced through, the reconciler refuses before the proxy container is touched. Fixing each lets the setup finish. |
| `TestHomeAutomationCatalogWiring` | Every component resolves to a shipped package the orchestration can fill without the household typing a value the node reports. |

```
cd keystone && go test ./api/ ./internal/fakenode/ -run 'TestHomeAutomation|TestAgentSideRules' -v
```

These run in CI's ordinary `go test -race ./...`.

## Tier 2: the real images, behind `WILDSTONE_DOCKER_TESTS`

The image contracts — real Mosquitto authentication, Home Assistant's
seeded proxy trust and host networking, host BlueZ against a fake BlueZ,
Z-Wave JS UI's WebSocket, the OTBR image and its REST API, the Matter.js
Server WebSocket, and Caddy reaching a host service — need a Docker daemon
and pull digest-pinned images, so they stay opt-in (CI's runner has no
Docker socket on purpose):

```
cd keystone && WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ ./reconcile/ -count=1 -timeout 40m -v \
  -run 'TestSecuredMosquitto|TestShippedMQTTConsumers|TestSeededHomeAssistant|TestHomeAssistantOnTheHostNetwork|TestHomeAssistantHostBlueZFixtureDiscovery|TestZwaveJSUIServesHomeAssistant|TestOTBR|TestMatterServerServesHomeAssistant|TestCaddyOnABridgeReachesAHostService'
```

Where ghcr.io blobs are unreachable, add `WILDSTONE_MATTER_STANDIN=1`
(the Matter test then builds a stand-in and says so). The OTBR dataset
test additionally needs `WILDSTONE_OTBR_RCP_SIM` and a kernel with IPv6;
it skips otherwise.

What neither tier can prove — real radios, a real LAN, a real Home
Assistant integration — is in the VM runbook notes of each slice.
