package reconcile

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// dockerShapedOps are the Wardstone operations that only make sense to a
// Docker Engine: containers, images, volumes and the managed network.
// Host operations every backend needs (managed files, mounts, probes,
// secrets) are deliberately not here.
var dockerShapedOps = map[string]bool{
	"Operation_EnsureContainer":     true,
	"Operation_InspectContainer":    true,
	"Operation_ContainerStats":      true,
	"Operation_StopContainer":       true,
	"Operation_RemoveContainer":     true,
	"Operation_PullImage":           true,
	"Operation_EnsureVolume":        true,
	"Operation_EnsureDockerNetwork": true,
	"Operation_StatVolumeFile":      true,
	"Operation_SeedVolumeFile":      true,
}

// Only the Docker translator builds a Docker-shaped operation (W050
// slice 2, #158): a cluster backend then needs no change to how the
// reconciler decides, orders or approves anything.
func TestDockerOperationsStayInTheTranslator(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "docker.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && dockerShapedOps[sel.Sel.Name] {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "wsv1" {
					t.Errorf("%s builds wsv1.%s outside the Docker translator", f, sel.Sel.Name)
				}
			}
			return true
		})
	}
}

// recordingApplier passes operations through and records their kinds.
type recordingApplier struct {
	next Applier
	mu   sync.Mutex
	ops  []string
}

func (a *recordingApplier) Apply(ctx context.Context, nodeID string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	a.mu.Lock()
	name := strings.TrimPrefix(fmt.Sprintf("%T", op.GetOp()), "*wardstonev1.Operation_")
	if w := op.GetWriteManagedFile(); w != nil {
		name += ":" + filepath.Base(w.GetPath())
	}
	a.ops = append(a.ops, name)
	a.mu.Unlock()
	return a.next.Apply(ctx, nodeID, op)
}

func (a *recordingApplier) index(t *testing.T, name string) int {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, op := range a.ops {
		if op == name {
			return i
		}
	}
	t.Fatalf("no %s in %v", name, a.ops)
	return -1
}

// The order a fresh container is created in, which the translator
// extraction must not change: the managed network and the base Caddyfile
// before anything is attached to them, the declared secret and the volume
// before the container that reads them, and the image before the
// container that runs it.
func TestContainerCreateKeepsItsOrder(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	app := containerResource("app", testDigestImage)
	app.Container.Env = map[string]string{"TOKEN": "secret/app-token"}
	app.Container.Mounts = []resource.Mount{{Source: "data", Target: "/data"}}
	desired := []resource.Resource{
		secretResource("app-token", ""),
		volumeResource("data"),
		app,
	}

	rec := &recordingApplier{next: &GatewayApplier{Gateway: h.gw}}
	outcome, err := newReconciler(rec).Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("not applied: %+v", outcome)
	}
	container := rec.index(t, "EnsureContainer")
	for _, before := range []string{"EnsureSecret", "EnsureVolume", "EnsureDockerNetwork", "WriteManagedFile:Caddyfile", "PullImage"} {
		if rec.index(t, before) > container {
			t.Errorf("%s ran after EnsureContainer: %v", before, rec.ops)
		}
	}
	if rec.index(t, "EnsureDockerNetwork") > rec.index(t, "PullImage") {
		t.Errorf("the image was pulled before the network existed: %v", rec.ops)
	}
}
