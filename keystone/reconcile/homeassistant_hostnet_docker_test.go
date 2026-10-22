package reconcile

// An opt-in test that boots the REAL pinned Home Assistant image the way
// packages/home-assistant 2.x renders it — networkMode: host, its seeded
// /config, its env — beside the real broker and the real ingress image
// (#440), and checks the four things the move to host networking is for
// or could break:
//
//   - the Wildstone URL: Caddy on a bridge, loaded with the fragment this
//     package renders for the package's host/8123 Route, reaches Home
//     Assistant through the host gateway, and Home Assistant accepts the
//     proxied request (the seeded trust covers the path Caddy really
//     takes; an untrusted peer with X-Forwarded-For gets 400);
//   - LAN discovery: an SSDP multicast sent from another machine on the
//     "LAN" (a container on Docker's default bridge, whose host side is
//     docker0) arrives inside the Home Assistant container;
//   - MQTT: the broker address and credential the package hands Home
//     Assistant authenticate to the secured broker from inside the
//     container, and a wrong password does not;
//   - nothing else: the test refuses to run if port 8123 is already taken
//     on this machine, since that is exactly the case a real install is
//     refused for.
//
// Opt-in because it needs a Docker daemon, host networking and the ~1.5GB
// image, and binds port 8123 on the machine running it:
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./reconcile/ -run TestHomeAssistantOnTheHostNetwork -v

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// mqttConnectPy sends one MQTT 3.1.1 CONNECT with the credential in the
// container's WILDSTONE_MQTT_* env and prints the CONNACK return code:
// 0 accepted, 4 bad username or password, 5 not authorised. Raw bytes
// rather than a client library, so the check does not depend on which
// MQTT library the image happens to ship.
const mqttConnectPy = `
import os, socket, struct
def s(x):
    b = x.encode()
    return struct.pack('!H', len(b)) + b
e = os.environ
body = s('MQTT') + bytes([4, 0xC2]) + struct.pack('!H', 30) + s('wildstone-440') + s(e['WILDSTONE_MQTT_USERNAME']) + s(e['WILDSTONE_MQTT_PASSWORD'])
n, rl = len(body), b''
while True:
    d, n = n % 128, n // 128
    rl += bytes([d | (0x80 if n else 0)])
    if not n:
        break
c = socket.create_connection((e['WILDSTONE_MQTT_HOST'], int(e['WILDSTONE_MQTT_PORT'])), timeout=10)
c.sendall(b'\x10' + rl + body)
a = c.recv(4)
print('rc=%d' % a[3])
`

// ssdpListenPy joins the SSDP group on the given interface address and
// prints the first datagram carrying the marker.
const ssdpListenPy = `
import socket, struct, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_UDP)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
s.bind(('', 1900))
s.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, struct.pack('4s4s', socket.inet_aton('239.255.255.250'), socket.inet_aton(sys.argv[1])))
s.settimeout(40)
while True:
    d, a = s.recvfrom(2048)
    if b'wildstone-440' in d:
        print('got %s from %s' % (d.decode().strip(), a[0]))
        break
`

func TestHomeAssistantOnTheHostNetwork(t *testing.T) {
	if os.Getenv("WILDSTONE_DOCKER_TESTS") != "1" {
		t.Skip("set WILDSTONE_DOCKER_TESTS=1 to boot the real Home Assistant image on the host network")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if ln, err := net.Listen("tcp", ":8123"); err != nil {
		t.Skipf("port 8123 is taken on this machine (%v); Home Assistant on the host network needs it", err)
	} else {
		ln.Close()
	}
	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	load := func(name string) *pkgspec.Package {
		p, err := pkgspec.Load(filepath.Join("..", "..", "packages", name, "package.yaml"))
		if err != nil {
			t.Fatalf("loading packages/%s: %v", name, err)
		}
		return p
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	prefix := "wildstone-test-440-" + hex.EncodeToString(suffix)

	// The "LAN": Docker's default bridge. Its gateway is an address of
	// this machine that a container on it reaches, as a LAN address is.
	lanAddr, err := docker("network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		t.Fatalf("docker network inspect: %v: %s", err, lanAddr)
	}

	// ---- The broker, as mqtt-mosquitto renders it on that address.
	broker := load("mqtt-mosquitto")
	brokerOut, err := capability.NewEngine().Expand(broker,
		map[string]string{"node": "n1", "listen_addr": lanAddr, "listen_port": "18834"}, capability.NewCatalog(broker))
	if err != nil {
		t.Fatalf("expanding the broker: %v", err)
	}
	var brokerSpec *resource.ContainerSpec
	cfg := map[string]string{}
	for _, r := range brokerOut {
		switch {
		case r.Kind == resource.KindContainer && r.Metadata.Name == "mosquitto":
			brokerSpec = r.Container
		case r.Kind == resource.KindConfigFile:
			cfg[r.ConfigFile.Filename] = r.ConfigFile.Content
		}
	}
	// The node's side: a generated password per Secret, hashed into the
	// password file the way the agent materializes `mosquitto:secret/...`.
	passwords := map[string]string{}
	ref := regexp.MustCompile(`mosquitto:secret/([A-Za-z0-9._-]+)`)
	cfg["passwd"] = ref.ReplaceAllStringFunc(cfg["passwd"], func(m string) string {
		name := ref.FindStringSubmatch(m)[1]
		raw := make([]byte, 24)
		_, _ = rand.Read(raw)
		passwords[name] = hex.EncodeToString(raw)
		salt := make([]byte, 12)
		_, _ = rand.Read(salt)
		key, err := pbkdf2.Key(sha512.New, passwords[name], salt, 101, sha512.Size)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("$7$101$%s$%s", base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key))
	})
	brokerDir := t.TempDir()
	for name, content := range cfg {
		if err := os.WriteFile(filepath.Join(brokerDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	brokerName := prefix + "-mosquitto"
	t.Cleanup(func() { _, _ = docker("rm", "-f", brokerName) })
	if out, err := docker("run", "-d", "--name", brokerName, "-p", brokerSpec.Ports[0],
		"-v", brokerDir+":/mosquitto/config:ro", brokerSpec.Image); err != nil {
		t.Fatalf("starting the broker: %v: %s", err, out)
	}

	// ---- Home Assistant, as its package renders it with MQTT on.
	ha := load("home-assistant")
	cat := capability.NewCatalog(broker).WithCurrent(brokerOut)
	haOut, err := capability.NewEngine().Expand(ha,
		map[string]string{"public_domain": "home.example.internal", "node": "n1", "mqtt_enabled": "true"}, cat)
	if err != nil {
		t.Fatalf("expanding home-assistant: %v", err)
	}
	var haSpec *resource.ContainerSpec
	var route *resource.RouteSpec
	seedDir := t.TempDir()
	for _, r := range haOut {
		switch {
		case r.Kind == resource.KindContainer && r.Metadata.Name == "home-assistant":
			haSpec = r.Container
		case r.Kind == resource.KindRoute && r.Metadata.Name == "hass":
			route = r.Route
		case r.Kind == resource.KindSeedFile:
			p := filepath.Join(seedDir, filepath.FromSlash(r.SeedFile.Path))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(r.SeedFile.Content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if haSpec == nil || route == nil || !haSpec.HostNetwork() {
		t.Fatalf("home-assistant renders container %+v, route %+v; want a host-networked container and Route/hass", haSpec, route)
	}

	haName := prefix + "-ha"
	volume := haName
	t.Cleanup(func() { _, _ = docker("rm", "-f", haName); _, _ = docker("volume", "rm", volume) })
	if out, err := docker("volume", "create", volume); err != nil {
		t.Fatalf("volume create: %v: %s", err, out)
	}
	args := []string{"create", "--name", haName, "--network", "host", "-v", volume + ":/config"}
	for k, v := range haSpec.Env {
		// The agent substitutes an env value that is exactly secret/<name>.
		if name, ok := strings.CutPrefix(v, "secret/"); ok {
			p, ok := passwords[name]
			if !ok {
				t.Fatalf("%s references secret/%s, which the broker does not issue", k, name)
			}
			v = p
		}
		args = append(args, "-e", k+"="+v)
	}
	if out, err := docker(append(args, haSpec.Image)...); err != nil {
		t.Fatalf("creating home-assistant: %v: %s", err, out)
	}
	// Seeded before the first start, as the reconciler orders it.
	if out, err := docker("cp", seedDir+"/.", haName+":/config/"); err != nil {
		t.Fatalf("seeding /config: %v: %s", err, out)
	}
	if out, err := docker("start", haName); err != nil {
		t.Fatalf("starting home-assistant: %v: %s", err, out)
	}

	// ---- The ingress proxy on a bridge, with the fragment this Route renders.
	caddyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(caddyDir, "routes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caddyDir, "Caddyfile"), renderBaseCaddyfile(terminatedUpstream), 0o644); err != nil {
		t.Fatal(err)
	}
	frag := renderRouteFragment(route, DefaultAutheliaAddr, terminatedUpstream)
	if err := os.WriteFile(filepath.Join(caddyDir, "routes", "hass.caddy"), frag, 0o644); err != nil {
		t.Fatal(err)
	}
	caddyName := prefix + "-caddy"
	t.Cleanup(func() { _, _ = docker("rm", "-f", caddyName); _, _ = docker("network", "rm", caddyName) })
	if out, err := docker("network", "create", caddyName); err != nil {
		t.Fatalf("network create: %v: %s", err, out)
	}
	if out, err := docker("run", "-d", "--name", caddyName, "--network", caddyName,
		"--add-host", resource.HostGatewayAlias+":host-gateway",
		"-v", caddyDir+":/etc/wildstone:ro",
		ingressCaddyImage, "caddy", "run", "--config", "/etc/wildstone/Caddyfile", "--adapter", "caddyfile"); err != nil {
		t.Fatalf("starting caddy: %v: %s", err, out)
	}

	// The Wildstone URL. Caddy sets X-Forwarded-For, so a peer Home
	// Assistant does not trust would get 400 here, not 200.
	status := regexp.MustCompile(`HTTP/1\.\d (\d{3})`)
	var code, last string
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		last, _ = docker("exec", caddyName, "wget", "-S", "-O", "/dev/null",
			"--header", "Host: "+route.Host, "http://127.0.0.1:80/")
		if m := status.FindAllStringSubmatch(last, -1); len(m) > 0 {
			code = m[len(m)-1][1]
			if code != "502" && code != "503" {
				break
			}
		}
		time.Sleep(5 * time.Second)
	}
	if code != "200" {
		logs, _ := docker("logs", "--tail", "40", haName)
		t.Fatalf("through the proxy at %s: last status %q (400 means the proxy's path is not trusted)\n%s\nfragment:\n%s\nhome-assistant log:\n%s",
			route.Host, code, last, frag, logs)
	}

	// LAN discovery: an SSDP M-SEARCH from a container on the default
	// bridge reaches a listener inside the Home Assistant container.
	listener := exec.Command("docker", "exec", haName, "python3", "-c", ssdpListenPy, lanAddr)
	var got strings.Builder
	listener.Stdout, listener.Stderr = &got, &got
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- listener.Wait() }()
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: wildstone-440\r\n\r\n"
	var lerr error
	sent := false
	for i := 0; i < 20 && !sent; i++ {
		time.Sleep(time.Second)
		if out, err := docker("run", "--rm", "--network", "bridge", "--entrypoint", "sh", brokerSpec.Image, "-c",
			fmt.Sprintf("printf '%s' | nc -u -w1 239.255.255.250 1900", strings.ReplaceAll(msg, "\r\n", `\r\n`))); err != nil {
			t.Logf("sending the multicast: %v: %s", err, out)
		}
		select {
		case lerr = <-done:
			sent = true
		default:
		}
	}
	if !sent {
		_ = listener.Process.Kill()
		lerr = <-done
	}
	if lerr != nil || !strings.Contains(got.String(), "got M-SEARCH") {
		t.Errorf("an SSDP multicast from the LAN never reached the container: %v: %s", lerr, got.String())
	} else {
		t.Logf("inside home-assistant: %s", strings.TrimSpace(got.String()))
	}

	// MQTT: the credential the package handed over authenticates.
	out, err := docker("exec", haName, "python3", "-c", mqttConnectPy)
	if err != nil || !strings.Contains(out, "rc=0") {
		logs, _ := docker("logs", brokerName)
		t.Errorf("Home Assistant's rendered MQTT credential was refused: %v: %s\nbroker log:\n%s", err, out, logs)
	}
	out, err = docker("exec", "-e", "WILDSTONE_MQTT_PASSWORD=not-the-password", haName, "python3", "-c", mqttConnectPy)
	if err != nil || strings.Contains(out, "rc=0") {
		t.Errorf("a wrong password was not refused: %v: %s", err, out)
	}
}
