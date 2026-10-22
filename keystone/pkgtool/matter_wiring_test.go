package pkgtool

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/trait"
)

// The managed Matter.js Server package (#442). Each test pins one clause
// of the issue that can be judged from what the package renders;
// TestMatterServerServesHomeAssistant (Docker-gated) boots the image on
// the same output to judge the rest.

const matterPackage = "matter-matterjs"

func renderedMatter(t *testing.T) (*pkgspec.Package, []resource.Resource) {
	t.Helper()
	sp, ok := walkShipped(t)[matterPackage]
	if !ok {
		t.Fatalf("%s is not in the catalog", matterPackage)
	}
	return sp.pkg, sp.rendered
}

func matterContainer(t *testing.T, rendered []resource.Resource) *resource.ContainerSpec {
	t.Helper()
	var found []resource.Resource
	for _, r := range rendered {
		if r.Kind == resource.KindContainer && r.Container != nil {
			found = append(found, r)
		}
	}
	if len(found) != 1 || found[0].Metadata.Name != "matter-server" {
		t.Fatalf("want exactly one container, matter-server; got %d", len(found))
	}
	return found[0].Container
}

// The current standalone server, not the archived Python one, pinned.
func TestMatterRunsTheMatterJSServerImage(t *testing.T) {
	_, rendered := renderedMatter(t)
	c := matterContainer(t, rendered)
	repo, digest, ok := strings.Cut(c.Image, "@sha256:")
	if !ok || len(digest) != 64 {
		t.Fatalf("image %q is not digest-pinned", c.Image)
	}
	if repo != "ghcr.io/matter-js/matterjs-server" {
		t.Errorf("image repository %q: want ghcr.io/matter-js/matterjs-server", repo)
	}
	if strings.Contains(c.Image, "python-matter-server") {
		t.Error("the archived Python Matter Server is not to be used")
	}
	// The image's ENTRYPOINT is the server; configuration is env only.
	if len(c.Command) != 0 {
		t.Errorf("command %v: the image's entrypoint runs the server, configured by env", c.Command)
	}
}

// Host mode, and the unauthenticated WebSocket on loopback only — through
// the image's own LISTEN_ADDRESS, which its healthcheck also reads.
func TestMatterWebSocketIsLoopbackOnly(t *testing.T) {
	pkg, rendered := renderedMatter(t)
	c := matterContainer(t, rendered)
	if !c.HostNetwork() {
		t.Error("matter-server is not on host networking: Matter's mDNS and IPv6 link-local traffic would not reach it")
	}
	if len(c.Ports) != 0 {
		t.Errorf("ports %v: a host-networked container publishes nothing", c.Ports)
	}
	if c.Env["LISTEN_ADDRESS"] != "127.0.0.1" {
		t.Errorf("LISTEN_ADDRESS = %q: the WebSocket must be bound to loopback only", c.Env["LISTEN_ADDRESS"])
	}
	if c.Env["PORT"] != "5580" {
		t.Errorf("PORT = %q, want 5580, what Home Assistant's Matter integration expects", c.Env["PORT"])
	}
	for _, d := range pkg.Params {
		switch d.Name {
		case "listen_addr", "listen_address", "port", "ws_port":
			t.Errorf("param %s would let the WebSocket off loopback or away from 5580", d.Name)
		}
	}
	for _, r := range rendered {
		if r.Kind == resource.KindRoute {
			t.Errorf("Route %s: the dashboard shares the unauthenticated WebSocket's port and is not proxied", r.Metadata.Name)
		}
	}
	if len(pkg.Requires) != 0 {
		t.Errorf("requires %v: with no Route there is nothing to require", pkg.Requires)
	}
}

// The primary interface is optional, validated, and reaches the server
// only when set.
func TestMatterPrimaryInterfaceIsOptional(t *testing.T) {
	pkg, _ := renderedMatter(t)
	var decl *pkgspec.ParamDecl
	for i := range pkg.Params {
		if pkg.Params[i].Name == "primary_interface" {
			decl = &pkg.Params[i]
		}
	}
	if decl == nil {
		t.Fatal("no primary_interface param")
	}
	if decl.Required || decl.Default != "" || decl.Validate != "interface_name" {
		t.Errorf("primary_interface: required %v default %q validate %q; want optional, empty, interface_name", decl.Required, decl.Default, decl.Validate)
	}
	expand := func(params map[string]string) (*resource.ContainerSpec, error) {
		out, err := capability.NewEngine().Expand(pkg, params, capability.NewCatalog(pkg))
		if err != nil {
			return nil, err
		}
		return matterContainer(t, out), nil
	}
	c, err := expand(map[string]string{"node": "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, set := c.Env["PRIMARY_INTERFACE"]; set {
		t.Error("PRIMARY_INTERFACE is set with no primary_interface")
	}
	if c, err = expand(map[string]string{"node": "n1", "primary_interface": "enp3s0"}); err != nil {
		t.Fatal(err)
	} else if c.Env["PRIMARY_INTERFACE"] != "enp3s0" {
		t.Errorf("PRIMARY_INTERFACE = %q, want enp3s0", c.Env["PRIMARY_INTERFACE"])
	}
	if _, err := expand(map[string]string{"node": "n1", "primary_interface": "eth0; rm -rf /"}); err == nil {
		t.Error("a primary_interface that is not an interface name was accepted")
	}
}

// The fabric is persistent, backed up as irreplaceable, and the only
// secret is the backup repository's.
func TestMatterFabricIsPersistentAndCritical(t *testing.T) {
	_, rendered := renderedMatter(t)
	c := matterContainer(t, rendered)
	var stored bool
	for _, m := range c.Mounts {
		if m.Source == "matter-data" && m.Target == "/data" && !m.ReadOnly {
			stored = true
		}
	}
	if !stored || c.Env["STORAGE_PATH"] != "/data" {
		t.Errorf("the fabric is not stored on matter-data at /data (mounts %v, STORAGE_PATH %q)", c.Mounts, c.Env["STORAGE_PATH"])
	}
	if !c.ReadOnlyRootfs {
		t.Error("the root filesystem is writable: anything written outside /data would be lost on recreate, unnoticed")
	}
	var plans int
	for _, r := range rendered {
		switch {
		case r.Kind == resource.KindBackupPlan:
			plans++
			bp := r.BackupPlan
			if len(bp.Volumes) != 1 || bp.Volumes[0] != "matter-data" || bp.Schedule != "daily" {
				t.Errorf("BackupPlan %s: volumes %v schedule %q; want [matter-data] daily", r.Metadata.Name, bp.Volumes, bp.Schedule)
			}
			// Identity-bearing: a lost fabric is every device re-commissioned.
			if bp.Class != "critical" {
				t.Errorf("BackupPlan %s class %q: the fabric is irreplaceable, want critical", r.Metadata.Name, bp.Class)
			}
		case r.Kind == resource.KindSecret && r.Secret.Name != "restic-matter":
			t.Errorf("Secret %s: the fabric's keys are the server's own state, not Wildstone secrets", r.Secret.Name)
		case r.Kind == resource.KindSeedFile || r.Kind == resource.KindConfigFile:
			t.Errorf("%s %s: nothing is written for the server; a root-owned file in /data would break the UID-1000 server", r.Kind, r.Metadata.Name)
		}
	}
	if plans != 1 {
		t.Errorf("want one BackupPlan, got %d", plans)
	}
}

// The package provides matter:server and fills every key from what the
// installed host-networked container was told to listen on.
func TestMatterFillsEveryServerExport(t *testing.T) {
	pkg, _ := renderedMatter(t)
	if pkg.Metadata.Instanced {
		t.Error("the package is instanced: a consumer would get one matter:server answer per instance and no way to choose")
	}
	tr, _ := trait.Lookup("matter")
	mode, ok := tr.LookupMode("server")
	if !ok {
		t.Fatal("trait matter has no server mode")
	}
	var exports map[string]string
	for _, p := range pkg.Provides {
		if p.Trait == "matter" && p.Mode == "server" {
			exports = p.Exports
		}
	}
	if exports == nil {
		t.Fatal("matter-matterjs does not provide matter:server")
	}
	for _, key := range mode.ExportKeys() {
		expr, ok := exports[key]
		if !ok {
			t.Errorf("does not export %s", key)
			continue
		}
		if !strings.Contains(expr, `hostNetworkEnv "matter-server"`) || strings.Contains(expr, "//matter-server") {
			t.Errorf("%s = %s: want the installed listener, never the container name", key, expr)
		}
	}
}
