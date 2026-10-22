package pkgtool

// An opt-in test that boots the REAL pinned Mosquitto image on the
// configuration and password file this package renders, and connects to
// it the two ways a consumer will: from the host's own network stack (a
// host-networked Home Assistant) and from a bridge-networked container
// (Zigbee2MQTT, Frigate).
//
// It exists because the broker couples to Mosquitto's own password-file
// format and its privilege handling, and the failure mode when either
// breaks is invisible to every other test: the config renders, the plan
// applies, the container runs, and every client is refused.
//
// Opt-in rather than part of `go test ./...` because it needs a Docker
// daemon and pulls the broker image:
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run TestSecuredMosquitto -v
//
// The password file is materialized here the way the node does it
// (wardstone/internal/agent: mosquittoPasswordHash — `$7$`, PBKDF2-
// HMAC-SHA512, 101 iterations, 12-byte salt, 64-byte key, padded base64).
// keystone cannot import the agent, so the derivation is restated; the
// agent's own tests pin that side against the same format.

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

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func TestSecuredMosquittoAuthenticatesFromHostAndBridge(t *testing.T) {
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

	rendered := renderedMosquitto(t)
	var image string
	for _, r := range rendered {
		if r.Kind == resource.KindContainer && r.Container != nil && r.Metadata.Name == "mosquitto" {
			image = r.Container.Image
		}
	}
	if image == "" {
		t.Fatal("no mosquitto container in the rendered package")
	}

	// One random password per issued client, standing in for the node's
	// generated secrets; the file gets only their hashes.
	passwords := map[string]string{}
	ref := regexp.MustCompile(`mosquitto:secret/([A-Za-z0-9._-]+)`)
	passwd := ref.ReplaceAllStringFunc(mosquittoConfigFile(t, rendered, "passwd"), func(m string) string {
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
	pass, ok := passwords["mqtt-zigbee"]
	if !ok {
		t.Fatal("the password file issues no credential to zigbee")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mosquitto.conf"), []byte(mosquittoConfigFile(t, rendered, "mosquitto.conf")), 0o644); err != nil {
		t.Fatal(err)
	}
	// 0600, as the node writes a secret-bearing file.
	if err := os.WriteFile(filepath.Join(dir, "passwd"), []byte(passwd), 0o600); err != nil {
		t.Fatal(err)
	}

	// The listen address: the default bridge's gateway, a private address
	// on this host that both the host itself and a bridge container can
	// reach — the same property a LAN address has on a real node.
	gw, err := run("network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		t.Fatalf("docker network inspect: %v: %s", err, gw)
	}
	addr := strings.TrimSpace(gw)
	const port = "18830"

	name := "wildstone-mqtt-authtest"
	_, _ = run("rm", "-f", name)
	t.Cleanup(func() { _, _ = run("rm", "-f", name) })
	if out, err := run("run", "-d", "--name", name,
		"-p", addr+":"+port+":1883",
		"-v", dir+":/mosquitto/config:ro",
		image); err != nil {
		t.Fatalf("starting the broker: %v: %s", err, out)
	}

	pub := func(network string, args ...string) (string, error) {
		base := []string{"run", "--rm"}
		if network != "" {
			base = append(base, "--network", network)
		}
		base = append(base, image, "mosquitto_pub", "-h", addr, "-p", port, "-t", "wildstone/test", "-m", "hello")
		return run(append(base, args...)...)
	}

	// Wait for the broker to accept an authenticated client, so a refusal
	// below is the broker's answer and not a broker still starting.
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := pub("host", "-u", "zigbee", "-P", pass)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := run("logs", name)
			t.Fatalf("broker never accepted the zigbee credential: %v: %s\nbroker log:\n%s", err, out, logs)
		}
		time.Sleep(time.Second)
	}

	if out, err := pub("host"); err == nil {
		t.Errorf("an anonymous client was accepted: %s", out)
	}
	if out, err := pub("host", "-u", "zigbee", "-P", "wrong-"+pass); err == nil {
		t.Errorf("a wrong password was accepted: %s", out)
	}
	if out, err := pub("", "-u", "zigbee", "-P", pass); err != nil {
		t.Errorf("a bridge-networked client with the credential was refused: %v: %s", err, out)
	}
	if out, err := pub("", "-u", "home-assistant", "-P", passwords["mqtt-home-assistant"]); err != nil {
		t.Errorf("home-assistant's credential was refused: %v: %s", err, out)
	}
	if out, err := pub("", "-u", "home-assistant", "-P", pass); err == nil {
		t.Errorf("one client's password authenticated another: %s", out)
	}
}
