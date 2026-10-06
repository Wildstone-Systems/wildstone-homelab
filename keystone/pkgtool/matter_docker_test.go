package pkgtool

// An opt-in test that boots the Matter.js Server on the container spec
// this package renders — host networking, env, the fabric volume, the
// read-only root and no-new-privileges — and then does what Home
// Assistant's Matter integration does on setup: open ws://<host>:<port>/ws,
// read the server_info the server sends first, and start_listening.
//
// It exists because the package couples to things only the image can
// answer: that LISTEN_ADDRESS really keeps the unauthenticated WebSocket
// off every address but loopback, that the image's healthcheck agrees
// with it, that an empty named volume really comes up writable for the
// image's UID 1000 under a read-only root, and that the fabric survives a
// restart and a recreate — the one failure that costs a household every
// device, and is silent everywhere else.
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run TestMatterServerServesHomeAssistant -v
//
// The pinned image is on GHCR. Where GHCR's blob host is unreachable (the
// authoring sandbox refuses it), the pull fails and the test SKIPS, saying
// so — unless WILDSTONE_MATTER_STANDIN=1, which builds
// testdata/matterjs-standin: upstream's own Dockerfile for the same
// matter-server release from npm, minus BLE. A stand-in pass proves the
// package against the same server code, not against the digest; the VM
// runbook carries the pinned-image run.
//
// Matter needs IPv6. On a Docker host whose kernel has none (no
// /proc/net/if_inet6 — again, a sandbox) the test loads
// testdata/matterjs-standin/no-ipv6-shim.mjs, which moves matter.js's
// UDP sockets to IPv4 so the server can start at all. Everything judged
// here is TCP on 127.0.0.1 and files in /data, which the shim does not
// touch; it says so in the container log.

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// matterServerVersion is the npm release the stand-in is built from: the
// version the pinned digest was published as (package header).
const matterServerVersion = "1.4.0"

func TestMatterServerServesHomeAssistant(t *testing.T) {
	if os.Getenv("WILDSTONE_DOCKER_TESTS") != "1" {
		t.Skip("set WILDSTONE_DOCKER_TESTS=1 to boot the Matter.js Server image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if h := os.Getenv("DOCKER_HOST"); h != "" && !strings.HasPrefix(h, "unix://") {
		t.Skip("host networking is judged from this machine; DOCKER_HOST points elsewhere")
	}
	run := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}

	_, rendered := renderedMatter(t)
	c := matterContainer(t, rendered)

	raw, err := os.ReadFile(filepath.Join(repoRootDir(t), "packages", matterPackage, "package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ghcr.io/matter-js/matterjs-server:"+matterServerVersion) {
		t.Fatalf("matterServerVersion %s is not the release the package header says it pins", matterServerVersion)
	}

	image := c.Image
	if _, err := run("image", "inspect", image); err != nil {
		if out, err := run("pull", image); err != nil {
			if os.Getenv("WILDSTONE_MATTER_STANDIN") != "1" {
				t.Skipf("cannot pull the pinned image %s (%v): %s\n"+
					"Set WILDSTONE_MATTER_STANDIN=1 to build upstream's Dockerfile for matter-server %s from npm "+
					"instead, and run the pinned image on the test VM (VM_RUNBOOK_NOTES #442).",
					image, err, lastLine(out), matterServerVersion)
			}
			image = buildMatterStandin(t)
		}
	}

	// A free loopback port, so the test cannot collide with a Matter
	// server already on this machine; the rendered 5580 is pinned by
	// TestMatterWebSocketIsLoopbackOnly.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	const (
		name   = "wildstone-matter-test"
		volume = "wildstone-matter-test-data"
	)
	cleanup := func() {
		_, _ = run("rm", "-f", name)
		_, _ = run("volume", "rm", "-f", volume)
	}
	cleanup()
	t.Cleanup(cleanup)
	// A named volume, created empty, as the node's EnsureVolume leaves it.
	if out, err := run("volume", "create", volume); err != nil {
		t.Fatalf("docker volume create: %v: %s", err, out)
	}

	shimDir, _ := filepath.Abs(filepath.Join("testdata", "matterjs-standin"))
	_, statErr := os.Stat("/proc/net/if_inet6")
	noIPv6 := os.IsNotExist(statErr)

	start := func() {
		t.Helper()
		args := []string{"run", "-d", "--name", name}
		if c.HostNetwork() {
			args = append(args, "--network", "host")
		}
		if c.ReadOnlyRootfs {
			args = append(args, "--read-only")
		}
		if c.NoNewPrivileges {
			args = append(args, "--security-opt", "no-new-privileges")
		}
		for _, m := range c.Mounts {
			src := m.Source
			if src == "matter-data" {
				src = volume
			}
			args = append(args, "-v", src+":"+m.Target)
		}
		for k, v := range c.Env {
			if k == "PORT" {
				v = port
			}
			args = append(args, "-e", k+"="+v)
		}
		if noIPv6 {
			args = append(args, "-v", shimDir+":/wildstone-test-shim:ro",
				"-e", "NODE_OPTIONS=--import=/wildstone-test-shim/no-ipv6-shim.mjs")
		}
		args = append(args, image)
		args = append(args, c.Command...)
		if out, err := run(args...); err != nil {
			t.Fatalf("starting the Matter server: %v: %s", err, out)
		}
	}
	logs := func() string {
		out, _ := run("logs", "--tail", "60", name)
		return out
	}

	// ---- First start: a new fabric, and Home Assistant gets in.
	start()
	first := matterHAConnect(t, "127.0.0.1:"+port, logs)
	if first.compressedFabricID == "" || first.compressedFabricID == "0" || first.schemaVersion == 0 {
		t.Fatalf("server_info without a fabric: %+v", first)
	}
	t.Logf("fabric %s (compressed %s), schema %d, %s", first.fabricID, first.compressedFabricID, first.schemaVersion, first.sdkVersion)

	// The image's own healthcheck reads the same LISTEN_ADDRESS/PORT.
	if raw, err := run("image", "inspect", "--format", "{{json .Config.Healthcheck}}", image); err == nil && strings.Contains(raw, "healthcheck") {
		waitHealthy(t, run, name, logs)
	}

	// ---- Loopback only: every other address of this machine refuses.
	if l := logs(); !strings.Contains(l, "listening on http://127.0.0.1:"+port) {
		t.Errorf("the server did not report listening on 127.0.0.1:%s only:\n%s", port, l)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var probed int
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		probed++
		hp := net.JoinHostPort(ipn.IP.String(), port)
		if conn, err := net.DialTimeout("tcp", hp, 2*time.Second); err == nil {
			conn.Close()
			t.Errorf("the unauthenticated WebSocket answers on %s: it must be loopback only", hp)
		}
	}
	if probed == 0 {
		t.Log("this machine has no non-loopback address to probe; loopback-only binding judged from the log alone")
	}

	// ---- Restart: the same fabric.
	if out, err := run("restart", name); err != nil {
		t.Fatalf("docker restart: %v: %s", err, out)
	}
	again := matterHAConnect(t, "127.0.0.1:"+port, logs)
	if again.compressedFabricID != first.compressedFabricID || again.fabricID != first.fabricID {
		t.Errorf("restart changed the fabric: %+v -> %+v", first, again)
	}

	// ---- Recreate (an image upgrade, a primary_interface change): the
	// container replaced on the same volume, the same fabric.
	if out, err := run("rm", "-f", name); err != nil {
		t.Fatalf("docker rm: %v: %s", err, out)
	}
	start()
	moved := matterHAConnect(t, "127.0.0.1:"+port, logs)
	if moved.compressedFabricID != first.compressedFabricID || moved.fabricID != first.fabricID {
		t.Errorf("recreate changed the fabric: %+v -> %+v", first, moved)
	}
}

// buildMatterStandin builds testdata/matterjs-standin for the pinned
// release. A host whose egress intercepts TLS hands its CA to npm as a
// build secret (never a layer).
func buildMatterStandin(t *testing.T) string {
	t.Helper()
	tag := "wildstone-matterjs-standin:" + matterServerVersion
	args := []string{"build", "--network", "host", "--build-arg", "MATTERJS_SERVER_VERSION=" + matterServerVersion, "-t", tag}
	if ca := os.Getenv("NODE_EXTRA_CA_CERTS"); ca != "" {
		args = append(args, "--secret", "id=extra_ca,src="+ca)
	}
	args = append(args, filepath.Join("testdata", "matterjs-standin"))
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("building the stand-in: %v: %s", err, out)
	}
	t.Logf("NOT the pinned image: running the stand-in %s (upstream's Dockerfile, matter-server %s from npm)", tag, matterServerVersion)
	return tag
}

func waitHealthy(t *testing.T, run func(...string) (string, error), name string, logs func() string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		out, _ := run("inspect", "--format", "{{.State.Health.Status}}", name)
		status = strings.TrimSpace(out)
		if status == "healthy" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("the image's healthcheck never passed (status %q): it reads LISTEN_ADDRESS/PORT and must agree with them\n%s", status, logs())
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func repoRootDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

type matterServerInfo struct {
	fabricID           string
	compressedFabricID string
	schemaVersion      int
	sdkVersion         string
}

// matterHAConnect does what Home Assistant's Matter integration does on
// setup (matter_server.client: MatterClient.connect, then
// start_listening): open /ws, read the server_info message the server
// sends unprompted, then ask for every node and expect a result.
func matterHAConnect(t *testing.T, hostport string, logs func() string) matterServerInfo {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		info, err := tryMatterHAConnect(hostport)
		if err == nil {
			return info
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no Home-Assistant-style client got through at %s: %v\ncontainer log:\n%s", hostport, lastErr, logs())
	return matterServerInfo{}
}

func tryMatterHAConnect(hostport string) (matterServerInfo, error) {
	conn, err := net.DialTimeout("tcp", hostport, 3*time.Second)
	if err != nil {
		return matterServerInfo{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", hostport, base64.StdEncoding.EncodeToString(nonce))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return matterServerInfo{}, fmt.Errorf("handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return matterServerInfo{}, fmt.Errorf("handshake: %s", resp.Status)
	}

	var info struct {
		FabricID           json.Number `json:"fabric_id"`
		CompressedFabricID json.Number `json:"compressed_fabric_id"`
		SchemaVersion      int         `json:"schema_version"`
		MinSchemaVersion   int         `json:"min_supported_schema_version"`
		SDKVersion         string      `json:"sdk_version"`
	}
	if err := readWSJSON(br, &info); err != nil {
		return matterServerInfo{}, fmt.Errorf("server_info: %w", err)
	}
	if info.SchemaVersion == 0 || info.SDKVersion == "" {
		return matterServerInfo{}, fmt.Errorf("first message is not server_info: %+v", info)
	}

	if err := writeWSJSON(conn, map[string]any{"message_id": "listen", "command": "start_listening"}); err != nil {
		return matterServerInfo{}, err
	}
	for {
		var msg struct {
			MessageID string          `json:"message_id"`
			Result    json.RawMessage `json:"result"`
			ErrorCode *int            `json:"error_code"`
			Details   string          `json:"details"`
		}
		if err := readWSJSON(br, &msg); err != nil {
			return matterServerInfo{}, fmt.Errorf("start_listening: %w", err)
		}
		if msg.MessageID != "listen" {
			continue // an event
		}
		if msg.ErrorCode != nil {
			return matterServerInfo{}, fmt.Errorf("start_listening failed: %d %s", *msg.ErrorCode, msg.Details)
		}
		var nodes []json.RawMessage
		if err := json.Unmarshal(msg.Result, &nodes); err != nil {
			return matterServerInfo{}, fmt.Errorf("start_listening result is not a node list: %s", msg.Result)
		}
		break
	}
	return matterServerInfo{
		fabricID:           info.FabricID.String(),
		compressedFabricID: info.CompressedFabricID.String(),
		schemaVersion:      info.SchemaVersion,
		sdkVersion:         info.SDKVersion,
	}, nil
}
