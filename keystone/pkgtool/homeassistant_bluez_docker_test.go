package pkgtool

// An opt-in test of bluetooth: host-bluez (#445) against a FIXTURE BlueZ,
// since the machine running it has no Bluetooth to lend: a private D-Bus
// system bus with a fake org.bluez on it (one adapter, hci0, that reports
// one BLE device when discovery starts), served from a directory that
// stands in for the host's /run/dbus.
//
// The container under test is the pinned Home Assistant image started
// with exactly the host access the package renders — its network mode,
// its capAdd, its mounts, with /run/dbus pointed at the fixture's
// directory — running a BLE scan with the library Home Assistant's own
// Bluetooth stack is built on (bleak), rather than booting Home Assistant.
// It checks that the scan finds the fixture device over the read-only
// bus, that the bus directory really is read-only, that the container has
// NET_ADMIN and NET_RAW and not SYS_ADMIN, and that proxy mode sees no bus
// at all — and, around a proxy -> host-bluez -> proxy switch, that a file
// in /config survives each replacement.
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run TestHomeAssistantHostBlueZFixtureDiscovery -v

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// fixtureBusConf is a system bus that lets anyone own and call anything:
// the fixture's policy, not a host's.
const fixtureBusConf = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>system</type>
  <listen>unix:path=/run/dbus/system_bus_socket</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow user="*"/>
    <allow own="*"/>
    <allow send_destination="*"/>
    <allow receive_sender="*"/>
  </policy>
</busconfig>
`

// fakeBlueZPy starts the bus and serves org.bluez on it with dbus-fast,
// which the Home Assistant image ships.
const fakeBlueZPy = `
import asyncio, os, subprocess, time
from dbus_fast import BusType, Variant
from dbus_fast.aio import MessageBus
from dbus_fast.service import ServiceInterface, method, dbus_property, PropertyAccess

subprocess.Popen(["dbus-daemon", "--config-file=/fixture/bus.conf", "--nofork", "--nopidfile"])
for _ in range(100):
    if os.path.exists("/run/dbus/system_bus_socket"):
        break
    time.sleep(0.1)

ADAPTER = "/org/bluez/hci0"
DEVICE = ADAPTER + "/dev_AA_BB_CC_DD_EE_FF"

class Device(ServiceInterface):
    def __init__(self):
        super().__init__("org.bluez.Device1")
    @dbus_property(access=PropertyAccess.READ)
    def Address(self) -> "s": return "AA:BB:CC:DD:EE:FF"
    @dbus_property(access=PropertyAccess.READ)
    def AddressType(self) -> "s": return "public"
    @dbus_property(access=PropertyAccess.READ)
    def Name(self) -> "s": return "wildstone-445-fixture"
    @dbus_property(access=PropertyAccess.READ)
    def Alias(self) -> "s": return "wildstone-445-fixture"
    @dbus_property(access=PropertyAccess.READ)
    def Adapter(self) -> "o": return ADAPTER
    @dbus_property(access=PropertyAccess.READ)
    def Paired(self) -> "b": return False
    @dbus_property(access=PropertyAccess.READ)
    def Connected(self) -> "b": return False
    @dbus_property(access=PropertyAccess.READ)
    def RSSI(self) -> "n": return -42
    @dbus_property(access=PropertyAccess.READ)
    def UUIDs(self) -> "as": return ["0000181a-0000-1000-8000-00805f9b34fb"]
    @dbus_property(access=PropertyAccess.READ)
    def ManufacturerData(self) -> "a{qv}": return {0xFFFF: Variant("ay", b"\x04\x45")}
    @dbus_property(access=PropertyAccess.READ)
    def ServiceData(self) -> "a{sv}": return {}

class Adapter(ServiceInterface):
    def __init__(self, bus):
        super().__init__("org.bluez.Adapter1")
        self.bus, self.discovering, self.device = bus, False, None
    @method()
    def SetDiscoveryFilter(self, properties: "a{sv}"): pass
    @method()
    def GetDiscoveryFilters(self) -> "as": return ["UUIDs", "RSSI", "Transport", "DuplicateData"]
    @method()
    def StartDiscovery(self):
        self.discovering = True
        self.emit_properties_changed({"Discovering": True})
        if self.device is None:
            self.device = Device()
            self.bus.export(DEVICE, self.device)
    @method()
    def StopDiscovery(self):
        self.discovering = False
        self.emit_properties_changed({"Discovering": False})
    @dbus_property(access=PropertyAccess.READ)
    def Address(self) -> "s": return "00:00:5E:00:53:01"
    @dbus_property(access=PropertyAccess.READ)
    def Name(self) -> "s": return "wildstone-fixture"
    @dbus_property(access=PropertyAccess.READ)
    def Alias(self) -> "s": return "wildstone-fixture"
    @dbus_property(access=PropertyAccess.READ)
    def Powered(self) -> "b": return True
    @dbus_property(access=PropertyAccess.READ)
    def Discovering(self) -> "b": return self.discovering
    @dbus_property(access=PropertyAccess.READ)
    def Roles(self) -> "as": return ["central", "peripheral"]

async def main():
    bus = await MessageBus(bus_type=BusType.SYSTEM).connect()
    bus.export(ADAPTER, Adapter(bus))
    await bus.request_name("org.bluez")
    print("fake bluez ready", flush=True)
    await asyncio.Event().wait()

asyncio.run(main())
`

// probePy runs inside the container under test. argv[1] is the mode.
// It reports the capabilities it holds, whether the bus is there and
// writable, the /config marker, and in host-bluez mode what a scan finds.
const probePy = `
import asyncio, errno, os, sys
mode = sys.argv[1]
eff = 0
for line in open("/proc/self/status"):
    if line.startswith("CapEff:"):
        eff = int(line.split()[1], 16)
print("cap NET_ADMIN=%d NET_RAW=%d SYS_ADMIN=%d" % (eff >> 12 & 1, eff >> 13 & 1, eff >> 21 & 1))
print("bus present=%d" % os.path.exists("/run/dbus/system_bus_socket"))
if os.path.isdir("/run/dbus"):
    try:
        open("/run/dbus/wildstone-445", "w").close()
        print("bus writable")
    except OSError as e:
        print("bus read-only" if e.errno == errno.EROFS else "bus write error %d" % e.errno)
marker = "/config/wildstone-445-marker"
print("marker=%s" % (open(marker).read() if os.path.exists(marker) else "none"))
with open(marker, "w") as f:
    f.write(mode)
if mode == "host-bluez":
    from bleak import BleakScanner
    async def scan():
        found = await BleakScanner.discover(timeout=5, return_adv=True)
        for d, adv in found.values():
            print("found %s %s rssi=%s mfr=%s" % (d.address, adv.local_name, adv.rssi, dict(adv.manufacturer_data)), flush=True)
    asyncio.run(scan())
`

func TestHomeAssistantHostBlueZFixtureDiscovery(t *testing.T) {
	if os.Getenv("WILDSTONE_DOCKER_TESTS") != "1" {
		t.Skip("set WILDSTONE_DOCKER_TESTS=1 to scan a fixture BlueZ from the real Home Assistant image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	prefix := "wildstone-test-445-" + hex.EncodeToString(suffix)

	render := func(mode string) *resource.ContainerSpec {
		params := homeAssistantParams("n1")
		params["bluetooth"] = mode
		out, err := capability.NewEngine().Expand(loadShipped(t, "home-assistant"), params, brokerDeployment(t, ""))
		if err != nil {
			t.Fatalf("expand bluetooth=%s: %v", mode, err)
		}
		return findContainer(t, out, "home-assistant")
	}

	fixture := t.TempDir()
	busDir := t.TempDir()
	for name, content := range map[string]string{"bus.conf": fixtureBusConf, "fake_bluez.py": fakeBlueZPy, "probe.py": probePy} {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := render("proxy").Image

	// ---- The fixture: a system bus and a fake BlueZ, in busDir.
	busName := prefix + "-bluez"
	t.Cleanup(func() { _, _ = docker("rm", "-f", busName) })
	if out, err := docker("run", "-d", "--name", busName, "-v", busDir+":/run/dbus", "-v", fixture+":/fixture:ro",
		"--entrypoint", "python3", image, "/fixture/fake_bluez.py"); err != nil {
		t.Fatalf("starting the fixture BlueZ: %v: %s", err, out)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		logs, _ := docker("logs", busName)
		if strings.Contains(logs, "fake bluez ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture BlueZ never came up: %s", logs)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// ---- The container under test, as the package renders it for mode.
	volume := prefix + "-config"
	t.Cleanup(func() { _, _ = docker("volume", "rm", "-f", volume) })
	probe := func(mode string) string {
		t.Helper()
		spec := render(mode)
		args := []string{"run", "--rm", "--name", prefix + "-ha", "-v", fixture + ":/fixture:ro", "--entrypoint", "python3"}
		if spec.HostNetwork() {
			args = append(args, "--network", "host")
		}
		for _, c := range spec.CapAdd {
			args = append(args, "--cap-add", c)
		}
		for _, m := range spec.Mounts {
			src := m.Source
			switch {
			case src == "home-assistant-config":
				src = volume
			case resource.HostSystemMountAllowed(src):
				src = busDir // the fixture stands in for the host's /run/dbus
			default:
				t.Fatalf("bluetooth=%s renders an unexpected mount %+v", mode, m)
			}
			v := src + ":" + m.Target
			if m.ReadOnly {
				v += ":ro"
			}
			args = append(args, "-v", v)
		}
		out, err := docker(append(args, image, "/fixture/probe.py", mode)...)
		if err != nil {
			t.Fatalf("bluetooth=%s probe: %v: %s", mode, err, out)
		}
		t.Logf("bluetooth=%s:\n%s", mode, out)
		return out
	}
	expect := func(out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("probe output does not contain %q:\n%s", w, out)
			}
		}
	}

	// proxy: nothing of the host — no bus, no added capabilities.
	out := probe("proxy")
	expect(out, "cap NET_ADMIN=0 NET_RAW=1 SYS_ADMIN=0", "bus present=0", "marker=none")

	// host-bluez: the fixture adapter's device is discovered over the
	// read-only bus; /config still holds what proxy mode wrote.
	out = probe("host-bluez")
	expect(out, "cap NET_ADMIN=1 NET_RAW=1 SYS_ADMIN=0", "bus present=1", "bus read-only", "marker=proxy",
		"found AA:BB:CC:DD:EE:FF wildstone-445-fixture rssi=-42")

	// And back: the bus is gone again, /config kept.
	out = probe("off")
	expect(out, "cap NET_ADMIN=0", "bus present=0", "marker=host-bluez")
}
