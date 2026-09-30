package pkgtool

// An opt-in test that boots the REAL pinned Z-Wave JS UI image on the
// files this package renders — its start.mjs, its external settings and
// its seeded settings.json — against zwave-js's own mock controller, and
// then connects to the WS server the way Home Assistant's Z-Wave JS
// integration does: over the published private address, with a
// WebSocket handshake, set_api_schema and start_listening.
//
// It exists because the package couples to three things only the image
// can answer: that ZWAVE_EXTERNAL_SETTINGS really turns the WS server on,
// that start.mjs really hands over to Z-Wave JS UI, and that the keys it
// generates survive a restart and a reconfigure (a container recreated
// on the same store). Each fails silently everywhere else — the plan
// applies, the container runs, and Home Assistant cannot connect, or
// every secure device has to be re-included.
//
// There is no stick in CI, so the controller is the image's own
// mock-server (node_modules/zwave-js/bin/mock-server.cjs) on a private
// Docker network and ZWAVE_PORT points at it over tcp://. Everything
// else is the package's rendered container spec.
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run TestZwaveJSUIServesHomeAssistant -v

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func TestZwaveJSUIServesHomeAssistant(t *testing.T) {
	if os.Getenv("WILDSTONE_DOCKER_TESTS") != "1" {
		t.Skip("set WILDSTONE_DOCKER_TESTS=1 to boot the real Z-Wave JS UI image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	run := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}

	_, rendered := renderedZwave(t)
	c := zwaveContainer(t, rendered).Container

	// The managed directory, as the node writes it.
	managed := t.TempDir()
	var managedTarget string
	for _, m := range c.Mounts {
		if m.Source == "/etc/wildstone/zwave" {
			managedTarget = m.Target
		}
	}
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile {
			if err := os.WriteFile(filepath.Join(managed, r.ConfigFile.Filename), []byte(r.ConfigFile.Content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The store volume, with the seed in it, as the node leaves it before
	// the first start.
	store := t.TempDir()
	seed := zwaveSeed(t, rendered, "settings.json")
	if err := os.WriteFile(filepath.Join(store, seed.Path), []byte(seed.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Files the container wrote are root's; clear them from inside.
		_, _ = run("run", "--rm", "-v", store+":/s", "--entrypoint", "rm", c.Image, "-rf", "/s/.tmp", "/s/logs", "/s/backups", "/s/config", "/s/snippets")
	})

	gw, err := run("network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		t.Fatalf("docker network inspect: %v: %s", err, gw)
	}
	addr := strings.TrimSpace(gw)

	const (
		network = "wildstone-zwave-test"
		mock    = "wildstone-zwave-mock"
		name    = "wildstone-zwave-ui"
	)
	cleanup := func() {
		_, _ = run("rm", "-f", name, mock)
		_, _ = run("network", "rm", network)
	}
	cleanup()
	t.Cleanup(cleanup)
	if out, err := run("network", "create", network); err != nil {
		t.Fatalf("docker network create: %v: %s", err, out)
	}
	// --restart always: the mock controller exits when the driver's
	// first bytes on a fresh connection are not a whole frame ("does not
	// start with SOF"), a race in the mock rather than in anything under
	// test. A real stick shrugs that off; the mock is simply restarted,
	// and the driver's own retry reconnects to it. Its home id is fixed,
	// so a restarted mock is the same network.
	if out, err := run("run", "-d", "--name", mock, "--network", network, "--restart", "always",
		"--entrypoint", "node", c.Image, "/usr/src/app/node_modules/zwave-js/bin/mock-server.cjs"); err != nil {
		t.Fatalf("starting the mock controller: %v: %s", err, out)
	}

	// start runs the package's container spec — command, env, mounts,
	// one published port — with the controller swapped for the mock.
	start := func(port string) {
		t.Helper()
		args := []string{"run", "-d", "--name", name, "--network", network,
			"-p", addr + ":" + port + ":3000",
			"-v", managed + ":" + managedTarget + ":ro",
			"-v", store + ":/usr/src/app/store",
		}
		for k, v := range c.Env {
			if k == "ZWAVE_PORT" {
				v = "tcp://" + mock + ":5555"
			}
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, c.Image)
		args = append(args, c.Command...)
		if out, err := run(args...); err != nil {
			t.Fatalf("starting zwave-js-ui: %v: %s", err, out)
		}
	}
	logs := func() string {
		out, _ := run("logs", name)
		state, _ := run("inspect", "--format", "{{.State.Status}} exit={{.State.ExitCode}}", mock)
		mockLog, _ := run("logs", mock)
		return out + "\n--- mock controller (" + strings.TrimSpace(state) + "):\n" + mockLog
	}

	readKeys := func() map[string]string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(store, "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Mqtt  map[string]any `json:"mqtt"`
			Zwave struct {
				SecurityKeys          map[string]string `json:"securityKeys"`
				SecurityKeysLongRange map[string]string `json:"securityKeysLongRange"`
			} `json:"zwave"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("settings.json after start: %v", err)
		}
		if s.Mqtt["disabled"] != true {
			t.Errorf("the MQTT gateway is no longer disabled: %v", s.Mqtt)
		}
		out := map[string]string{}
		for k, v := range s.Zwave.SecurityKeys {
			out[k] = v
		}
		for k, v := range s.Zwave.SecurityKeysLongRange {
			out["LR_"+k] = v
		}
		return out
	}

	// ---- First start: keys generated, WS server answers a client.
	start("13000")
	first := haConnect(t, addr+":13000", logs)
	keys := readKeys()
	hex32 := regexp.MustCompile(`^[0-9A-F]{32}$`)
	for _, k := range []string{"S0_Legacy", "S2_Unauthenticated", "S2_Authenticated", "S2_AccessControl", "LR_S2_Authenticated", "LR_S2_AccessControl"} {
		if !hex32.MatchString(keys[k]) {
			t.Errorf("key %s = %q after first start; want 32 hex digits", k, keys[k])
		}
	}
	l := logs()
	for k, v := range keys {
		if v != "" && strings.Contains(l, v) {
			t.Errorf("key %s appears in the container log", k)
		}
	}
	if !strings.Contains(l, "MQTT is disabled") {
		t.Errorf("Z-Wave JS UI did not report its MQTT gateway disabled:\n%s", l)
	}

	// ---- Restart: same keys, same network.
	if out, err := run("restart", name); err != nil {
		t.Fatalf("docker restart: %v: %s", err, out)
	}
	again := haConnect(t, addr+":13000", logs)
	if got := readKeys(); !sameKeys(got, keys) {
		t.Errorf("restart changed the security keys")
	}
	if again.homeID != first.homeID {
		t.Errorf("restart changed the home id: %d -> %d", first.homeID, again.homeID)
	}

	// ---- Reconfigure: the container recreated with another published
	// port (a ws_port or listen_addr change), on the same store.
	if out, err := run("rm", "-f", name); err != nil {
		t.Fatalf("docker rm: %v: %s", err, out)
	}
	start("13001")
	moved := haConnect(t, addr+":13001", logs)
	if got := readKeys(); !sameKeys(got, keys) {
		t.Errorf("reconfigure changed the security keys")
	}
	if moved.homeID != first.homeID {
		t.Errorf("reconfigure changed the home id: %d -> %d", first.homeID, moved.homeID)
	}
}

func sameKeys(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

type zwaveServerInfo struct {
	homeID uint64
}

// haConnect does what Home Assistant's Z-Wave JS integration does on
// setup (zwave-js-server-python: Client.connect, then listen): read the
// server's version message, pin the API schema, start listening, and
// expect a successful result carrying the controller state.
func haConnect(t *testing.T, hostport string, logs func() string) zwaveServerInfo {
	t.Helper()
	deadline := time.Now().Add(150 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		info, err := tryHAConnect(hostport)
		if err == nil {
			return info
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no Home-Assistant-style client got through at %s: %v\ncontainer log:\n%s", hostport, lastErr, logs())
	return zwaveServerInfo{}
}

func tryHAConnect(hostport string) (zwaveServerInfo, error) {
	conn, err := net.DialTimeout("tcp", hostport, 3*time.Second)
	if err != nil {
		return zwaveServerInfo{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", hostport, base64.StdEncoding.EncodeToString(nonce))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return zwaveServerInfo{}, fmt.Errorf("handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return zwaveServerInfo{}, fmt.Errorf("handshake: %s", resp.Status)
	}

	var version struct {
		Type          string `json:"type"`
		ServerVersion string `json:"serverVersion"`
		HomeID        uint64 `json:"homeId"`
		MaxSchema     int    `json:"maxSchemaVersion"`
		DriverVersion string `json:"driverVersion"`
	}
	if err := readWSJSON(br, &version); err != nil {
		return zwaveServerInfo{}, fmt.Errorf("version message: %w", err)
	}
	if version.Type != "version" || version.ServerVersion == "" || version.MaxSchema == 0 {
		return zwaveServerInfo{}, fmt.Errorf("unexpected first message: %+v", version)
	}

	for i, cmd := range []map[string]any{
		{"messageId": "schema", "command": "set_api_schema", "schemaVersion": version.MaxSchema},
		{"messageId": "listen", "command": "start_listening"},
	} {
		if err := writeWSJSON(conn, cmd); err != nil {
			return zwaveServerInfo{}, err
		}
		// Skip events until this command's result arrives.
		for {
			var msg struct {
				Type      string          `json:"type"`
				MessageID string          `json:"messageId"`
				Success   bool            `json:"success"`
				ErrorCode string          `json:"errorCode"`
				Result    json.RawMessage `json:"result"`
			}
			if err := readWSJSON(br, &msg); err != nil {
				return zwaveServerInfo{}, fmt.Errorf("%s: %w", cmd["command"], err)
			}
			if msg.Type != "result" || msg.MessageID != cmd["messageId"] {
				continue
			}
			if !msg.Success {
				return zwaveServerInfo{}, fmt.Errorf("%s failed: %s", cmd["command"], msg.ErrorCode)
			}
			if i == 1 && !strings.Contains(string(msg.Result), `"controller"`) {
				return zwaveServerInfo{}, fmt.Errorf("start_listening returned no controller state")
			}
			break
		}
	}
	return zwaveServerInfo{homeID: version.HomeID}, nil
}

// writeWSJSON sends one masked text frame, as a client must (RFC 6455 5.3).
func writeWSJSON(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	hdr := []byte{0x81}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	hdr = append(hdr, mask...)
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	_, err = w.Write(append(hdr, payload...))
	return err
}

// readWSJSON reads one whole (possibly fragmented) text message and
// decodes it. Control frames are skipped.
func readWSJSON(r *bufio.Reader, v any) error {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 64<<20 {
			return fmt.Errorf("frame of %d bytes", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		if op == 0x8 {
			return fmt.Errorf("server closed the connection: %q", buf)
		}
		if op >= 0x8 {
			continue // ping/pong
		}
		msg = append(msg, buf...)
		if fin {
			return json.Unmarshal(msg, v)
		}
	}
}
