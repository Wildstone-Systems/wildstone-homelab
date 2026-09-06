package pkgtool

import (
	"path"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// defaultManagedPrefix is cmd/keystone's --managed-prefix default. A
// package's bind mount has to name an absolute host path, so it assumes
// this value; the assumption is stated here rather than left implicit in
// every package that mounts a subtree of it.
const defaultManagedPrefix = "/etc/wildstone"

// TestRenderedConfigFilesAreMounted asserts that every ConfigFile a
// shipped package renders is actually visible to a container in that
// same package.
//
// This is the failure the ConfigFile kind made possible and nothing
// caught: Keystone renders the file, the reconciler writes it to the
// node, every test is green, and the application never reads a byte of
// it because no mount puts it inside the container. Authelia shipped in
// exactly that state — a correct configuration.yml on disk while the
// container ran the config it had seeded for itself.
//
// The check is deliberately structural (is the path reachable?) rather
// than behavioural (does the app read it?): whether an app finds a
// mounted file by convention or by a flag is per-app, but a file no
// mount exposes cannot be read by any means at all.
func TestRenderedConfigFilesAreMounted(t *testing.T) {
	checked := 0
	for name, sp := range walkShipped(t) {
		rendered := sp.rendered
		t.Run(name, func(t *testing.T) {
			// A package may declare a rendered config as read by
			// something Wildstone does not run (pkgspec.ExternalConfig,
			// ADR-W024 A10) — the Home Assistant adapter is the shipped
			// case. The declaration is honoured here and audited below:
			// one that names no rendered file is as much a defect as an
			// unmounted config, for the same reason an unused policy
			// exemption fails startup (ADR-W009 A3).
			external := map[string]string{}
			for _, ec := range sp.pkg.ExternalConfig {
				external[ec.Filename] = ec.Consumer
			}
			claimed := map[string]bool{}

			for _, r := range rendered {
				if r.Kind != resource.KindConfigFile || r.ConfigFile == nil {
					continue
				}
				if consumer, ok := external[r.ConfigFile.Filename]; ok {
					claimed[r.ConfigFile.Filename] = true
					t.Logf("ConfigFile %q is declared external, read by %s", r.Metadata.Name, consumer)
					continue
				}
				capName, ok := strings.CutPrefix(r.Metadata.Owner, "capability/")
				if !ok || capName == "" {
					t.Errorf("ConfigFile %q has owner %q, not capability/<name> — the reconciler derives "+
						"the file's directory from that owner and cannot place this one", r.Metadata.Name, r.Metadata.Owner)
					continue
				}
				hostPath := path.Join(defaultManagedPrefix, capName, r.ConfigFile.Filename)
				checked++

				if inside := mountedInto(rendered, hostPath); len(inside) == 0 {
					t.Errorf("ConfigFile %q renders to %s, but no container in this package mounts it: "+
						"the file will be written to the node and read by nothing. Add a read-only mount of "+
						"%s to the container that consumes it.",
						r.Metadata.Name, hostPath, path.Dir(hostPath))
				}
			}

			for filename := range external {
				if !claimed[filename] {
					t.Errorf("externalConfig declares %q but this package renders no ConfigFile with that filename: "+
						"a stale declaration silently exempts nothing and hides the next real one", filename)
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped package rendered a ConfigFile — the walk is broken, not the packages")
	}
}

// mountedInto returns the in-container paths at which hostPath is
// visible, across every container in the rendered set. A mount whose
// source is a host directory covers everything beneath it; a named
// volume (no leading slash) covers nothing on the managed prefix.
func mountedInto(rendered []resource.Resource, hostPath string) []string {
	var out []string
	// Jobs count. A ConfigFile whose only reader is a provisioning job —
	// the group list's provisioning script, say — is mounted by nothing
	// if this only walks Containers, and the check would pass or fail
	// for the wrong reason.
	for _, r := range runnables(rendered) {
		for _, m := range r.spec.Mounts {
			if !strings.HasPrefix(m.Source, "/") {
				continue // named volume
			}
			rel, ok := underDir(m.Source, hostPath)
			if !ok {
				continue
			}
			out = append(out, path.Join(m.Target, rel))
		}
	}
	return out
}

// underDir reports whether file sits at or beneath dir, and where. It
// compares path SEGMENTS, so /etc/wildstone/identity-old does not count
// as being under /etc/wildstone/identity.
func underDir(dir, file string) (string, bool) {
	dir = path.Clean(dir)
	file = path.Clean(file)
	if dir == file {
		return "", true
	}
	if !strings.HasPrefix(file, dir+"/") {
		return "", false
	}
	return strings.TrimPrefix(file, dir+"/"), true
}

// TestAutheliaIsPointedAtItsManagedFiles is the behavioural half, for
// the one app where "mounted" is not enough: Authelia reads
// X_AUTHELIA_CONFIG's default (/config/configuration.yml, the file it
// seeds for itself) unless --config says otherwise, so a mounted config
// it is not told about is inert. Both files it needs are asserted here —
// its own configuration.yml, and the clients.yml Keystone merges from
// every OidcClient in the deployment.
//
// The two live in differently-named directories on purpose (a
// ConfigFile's directory comes from its owning capability; the
// reconciler's Authelia renderings are named for the provider that reads
// them, per ADR-W005 A3/B1), which is exactly the kind of split where a
// path drifts silently. Hence the assertion on the literal strings.
func TestAutheliaIsPointedAtItsManagedFiles(t *testing.T) {
	rendered, ok := renderShippedPackages(t)["identity"]
	if !ok {
		t.Fatal("no identity package under packages/")
	}

	var authelia *resource.Resource
	for i := range rendered {
		if rendered[i].Kind == resource.KindContainer && rendered[i].Metadata.Name == "authelia" {
			authelia = &rendered[i]
			break
		}
	}
	if authelia == nil {
		t.Fatal("the identity package renders no container named authelia")
	}

	want := []string{
		path.Join(defaultManagedPrefix, "identity", "configuration.yml"),
		path.Join(defaultManagedPrefix, "authelia", "clients.yml"),
	}
	cmd := authelia.Container.Command
	for _, w := range want {
		named := false
		for i, arg := range cmd {
			// --config <path>, and the comma-separated form Authelia
			// also accepts.
			if i > 0 && cmd[i-1] == "--config" && slicesContainsField(arg, w) {
				named = true
			}
			if after, ok := strings.CutPrefix(arg, "--config="); ok && slicesContainsField(after, w) {
				named = true
			}
		}
		if !named {
			t.Errorf("authelia's command does not pass --config %s (command: %q) — Authelia will fall back "+
				"to /config/configuration.yml, the config it seeds for itself, and this file will be inert", w, cmd)
			continue
		}
		if got := mountedInto(rendered, w); len(got) == 0 {
			t.Errorf("authelia is told to read %s but nothing mounts it — it will fail to start on a missing "+
				"config rather than quietly ignoring it", w)
		} else if got[0] != w {
			t.Errorf("authelia is told to read %s, but that host path is mounted at %s inside the container: "+
				"the path on the command line is the CONTAINER's, so these must agree", w, got[0])
		}
	}
}

// slicesContainsField reports whether a --config value (one path, or
// several comma-separated) contains want.
func slicesContainsField(value, want string) bool {
	for _, f := range strings.Split(value, ",") {
		if strings.TrimSpace(f) == want {
			return true
		}
	}
	return false
}
