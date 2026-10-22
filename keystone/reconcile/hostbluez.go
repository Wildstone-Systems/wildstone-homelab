package reconcile

// Host BlueZ, checked before the old container goes (#445).
//
// A container that mounts the host's /run/dbus (the one host system mount,
// resource.HostSystemMounts) is asking to use the host's Bluetooth through
// BlueZ: home-assistant with bluetooth: host-bluez. None of that is
// Wildstone's to install — BlueZ, the D-Bus system bus and the adapter
// belong to the host — and when one is missing nothing fails where
// Wildstone can see it: Docker starts the container, Home Assistant logs
// that it found no adapter, and a household that switched from proxy mode
// to get local Bluetooth ends up with neither, behind a green step.
//
// So before such a container is created or replaced, the node's
// HomeAutomationReadiness (protocol minor 26, #436; any agent that accepts
// the mount speaks it) is read, and DEFINITE evidence of absence refuses
// the step before any pull or EnsureContainer, leaving the running
// container — a proxy-mode Home Assistant during a switch — untouched:
//
//   - dbus_system_bus MISSING or UNSUPPORTED: no system bus to mount;
//   - bluez_service MISSING or UNSUPPORTED: BlueZ not installed or not
//     running;
//   - no bluetooth_adapter READY while one is MISSING or UNSUPPORTED: no
//     controller, or every controller rfkill-blocked.
//
// UNKNOWN is not refused. The agent reports it when it looked and could
// not tell — most often systemd could not be asked about bluetooth.service
// on a host that runs bluetoothd some other way — and refusing there would
// block a working setup on the probe's blind spot. The step goes ahead and
// the same evidence stays visible at GET /api/v1/nodes/{node}/probes/
// home-automation. A readiness op that fails outright is refused, as the
// host-port check does: no evidence is not the same as no problem, and the
// next pass retries.

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// The readiness checks host BlueZ depends on, by their wire names
// (wardstone/internal/agent/readiness.go).
const (
	readinessDBusSystemBus    = "dbus_system_bus"
	readinessBlueZService     = "bluez_service"
	readinessBluetoothAdapter = "bluetooth_adapter"
)

// hostBlueZProblems returns, from one readiness result, each definite
// reason host BlueZ cannot work on the node; nil when nothing is
// definitely missing.
func hostBlueZProblems(res *wsv1.HomeAutomationReadinessResult) []string {
	absent := func(s wsv1.ReadinessState) bool {
		return s == wsv1.ReadinessState_READINESS_STATE_MISSING || s == wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED
	}
	describe := func(c *wsv1.ReadinessCheck) string {
		state := strings.ToLower(strings.TrimPrefix(c.GetState().String(), "READINESS_STATE_"))
		s := fmt.Sprintf("%s %s", c.GetCheck(), state)
		if c.GetSubject() != "" {
			s += " (" + c.GetSubject() + ")"
		}
		if c.GetDetail() != "" {
			s += ": " + c.GetDetail()
		}
		return s
	}
	var problems, adapterProblems []string
	adapterReady := false
	for _, c := range res.GetChecks() {
		switch c.GetCheck() {
		case readinessDBusSystemBus, readinessBlueZService:
			if absent(c.GetState()) {
				problems = append(problems, describe(c))
			}
		case readinessBluetoothAdapter:
			if c.GetState() == wsv1.ReadinessState_READINESS_STATE_READY {
				adapterReady = true
			} else if absent(c.GetState()) {
				adapterProblems = append(adapterProblems, describe(c))
			}
		}
	}
	if !adapterReady {
		problems = append(problems, adapterProblems...)
	}
	return problems
}

// refuseIfHostBlueZMissing returns a refusal when the container about to be
// created or replaced mounts the host's D-Bus for BlueZ and the node's
// readiness evidence says BlueZ cannot work there; "" to go ahead.
func (r *Reconciler) refuseIfHostBlueZMissing(ctx context.Context, nodeID string, want resource.Resource) string {
	if !want.Container.UsesHostSystemMount() {
		return ""
	}
	res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_HomeAutomationReadiness{
		HomeAutomationReadiness: &wsv1.HomeAutomationReadiness{},
	}})
	if err == nil && res.GetError() != nil {
		err = fmt.Errorf("%s: %s", res.GetError().GetCode(), res.GetError().GetMessage())
	}
	if err != nil {
		return fmt.Sprintf("Container/%s mounts the host's /run/dbus to use its Bluetooth through BlueZ, and node %s's "+
			"readiness could not be read (%v); refused before the existing container is touched",
			want.Metadata.Name, nodeID, err)
	}
	problems := hostBlueZProblems(res.GetHomeAutomationReadiness())
	if len(problems) == 0 {
		return ""
	}
	return fmt.Sprintf("Container/%s mounts the host's /run/dbus to use its Bluetooth through BlueZ, but node %s is "+
		"not ready for it: %s. Refused before the existing container is touched: started anyway it would run with no "+
		"Bluetooth at all. Install and start BlueZ (bluetooth.service) with a working, unblocked adapter on %s, or "+
		"use bluetooth: proxy (an ESPHome Bluetooth proxy), which needs nothing from this machine",
		want.Metadata.Name, nodeID, strings.Join(problems, "; "), nodeID)
}
