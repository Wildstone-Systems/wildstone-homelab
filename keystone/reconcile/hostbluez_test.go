package reconcile

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// The host BlueZ preflight (#445): a container mounting the host's
// /run/dbus is refused, before the node is touched, when the node's
// readiness evidence says BlueZ cannot work there.

func check(name string, state wsv1.ReadinessState, subject, detail string) *wsv1.ReadinessCheck {
	return &wsv1.ReadinessCheck{Check: name, State: state, Subject: subject, Detail: detail}
}

var (
	ready       = wsv1.ReadinessState_READINESS_STATE_READY
	missing     = wsv1.ReadinessState_READINESS_STATE_MISSING
	unknown     = wsv1.ReadinessState_READINESS_STATE_UNKNOWN
	unsupported = wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED
)

func bluezReady() []*wsv1.ReadinessCheck {
	return []*wsv1.ReadinessCheck{
		check("ipv6", ready, "/proc/net/if_inet6", ""),
		check("dbus_system_bus", ready, "/run/dbus/system_bus_socket", "D-Bus system bus socket present"),
		check("bluez_service", ready, "bluetooth.service", "BlueZ is running"),
		check("bluetooth_adapter", ready, "hci0", "hci0 present"),
	}
}

func TestHostBlueZProblems(t *testing.T) {
	with := func(replace ...*wsv1.ReadinessCheck) *wsv1.HomeAutomationReadinessResult {
		checks := bluezReady()
		for _, r := range replace {
			for i, c := range checks {
				if c.Check == r.Check {
					checks[i] = r
				}
			}
		}
		return &wsv1.HomeAutomationReadinessResult{PlatformSupported: true, Checks: checks}
	}
	for name, tc := range map[string]struct {
		res  *wsv1.HomeAutomationReadinessResult
		want []string // substrings, one per problem; nil for none
	}{
		"all ready":       {with(), nil},
		"no bus":          {with(check("dbus_system_bus", missing, "/run/dbus/system_bus_socket", "no D-Bus system bus socket")), []string{"dbus_system_bus missing (/run/dbus/system_bus_socket): no D-Bus system bus socket"}},
		"no bluez":        {with(check("bluez_service", missing, "bluetooth.service", "BlueZ is not installed")), []string{"bluez_service missing (bluetooth.service): BlueZ is not installed"}},
		"no adapter":      {with(check("bluetooth_adapter", missing, "/sys/class/bluetooth", "no Bluetooth controller")), []string{"bluetooth_adapter missing"}},
		"unsupported":     {with(check("dbus_system_bus", unsupported, "", "")), []string{"dbus_system_bus unsupported"}},
		"bluez unknown":   {with(check("bluez_service", unknown, "bluetooth.service", "connect to systemd: refused")), nil},
		"adapter unknown": {with(check("bluetooth_adapter", unknown, "/sys/class/bluetooth", "read: EIO")), nil},
		// One blocked adapter beside a working one is not a problem.
		"one blocked of two": {&wsv1.HomeAutomationReadinessResult{Checks: append(bluezReady(),
			check("bluetooth_adapter", missing, "hci1", "hci1 is soft-blocked by rfkill"))}, nil},
		"only blocked": {with(check("bluetooth_adapter", missing, "hci0", "hci0 is soft-blocked by rfkill")), []string{"rfkill"}},
		// Checks unrelated to Bluetooth never refuse it.
		"no multicast": {&wsv1.HomeAutomationReadinessResult{Checks: append(bluezReady(), check("multicast", missing, "eth0", ""))}, nil},
	} {
		got := hostBlueZProblems(tc.res)
		if len(got) != len(tc.want) {
			t.Errorf("%s: problems = %q, want %d", name, got, len(tc.want))
			continue
		}
		for i := range got {
			if !strings.Contains(got[i], tc.want[i]) {
				t.Errorf("%s: problem %q does not say %q", name, got[i], tc.want[i])
			}
		}
	}
}

// hubWithBluetooth is a Home-Assistant-shaped container in the given
// bluetooth mode, rendered as packages/home-assistant renders it, beside
// its config volume.
func hubWithBluetooth(mode string) []resource.Resource {
	vol := volumeResource("hub-config")
	hub := containerResource("hub", testDigestImage)
	hub.Metadata.Owner, hub.Metadata.Package = "capability/home-assistant", "home-assistant"
	vol.Metadata.Owner, vol.Metadata.Package = hub.Metadata.Owner, hub.Metadata.Package
	hub.Container.NetworkMode = resource.NetworkModeHost
	hub.Container.Mounts = []resource.Mount{{Source: "hub-config", Target: "/config"}}
	if mode == "host-bluez" {
		hub.Container.Mounts = append(hub.Container.Mounts, resource.Mount{Source: "/run/dbus", Target: "/run/dbus", ReadOnly: true})
		hub.Container.CapAdd = []string{"NET_ADMIN", "NET_RAW"}
	}
	return []resource.Resource{vol, hub}
}

func TestHostBlueZMissingIsRefusedBeforeTheOldContainerIsReplaced(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = minor(resource.HostSystemMountProtocolMinor)
	r.Approve = func(*plan.Plan) bool { return true }
	ctx := context.Background()
	configOnly := []*wsv1.Mount{{Source: "hub-config", Target: "/config"}}

	// proxy: no host access, and the readiness op is never asked.
	if _, err := r.Reconcile(ctx, "node-a", hubWithBluetooth("proxy")); err != nil {
		t.Fatal(err)
	}
	if agent.opCount("HomeAutomationReadiness") != 0 {
		t.Error("a proxy-mode container's step read the node's readiness")
	}
	if mounts, caps, _ := agent.containerHostAccess("hub"); !mountsEqual(mounts, configOnly) || len(caps) != 0 {
		t.Fatalf("proxy mode: mounts %v caps %v", mounts, caps)
	}
	ensures := agent.opCount("EnsureContainer")

	// Switching to host-bluez on a node without BlueZ is refused, with the
	// evidence, and the proxy-mode container keeps running.
	agent.setReadiness(append(bluezReady()[:2],
		check("bluez_service", missing, "bluetooth.service", "BlueZ is not installed"),
		check("bluetooth_adapter", ready, "hci0", "hci0 present"))...)
	outcome, err := r.Reconcile(ctx, "node-a", hubWithBluetooth("host-bluez"))
	if err != nil {
		t.Fatal(err)
	}
	msg := hubStepError(t, outcome)
	for _, want := range []string{"/run/dbus", "node-a", "bluez_service missing", "BlueZ is not installed", "before the existing container is touched", "bluetooth: proxy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not say %q", msg, want)
		}
	}
	if agent.opCount("EnsureContainer") != ensures || agent.opCount("StopContainer") != 0 || agent.opCount("RemoveContainer") != 0 {
		t.Error("the node's container was touched before the refusal")
	}
	if mounts, _, ok := agent.containerHostAccess("hub"); !ok || !mountsEqual(mounts, configOnly) {
		t.Errorf("old proxy-mode container disturbed: %v %v", mounts, ok)
	}

	// BlueZ is installed and running: the same desired state converges
	// with exactly the documented host access, /config still first.
	agent.setReadiness(bluezReady()...)
	outcome, err = r.Reconcile(ctx, "node-a", hubWithBluetooth("host-bluez"))
	if err != nil {
		t.Fatal(err)
	}
	if msg := hubStepError(t, outcome); msg != "" {
		t.Fatalf("host-bluez with BlueZ ready: %s", msg)
	}
	wantMounts := append(append([]*wsv1.Mount(nil), configOnly...), &wsv1.Mount{Source: "/run/dbus", Target: "/run/dbus", ReadOnly: true})
	if mounts, caps, _ := agent.containerHostAccess("hub"); !mountsEqual(mounts, wantMounts) || !reflect.DeepEqual(caps, []string{"NET_ADMIN", "NET_RAW"}) {
		t.Errorf("host-bluez: mounts %v caps %v", mounts, caps)
	}

	// And back to off: the host access goes, the config volume stays.
	outcome, err = r.Reconcile(ctx, "node-a", hubWithBluetooth("off"))
	if err != nil {
		t.Fatal(err)
	}
	if msg := hubStepError(t, outcome); msg != "" {
		t.Fatalf("off: %s", msg)
	}
	if mounts, caps, _ := agent.containerHostAccess("hub"); !mountsEqual(mounts, configOnly) || len(caps) != 0 {
		t.Errorf("off: mounts %v caps %v", mounts, caps)
	}
	if !agent.volumeExists("hub-config") {
		t.Error("the config volume did not survive the mode switches")
	}
}

// An agent too old for the /run/dbus mount is refused by name, before the
// readiness op or the node's container.
func TestHostBlueZNeedsTheAgentMinor(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.AgentProtocolMinor = minor(resource.HostSystemMountProtocolMinor - 1)
	r.Approve = func(*plan.Plan) bool { return true }
	outcome, err := r.Reconcile(context.Background(), "node-a", hubWithBluetooth("host-bluez"))
	if err != nil {
		t.Fatal(err)
	}
	msg := hubStepError(t, outcome)
	if !strings.Contains(msg, "minor 27") || !strings.Contains(msg, "/run/dbus") {
		t.Errorf("refusal %q does not name the minor and the mount", msg)
	}
	if agent.opCount("EnsureContainer") != 0 || agent.opCount("HomeAutomationReadiness") != 0 {
		t.Error("the node was asked before the minor gate")
	}
}

func mountsEqual(a, b []*wsv1.Mount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].GetSource() != b[i].GetSource() || a[i].GetTarget() != b[i].GetTarget() || a[i].GetReadOnly() != b[i].GetReadOnly() {
			return false
		}
	}
	return true
}
