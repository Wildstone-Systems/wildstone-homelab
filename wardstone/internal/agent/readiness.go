package agent

// HomeAutomationReadiness (protocol minor 26): the platform-neutral half
// — the closed set of check names and the helpers every check result is
// built with. The reads live in readiness_linux.go; readiness_other.go
// answers every check UNSUPPORTED.

import (
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// Readiness check names. Closed, like every vocabulary on this wire:
// Keystone and the Console key on them, so a new prerequisite is a new
// constant here and a protocol comment, never a free-form string.
const (
	checkIPv6             = "ipv6"
	checkInterfaceIPv6    = "interface_ipv6"
	checkIPv6Forwarding   = "ipv6_forwarding"
	checkMulticast        = "multicast"
	checkDBusSystemBus    = "dbus_system_bus"
	checkBlueZService     = "bluez_service"
	checkBluetoothAdapter = "bluetooth_adapter"
	checkTun              = "tun"
)

// readinessChecks is every check in the order a result lists them.
var readinessChecks = []string{
	checkIPv6, checkInterfaceIPv6, checkIPv6Forwarding, checkMulticast,
	checkDBusSystemBus, checkBlueZService, checkBluetoothAdapter, checkTun,
}

func readinessCheck(name string, state wsv1.ReadinessState, subject, value, detail string) *wsv1.ReadinessCheck {
	return &wsv1.ReadinessCheck{Check: name, State: state, Subject: subject, Value: cleanAttr(value), Detail: detail}
}
