package pkgtool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Shared walk for the tests that assert something about every SHIPPED
// package's rendered output. Extracted when a second such test arrived:
// two copies of the render loop is how one of them quietly stops seeing
// a package the other one checks.

// walkFillers supplies values for required params that must look real —
// a domain that is not a domain renders a Route no test can reason
// about. Anything else gets its default, or a marker.
var walkFillers = map[string]string{
	"public_domain":     "example.internal",
	"admin_domain":      "admin.example.internal",
	"node":              "n1",
	"admin_listen_addr": "100.64.0.1",
	"listen_addr":       "100.64.0.1",
}

// renderShippedPackages expands every package under packages/ against a
// catalog containing all of them, so a package with `requires` expands
// instead of skipping. A skip would be the same hole as a guard that
// silently stops running: green, and checking nothing.
func renderShippedPackages(t *testing.T) map[string][]resource.Resource {
	out := make(map[string][]resource.Resource)
	for name, sp := range walkShipped(t) {
		out[name] = sp.rendered
	}
	return out
}

// shipped pairs a package with what it renders, for the checks that need
// to read a package's own declarations to judge its output.
type shipped struct {
	pkg      *pkgspec.Package
	rendered []resource.Resource
}

// walkShipped is the single walk both accessors share. Two walks is how
// one of them quietly stops seeing a package the other one checks.
func walkShipped(t *testing.T) map[string]shipped {
	t.Helper()
	root, err := filepath.Abs("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	var all []*pkgspec.Package
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := pkgspec.Load(filepath.Join(root, e.Name(), "package.yaml"))
		if err != nil {
			t.Fatalf("load %s: %v", e.Name(), err)
		}
		names = append(names, e.Name())
		all = append(all, p)
	}
	if len(all) == 0 {
		t.Fatal("no packages found under packages/ — the walk is broken, not the packages")
	}

	catalog := capability.NewCatalog(all...)
	out := make(map[string]shipped, len(all))
	for i, pkg := range all {
		params := map[string]string{}
		for _, d := range pkg.Params {
			if v, ok := walkFillers[d.Name]; ok {
				params[d.Name] = v
			} else if d.Default != "" {
				params[d.Name] = d.Default
			} else {
				params[d.Name] = "lint-" + sanitizeParam(d.Name)
			}
		}
		rendered, err := capability.NewEngine().Expand(pkg, params, catalog)
		if err != nil {
			t.Fatalf("expand %s: %v", names[i], err)
		}
		out[names[i]] = shipped{pkg: pkg, rendered: rendered}
	}
	return out
}

// runnable is one container specification a package renders, with the
// resource that carried it.
//
// It exists because a ContainerSpec now reaches a node from two kinds:
// Container, and Job (whose Run is one). Every check that reasons about
// "what will actually run" has to see both, and the way it stops seeing
// one is by reaching through r.Container directly — which is exactly
// what happened when Job was added: the argv0 table, the whole point of
// which is to stop an F23 repeat, silently ignored the first shipped Job.
type runnable struct {
	kind resource.Kind
	name string
	spec *resource.ContainerSpec
}

// runnables returns every container specification in rendered, from
// whichever kind carries it.
func runnables(rendered []resource.Resource) []runnable {
	var out []runnable
	for _, r := range rendered {
		switch {
		case r.Kind == resource.KindContainer && r.Container != nil:
			out = append(out, runnable{r.Kind, r.Metadata.Name, r.Container})
		case r.Kind == resource.KindJob && r.Job != nil:
			out = append(out, runnable{r.Kind, r.Metadata.Name, &r.Job.Run})
		}
	}
	return out
}
