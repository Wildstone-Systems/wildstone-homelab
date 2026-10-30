package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestBindSourceRefusesDataDirOverlap pins the finding from the identity
// probe: /etc/wildstone is bind-mounted into the Caddy containers, and the
// agent's data directory defaults to /etc/wildstone/wardstone — so the
// internet-facing reverse proxy could read node.key, secret.key and every
// secret under them. The host dir is 0700 and the mount is readOnly;
// neither helps, because the container runs as uid 0 and reading is the
// compromise.
func TestBindSourceRefusesDataDirOverlap(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "etc", "wildstone")
	data := filepath.Join(prefix, "wardstone")
	a := &Agent{cfg: Config{DataDir: data}}

	refused := []struct{ name, src string }{
		{"the data dir itself", data},
		{"the prefix that contains it", prefix},
		{"an ancestor of the prefix", filepath.Dir(prefix)},
		{"the filesystem root", "/"},
		{"the secret store directly", filepath.Join(data, "secrets")},
		{"a single secret file", filepath.Join(data, "secret.key")},
	}
	for _, tc := range refused {
		t.Run("refuse/"+tc.name, func(t *testing.T) {
			oe := a.checkBindSource(tc.src)
			if oe == nil {
				t.Fatalf("checkBindSource(%q) allowed it; a container with this path reads the node's identity", tc.src)
			}
			if !strings.Contains(oe.GetMessage(), "data directory") {
				t.Errorf("message does not explain why: %q", oe.GetMessage())
			}
		})
	}

	allowed := []struct{ name, src string }{
		// The shape the ingress packages should move to (option 1/2).
		{"a sibling subtree under the prefix", filepath.Join(prefix, "caddy")},
		{"an unrelated absolute path", filepath.Join(root, "srv", "wildstone")},
		// A name that merely shares a textual prefix must not be caught:
		// the comparison is on whole path elements.
		{"a lookalike sibling of the data dir", data + "-other"},
		// Volume names are not host paths and are not this check's business.
		{"a docker volume name", "caddy-tailnet-data"},
		{"empty source", ""},
	}
	for _, tc := range allowed {
		t.Run("allow/"+tc.name, func(t *testing.T) {
			if oe := a.checkBindSource(tc.src); oe != nil {
				t.Fatalf("checkBindSource(%q) refused a legitimate mount: %s", tc.src, oe.GetMessage())
			}
		})
	}
}

// TestBindSourceResolvesSymlinks: a link is the obvious way to launder the
// path past a textual comparison.
func TestBindSourceResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "etc", "wildstone", "wardstone")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent")
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{DataDir: data}}
	if oe := a.checkBindSource(link); oe == nil {
		t.Fatal("a symlink to the data directory was allowed through")
	}
}

func TestBindSourceRefusalLeavesExistingContainerRunning(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "wardstone")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.DockerEndpoint = fake.URL()
		c.DataDir = data
		c.ManagedPrefixes = []string{root}
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	ensure := func(hash string, mounts ...*wsv1.Mount) *wsv1.OpResult {
		return dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
			EnsureContainer: &wsv1.EnsureContainer{Name: "app", Image: testDigestImage,
				Labels: map[string]string{SpecHashLabel: hash}, Mounts: mounts},
		}})
	}
	if oe := ensure("v1").GetError(); oe != nil {
		t.Fatalf("first ensure: %v", oe)
	}
	fake.mu.Lock()
	firstID := fake.containers["app"].id
	fake.mu.Unlock()

	overlap := &wsv1.Mount{Source: data, Target: "/private", ReadOnly: true}
	if oe := ensure("v2", overlap).GetError(); oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("overlapping bind result = %v, want FORBIDDEN", oe)
	}
	fake.mu.Lock()
	current := fake.containers["app"]
	fake.mu.Unlock()
	if current == nil || current.id != firstID || !current.running || current.labels[SpecHashLabel] != "v1" {
		t.Fatalf("bind refusal disturbed current workload: %+v", current)
	}
}
