package agent

import (
	"os"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// The two-stick case from the real-hardware run (#453 §2.1): a Zigbee
// stick created as /dev/ttyUSB1, both sticks replugged in the other
// order, so the by-id alias now resolves to /dev/ttyUSB0 and ttyUSB1 is
// the Thread RCP. Neither a plain docker start nor an unchanged spec
// hash may hand the container the node it was created with.
const movedStick = "/dev/serial/by-id/usb-Itead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_V2_0001-if00-port0"

func stickResolvesTo(t *testing.T, node *string) {
	t.Helper()
	prevEval, prevStat := evalSymlinks, statDevice
	evalSymlinks = func(p string) (string, error) {
		if p == movedStick {
			return *node, nil
		}
		return "", os.ErrNotExist
	}
	statDevice = func(string) (os.FileInfo, error) { return os.Stat(os.DevNull) }
	t.Cleanup(func() { evalSymlinks, statDevice = prevEval, prevStat })
}

func ensureZigbee(t *testing.T, h *harness) {
	t.Helper()
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "zigbee2mqtt", Image: testDigestImage,
			Labels:  map[string]string{SpecHashLabel: "v1"},
			Devices: []*wsv1.DeviceMapping{{HostPath: movedStick, ContainerPath: "/dev/zigbee"}},
		},
	}})
	if oe := res.GetError(); oe != nil {
		t.Fatalf("ensure: %v", oe)
	}
}

func TestStoppedContainerWhoseStickMovedIsRecreatedNotStarted(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	node := "/dev/ttyUSB1"
	stickResolvesTo(t, &node)

	ensureZigbee(t, h)
	fake.mu.Lock()
	first := fake.containers["zigbee2mqtt"]
	firstID := first.id
	if got := first.host.Devices; len(got) != 1 || got[0].PathOnHost != "/dev/ttyUSB1" {
		t.Fatalf("Devices = %+v", got)
	}
	if first.labels[DeviceSourcesLabel] == "" {
		t.Error("no device-sources label on the created container")
	}
	fake.mu.Unlock()

	// Unchanged stick, stopped container: a plain start, same container.
	fake.exitContainer("zigbee2mqtt", 2)
	ensureZigbee(t, h)
	fake.mu.Lock()
	if c := fake.containers["zigbee2mqtt"]; c.id != firstID || !c.running {
		t.Errorf("unchanged stick: id=%s running=%v, want %s restarted in place", c.id, c.running, firstID)
	}
	fake.mu.Unlock()

	// Replugged the other way round, container stopped by the unplug.
	fake.exitContainer("zigbee2mqtt", 2)
	node = "/dev/ttyUSB0"
	ensureZigbee(t, h)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	c := fake.containers["zigbee2mqtt"]
	if c.id == firstID {
		t.Fatal("the stopped container was started again with the node it was created with; that node is another stick now")
	}
	if got := c.host.Devices; len(got) != 1 || got[0].PathOnHost != "/dev/ttyUSB0" || got[0].PathInContainer != "/dev/zigbee" {
		t.Errorf("Devices after re-create = %+v, want /dev/ttyUSB0 -> /dev/zigbee", got)
	}
}

func TestInspectReportsAMovedStickAsDrift(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	node := "/dev/ttyUSB1"
	stickResolvesTo(t, &node)
	ensureZigbee(t, h)

	hash := func() string {
		t.Helper()
		res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
			InspectContainer: &wsv1.InspectContainer{Name: "zigbee2mqtt"},
		}})
		if oe := res.GetError(); oe != nil {
			t.Fatalf("inspect: %v", oe)
		}
		return res.GetInspectContainer().GetLabels()[SpecHashLabel]
	}
	if got := hash(); got != "v1" {
		t.Fatalf("converged container: spec hash = %q, want v1", got)
	}

	// Docker's restart policy started it again after a reboot that
	// swapped the sticks: running, same hash, wrong radio.
	node = "/dev/ttyUSB0"
	if got := hash(); got != "" {
		t.Errorf("moved stick: spec hash = %q, want it withheld so observe sees drift", got)
	}

	// Unplugged entirely: nothing to compare with, so not drift here.
	node = ""
	evalSymlinks = func(string) (string, error) { return "", os.ErrNotExist }
	if got := hash(); got != "v1" {
		t.Errorf("unplugged stick: spec hash = %q, want v1 (re-create would only fail NOT_FOUND)", got)
	}
}
