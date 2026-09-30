package api

// The home-automation component table (#446). Everything the aggregate
// knows about a protocol's dependencies, radios, node prerequisites and
// Home-Assistant-side steps lives in this one table, keyed on catalog
// CAPABILITY names rather than on package names: a package that appears
// in the catalog under one of these capabilities lights its component up
// with no code change, and a component whose capability no catalog
// package provides reports `unavailable` with that reason. The Console
// renders what this says and never re-derives it (S17–S20).

import (
	"fmt"
	"strings"
)

// Component topologies. "managed" parts exist only when Wildstone runs
// Home Assistant, "external" only when it adapts one the household
// already has, and "shared" parts serve either.
const (
	haTopologyManaged  = "managed"
	haTopologyExternal = "external"
	haTopologyShared   = "shared"
)

// Component ids. Stable wire vocabulary: the Console, the orchestration
// API (#447) and `?select=` all key on them.
const (
	haMQTT          = "mqtt"
	haHomeAssistant = "home-assistant"
	haExternalHA    = "home-automation"
	haZigbee        = "zigbee"
	haZWave         = "zwave"
	haMatter        = "matter"
	haThread        = "thread"
	haBluetooth     = "bluetooth"
)

// haComponentSpec is one row of the table.
type haComponentSpec struct {
	ID    string
	Title string
	// Capability is the catalog capability whose packages provide this
	// component. A mode component (ModeParams set) is a setting of that
	// capability's package rather than a package of its own.
	Capability string
	Topology   string
	// Protocol marks the parts a household chooses as goals (Zigbee,
	// Z-Wave, ...), as opposed to the infrastructure they need.
	Protocol bool
	// Requires are component ids that must be ready before this one is
	// proposed. The catalog adds to them: a package's `requires` trait
	// provided by another component's package becomes a dependency too.
	Requires []string
	// ModeParams are the param names (first match wins) that carry this
	// component's mode on its capability's package, and OnModes the
	// values that mean "on". A catalog package without such a param makes
	// the component unavailable.
	ModeParams []string
	OnModes    []string
	// RadioKind is the ListRadios kind this component drives. Radio
	// candidates and ownership are evaluated only for RadioClaims
	// components; a Bluetooth adapter is reached through BlueZ, not
	// handed to a container, so it is judged by readiness checks instead.
	RadioKind     string
	RadioClaims   bool
	RadioRequired bool
	// Checks are the readiness-probe checks this component needs on its
	// node, optionally only in one mode.
	Checks []haCheckNeed
	// Steps are what somebody must do outside Wildstone, in order.
	Steps []haStepSpec
	// ManagedHAParams are settings a Wildstone-run Home Assistant needs
	// for this component to reach it (Zigbee2MQTT needs Home Assistant's
	// own MQTT login). Checked only when that package declares the param.
	ManagedHAParams map[string]string
}

type haCheckNeed struct {
	Check string
	// Mode, when set, limits the need to that mode of a mode component.
	Mode string
}

// haStepSpec is one Home-Assistant-side (or otherwise external) step.
type haStepSpec struct {
	ID    string
	Title string
	// Mode, when set, limits the step to that mode.
	Mode         string
	Instructions func(f haFacts) []string
	// Verify reports whether Wildstone can see the step done. nil means
	// it cannot observe it at all: the step is listed "unverified" and
	// never holds the component in external_step, so an orchestrator
	// cannot loop on something no read can ever confirm.
	Verify func(b *haBuild, c *haComponent) (status string, evidence []haEvidence)
	// Action is what the Console may do about a pending step.
	Action func(c *haComponent) *haAction
}

// haFacts are the deployment-specific values a step's instructions name.
type haFacts struct {
	Node        string
	HostAddress string
	MQTTHost    string
	MQTTPort    string
	ZWaveHost   string
	ZWavePort   string
}

func orHost(v string, f haFacts) string {
	if v != "" {
		return v
	}
	return f.host()
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

func (f haFacts) host() string {
	if f.HostAddress != "" {
		return f.HostAddress
	}
	if f.Node != "" {
		return "the address of " + f.Node
	}
	return "the machine's address"
}

var readinessIPv6Needs = []haCheckNeed{{Check: "ipv6"}, {Check: "interface_ipv6"}, {Check: "multicast"}}

// haComponents is the table, in dependency order: a component's static
// Requires always appear above it.
var haComponents = []haComponentSpec{
	{
		ID: haMQTT, Title: "MQTT broker", Capability: "mqtt", Topology: haTopologyShared,
	},
	{
		ID: haHomeAssistant, Title: "Home Assistant (runs here)", Capability: "home-assistant", Topology: haTopologyManaged,
	},
	{
		ID: haExternalHA, Title: "Existing Home Assistant", Capability: "home-automation", Topology: haTopologyExternal,
		Steps: []haStepSpec{
			{
				ID: "publisher_token", Title: "Give the adapter its publisher credential",
				Instructions: func(f haFacts) []string {
					return []string{
						fmt.Sprintf("Mint the home-automation publisher credential for %s (Console action, or `wild household publisher home-automation -node %s`).", nodeOr(f.Node), nodeOr(f.Node)),
						"Wildstone places it on the machine as secret/home-automation-publisher and writes the Home Assistant file once it exists.",
					}
				},
				Verify: verifyPublisher,
				Action: func(c *haComponent) *haAction {
					return &haAction{Kind: "mint_publisher", Label: "Create the publisher credential", Capability: "home-automation", Node: c.Node}
				},
			},
			{
				ID: "include_package_file", Title: "Include Wildstone's file from Home Assistant's configuration.yaml",
				Instructions: func(f haFacts) []string {
					return []string{
						"Add to configuration.yaml on your Home Assistant:",
						"homeassistant:\n  packages: !include_dir_named /etc/wildstone/home-automation",
						"Restart Home Assistant. Wildstone cannot see inside it, so check this off yourself.",
					}
				},
			},
		},
	},
	{
		ID: haZigbee, Title: "Zigbee", Capability: "zigbee", Topology: haTopologyShared, Protocol: true,
		Requires:  []string{haMQTT},
		RadioKind: "zigbee", RadioClaims: true,
		ManagedHAParams: map[string]string{"mqtt_enabled": "true"},
		Steps: []haStepSpec{{
			ID: "ha_mqtt_integration", Title: "Connect Home Assistant to the MQTT broker",
			Instructions: func(f haFacts) []string {
				broker := f.MQTTHost
				if broker == "" {
					broker = f.host()
				}
				port := f.MQTTPort
				if port == "" {
					port = "1883"
				}
				return []string{
					"In Home Assistant: Settings → Devices & services → Add integration → MQTT.",
					fmt.Sprintf("Broker %s, port %s, username home-assistant, and the password Wildstone keeps as secret/mqtt-home-assistant on %s.", broker, port, nodeOr(f.Node)),
					"A Home Assistant Wildstone runs has these as WILDSTONE_MQTT_HOST, WILDSTONE_MQTT_PORT and WILDSTONE_MQTT_PASSWORD: docker exec home-assistant printenv on that machine.",
					"Zigbee devices then appear through MQTT discovery.",
				}
			},
		}},
	},
	{
		ID: haZWave, Title: "Z-Wave", Capability: "zwave", Topology: haTopologyShared, Protocol: true,
		RadioKind: "zwave", RadioClaims: true, RadioRequired: true,
		Steps: []haStepSpec{{
			ID: "ha_zwave_integration", Title: "Add the Z-Wave integration in Home Assistant",
			Instructions: func(f haFacts) []string {
				return []string{
					"In Home Assistant: Settings → Devices & services → Add integration → Z-Wave.",
					fmt.Sprintf("Untick \"Use the Z-Wave Supervisor add-on\" and enter ws://%s:%s.", orHost(f.ZWaveHost, f), orDefault(f.ZWavePort, "3000")),
				}
			},
		}},
	},
	{
		ID: haMatter, Title: "Matter", Capability: "matter", Topology: haTopologyShared, Protocol: true,
		Checks: readinessIPv6Needs,
		Steps: []haStepSpec{{
			ID: "ha_matter_integration", Title: "Add the Matter integration in Home Assistant",
			Instructions: func(f haFacts) []string {
				// matter-matterjs (matter:server, #442) binds its
				// unauthenticated WebSocket to loopback only, so the URL is
				// the same everywhere and only a host-networked Home
				// Assistant on the Matter server's own machine can use it.
				return []string{
					"In Home Assistant: Settings → Devices & services → Add integration → Matter.",
					"Untick the add-on option and enter ws://127.0.0.1:5580/ws.",
					fmt.Sprintf("That address works only for a Home Assistant running on %s itself, on the host's network (the Home Assistant Wildstone runs is); the Matter server does not listen anywhere else.", nodeOr(f.Node)),
				}
			},
		}},
	},
	{
		ID: haThread, Title: "Thread border router", Capability: "thread", Topology: haTopologyShared, Protocol: true,
		RadioKind: "thread", RadioClaims: true, RadioRequired: true,
		Checks: []haCheckNeed{{Check: "ipv6"}, {Check: "interface_ipv6"}, {Check: "ipv6_forwarding"}, {Check: "multicast"}, {Check: "tun"}},
		Steps: []haStepSpec{
			{
				ID: "ha_otbr_integration", Title: "Add the Open Thread Border Router integration in Home Assistant",
				Instructions: func(f haFacts) []string {
					return []string{
						"In Home Assistant: Settings → Devices & services → Add integration → Open Thread Border Router.",
						fmt.Sprintf("Enter http://%s:8081.", f.host()),
					}
				},
			},
			{
				ID: "thread_credentials", Title: "Share the Thread network with your phone",
				Instructions: func(haFacts) []string {
					return []string{
						"In Home Assistant: Settings → Devices & services → Thread → make this border router's network preferred.",
						"In the Home Assistant companion app: Settings → Companion app → Troubleshooting → Sync Thread credentials.",
					}
				},
			},
		},
	},
	{
		ID: haBluetooth, Title: "Bluetooth", Capability: "home-assistant", Topology: haTopologyManaged, Protocol: true,
		Requires:   []string{haHomeAssistant},
		ModeParams: []string{"bluetooth", "bluetooth_mode"},
		OnModes:    []string{"host-bluez", "proxy"},
		RadioKind:  "bluetooth",
		Checks: []haCheckNeed{
			{Check: "dbus_system_bus", Mode: "host-bluez"},
			{Check: "bluez_service", Mode: "host-bluez"},
			{Check: "bluetooth_adapter", Mode: "host-bluez"},
		},
		Steps: []haStepSpec{{
			ID: "esphome_bluetooth_proxies", Title: "Adopt your ESPHome Bluetooth proxies in Home Assistant", Mode: "proxy",
			Instructions: func(haFacts) []string {
				return []string{
					"Flash an ESP32 as a Bluetooth proxy (esphome.io/projects → Bluetooth proxy) on the same network.",
					"In Home Assistant: Settings → Devices & services → accept the discovered ESPHome device.",
				}
			},
		}},
	},
}

func haSpec(id string) (haComponentSpec, bool) {
	for _, s := range haComponents {
		if s.ID == id {
			return s, true
		}
	}
	return haComponentSpec{}, false
}

func nodeOr(node string) string {
	if node == "" {
		return "<node>"
	}
	return node
}

// readinessRemedy is the exact manual fix for a failed readiness check on
// one node, in the words and commands an operator can run. Nothing here
// is executed by Wildstone: host kernel and service settings are the
// operator's, which is why these arrive as instructions and a "Check
// again" rather than as a plan.
func readinessRemedy(check, node, subject string) []string {
	on := " on " + nodeOr(node)
	iface := subject
	if iface == "" {
		iface = "<interface>"
	}
	switch check {
	case "ipv6":
		return []string{
			"IPv6 is switched off" + on + ".",
			"Run: sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.default.disable_ipv6=0",
			"Persist it in /etc/sysctl.d/60-wildstone-ipv6.conf, and remove ipv6.disable=1 from the kernel command line if it is there.",
		}
	case "interface_ipv6":
		return []string{
			"Interface " + iface + on + " has no IPv6 address, so Matter and Thread devices cannot reach it.",
			"Run: sudo sysctl -w net.ipv6.conf." + iface + ".disable_ipv6=0 and make sure the interface accepts router advertisements or has a link-local address.",
		}
	case "ipv6_forwarding":
		return []string{
			"IPv6 forwarding is off" + on + "; a Thread border router routes IPv6 between Thread and your LAN.",
			"Run: sudo sysctl -w net.ipv6.conf.all.forwarding=1 net.ipv6.conf." + iface + ".accept_ra=2",
			"Persist both in /etc/sysctl.d/60-wildstone-thread.conf (accept_ra=2 keeps the machine's own IPv6 route while forwarding).",
		}
	case "multicast":
		return []string{
			"Interface " + iface + on + " has multicast off, so device discovery (mDNS) cannot work.",
			"Run: sudo ip link set " + iface + " multicast on, and persist it in the interface's network configuration.",
		}
	case "dbus_system_bus":
		return []string{
			"The D-Bus system bus is not running" + on + ".",
			"Install and start it: sudo apt install dbus && sudo systemctl enable --now dbus",
		}
	case "bluez_service":
		return []string{
			"The BlueZ Bluetooth service is not running" + on + ".",
			"Install and start it: sudo apt install bluez && sudo systemctl enable --now bluetooth",
		}
	case "bluetooth_adapter":
		return []string{
			"No usable Bluetooth adapter was found" + on + ".",
			"Plug in a USB Bluetooth adapter, or unblock the built-in one: sudo rfkill unblock bluetooth",
			"Or choose Bluetooth proxies instead, which need no adapter on this machine.",
		}
	case "tun":
		return []string{
			"/dev/net/tun is missing" + on + "; the Thread border router needs a TUN device.",
			"Run: sudo modprobe tun, and add tun to /etc/modules-load.d/tun.conf so it survives a reboot.",
		}
	}
	return []string{"Fix " + check + on + ", then check again."}
}

// readinessCheckTitle is the household wording for a readiness check.
var readinessCheckTitle = map[string]string{
	"ipv6":              "IPv6",
	"interface_ipv6":    "IPv6 on the network interface",
	"ipv6_forwarding":   "IPv6 forwarding",
	"multicast":         "Multicast on the network interface",
	"dbus_system_bus":   "D-Bus system bus",
	"bluez_service":     "BlueZ Bluetooth service",
	"bluetooth_adapter": "Bluetooth adapter",
	"tun":               "TUN device",
}

// interfaceScopedChecks repeat once per candidate interface; only the
// interfaces carrying the default route decide them when any do.
var interfaceScopedChecks = map[string]bool{"interface_ipv6": true, "multicast": true}

func splitCSV(values []string) []string {
	var out []string
	for _, v := range values {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
