package reconcile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestToolsITToolsClusterIntentGolden(t *testing.T) {
	pkg, err := pkgspec.Load(filepath.Join("..", "..", "packages", "tools-it-tools", "package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's identity requirement concerns package installation;
	// this golden pins the exact workload translation of its own templates.
	pkg.Requires = nil
	rs, err := capability.NewEngine().Expand(pkg, map[string]string{"node": "node-a", "public_domain": "example.test"}, capability.NewCatalog())
	if err != nil {
		t.Fatal(err)
	}
	var container resource.Resource
	for i := range rs {
		if rs[i].Kind == resource.KindContainer {
			rs[i].Container.Backend = "k3s"
			container = rs[i]
		}
	}
	if container.Container == nil {
		t.Fatal("fixture has no Container")
	}
	intent, err := ClusterIntent(container, rs)
	if err != nil {
		t.Fatal(err)
	}
	w, err := wireWorkload(intent)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := protojson.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, wire, "", "  "); err != nil {
		t.Fatal(err)
	}
	got := formatted.Bytes()
	got = append(got, '\n')
	golden := filepath.Join("testdata", "tools-it-tools-workload.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("tools-it-tools workload changed:\n%s\nwant:\n%s", got, want)
	}
	if len(intent.ServicePorts) != 1 || intent.ServicePorts[0].ContainerPort != 80 {
		t.Fatalf("Route service port = %+v", intent.ServicePorts)
	}
	container.Container.Mounts = []resource.Mount{{Source: "data", Target: "/data"}}
	if _, err := ClusterIntent(container, rs); err == nil || !strings.Contains(err.Error(), "stateless") {
		t.Fatalf("mounted workload accepted: %v", err)
	}
}
