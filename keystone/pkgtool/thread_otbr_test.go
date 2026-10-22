package pkgtool

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// The OpenThread Border Router package (#443). These tests expand the
// REAL manifest and hold it to the contract its header states: host
// networking with exactly the host access OTBR needs and nothing more,
// a REST API that only this node can reach, the Thread dataset on a
// backed-up volume, and a prerequisite check that names what is missing.
// The image half of the contract — that the pinned entrypoint really
// reads these arguments and variables — is TestOTBRImageContract.

const otbrRadio = "/dev/serial/by-id/usb-Nabu_Casa_Home_Assistant_Connect_ZBT-1_REPLACED-SERIAL-if00-port0"

func otbrParams(over map[string]string) map[string]string {
	p := map[string]string{"node": "n1", "radio_device": otbrRadio, "backbone_interface": "enp1s0"}
	for k, v := range over {
		p[k] = v
	}
	return p
}

func expandOTBR(t *testing.T, over map[string]string) ([]resource.Resource, error) {
	t.Helper()
	pkg := loadShipped(t, "thread-otbr")
	return capability.NewEngine().Expand(pkg, otbrParams(over), capability.NewCatalog(pkg))
}

func mustExpandOTBR(t *testing.T, over map[string]string) []resource.Resource {
	t.Helper()
	out, err := expandOTBR(t, over)
	if err != nil {
		t.Fatalf("expanding thread-otbr: %v", err)
	}
	return out
}

func TestThreadOTBRHostAccessIsExactlyWhatOTBRNeeds(t *testing.T) {
	rendered := mustExpandOTBR(t, nil)
	c := findContainer(t, rendered, "otbr")

	if !c.HostNetwork() {
		t.Errorf("networkMode = %q; OTBR creates wpan0 and routes to the LAN, so it must run in the host namespace", c.NetworkMode)
	}
	if len(c.Ports) != 0 {
		t.Errorf("ports = %v; a host-networked container publishes nothing", c.Ports)
	}
	caps := append([]string(nil), c.CapAdd...)
	sort.Strings(caps)
	if !reflect.DeepEqual(caps, []string{"IPC_LOCK", "NET_ADMIN"}) {
		t.Errorf("capAdd = %v; want exactly [IPC_LOCK NET_ADMIN] — never SYS_ADMIN or anything broader", c.CapAdd)
	}
	for _, capName := range c.CapAdd {
		if !resource.ContainerCapabilities[capName] {
			t.Errorf("capability %q is not on the allowlist", capName)
		}
	}
	want := []resource.DeviceMapping{
		{Source: otbrRadio, Target: "/dev/ttyRCP"},
		{Source: "/dev/net/tun"},
	}
	if !reflect.DeepEqual(c.Devices, want) {
		t.Errorf("devices = %+v; want the one radio at /dev/ttyRCP and /dev/net/tun", c.Devices)
	}
	for _, d := range c.Devices {
		if !resource.DeviceAllowed(d.Source) {
			t.Errorf("device %q is not on the allowlist", d.Source)
		}
	}
	// Host networking is the highest minor this spec needs; IPC_LOCK (21)
	// and devices (12) come with it.
	if got := c.RequiredProtocolMinor(); got != resource.HostNetworkProtocolMinor {
		t.Errorf("RequiredProtocolMinor = %d, want %d", got, resource.HostNetworkProtocolMinor)
	}
	if c.ReadOnlyRootfs {
		t.Error("readOnlyRootfs: upstream's entrypoint rewrites /etc/default and /etc/sysctl.d at start")
	}
}

func TestThreadOTBRRESTIsLoopbackOnlyAndHasNoRoute(t *testing.T) {
	rendered := mustExpandOTBR(t, nil)
	for _, r := range rendered {
		if r.Kind == resource.KindRoute {
			t.Errorf("Route/%s rendered: the REST API has no authentication and must not be proxied anywhere", r.Metadata.Name)
		}
	}
	c := findContainer(t, rendered, "otbr")
	for k, want := range map[string]string{
		"REST_HOST": "127.0.0.1", // otbr-agent --rest-listen-address
		"HTTP_HOST": "127.0.0.1", // REST_HOST's fallback, and the web GUI's
		"WEB_GUI":   "0",         // the unauthenticated web GUI is not started
	} {
		if c.Env[k] != want {
			t.Errorf("env %s = %q, want %q", k, c.Env[k], want)
		}
	}
}

func TestThreadOTBRCommandFollowsTheClosedChoices(t *testing.T) {
	cases := []struct {
		name     string
		over     map[string]string
		radioURL string
		firewall string
		nat64    string
	}{
		{"defaults", nil,
			"spinel+hdlc+uart:///dev/ttyRCP?uart-baudrate=460800&uart-flow-control", "1", "0"},
		{"no flow control, slower radio, firewall off, nat64 on",
			map[string]string{"baud_rate": "115200", "flow_control": "none", "firewall": "off", "nat64": "on"},
			"spinel+hdlc+uart:///dev/ttyRCP?uart-baudrate=115200", "0", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := findContainer(t, mustExpandOTBR(t, tc.over), "otbr")
			wantCmd := []string{"--radio-url", tc.radioURL, "--backbone-interface", "enp1s0", "--debug-level", "5"}
			if !reflect.DeepEqual(c.Command, wantCmd) {
				t.Errorf("command = %q, want %q", c.Command, wantCmd)
			}
			if c.Env["FIREWALL"] != tc.firewall || c.Env["NAT64"] != tc.nat64 {
				t.Errorf("FIREWALL=%q NAT64=%q, want %q %q", c.Env["FIREWALL"], c.Env["NAT64"], tc.firewall, tc.nat64)
			}
			// INFRA_IF_NAME is what upstream's firewall/NAT scripts and the
			// preflight read as the backbone.
			if c.Env["INFRA_IF_NAME"] != "enp1s0" {
				t.Errorf("INFRA_IF_NAME = %q, want the backbone", c.Env["INFRA_IF_NAME"])
			}
		})
	}
}

func TestThreadOTBRRefusesValuesOutsideItsChoices(t *testing.T) {
	for param, bad := range map[string]string{
		"radio_device":       "/dev/ttyUSB0",
		"backbone_interface": "eth0/../lo",
		"baud_rate":          "9600",
		"flow_control":       "software",
		"firewall":           "maybe",
		"nat64":              "yes",
	} {
		if _, err := expandOTBR(t, map[string]string{param: bad}); err == nil {
			t.Errorf("%s=%q expanded; want it refused", param, bad)
		}
	}
}

func TestThreadOTBRDatasetIsPersistedAndBackedUp(t *testing.T) {
	rendered := mustExpandOTBR(t, nil)
	c := findContainer(t, rendered, "otbr")
	var dataVolume string
	for _, m := range c.Mounts {
		if m.Target == "/var/lib/thread" {
			dataVolume = m.Source
			if m.ReadOnly {
				t.Error("/var/lib/thread is read-only: otbr-agent writes the dataset there")
			}
		}
	}
	if dataVolume == "" {
		t.Fatal("nothing is mounted at /var/lib/thread, where otbr-agent keeps the dataset and network key: a re-create would lose the Thread network")
	}
	var plan *resource.BackupPlanSpec
	for _, r := range rendered {
		if r.Kind == resource.KindBackupPlan && r.BackupPlan != nil {
			plan = r.BackupPlan
		}
	}
	if plan == nil {
		t.Fatal("no BackupPlan: losing the dataset means re-commissioning every Thread device")
	}
	if !reflect.DeepEqual(plan.Volumes, []string{dataVolume}) {
		t.Errorf("BackupPlan volumes = %v, want [%s]", plan.Volumes, dataVolume)
	}
	if plan.Class != "critical" {
		t.Errorf("BackupPlan class = %q, want critical: the network key is irreplaceable", plan.Class)
	}
}

func TestThreadOTBRPreflightHookIsWhereTheImageLooks(t *testing.T) {
	rendered := mustExpandOTBR(t, nil)
	c := findContainer(t, rendered, "otbr")
	if c.Env["PLATFORM"] != "wildstone" {
		t.Errorf("PLATFORM = %q: script/_initrc sources examples/platforms/$PLATFORM/before_server", c.Env["PLATFORM"])
	}
	var hook bool
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.ConfigFile.Filename == "before_server" {
			hook = r.Metadata.Owner == "capability/thread"
		}
	}
	if !hook {
		t.Fatal("no before_server ConfigFile owned by capability/thread")
	}
	var mounted bool
	for _, m := range c.Mounts {
		if m.Source == "/etc/wildstone/thread" && m.Target == "/app/examples/platforms/wildstone" && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Error("/etc/wildstone/thread is not mounted read-only at /app/examples/platforms/wildstone, where the hook is looked up")
	}
}

// otbrPreflight returns the rendered before_server script.
func otbrPreflight(t *testing.T, over map[string]string) string {
	t.Helper()
	for _, r := range mustExpandOTBR(t, over) {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.ConfigFile.Filename == "before_server" {
			return r.ConfigFile.Content
		}
	}
	t.Fatal("no before_server rendered")
	return ""
}

// otbrFixtureRoot builds a node that meets every prerequisite, for a
// test to break one piece of.
func otbrFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(p, content string) {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("proc/sys/net/ipv6/conf/all/forwarding", "1\n")
	write("proc/sys/net/ipv6/conf/enp1s0/disable_ipv6", "0\n")
	write("proc/sys/net/ipv6/conf/enp1s0/accept_ra", "2\n")
	write("proc/sys/net/ipv4/ip_forward", "1\n")
	write("sys/class/net/enp1s0/flags", "0x1003\n")
	write("sys/class/net/lo/flags", "0x9\n")
	write("dev/net/tun", "")
	write("dev/ttyRCP", "")
	return root
}

// runOTBRPreflight sources the hook the way /app/script/server does:
// under `set -euxo pipefail`, with the image's environment.
func runOTBRPreflight(t *testing.T, script, root string, env map[string]string) (string, bool) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	hook := filepath.Join(t.TempDir(), "before_server")
	if err := os.WriteFile(hook, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", `set -euxo pipefail; . "$1"; echo SERVER-CONTINUES`, "server", hook)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "WILDSTONE_PREFLIGHT_ROOT=" + root,
		"INFRA_IF_NAME=enp1s0", "FIREWALL=1", "NAT64=0", "WILDSTONE_OTBR_RADIO=/dev/ttyRCP"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err == nil && strings.Contains(string(out), "SERVER-CONTINUES")
}

func TestThreadOTBRPreflightPassesOnAReadyNode(t *testing.T) {
	out, ok := runOTBRPreflight(t, otbrPreflight(t, nil), otbrFixtureRoot(t), nil)
	if !ok {
		t.Fatalf("preflight refused a ready node:\n%s", out)
	}
	if !strings.Contains(out, "wildstone-otbr preflight: ok (backbone enp1s0, radio /dev/ttyRCP)") {
		t.Errorf("no ok line:\n%s", out)
	}
	if strings.Contains(out, "WARN") {
		t.Errorf("a ready node drew a warning:\n%s", out)
	}
}

// Each broken prerequisite stops the server script with a line naming
// the check (the readiness probe's own name where one exists), the
// evidence, and the fix.
func TestThreadOTBRPreflightNamesEachMissingPrerequisite(t *testing.T) {
	script := otbrPreflight(t, nil)
	cases := []struct {
		name    string
		breakIt func(root string)
		env     map[string]string
		want    []string
	}{
		{"no ipv6 forwarding", func(r string) { otbrWrite(t, r, "proc/sys/net/ipv6/conf/all/forwarding", "0\n") }, nil,
			[]string{"FAIL [ipv6_forwarding]", "is 0", "net.ipv6.conf.all.forwarding = 1", "accept_ra = 2"}},
		{"kernel without ipv6", func(r string) { otbrRemove(t, r, "proc/sys/net/ipv6") }, nil,
			[]string{"FAIL [ipv6]", "ipv6.disable=1"}},
		{"backbone missing", nil, map[string]string{"INFRA_IF_NAME": "eth0"},
			[]string{"FAIL [backbone_interface]", "'eth0' does not exist", "enp1s0", "ip route show default"}},
		{"ipv6 disabled on backbone", func(r string) { otbrWrite(t, r, "proc/sys/net/ipv6/conf/enp1s0/disable_ipv6", "1\n") }, nil,
			[]string{"FAIL [interface_ipv6]", "disable_ipv6=1"}},
		{"multicast off", func(r string) { otbrWrite(t, r, "sys/class/net/enp1s0/flags", "0x3\n") }, nil,
			[]string{"FAIL [multicast]", "ip link set enp1s0 multicast on"}},
		{"no tun", func(r string) { otbrRemove(t, r, "dev/net/tun") }, nil,
			[]string{"FAIL [tun]", "modprobe tun"}},
		{"no radio", func(r string) { otbrRemove(t, r, "dev/ttyRCP") }, nil,
			[]string{"FAIL [radio]", "/dev/serial/by-id/"}},
		{"nat64 without ipv4 forwarding", func(r string) { otbrWrite(t, r, "proc/sys/net/ipv4/ip_forward", "0\n") },
			map[string]string{"NAT64": "1"},
			[]string{"FAIL [ipv4_forwarding]", "nat64=off"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := otbrFixtureRoot(t)
			if tc.breakIt != nil {
				tc.breakIt(root)
			}
			out, ok := runOTBRPreflight(t, script, root, tc.env)
			if ok {
				t.Fatalf("preflight let the server continue:\n%s", out)
			}
			for _, w := range append(tc.want, "not starting otbr-agent") {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

// accept_ra is a warning, not a refusal: OTBR runs, but the node loses
// its own IPv6 default route, which is worth saying at start.
func TestThreadOTBRPreflightWarnsAboutAcceptRA(t *testing.T) {
	root := otbrFixtureRoot(t)
	otbrWrite(t, root, "proc/sys/net/ipv6/conf/enp1s0/accept_ra", "1\n")
	out, ok := runOTBRPreflight(t, otbrPreflight(t, nil), root, nil)
	if !ok {
		t.Fatalf("a warning stopped the server:\n%s", out)
	}
	if !strings.Contains(out, "WARN [accept_ra]") {
		t.Errorf("no accept_ra warning:\n%s", out)
	}
}

// accept_ra=0 is the kernel leaving router advertisements to a network
// manager (systemd-networkd pins it there), so "set it to 2" must not
// be the only advice: the live run's netplan VM needed accept-ra: true.
func TestThreadOTBRPreflightAcceptRAZeroPointsAtTheNetworkManager(t *testing.T) {
	root := otbrFixtureRoot(t)
	otbrWrite(t, root, "proc/sys/net/ipv6/conf/enp1s0/accept_ra", "0\n")
	out, ok := runOTBRPreflight(t, otbrPreflight(t, nil), root, nil)
	if !ok {
		t.Fatalf("a warning stopped the server:\n%s", out)
	}
	for _, want := range []string{"WARN [accept_ra]", "systemd-networkd", "accept-ra: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("accept_ra=0 output lacks %q:\n%s", want, out)
		}
	}
}

func otbrWrite(t *testing.T, root, p, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func otbrRemove(t *testing.T, root, p string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(p))); err != nil {
		t.Fatal(err)
	}
}
