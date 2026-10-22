package reconcile

// An opt-in test that puts the REAL ingress image on a user-defined
// bridge, gives it the host-gateway alias exactly as the agent writes it
// (wardstone hostaliases.go, protocol minor 24), loads the Caddy config
// this package renders for a "host/<port>" Route, and proxies a request
// to a service listening on the host (#433).
//
// Opt-in because it needs a Docker daemon and pulls the Caddy image:
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./reconcile/ -run TestCaddyOnABridgeReachesAHostService -v

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// ingressCaddyImage is the image packages/ingress-tailnet ships.
const ingressCaddyImage = "caddy@sha256:df7f1c2fb114453b951de51a98efc010db1655a92c2e86be6706714e2417a78d"

func TestCaddyOnABridgeReachesAHostService(t *testing.T) {
	if os.Getenv("WILDSTONE_DOCKER_TESTS") != "1" {
		t.Skip("set WILDSTONE_DOCKER_TESTS=1 to run Caddy against a host service")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// The host service: all interfaces, as a host-networked hub listens.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	const body = "served-from-the-node-host"
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	// Rendered exactly as the reconciler writes it.
	route := resource.Resource{
		Kind:     resource.KindRoute,
		Metadata: resource.Metadata{Name: "home", Owner: "capability/home-automation", Node: "node-a"},
		Route: &resource.RouteSpec{Host: "home.example.internal", Upstream: "host/" + strconv.Itoa(port),
			Auth: "none", Classification: "family_private"},
	}
	if err := route.Validate(); err != nil {
		t.Fatalf("host upstream refused: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "routes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), renderBaseCaddyfile(terminatedUpstream), 0o644); err != nil {
		t.Fatal(err)
	}
	frag := renderRouteFragment(route.Route, DefaultAutheliaAddr, terminatedUpstream)
	if err := os.WriteFile(filepath.Join(dir, "routes", "home.caddy"), frag, 0o644); err != nil {
		t.Fatal(err)
	}

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name := "wildstone-test-433-" + hex.EncodeToString(suffix)
	if out, err := docker("network", "create", name); err != nil {
		t.Fatalf("network create: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = docker("network", "rm", name) })
	out, err := docker("run", "-d", "--name", name, "--network", name,
		"--add-host", resource.HostGatewayAlias+":host-gateway", // what the agent writes
		"-v", dir+":/etc/wildstone:ro",
		ingressCaddyImage, "caddy", "run", "--config", "/etc/wildstone/Caddyfile", "--adapter", "caddyfile")
	t.Cleanup(func() { _, _ = docker("rm", "-f", name) })
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}

	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last, err = docker("exec", name, "wget", "-qO-", "--header", "Host: home.example.internal", "http://127.0.0.1:80/")
		if err == nil && last == body {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	logs, _ := docker("logs", name)
	t.Fatalf("Caddy on the bridge never reached the host service at %s:%d: last=%q err=%v\nfragment:\n%s\nlogs:\n%s",
		resource.HostGatewayAlias, port, last, err, frag, logs)
}
