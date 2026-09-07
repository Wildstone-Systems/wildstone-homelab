package pkgtool

// An opt-in test that boots the REAL pinned Home Assistant image on the
// seed this package renders.
//
// It exists because this package couples to another project's on-disk
// settings store, and the failure mode when that coupling breaks is
// invisible: the install converges, the dashboard says healthy, and
// every proxied request returns 400.
//
// Opt-in rather than part of `go test ./...` because it needs a Docker
// daemon and pulls a ~1.5GB image:
//
//	WILDSTONE_DOCKER_TESTS=1 go test ./pkgtool/ -run TestSeededHomeAssistant -v

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func TestSeededHomeAssistantTrustsTheProxy(t *testing.T) {
	if os.Getenv("WILDSTONE_DOCKER_TESTS") != "1" {
		t.Skip("set WILDSTONE_DOCKER_TESTS=1 to boot the real Home Assistant image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}

	rendered, ok := renderShippedPackages(t)["home-assistant"]
	if !ok {
		t.Fatal("home-assistant is not in the catalog")
	}

	// Write the seeds exactly as the reconciler would place them.
	dir := t.TempDir()
	var image string
	for _, r := range rendered {
		if r.Kind == resource.KindContainer && r.Container != nil && r.Metadata.Name == "home-assistant" {
			image = r.Container.Image
		}
		if r.Kind != resource.KindSeedFile || r.SeedFile == nil {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(r.SeedFile.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(r.SeedFile.Content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if image == "" {
		t.Fatal("no home-assistant container image found in the rendered package")
	}

	name := "wildstone-seedtest"
	run := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}
	_, _ = run("rm", "-f", name)
	_, _ = run("volume", "rm", name)
	if out, err := run("volume", "create", name); err != nil {
		t.Fatalf("docker volume create: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = run("rm", "-f", name); _, _ = run("volume", "rm", name) })

	mount, err := exec.Command("docker", "volume", "inspect", name, "--format", "{{.Mountpoint}}").Output()
	if err != nil {
		t.Fatalf("volume inspect: %v", err)
	}
	if out, err := run("run", "--rm", "-v", name+":/seed", "-v", dir+":/src:ro",
		"docker.io/library/busybox", "sh", "-c", "cp -a /src/. /seed/"); err != nil {
		t.Fatalf("seeding the volume: %v: %s", err, out)
	}
	_ = strings.TrimSpace(string(mount))

	if out, err := run("run", "-d", "--name", name, "-v", name+":/config", image); err != nil {
		t.Fatalf("starting home-assistant: %v: %s", err, out)
	}

	// Wait for it to answer at all.
	deadline := time.Now().Add(5 * time.Minute)
	httpLine := regexp.MustCompile(`HTTP/1\.\d (\d{3})`)
	var code string
	for time.Now().Before(deadline) {
		out, _ := run("exec", name, "sh", "-c",
			`wget -S -O /dev/null --header="X-Forwarded-For: 10.1.2.3" http://127.0.0.1:8123/ 2>&1`)
		if m := httpLine.FindStringSubmatch(out); m != nil {
			code = m[1]
			break
		}
		time.Sleep(5 * time.Second)
	}
	if code == "" {
		t.Fatal("home-assistant never answered")
	}
	if code == "400" {
		t.Fatalf("a proxied request was refused (400): the seeded proxy trust did not take effect")
	}

	// The load-bearing assertion. A setting delivered through YAML lands
	// in `pending` as a five-minute trial and is REVERTED unless a person
	// promotes it in the UI — which is exactly the trap this seed exists
	// to avoid. `pending: null` with the value in `stable` is what says
	// there is no trial to lose.
	out, err := run("exec", name, "sh", "-c", "cat /config/.storage/http")
	if err != nil {
		t.Fatalf("reading the settings store: %v: %s", err, out)
	}
	var store struct {
		Data struct {
			Stable            map[string]any `json:"stable"`
			Pending           map[string]any `json:"pending"`
			YamlMigrationDone bool           `json:"yaml_migration_done"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &store); err != nil {
		t.Fatalf("settings store is not the JSON shape this package seeds (%v):\n%s", err, out)
	}
	if store.Data.Stable["use_x_forwarded_for"] != true {
		t.Errorf("use_x_forwarded_for is not in the confirmed `stable` slot: %v", store.Data.Stable)
	}
	if store.Data.Pending != nil {
		t.Errorf("a pending config exists, so the setting is on trial and will auto-revert: %v", store.Data.Pending)
	}
	if !store.Data.YamlMigrationDone {
		t.Error("yaml_migration_done is false: Home Assistant will migrate YAML and stage a trial")
	}
}
