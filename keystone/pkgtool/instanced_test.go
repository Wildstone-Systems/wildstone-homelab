package pkgtool

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// expandInstance renders a shipped package as one named instance, with
// every other param filled the way walkShipped fills them.
func expandInstance(t *testing.T, pkg *pkgspec.Package, instance string) []resource.Resource {
	t.Helper()
	params := map[string]string{}
	for _, d := range pkg.Params {
		switch {
		case d.Name == pkgspec.InstanceParam:
			params[d.Name] = instance
		case walkFillers[d.Name] != "":
			params[d.Name] = walkFillers[d.Name]
		case d.Default != "":
			params[d.Name] = d.Default
		case d.Example != "":
			params[d.Name] = d.Example
		default:
			params[d.Name] = "lint-" + sanitizeParam(d.Name)
		}
	}
	out, err := capability.NewEngine().Expand(pkg, params, capability.NewCatalog(pkg))
	if err != nil {
		t.Fatalf("%s as %q: %v", pkg.Metadata.Name, instance, err)
	}
	return out
}

// Two instances of an instanced package share nothing (#246): every
// resource one renders is named apart from the other's and owned by its
// own instance. A single shared name would let installing the second
// bot take over the first's container or token.
func TestInstancesOfAPackageShareNothing(t *testing.T) {
	checked := 0
	for name, sp := range walkShipped(t) {
		if !sp.pkg.Metadata.Instanced {
			continue
		}
		checked++
		a, b := expandInstance(t, sp.pkg, "joan"), expandInstance(t, sp.pkg, "alfred")
		seen := map[string]bool{}
		for _, r := range a {
			seen[string(r.Kind)+"/"+r.Metadata.Name] = true
			if r.Metadata.Owner != "capability/"+sp.pkg.Metadata.Capability+"-joan" {
				t.Errorf("%s: %s/%s is owned by %q, not the instance", name, r.Kind, r.Metadata.Name, r.Metadata.Owner)
			}
		}
		for _, r := range b {
			if seen[string(r.Kind)+"/"+r.Metadata.Name] {
				t.Errorf("%s: both instances render %s/%s", name, r.Kind, r.Metadata.Name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no instanced package in the catalog — the chat packages should be")
	}
}

// Each chat platform's package provides exactly the credentials that
// platform needs, plus the adapter's Keystone token, and its container
// reads each of them (ADR-W033 B7.1). The Console's connect form asks
// for these and nothing else.
func TestEachChatPlatformProvidesItsCredentials(t *testing.T) {
	want := map[string][]string{
		"chat-telegram": {"adapter", "bot"},
		"chat-discord":  {"adapter", "bot"},
		"chat-slack":    {"adapter", "app", "bot"},
		"chat-matrix":   {"adapter", "token"},
	}
	for name, suffixes := range want {
		pkg, err := pkgspec.Load(filepath.Join("..", "..", "packages", name, "package.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !pkg.Metadata.Instanced {
			t.Errorf("%s is not instanced: a household could run only one bot on it", name)
		}
		base := name + "-joan-"
		var provided []string
		env := map[string]bool{}
		for _, r := range expandInstance(t, pkg, "joan") {
			switch {
			case r.Kind == resource.KindSecret && r.Secret.Provided:
				provided = append(provided, strings.TrimPrefix(r.Secret.Name, base))
			case r.Kind == resource.KindContainer:
				for _, v := range r.Container.Env {
					env[v] = true
				}
			}
		}
		sort.Strings(provided)
		if strings.Join(provided, ",") != strings.Join(suffixes, ",") {
			t.Errorf("%s provides %v, want %v", name, provided, suffixes)
		}
		for _, sfx := range suffixes {
			if !env["secret/"+base+sfx] {
				t.Errorf("%s: the container never reads secret/%s%s", name, base, sfx)
			}
		}
	}
}
