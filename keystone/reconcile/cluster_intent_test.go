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
	if err := ClusterRouteConflict(container, rs); err == nil || !strings.Contains(err.Error(), "#212") {
		t.Fatalf("routed k3s install must wait for coexistence upstream: %v", err)
	}
	container.Container.Mounts = []resource.Mount{{Source: "data", Target: "/data"}}
	if _, err := ClusterIntent(container, rs); err == nil || !strings.Contains(err.Error(), "stateless") {
		t.Fatalf("mounted workload accepted: %v", err)
	}
}

func TestClusterIntentSupportsAnUnroutedExposedPort(t *testing.T) {
	container := containerResource("worker", testDigestImage)
	container.Metadata.Owner, container.Metadata.Package = "capability/worker", "worker"
	container.Container.Backend = "k3s"
	container.Container.Expose = []uint32{8080}

	intent, err := ClusterIntent(container, []resource.Resource{container})
	if err != nil {
		t.Fatal(err)
	}
	if len(intent.ServicePorts) != 1 || intent.ServicePorts[0] != (WorkloadPort{Name: "port-8080", ContainerPort: 8080, ServicePort: 8080, Protocol: "tcp"}) {
		t.Fatalf("unrouted exposed ports = %+v", intent.ServicePorts)
	}
	if err := ClusterRouteConflict(container, []resource.Resource{container}); err != nil {
		t.Fatalf("unrouted workload was rejected: %v", err)
	}
}

func TestClusterIntentRejectsUnresolvedSecretReferences(t *testing.T) {
	container := containerResource("worker", testDigestImage)
	container.Metadata.Owner, container.Metadata.Package = "capability/worker", "worker"
	container.Container.Backend = "k3s"
	container.Container.Expose = []uint32{8080}
	container.Container.Env = map[string]string{"TOKEN": "secret/app-token"}

	if _, err := ClusterIntent(container, []resource.Resource{container}); err == nil || !strings.Contains(err.Error(), "unresolved secret references") {
		t.Fatalf("secret-backed workload accepted: %v", err)
	}
}

func TestClusterIntentRejectsUnsupportedResourceQuantitiesBeforeApproval(t *testing.T) {
	for name, resources := range map[string]*resource.ContainerResources{
		"sub-millicore CPU":  {Cpus: "0.0001"},
		"overflowing memory": {Memory: "9999999999999999999g"},
	} {
		t.Run(name, func(t *testing.T) {
			container := containerResource("worker", testDigestImage)
			container.Metadata.Owner, container.Metadata.Package = "capability/worker", "worker"
			container.Container.Backend = "k3s"
			container.Container.Expose = []uint32{8080}
			container.Container.Resources = resources

			if _, err := ClusterIntent(container, []resource.Resource{container}); err == nil {
				t.Fatal("unsupported cluster resource quantity passed proposal preflight")
			}
		})
	}

	container := containerResource("worker", testDigestImage)
	container.Metadata.Owner, container.Metadata.Package = "capability/worker", "worker"
	container.Container.Backend = "k3s"
	container.Container.Expose = []uint32{8080}
	container.Container.Resources = &resource.ContainerResources{Memory: "9223372036854775807", Cpus: "0.001"}
	if _, err := ClusterIntent(container, []resource.Resource{container}); err != nil {
		t.Fatalf("boundary-supported resource quantities were rejected: %v", err)
	}
}
