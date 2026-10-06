package pkgtool

// An opt-in test that boots the REAL pinned Mosquitto image on the
// configuration the broker package renders, published exactly as its
// rendered port mapping says, and authenticates to it with what the
// shipped CONSUMERS render — Zigbee2MQTT's env and Frigate's config —
// after substituting their secret references the way the node does.
//
// The clients are mosquitto_pub on the Wildstone-style bridge network,
// which is where both consumers run, rather than the consumers' own
// images: Zigbee2MQTT starts its Zigbee adapter before it connects to
// MQTT and exits without a coordinator, and Frigate is a multi-gigabyte
// image for one CONNECT. What is under test is the rendered host, port,
// username and password, which is what either image would send.
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run TestShippedMQTTConsumers -v

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
)

func TestShippedMQTTConsumersAuthenticateToTheSecuredBroker(t *testing.T) {
	if os.Getenv("WILDSTONE_DOCKER_TESTS") != "1" {
		t.Skip("set WILDSTONE_DOCKER_TESTS=1 to boot the real Mosquitto image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	run := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}

	// The broker's listen address: the default bridge's gateway, a
	// private address both the host and a bridge container reach, as a
	// LAN address does on a real node.
	gw, err := run("network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		t.Fatalf("docker network inspect: %v: %s", err, gw)
	}
	addr := strings.TrimSpace(gw)

	broker := loadShipped(t, "mqtt-mosquitto")
	brokerOut, err := capability.NewEngine().Expand(broker,
		map[string]string{"node": "n1", "listen_addr": addr, "listen_port": "18831"}, capability.NewCatalog(broker))
	if err != nil {
		t.Fatalf("expanding the broker on %s: %v", addr, err)
	}
	brokerSpec := findContainer(t, brokerOut, "mosquitto")
	if len(brokerSpec.Ports) != 1 {
		t.Fatalf("broker ports = %v", brokerSpec.Ports)
	}
	cat := capability.NewCatalog(loadShipped(t, "identity"), broker).WithCurrent(brokerOut)

	// The node's side: one generated password per Secret, and the
	// password file materialized as `$7$` hashes of them.
	passwords := map[string]string{}
	ref := regexp.MustCompile(`mosquitto:secret/([A-Za-z0-9._-]+)`)
	passwd := ref.ReplaceAllStringFunc(mosquittoConfigFile(t, brokerOut, "passwd"), func(m string) string {
		name := ref.FindStringSubmatch(m)[1]
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		passwords[name] = hex.EncodeToString(raw)
		salt := make([]byte, 12)
		if _, err := rand.Read(salt); err != nil {
			t.Fatal(err)
		}
		key, err := pbkdf2.Key(sha512.New, passwords[name], salt, 101, sha512.Size)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("$7$101$%s$%s", base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key))
	})
	// The agent substitutes an env value that is exactly secret/<name>.
	substitute := func(v string) string {
		name, ok := strings.CutPrefix(v, "secret/")
		if !ok {
			t.Fatalf("%q is not a secret reference", v)
		}
		p, ok := passwords[name]
		if !ok {
			t.Fatalf("the broker issues no secret/%s", name)
		}
		return p
	}

	// What each consumer would send: host, port, username, password.
	type client struct{ host, port, user, pass string }
	clients := map[string]client{}

	zOut, err := capability.NewEngine().Expand(loadShipped(t, "zigbee-zigbee2mqtt"), zigbeeParams("n1"), cat)
	if err != nil {
		t.Fatalf("expanding zigbee: %v", err)
	}
	zEnv := findContainer(t, zOut, "zigbee2mqtt").Env
	server := strings.TrimPrefix(zEnv["ZIGBEE2MQTT_CONFIG_MQTT_SERVER"], "mqtt://")
	zHost, zPort, ok := strings.Cut(server, ":")
	if !ok {
		t.Fatalf("ZIGBEE2MQTT_CONFIG_MQTT_SERVER = %q", zEnv["ZIGBEE2MQTT_CONFIG_MQTT_SERVER"])
	}
	clients["zigbee2mqtt"] = client{zHost, zPort, zEnv["ZIGBEE2MQTT_CONFIG_MQTT_USER"], substitute(zEnv["ZIGBEE2MQTT_CONFIG_MQTT_PASSWORD"])}

	fOut, err := capability.NewEngine().Expand(loadShipped(t, "cameras-frigate"), frigateParams("n1", "true"), cat)
	if err != nil {
		t.Fatalf("expanding frigate: %v", err)
	}
	fm := frigateMQTT(t, fOut)
	if fm["password"] != "{FRIGATE_MQTT_PASSWORD}" {
		t.Fatalf("frigate mqtt.password = %v", fm["password"])
	}
	clients["frigate"] = client{fmt.Sprint(fm["host"]), fmt.Sprint(fm["port"]), fmt.Sprint(fm["user"]),
		substitute(findContainer(t, fOut, "frigate").Env["FRIGATE_MQTT_PASSWORD"])}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mosquitto.conf"), []byte(mosquittoConfigFile(t, brokerOut, "mosquitto.conf")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "passwd"), []byte(passwd), 0o600); err != nil {
		t.Fatal(err)
	}

	name := "wildstone-mqtt-consumertest"
	_, _ = run("rm", "-f", name)
	t.Cleanup(func() { _, _ = run("rm", "-f", name) })
	if out, err := run("run", "-d", "--name", name,
		"-p", brokerSpec.Ports[0],
		"-v", dir+":/mosquitto/config:ro",
		brokerSpec.Image); err != nil {
		t.Fatalf("starting the broker: %v: %s", err, out)
	}

	pub := func(c client) (string, error) {
		return run("run", "--rm", brokerSpec.Image, "mosquitto_pub",
			"-h", c.host, "-p", c.port, "-u", c.user, "-P", c.pass, "-t", "wildstone/test", "-m", "hello")
	}

	// Wait for the broker, so a refusal below is its answer.
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := pub(clients["zigbee2mqtt"])
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := run("logs", name)
			t.Fatalf("broker never accepted Zigbee2MQTT's rendered credential %+v: %v: %s\nbroker log:\n%s",
				clients["zigbee2mqtt"], err, out, logs)
		}
		time.Sleep(time.Second)
	}
	f := clients["frigate"]
	if out, err := pub(f); err != nil {
		t.Errorf("Frigate's rendered credential was refused: %v: %s", err, out)
	}
	// And each is its own credential, not a shared one.
	f.pass = clients["zigbee2mqtt"].pass
	if out, err := pub(f); err == nil {
		t.Errorf("Zigbee2MQTT's password authenticated Frigate's username: %s", out)
	}
}
