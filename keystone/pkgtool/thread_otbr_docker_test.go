package pkgtool

// Opt-in tests that start the REAL pinned openthread/otbr image with the
// container spec thread-otbr renders (#443).
//
// TestOTBRImageContract needs only Docker. It proves the half of the
// package that no manifest check can: that the pinned entrypoint reads
// --radio-url and --backbone-interface as the package passes them, binds
// REST to REST_HOST, leaves the web GUI off, sources the before_server
// hook through PLATFORM, and — when a prerequisite is missing — stops
// instead of idling "Up" with no otbr-agent behind it.
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run TestOTBRImageContract -v
//
// TestOTBRRestartKeepsTheThreadDataset additionally needs a kernel with
// IPv6 and an OpenThread simulated RCP (ot-rcp, built with
// -DOT_PLATFORM=simulation; see the VM runbook), because otbr-agent
// will not start without a radio. It forms a Thread network over the
// REST API Home Assistant uses, restarts the container, and checks the
// active dataset came back from the volume:
//
//	WILDSTONE_DOCKER_TESTS=1 WILDSTONE_OTBR_RCP_SIM=/abs/path/ot-rcp \
//	  go test ./pkgtool/ -run TestOTBRRestartKeepsTheThreadDataset -v
//
// Both run the container on a Docker bridge network, never the test
// host's own namespace: host networking would create wpan0 and install
// ip6tables rules on the machine running the tests. The backbone is the
// container's eth0.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// otbrDocker is one test's container, volume and hook directory.
type otbrDocker struct {
	t      *testing.T
	name   string
	image  string
	spec   *resource.ContainerSpec
	hook   string // host directory holding the rendered before_server
	volume string
}

func dockerOut(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

func newOTBRDocker(t *testing.T, name string) *otbrDocker {
	t.Helper()
	if os.Getenv("WILDSTONE_DOCKER_TESTS") != "1" {
		t.Skip("set WILDSTONE_DOCKER_TESTS=1 to start the real openthread/otbr image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	rendered := mustExpandOTBR(t, map[string]string{"backbone_interface": "eth0"})
	d := &otbrDocker{t: t, name: name, volume: name, hook: t.TempDir()}
	d.spec = findContainer(t, rendered, "otbr")
	d.image = d.spec.Image
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil {
			if err := os.WriteFile(filepath.Join(d.hook, r.ConfigFile.Filename), []byte(r.ConfigFile.Content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, _ = dockerOut("rm", "-f", name)
	_, _ = dockerOut("volume", "rm", name)
	if out, err := dockerOut("volume", "create", name); err != nil {
		t.Fatalf("docker volume create: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := dockerOut("logs", "--tail", "80", name)
			t.Logf("last container log:\n%s", out)
		}
		_, _ = dockerOut("rm", "-f", name)
		_, _ = dockerOut("volume", "rm", name)
	})
	return d
}

// runArgs is `docker run` for the rendered spec: its env, capabilities,
// mounts and command, with the managed hook directory and the volume
// standing in for what the agent would mount. Devices other than
// /dev/net/tun are the caller's, since a CI runner has no Thread radio.
func (d *otbrDocker) runArgs(envOver map[string]string, extra []string, command []string) []string {
	args := []string{"run", "-d", "--name", d.name}
	for _, c := range d.spec.CapAdd {
		args = append(args, "--cap-add", c)
	}
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		args = append(args, "--device", "/dev/net/tun")
	}
	for _, m := range d.spec.Mounts {
		switch m.Target {
		case "/var/lib/thread":
			args = append(args, "-v", d.volume+":"+m.Target)
		case "/app/examples/platforms/wildstone":
			args = append(args, "-v", d.hook+":"+m.Target+":ro")
		default:
			d.t.Fatalf("unexpected mount %+v: teach this test what stands in for it", m)
		}
	}
	env := map[string]string{}
	for k, v := range d.spec.Env {
		env[k] = v
	}
	for k, v := range envOver {
		env[k] = v
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, extra...)
	args = append(args, d.image)
	return append(args, command...)
}

func (d *otbrDocker) running() bool {
	out, err := dockerOut("inspect", "-f", "{{.State.Running}}", d.name)
	return err == nil && strings.TrimSpace(out) == "true"
}

func TestOTBRImageContract(t *testing.T) {
	d := newOTBRDocker(t, "wildstone-otbr-contract")
	// No radio is mapped: the preflight must name it and stop.
	if out, err := dockerOut(d.runArgs(nil, nil, d.spec.Command)...); err != nil {
		t.Fatalf("docker run: %v: %s", err, out)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for d.running() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
	}
	if d.running() {
		t.Fatal("the container is still running with its radio missing: upstream idles forever once script/server fails, " +
			"and the preflight was supposed to stop it")
	}
	logs, _ := dockerOut("logs", d.name)
	if testing.Verbose() {
		for _, l := range strings.Split(logs, "\n") {
			if strings.Contains(l, "wildstone-otbr") || strings.HasPrefix(l, "RADIO_URL") {
				t.Log(l)
			}
		}
	}

	radioURL := d.spec.Command[1]
	for _, want := range []string{
		// The entrypoint parsed the package's arguments.
		"RADIO_URL: " + radioURL,
		"BACKBONE_INTERFACE: eth0",
		"DEBUG_LEVEL: 5",
		// script/server sourced the hook from examples/platforms/wildstone.
		"wildstone-otbr preflight: FAIL [radio]",
		"not starting otbr-agent",
		// And the entrypoint's TERM trap ran upstream's shutdown.
		"Shutting down",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	for _, never := range []string{"Starting thread border agent", "otbr-web"} {
		if strings.Contains(logs, never) {
			t.Errorf("log contains %q: nothing should start past a failed preflight, and the web GUI never", never)
		}
	}

	// What the entrypoint wrote for otbr-agent, from the stopped container.
	dst := filepath.Join(t.TempDir(), "otbr-agent")
	if out, err := dockerOut("cp", d.name+":/etc/default/otbr-agent", dst); err != nil {
		t.Fatalf("docker cp: %v: %s", err, out)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	opts := string(raw)
	for _, want := range []string{"-I wpan0", "-B eth0", "-d5", "--rest-listen-address 127.0.0.1", radioURL} {
		if !strings.Contains(opts, want) {
			t.Errorf("/etc/default/otbr-agent lacks %q: %s", want, opts)
		}
	}
}

// otbrREST calls the border router's REST API from inside its own
// network namespace — where Home Assistant, host-networked on the same
// node, calls it from — using the image's own python3.
func (d *otbrDocker) otbrREST(method, path, accept, body string) (int, string) {
	d.t.Helper()
	const script = `
import sys, urllib.request, urllib.error
m, p, accept, body = sys.argv[1:5]
req = urllib.request.Request("http://127.0.0.1:8081" + p, method=m,
    data=body.encode() if body else None,
    headers={"Accept": accept, "Content-Type": "application/json"})
try:
    with urllib.request.urlopen(req, timeout=5) as r:
        print(r.status); print(r.read().decode())
except urllib.error.HTTPError as e:
    print(e.code); print(e.read().decode())
except Exception as e:
    print(0); print(e)
`
	out, _ := dockerOut("run", "--rm", "--network", "container:"+d.name, "--entrypoint", "python3",
		d.image, "-c", script, method, path, accept, body)
	status, rest, _ := strings.Cut(strings.TrimSpace(out), "\n")
	code := 0
	_ = json.Unmarshal([]byte(status), &code)
	return code, strings.TrimSpace(rest)
}

func (d *otbrDocker) waitREST(timeout time.Duration) {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if code, _ := d.otbrREST("GET", "/node/state", "application/json", ""); code == 200 {
			return
		}
		time.Sleep(3 * time.Second)
	}
	d.t.Fatal("the REST API never answered GET /node/state on 127.0.0.1:8081")
}

func TestOTBRRestartKeepsTheThreadDataset(t *testing.T) {
	rcp := os.Getenv("WILDSTONE_OTBR_RCP_SIM")
	if rcp == "" {
		t.Skip("set WILDSTONE_OTBR_RCP_SIM to an OpenThread simulated ot-rcp binary (statically linked)")
	}
	if _, err := os.Stat("/proc/sys/net/ipv6"); err != nil {
		t.Skip("this kernel has no IPv6; otbr-agent cannot start")
	}
	d := newOTBRDocker(t, "wildstone-otbr-dataset")

	// The simulated radio replaces the serial URL, and the container's
	// own namespace is made to look like a prepared node. The firewall is
	// host packet filtering, which the VM runbook exercises instead.
	command := append([]string(nil), d.spec.Command...)
	command[1] = "spinel+hdlc+forkpty:///opt/ot-rcp?forkpty-arg=1"
	extra := []string{
		"-v", rcp + ":/opt/ot-rcp:ro",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=0",
		"--sysctl", "net.ipv6.conf.all.forwarding=1",
		"--sysctl", "net.ipv6.conf.eth0.disable_ipv6=0",
		"--sysctl", "net.ipv6.conf.eth0.accept_ra=2",
	}
	env := map[string]string{"FIREWALL": "0", "WILDSTONE_OTBR_RADIO": "/opt/ot-rcp"}
	if out, err := dockerOut(d.runArgs(env, extra, command)...); err != nil {
		t.Fatalf("docker run: %v: %s", err, out)
	}
	d.waitREST(3 * time.Minute)

	// Home Assistant's first-run path: create a dataset, enable Thread.
	if code, body := d.otbrREST("PUT", "/node/dataset/active", "application/json",
		`{"NetworkName":"wildstone-test"}`); code != 200 && code != 201 {
		t.Fatalf("PUT /node/dataset/active = %d: %s", code, body)
	}
	if code, body := d.otbrREST("PUT", "/node/state", "application/json", `"enable"`); code != 200 {
		t.Fatalf("PUT /node/state enable = %d: %s", code, body)
	}
	code, before := d.otbrREST("GET", "/node/dataset/active", "text/plain", "")
	if code != 200 || before == "" {
		t.Fatalf("GET /node/dataset/active = %d %q", code, before)
	}

	// Not reachable from the network, only from its own namespace.
	ip, _ := dockerOut("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", d.name)
	if out, _ := dockerOut("run", "--rm", "--entrypoint", "python3", d.image, "-c",
		`import socket,sys; s=socket.socket(); s.settimeout(3); print(s.connect_ex((sys.argv[1], 8081)))`,
		strings.TrimSpace(ip)); strings.TrimSpace(out) == "0" {
		t.Errorf("REST answered on %s:8081 from another container: it must be loopback-only", strings.TrimSpace(ip))
	}

	if out, err := dockerOut("restart", d.name); err != nil {
		t.Fatalf("docker restart: %v: %s", err, out)
	}
	d.waitREST(3 * time.Minute)
	code, after := d.otbrREST("GET", "/node/dataset/active", "text/plain", "")
	if code != 200 || after != before {
		t.Fatalf("after restart GET /node/dataset/active = %d %q, want the dataset formed before it (%q)", code, after, before)
	}
}
