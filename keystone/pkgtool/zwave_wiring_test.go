package pkgtool

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/trait"
)

// The managed Z-Wave JS UI package (#444). Each test pins one clause of
// the issue's "done when" that can be judged from what the package
// renders; TestZwaveJSUIServesHomeAssistant (Docker-gated) boots the
// pinned image on the same output to judge the rest.

const zwavePackage = "zwave-zwave-js-ui"

func renderedZwave(t *testing.T) (*pkgspec.Package, []resource.Resource) {
	t.Helper()
	sp, ok := walkShipped(t)[zwavePackage]
	if !ok {
		t.Fatalf("%s is not in the catalog", zwavePackage)
	}
	return sp.pkg, sp.rendered
}

func zwaveContainer(t *testing.T, rendered []resource.Resource) resource.Resource {
	t.Helper()
	var found []resource.Resource
	for _, r := range rendered {
		if r.Kind == resource.KindContainer && r.Container != nil {
			found = append(found, r)
		}
	}
	if len(found) != 1 || found[0].Metadata.Name != "zwave-js-ui" {
		t.Fatalf("want exactly one container, zwave-js-ui; got %d", len(found))
	}
	return found[0]
}

func zwaveConfigFile(t *testing.T, rendered []resource.Resource, filename string) string {
	t.Helper()
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.ConfigFile.Filename == filename {
			return r.ConfigFile.Content
		}
	}
	t.Fatalf("%s renders no ConfigFile %s", zwavePackage, filename)
	return ""
}

func zwaveSeed(t *testing.T, rendered []resource.Resource, file string) resource.SeedFileSpec {
	t.Helper()
	for _, r := range rendered {
		if r.Kind == resource.KindSeedFile && r.SeedFile != nil && r.SeedFile.Path == file {
			return *r.SeedFile
		}
	}
	t.Fatalf("%s seeds no %s", zwavePackage, file)
	return resource.SeedFileSpec{}
}

func zwaveParam(t *testing.T, pkg *pkgspec.Package, name string) pkgspec.ParamDecl {
	t.Helper()
	for _, d := range pkg.Params {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("%s has no %s param", zwavePackage, name)
	return pkgspec.ParamDecl{}
}

// The stick is named by its stable by-id alias and handed over as a
// device mapping, which is what duplicate-radio detection (#435) keys on.
// Inside the container it is always /dev/zwave: the host path is a long
// mixed-case run the policy engine reads as secret-shaped, so it must not
// travel in env, where secret-values-forbidden would refuse every real
// install.
func TestZwaveStickIsAStableSerialDevice(t *testing.T) {
	pkg, rendered := renderedZwave(t)
	d := zwaveParam(t, pkg, "usb_device")
	if !d.Required || d.Default != "" {
		t.Error("usb_device must be required with no default: the stick is this machine's, not the package's")
	}
	if !strings.HasPrefix(d.Example, "/dev/serial/by-id/") {
		t.Errorf("usb_device example %q is not a /dev/serial/by-id/ name", d.Example)
	}
	if !strings.Contains(d.Description, "/dev/serial/by-id/") {
		t.Error("usb_device's description must tell the operator to use the by-id name")
	}

	c := zwaveContainer(t, rendered).Container
	if len(c.Devices) != 1 {
		t.Fatalf("want exactly one device mapping, got %v", c.Devices)
	}
	if c.Devices[0].Source != d.Example {
		t.Errorf("device source %q: want the usb_device param verbatim (%q)", c.Devices[0].Source, d.Example)
	}
	if c.Devices[0].Target != "/dev/zwave" || c.Env["ZWAVE_PORT"] != c.Devices[0].Target {
		t.Errorf("ZWAVE_PORT %q and device target %q must both be /dev/zwave", c.Env["ZWAVE_PORT"], c.Devices[0].Target)
	}
	for k, v := range c.Env {
		if strings.Contains(v, "/dev/serial") || strings.Contains(v, "/dev/tty") {
			t.Errorf("env %s carries a host device path (%q); the device mapping carries it", k, v)
		}
	}
}

// The WS server — unauthenticated by nature — is published on the one
// private address the operator chose, and on nothing else. The UI port
// is not published at all: it is reachable only through its Route.
func TestZwaveWSServerIsBoundToAConfiguredPrivateAddress(t *testing.T) {
	pkg, rendered := renderedZwave(t)
	c := zwaveContainer(t, rendered).Container
	if len(c.Ports) != 1 {
		t.Fatalf("want exactly one published port (the WS server), got %v", c.Ports)
	}
	m, ok := resource.ParsePortMapping(c.Ports[0])
	if !ok || m.HostIP == "" || m.ContainerPort != 3000 || m.Protocol != "tcp" {
		t.Errorf("published %q: want <private address>:<port>:3000", c.Ports[0])
	}
	d := zwaveParam(t, pkg, "listen_addr")
	if !d.Required || d.Default != "" {
		t.Error("listen_addr must be required with no default: exposure is the operator's explicit choice")
	}
	if d.Validate != "private_ipv4" {
		t.Errorf("listen_addr validates %q; want private_ipv4", d.Validate)
	}
	if p := zwaveParam(t, pkg, "ws_port"); p.Validate != "port" || p.Default != "3000" {
		t.Errorf("ws_port: validate %q default %q; want port / 3000", p.Validate, p.Default)
	}
}

// The management UI is an operator tool behind the portal.
func TestZwaveUIIsPrivate(t *testing.T) {
	pkg, rendered := renderedZwave(t)
	var routes []resource.Resource
	for _, r := range rendered {
		if r.Kind == resource.KindRoute {
			routes = append(routes, r)
		}
	}
	if len(routes) != 1 {
		t.Fatalf("want exactly one Route, got %d", len(routes))
	}
	rt := routes[0].Route
	if rt.Upstream != "container/zwave-js-ui:8091" {
		t.Errorf("Route upstream %q, want container/zwave-js-ui:8091", rt.Upstream)
	}
	if rt.Auth != "authelia_oidc" || rt.Classification != "admin_tailscale" {
		t.Errorf("Route auth %q classification %q; want authelia_oidc / admin_tailscale", rt.Auth, rt.Classification)
	}
	if len(pkg.Policies.Classifications) != 1 || pkg.Policies.Classifications[0] != "admin_tailscale" {
		t.Errorf("policies.classifications = %v, want [admin_tailscale]", pkg.Policies.Classifications)
	}
}

// The WS server is on through the image's external-settings file (which
// wins over settings.json and shows as managed in the UI), and the MQTT
// gateway starts disabled in the seeded settings.json.
func TestZwaveServerOnAndMqttGatewayOff(t *testing.T) {
	_, rendered := renderedZwave(t)
	c := zwaveContainer(t, rendered).Container

	ext := c.Env["ZWAVE_EXTERNAL_SETTINGS"]
	if ext == "" {
		t.Fatal("ZWAVE_EXTERNAL_SETTINGS is not set: the WS server would be whatever settings.json says")
	}
	var mounted bool
	for _, m := range c.Mounts {
		if m.Target == path.Dir(ext) && m.Source == "/etc/wildstone/zwave" && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("%s is not inside the read-only mount of this capability's managed directory", ext)
	}
	var external map[string]any
	if err := json.Unmarshal([]byte(zwaveConfigFile(t, rendered, path.Base(ext))), &external); err != nil {
		t.Fatalf("external settings do not parse: %v", err)
	}
	if external["serverEnabled"] != true || external["serverPort"] != float64(3000) {
		t.Errorf("external settings %v: want serverEnabled true on port 3000", external)
	}
	if _, ok := external["securityKeys"]; ok {
		t.Error("external settings carry security keys: those would be plan content")
	}

	seed := zwaveSeed(t, rendered, "settings.json")
	if seed.Volume != "zwave-js-ui-store" {
		t.Errorf("settings.json is seeded into %q, want the store volume", seed.Volume)
	}
	var settings struct {
		Mqtt struct {
			Disabled *bool `json:"disabled"`
		} `json:"mqtt"`
		Gateway struct {
			HassDiscovery bool `json:"hassDiscovery"`
		} `json:"gateway"`
		Zwave map[string]any `json:"zwave"`
	}
	if err := json.Unmarshal([]byte(seed.Content), &settings); err != nil {
		t.Fatalf("seeded settings.json does not parse: %v", err)
	}
	if settings.Mqtt.Disabled == nil || !*settings.Mqtt.Disabled {
		t.Error("the MQTT gateway is not disabled in the seeded settings")
	}
	if settings.Gateway.HassDiscovery {
		t.Error("MQTT discovery is on: Home Assistant would see every device twice")
	}
	if settings.Zwave["serverEnabled"] != true {
		t.Error("settings.json disagrees with the external settings about the WS server")
	}
	for _, k := range []string{"securityKeys", "securityKeysLongRange", "port"} {
		if _, ok := settings.Zwave[k]; ok {
			t.Errorf("seeded settings.json sets zwave.%s; keys are generated on the node and the port comes from ZWAVE_PORT", k)
		}
	}
}

// S0/S2 keys are app-owned generated state: never a param, never a
// Wildstone secret, never anything that reaches a plan. The generator
// runs as the container's command and only fills keys that are absent.
func TestZwaveSecurityKeysNeverReachThePlan(t *testing.T) {
	pkg, rendered := renderedZwave(t)
	keyish := regexp.MustCompile(`(?i)(^|_)(key|s0|s2|network)(_|$)`)
	for _, d := range pkg.Params {
		if keyish.MatchString(d.Name) {
			t.Errorf("param %s looks like key material; the keys are generated on the node", d.Name)
		}
	}
	hex := regexp.MustCompile(`\b[0-9a-fA-F]{32}\b`)
	for _, r := range rendered {
		switch {
		case r.Kind == resource.KindSecret && r.Secret != nil && r.Secret.Name != "restic-zwave":
			t.Errorf("Secret %s: the only secret is the backup repository's", r.Secret.Name)
		case r.Kind == resource.KindConfigFile && hex.MatchString(r.ConfigFile.Content):
			t.Errorf("ConfigFile %s holds a 32-hex value", r.Metadata.Name)
		case r.Kind == resource.KindSeedFile && hex.MatchString(r.SeedFile.Content):
			t.Errorf("SeedFile %s holds a 32-hex value", r.Metadata.Name)
		}
	}
	c := zwaveContainer(t, rendered).Container
	for k := range c.Env {
		if strings.HasPrefix(k, "KEY_") || k == "NETWORK_KEY" {
			t.Errorf("env %s would override the store's keys on every start and put them in the plan", k)
		}
	}

	start := zwaveConfigFile(t, rendered, "start.mjs")
	if len(c.Command) != 2 || c.Command[0] != "node" || path.Base(c.Command[1]) != "start.mjs" {
		t.Fatalf("command %v: want node <managed dir>/start.mjs", c.Command)
	}
	for _, want := range []string{
		"S0_Legacy", "S2_Unauthenticated", "S2_Authenticated", "S2_AccessControl", "securityKeysLongRange",
		"randomBytes(16)",
		// Only an absent key is generated; a present one is never touched.
		"if (!settings.zwave[group][name])",
		// It hands over to the image's own entrypoint.
		"server/bin/www.js",
	} {
		if !strings.Contains(start, want) {
			t.Errorf("start.mjs lacks %q", want)
		}
	}
	if strings.Contains(start, "console.log(settings") || strings.Contains(start, "JSON.stringify(settings.zwave") {
		t.Error("start.mjs prints the settings, keys included")
	}
}

// The store — keys, node table, nightly NVM backups — is mounted where
// the image keeps it and backed up daily.
func TestZwaveStoreIsPersistentAndBackedUp(t *testing.T) {
	_, rendered := renderedZwave(t)
	c := zwaveContainer(t, rendered).Container
	var stored bool
	for _, m := range c.Mounts {
		if m.Source == "zwave-js-ui-store" && m.Target == "/usr/src/app/store" && !m.ReadOnly {
			stored = true
		}
	}
	if !stored {
		t.Error("zwave-js-ui-store is not mounted read-write at /usr/src/app/store")
	}
	var plans int
	for _, r := range rendered {
		if r.Kind != resource.KindBackupPlan {
			continue
		}
		plans++
		bp := r.BackupPlan
		if len(bp.Volumes) != 1 || bp.Volumes[0] != "zwave-js-ui-store" || bp.Schedule != "daily" {
			t.Errorf("BackupPlan %s: volumes %v schedule %q; want [zwave-js-ui-store] daily", r.Metadata.Name, bp.Volumes, bp.Schedule)
		}
	}
	if plans != 1 {
		t.Errorf("want one BackupPlan, got %d", plans)
	}
	var settings struct {
		Backup struct {
			NvmBackup bool   `json:"nvmBackup"`
			NvmCron   string `json:"nvmCron"`
		} `json:"backup"`
	}
	if err := json.Unmarshal([]byte(zwaveSeed(t, rendered, "settings.json").Content), &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.Backup.NvmBackup || settings.Backup.NvmCron == "" {
		t.Error("the nightly NVM backup is not on in the seeded settings")
	}
}

// The package provides zwave:server and fills every key, from what it
// published — never the container name.
func TestZwaveFillsEveryServerExport(t *testing.T) {
	pkg, _ := renderedZwave(t)
	if pkg.Metadata.Instanced {
		t.Error("the package is instanced: a consumer would get one zwave:server answer per instance and no way to choose")
	}
	tr, _ := trait.Lookup("zwave")
	mode, ok := tr.LookupMode("server")
	if !ok {
		t.Fatal("trait zwave has no server mode")
	}
	var exports map[string]string
	for _, p := range pkg.Provides {
		if p.Trait == "zwave" && p.Mode == "server" {
			exports = p.Exports
		}
	}
	if exports == nil {
		t.Fatal("zwave-zwave-js-ui does not provide zwave:server")
	}
	for _, key := range mode.ExportKeys() {
		expr, ok := exports[key]
		if !ok {
			t.Errorf("does not export %s", key)
			continue
		}
		if !strings.Contains(expr, `published`) || strings.Contains(expr, "//zwave-js-ui") {
			t.Errorf("%s = %s: want the published address, never the container name", key, expr)
		}
	}
}
