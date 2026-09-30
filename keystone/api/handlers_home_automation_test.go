package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// The home-automation aggregate (#446), driven through the HTTP route
// against a fixture catalog, a seeded Statefile, observations, plans and
// a fake agent. Every fixture also passes haContractHolds: every blocker
// has a stable code, human text, evidence and a (possibly empty) action
// list, and every state is in the closed vocabulary.

const (
	haZigbeeAlias = "/dev/serial/by-id/usb-Sonoff_Zigbee-if00"
	haZigbeeID    = "serial-by-id:usb-Sonoff_Zigbee-if00"
)

// haFixtureCatalog is the home-automation catalog as it will look once
// every protocol package exists — except thread, which stays absent so
// `unavailable` is exercised — with home-assistant carrying the
// Bluetooth mode param.
func haFixtureCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	img := `"ghcr.io/example/app@` + testDigest + `"`
	pkgs := map[string]string{
		"mqtt-test": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: mqtt-test, version: 2.0.0, capability: mqtt }
provides:
  - { trait: mqtt, mode: broker, exports: { MqttHost: "192.0.2.10" } }
params:
  - { name: node, required: true, source: node }
  - { name: listen_addr, default: "192.0.2.10" }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: mosquitto, node: "{{ .Params.node }}" }
    spec: { image: ` + img + ` }
`,
		"hass-test": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: hass-test, version: 1.2.0, capability: home-assistant }
requires:
  - { trait: mqtt, mode: broker, when: mqtt_enabled }
params:
  - { name: mqtt_enabled, default: "false", validate: "enum:true|false" }
  - { name: node, required: true, source: node }
  - { name: bluetooth, default: "off", validate: "enum:proxy|host-bluez|off" }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: home-assistant, node: "{{ .Params.node }}" }
    spec: { image: ` + img + ` }
`,
		"external-test": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: external-test, version: 1.0.0, capability: home-automation }
params:
  - { name: node, required: true, source: node }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: ConfigFile
    metadata: { name: home-assistant-wildstone, node: "{{ .Params.node }}" }
    spec: { filename: wildstone.yaml, content: "x" }
`,
		"zigbee-test": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: zigbee-test, version: 2.0.0, capability: zigbee }
requires:
  - { trait: mqtt, mode: broker }
params:
  - { name: node, required: true, source: node }
  - { name: usb_device, default: "" }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: zigbee2mqtt, node: "{{ .Params.node }}" }
    spec: { image: ` + img + ` }
`,
		"zwave-test": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: zwave-test, version: 1.0.0, capability: zwave }
params:
  - { name: node, required: true, source: node }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: zwave-js-ui, node: "{{ .Params.node }}" }
    spec: { image: ` + img + ` }
`,
		"matter-test": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: matter-test, version: 1.0.0, capability: matter }
params:
  - { name: node, required: true, source: node }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: matter-server, node: "{{ .Params.node }}" }
    spec: { image: ` + img + ` }
`,
	}
	dir := t.TempDir()
	for name, body := range pkgs {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "package.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := catalog.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	return c
}

type haFixture struct {
	h      *harness
	calls  atomic.Int32
	live   []LiveNode
	checks []*wsv1.ReadinessCheck
	radios []*wsv1.Radio
}

func allReadyChecks() []*wsv1.ReadinessCheck {
	ready := wsv1.ReadinessState_READINESS_STATE_READY
	return []*wsv1.ReadinessCheck{
		{Check: "ipv6", State: ready, Subject: "/proc/sys/net/ipv6/conf/all/disable_ipv6", Value: "0"},
		{Check: "interface_ipv6", State: ready, Subject: "eth0"},
		{Check: "interface_ipv6", State: wsv1.ReadinessState_READINESS_STATE_MISSING, Subject: "wlan0"},
		{Check: "ipv6_forwarding", State: ready, Subject: "/proc/sys/net/ipv6/conf/all/forwarding", Value: "1"},
		{Check: "multicast", State: ready, Subject: "eth0"},
		{Check: "dbus_system_bus", State: ready, Subject: "/run/dbus/system_bus_socket"},
		{Check: "bluez_service", State: ready, Subject: "bluetooth.service"},
		{Check: "bluetooth_adapter", State: ready, Subject: "hci0"},
		{Check: "tun", State: ready, Subject: "/dev/net/tun"},
	}
}

func fixtureRadios() []*wsv1.Radio {
	return []*wsv1.Radio{
		{Id: haZigbeeID, Kind: wsv1.RadioKind_RADIO_KIND_ZIGBEE, KernelName: "ttyUSB1", DevPath: "/dev/ttyUSB1", ByIdPaths: []string{haZigbeeAlias}},
		{Id: zwaveID, Kind: wsv1.RadioKind_RADIO_KIND_ZWAVE, KernelName: "ttyUSB0", DevPath: zwaveTTY, ByIdPaths: []string{zwaveAlias}},
		{Id: "sysfs:/sys/devices/platform/bt", Kind: wsv1.RadioKind_RADIO_KIND_BLUETOOTH, KernelName: "hci0"},
	}
}

// newHAFixture: one live node n1 (minor 25) with every prerequisite ready
// and a Zigbee stick, a Z-Wave stick and a Bluetooth adapter.
func newHAFixture(t *testing.T) *haFixture {
	f := &haFixture{h: newHarness(t), checks: allReadyChecks(), radios: fixtureRadios()}
	f.h.s.Catalog = haFixtureCatalog(t)
	if err := f.h.s.DB.RecordNodeJoin("n1"); err != nil {
		t.Fatal(err)
	}
	f.live = []LiveNode{{NodeID: "n1", AgentVersion: "test", ProtocolMinor: 25, Since: time.Now()}}
	f.h.s.LiveNodes = func() []LiveNode { return f.live }
	f.h.s.Agents = applierFunc(func(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
		f.calls.Add(1)
		if node != "n1" {
			return nil, errors.New("node not connected")
		}
		switch op.GetOp().(type) {
		case *wsv1.Operation_ListRadios:
			return &wsv1.OpResult{Result: &wsv1.OpResult_ListRadios{ListRadios: &wsv1.ListRadiosResult{
				PlatformSupported: true, Radios: f.radios,
			}}}, nil
		case *wsv1.Operation_HomeAutomationReadiness:
			return &wsv1.OpResult{Result: &wsv1.OpResult_HomeAutomationReadiness{HomeAutomationReadiness: &wsv1.HomeAutomationReadinessResult{
				PlatformSupported: true,
				Interfaces: []*wsv1.NodeInterface{
					{Name: "eth0", Addresses: []string{"192.0.2.10/24", "fe80::1/64"}, Up: true, Multicast: true, DefaultRoute: true},
					{Name: "wlan0", Up: true},
				},
				Checks: f.checks,
			}}}, nil
		}
		t.Fatalf("unexpected op %v", op)
		return nil, nil
	})
	return f
}

func haContainer(name, capName, pkg, node string, devices ...string) resource.Resource {
	r := deviceContainerResource(name, pkg, node, devices...)
	r.Metadata.Owner = "capability/" + capName
	return r
}

func haVolume(name, capName, pkg, node string) resource.Resource {
	return resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindVolume,
		Metadata: resource.Metadata{Name: name, Owner: "capability/" + capName, Package: pkg, Node: node},
		Volume:   &resource.VolumeSpec{Name: name}}
}

func haBackupPlan(name, capName, pkg, node, volume, repo string) resource.Resource {
	return resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
		Metadata: resource.Metadata{Name: name, Owner: "capability/" + capName, Package: pkg, Node: node},
		BackupPlan: &resource.BackupPlanSpec{Volumes: []string{volume}, Repository: repo,
			PasswordSecret: "secret/restic-" + name, Schedule: "daily", Retention: resource.BackupRetention{KeepDaily: 7}}}
}

// install records an applied plan for pkg (its origin is where installed
// version and params come from) and seeds its resources.
func (f *haFixture) install(t *testing.T, pkg, version string, params map[string]string, rs ...resource.Resource) {
	t.Helper()
	doc, _ := json.Marshal(plan.StoredProposal{Summary: "enable " + pkg, BasedOnHead: "x",
		Origin: &plan.CapabilityOrigin{Package: pkg, Version: version, Params: params}})
	id, err := f.h.s.DB.InsertPlan("root-admin", "", "enable "+pkg, "low", string(doc))
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying, runtimedb.PlanApplied} {
		if err := f.h.s.DB.TransitionPlan(id, st, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	seedDesired(t, f.h, rs...)
}

func (f *haFixture) observe(t *testing.T, kind resource.Kind, name string, healthy bool, appHealth, detail string) {
	t.Helper()
	if err := f.h.s.DB.UpsertObserved(runtimedb.Observed{Kind: string(kind), Name: name, Node: "n1",
		Healthy: healthy, AppHealth: appHealth, Detail: detail}); err != nil {
		t.Fatal(err)
	}
}

func (f *haFixture) get(t *testing.T, query string) haView {
	t.Helper()
	var v haView
	resp := f.h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/home-automation"+query, nil, &v)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", query, resp.StatusCode)
	}
	haContractHolds(t, v)
	return v
}

var haStates = map[haState]bool{haNotSelected: true, haBlocked: true, haReadyToPropose: true, haAwaitingApproval: true,
	haApplying: true, haExternalStep: true, haReady: true, haDegraded: true, haUnavailable: true}

func haContractHolds(t *testing.T, v haView) {
	t.Helper()
	check := func(where string, bs []haBlocker) {
		if bs == nil {
			t.Errorf("%s: blockers is null, want []", where)
		}
		for _, b := range bs {
			if b.Code == "" || b.Message == "" || len(b.Evidence) == 0 || b.Actions == nil ||
				(b.Severity != haBlocking && b.Severity != haWarning) {
				t.Errorf("%s: blocker breaks the contract: %+v", where, b)
			}
			for _, a := range b.Actions {
				if a.Kind == "" || a.Label == "" {
					t.Errorf("%s: action without kind/label: %+v", where, a)
				}
			}
		}
	}
	check("view", v.Blockers)
	for _, n := range v.Nodes {
		check("node "+n.ID, n.Blockers)
	}
	if len(v.Components) != len(haComponents) {
		t.Fatalf("components = %d, want %d", len(v.Components), len(haComponents))
	}
	for _, c := range v.Components {
		if !haStates[c.State] || c.Reason == "" && c.State != haNotSelected {
			t.Errorf("%s: state %q reason %q", c.ID, c.State, c.Reason)
		}
		if c.Actions == nil || c.ExternalSteps == nil {
			t.Errorf("%s: null actions or steps", c.ID)
		}
		check("component "+c.ID, c.Blockers)
	}
}

func (v haView) comp(t *testing.T, id string) haComponent {
	t.Helper()
	for _, c := range v.Components {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no component %s", id)
	return haComponent{}
}

func (c haComponent) blocker(code string) *haBlocker {
	for i := range c.Blockers {
		if c.Blockers[i].Code == code {
			return &c.Blockers[i]
		}
	}
	return nil
}

func (c haComponent) action(kind string) *haAction {
	for i := range c.Actions {
		if c.Actions[i].Kind == kind {
			return &c.Actions[i]
		}
	}
	return nil
}

func TestHomeAutomationCleanNode(t *testing.T) {
	f := newHAFixture(t)
	v := f.get(t, "")
	if v.Topology != "none" || v.Node != "n1" || len(v.Nodes) != 1 || !v.Nodes[0].Selected || !v.Nodes[0].Connected {
		t.Fatalf("view = %+v", v)
	}
	for _, id := range []string{haMQTT, haHomeAssistant, haExternalHA, haZigbee, haZWave, haMatter, haBluetooth} {
		if c := v.comp(t, id); c.State != haNotSelected || c.Selection != "none" {
			t.Errorf("%s = %s/%s, want not_selected", id, c.State, c.Selection)
		}
	}
	th := v.comp(t, haThread)
	if th.State != haUnavailable || th.blocker("package_unavailable") == nil || len(th.Packages) != 0 {
		t.Errorf("thread = %+v, want unavailable with package_unavailable", th)
	}
	// Zigbee depends on MQTT through the catalog trait as well as the table.
	if z := v.comp(t, haZigbee); len(z.Requires) != 1 || z.Requires[0] != haMQTT {
		t.Errorf("zigbee requires = %v", z.Requires)
	}
	// Radios come back with suitability from the inventory kind, and free.
	inv := v.Nodes[0].RadioProbe
	if inv == nil || inv.Probe != "ok" || len(inv.Radios) != 3 {
		t.Fatalf("radio inventory = %+v", inv)
	}
	if r := inv.Radios[0]; r.ID != haZigbeeID || len(r.Owners) != 0 || len(r.SuitableFor) != 1 || r.SuitableFor[0] != (haSuitability{haZigbee, "detected"}) {
		t.Errorf("zigbee radio = %+v", r)
	}
	if v.Nodes[0].HostAddress != "192.0.2.10" {
		t.Errorf("hostAddress = %q", v.Nodes[0].HostAddress)
	}

	// Choosing Zigbee pulls MQTT in as a dependency: MQTT is the next
	// safe proposal, Zigbee is blocked until it is ready.
	v = f.get(t, "?topology=managed&select=zigbee")
	m := v.comp(t, haMQTT)
	if m.Selection != "dependency" || m.State != haReadyToPropose {
		t.Fatalf("mqtt = %s/%s", m.Selection, m.State)
	}
	if a := m.action("install_package"); a == nil || a.Package != "mqtt-test" || a.Node != "n1" {
		t.Errorf("mqtt install action = %+v", m.Actions)
	}
	z := v.comp(t, haZigbee)
	if z.State != haBlocked || z.blocker("dependency_not_ready") == nil {
		t.Fatalf("zigbee = %s %+v", z.State, z.Blockers)
	}
	if len(z.RadioCandidates) != 1 || z.RadioCandidates[0] != haZigbeeID {
		t.Errorf("zigbee candidates = %v", z.RadioCandidates)
	}
	if a := z.blocker("dependency_not_ready").Actions[0]; a.Kind != "resolve_dependency" || a.Component != haMQTT {
		t.Errorf("dependency action = %+v", a)
	}
	if hass := v.comp(t, haHomeAssistant); hass.Selection != "requested" || hass.State != haReadyToPropose {
		t.Errorf("home-assistant = %s/%s", hass.Selection, hass.State)
	}
	if len(z.ExternalSteps) != 1 || z.ExternalSteps[0].Status != "unverified" ||
		!strings.Contains(strings.Join(z.ExternalSteps[0].Instructions, " "), "192.0.2.10") {
		t.Errorf("zigbee steps = %+v", z.ExternalSteps)
	}

	for _, bad := range []string{"?select=nope", "?topology=cloud", "?node=ghost", "?bluetooth=on"} {
		if resp := f.h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/home-automation"+bad, nil, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", bad, resp.StatusCode)
		}
	}
}

// Reading never proposes, commits or records anything, whatever it is
// asked to evaluate — and every read role may ask.
func TestHomeAutomationReadIsReadOnly(t *testing.T) {
	f := newHAFixture(t)
	f.install(t, "mqtt-test", "2.0.0", map[string]string{"node": "n1"}, haContainer("mosquitto", "mqtt", "mqtt-test", "n1"))
	head, _ := f.h.s.State.Head()
	before, _ := f.h.s.DB.ListPlans(0)
	for _, role := range []runtimedb.Role{runtimedb.RoleAdmin, runtimedb.RoleOperator, runtimedb.RoleButler} {
		if resp := f.h.do(role, http.MethodGet, "/api/v1/home-automation?topology=managed&select=zigbee,zwave,matter,bluetooth&bluetooth=host-bluez", nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", role, resp.StatusCode)
		}
	}
	after, _ := f.h.s.DB.ListPlans(0)
	head2, _ := f.h.s.State.Head()
	if len(after) != len(before) || head != head2 {
		t.Fatalf("a read changed state: plans %d→%d head %s→%s", len(before), len(after), head, head2)
	}
}

func TestHomeAutomationExternalHA(t *testing.T) {
	f := newHAFixture(t)
	cfg := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: "home-assistant-wildstone", Owner: "capability/home-automation", Package: "external-test", Node: "n1"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "wildstone.yaml", Content: "x"}}
	f.install(t, "external-test", "1.0.0", map[string]string{"node": "n1"}, cfg)
	f.observe(t, resource.KindConfigFile, "home-assistant-wildstone", false, "", "secret home-automation-publisher is not set on n1")

	v := f.get(t, "")
	c := v.comp(t, haExternalHA)
	if v.Topology != haTopologyExternal || c.State != haExternalStep || c.Selection != "installed" {
		t.Fatalf("external = %s/%s topology %s", c.State, c.Selection, v.Topology)
	}
	if len(c.ExternalSteps) != 2 || c.ExternalSteps[0].ID != "publisher_token" || c.ExternalSteps[0].Status != "pending" ||
		c.ExternalSteps[0].Action == nil || c.ExternalSteps[0].Action.Kind != "mint_publisher" || c.ExternalSteps[1].Status != "unverified" {
		t.Fatalf("steps = %+v", c.ExternalSteps)
	}
	if c.action("mint_publisher") == nil {
		t.Errorf("actions = %+v", c.Actions)
	}
	// Managed-only parts stay out of an external topology.
	if hass := v.comp(t, haHomeAssistant); hass.State != haNotSelected {
		t.Errorf("home-assistant = %s", hass.State)
	}

	// The credential minted and the file written: ready, with the
	// Home-Assistant-side include left as an unverified checklist item.
	if err := f.h.s.Principals.Create(publisherPrincipalPrefix+"home-automation", runtimedb.RolePublisher, ""); err != nil {
		t.Fatal(err)
	}
	f.observe(t, resource.KindConfigFile, "home-assistant-wildstone", true, "", "")
	c = f.get(t, "").comp(t, haExternalHA)
	if c.State != haReady || c.ExternalSteps[0].Status != "done" || c.ExternalSteps[1].Status != "unverified" {
		t.Fatalf("after mint = %s %+v", c.State, c.ExternalSteps)
	}
}

func TestHomeAutomationPartialManagedInstall(t *testing.T) {
	f := newHAFixture(t)
	f.install(t, "mqtt-test", "2.0.0", map[string]string{"node": "n1", "listen_addr": "192.0.2.10"},
		haContainer("mosquitto", "mqtt", "mqtt-test", "n1"))
	f.install(t, "hass-test", "1.1.0", map[string]string{"node": "n1"},
		haContainer("home-assistant", "home-assistant", "hass-test", "n1"),
		haVolume("home-assistant-config", "home-assistant", "hass-test", "n1"))
	f.observe(t, resource.KindContainer, "mosquitto", true, "", "")
	// Home Assistant applied but not yet observed.

	v := f.get(t, "?select=zigbee")
	if v.Topology != haTopologyManaged {
		t.Errorf("topology = %s", v.Topology)
	}
	if m := v.comp(t, haMQTT); m.State != haReady || m.action("open_service") == nil {
		t.Errorf("mqtt = %s %+v", m.State, m.Actions)
	}
	hass := v.comp(t, haHomeAssistant)
	if hass.State != haApplying || !hass.Stale || hass.InstalledVersion != "1.1.0" {
		t.Errorf("home-assistant = %s stale=%v installed=%s", hass.State, hass.Stale, hass.InstalledVersion)
	}
	if hass.blocker("stale_package") == nil || hass.blocker("backup_missing") == nil || hass.action("upgrade_package") == nil {
		t.Errorf("home-assistant blockers = %+v actions = %+v", hass.Blockers, hass.Actions)
	}
	z := v.comp(t, haZigbee)
	if z.State != haReadyToPropose {
		t.Fatalf("zigbee = %s %+v", z.State, z.Blockers)
	}
	if a := z.action("install_package"); a == nil || a.Package != "zigbee-test" || len(a.RadioIDs) != 1 || a.RadioIDs[0] != haZigbeeID {
		t.Errorf("zigbee install = %+v", z.Actions)
	}
	// Home Assistant was installed without its MQTT login: offered as a
	// reconfigure, not hidden.
	if b := z.blocker("ha_setting_needed"); b == nil || b.Severity != haWarning || b.Actions[0].Kind != "reconfigure_package" ||
		b.Actions[0].Package != "hass-test" || b.Actions[0].Params["mqtt_enabled"] != "true" {
		t.Errorf("zigbee ha setting = %+v", z.Blockers)
	}
	if len(hass.Requires) != 0 {
		t.Errorf("home-assistant without mqtt_enabled requires %v", hass.Requires)
	}
	if b := v.comp(t, haBluetooth); b.State != haNotSelected || len(b.Modes) != 3 {
		t.Errorf("bluetooth = %s modes %v", b.State, b.Modes)
	}
}

func TestHomeAutomationConflictingRadio(t *testing.T) {
	f := newHAFixture(t)
	f.install(t, "zwave-test", "1.0.0", map[string]string{"node": "n1"},
		haContainer("zwave-js-ui", "zwave", "zwave-test", "n1", zwaveAlias))
	seedDesired(t, f.h, deviceContainerResource("homeassistant-legacy", "legacy", "n1", zwaveTTY))
	f.observe(t, resource.KindContainer, "zwave-js-ui", true, "", "")

	v := f.get(t, "")
	c := v.comp(t, haZWave)
	b := c.blocker("radio_conflict")
	if c.State != haDegraded || b == nil || len(b.Evidence) != 2 {
		t.Fatalf("zwave = %s %+v", c.State, c.Blockers)
	}
	if len(c.Radios) != 1 || c.Radios[0].RadioID != zwaveID {
		t.Errorf("zwave radios = %+v", c.Radios)
	}
	var open, sel bool
	for _, a := range b.Actions {
		open = open || (a.Kind == "open_service" && a.Package == "legacy")
		sel = sel || a.Kind == "select_radio"
	}
	if !open || !sel {
		t.Errorf("conflict actions = %+v", b.Actions)
	}
	for _, r := range v.Nodes[0].RadioProbe.Radios {
		if r.ID == zwaveID && len(r.Owners) != 2 {
			t.Errorf("zwave radio owners = %+v", r.Owners)
		}
	}

	// A fresh Z-Wave choice on a node whose only stick is taken is
	// refused with the owner named, never offered the owned stick.
	f2 := newHAFixture(t)
	seedDesired(t, f2.h, deviceContainerResource("homeassistant-legacy", "legacy", "n1", zwaveTTY))
	z := f2.get(t, "?select=zwave").comp(t, haZWave)
	if z.State != haBlocked || z.blocker("radio_in_use") == nil || len(z.RadioCandidates) != 0 {
		t.Fatalf("fresh zwave = %s %+v", z.State, z.Blockers)
	}
}

func TestHomeAutomationOfflineNode(t *testing.T) {
	f := newHAFixture(t)
	f.install(t, "hass-test", "1.2.0", map[string]string{"node": "n1"}, haContainer("home-assistant", "home-assistant", "hass-test", "n1"))
	f.observe(t, resource.KindContainer, "home-assistant", true, "", "")
	f.live = nil

	v := f.get(t, "?select=matter")
	if f.calls.Load() != 0 {
		t.Errorf("an offline node was probed %d times", f.calls.Load())
	}
	if len(v.Nodes) != 1 || v.Nodes[0].Connected || v.Nodes[0].Readiness.Probe != "offline" {
		t.Fatalf("node = %+v", v.Nodes)
	}
	hass := v.comp(t, haHomeAssistant)
	if hass.State != haDegraded || hass.blocker("node_offline") == nil {
		t.Errorf("home-assistant = %s %+v", hass.State, hass.Blockers)
	}
	m := v.comp(t, haMatter)
	if m.State != haBlocked || m.blocker("node_offline") == nil || m.blocker("node_offline").Actions[0].Kind != "retry_probe" {
		t.Errorf("matter = %s %+v", m.State, m.Blockers)
	}

	// An agent too old for the readiness probe is named, not waited on.
	f.live = []LiveNode{{NodeID: "n1", ProtocolMinor: 24}}
	m = f.get(t, "?select=matter").comp(t, haMatter)
	if m.State != haBlocked || m.blocker("agent_outdated") == nil {
		t.Errorf("old agent matter = %s %+v", m.State, m.Blockers)
	}
}

func TestHomeAutomationPendingPlan(t *testing.T) {
	f := newHAFixture(t)
	var out struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	resp := f.h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities",
		enableRequest{Package: "mqtt-test", Params: map[string]string{"node": "n1"}}, &out)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enable = %d %s", resp.StatusCode, out.Error)
	}
	plans, _ := f.h.s.DB.ListPlans(0)
	id := plans[0].ID

	m := f.get(t, "?select=zigbee").comp(t, haMQTT)
	if m.State != haAwaitingApproval || m.Selection != "pending" || m.PendingPlan == nil || m.PendingPlan.ID != id {
		t.Fatalf("mqtt = %s/%s %+v", m.State, m.Selection, m.PendingPlan)
	}
	if a := m.action("open_plan"); a == nil || a.PlanID != id {
		t.Errorf("mqtt actions = %+v", m.Actions)
	}
	if err := f.h.s.DB.TransitionPlan(id, runtimedb.PlanApproved, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if m := f.get(t, "").comp(t, haMQTT); m.State != haApplying {
		t.Errorf("approved mqtt = %s", m.State)
	}
}

func TestHomeAutomationMissingIPv6AndBlueZ(t *testing.T) {
	f := newHAFixture(t)
	missing := wsv1.ReadinessState_READINESS_STATE_MISSING
	for _, c := range f.checks {
		switch c.Check {
		case "ipv6":
			c.State, c.Value = missing, "1"
		case "bluez_service":
			c.State = missing
		}
	}
	f.install(t, "hass-test", "1.2.0", map[string]string{"node": "n1"}, haContainer("home-assistant", "home-assistant", "hass-test", "n1"))
	f.observe(t, resource.KindContainer, "home-assistant", true, "", "")

	v := f.get(t, "?select=matter,bluetooth&bluetooth=host-bluez")
	m := v.comp(t, haMatter)
	b := m.blocker("ipv6_missing")
	if m.State != haBlocked || b == nil || b.Evidence[0].Value != "1" {
		t.Fatalf("matter = %s %+v", m.State, m.Blockers)
	}
	var manual, retry bool
	for _, a := range b.Actions {
		manual = manual || (a.Kind == "manual_step" && strings.Contains(strings.Join(a.Instructions, "\n"), "disable_ipv6=0"))
		retry = retry || a.Kind == "retry_probe"
	}
	if !manual || !retry {
		t.Errorf("ipv6 actions = %+v", b.Actions)
	}
	// The wlan0 row is missing too, but only the default-route
	// interface decides an interface-scoped check.
	if m.blocker("interface_ipv6_missing") != nil {
		t.Errorf("a non-default interface blocked matter: %+v", m.Blockers)
	}
	bt := v.comp(t, haBluetooth)
	if bt.State != haBlocked || bt.blocker("bluez_service_missing") == nil || bt.blocker("bluez_service_missing").Severity != haBlocking {
		t.Fatalf("bluetooth = %s %+v", bt.State, bt.Blockers)
	}
	// Without a chosen mode, BlueZ is a warning: proxy mode needs none.
	bt = f.get(t, "?select=bluetooth").comp(t, haBluetooth)
	if bt.State != haReadyToPropose || bt.blocker("bluez_service_missing").Severity != haWarning {
		t.Errorf("modeless bluetooth = %s %+v", bt.State, bt.Blockers)
	}
	if a := bt.action("reconfigure_package"); a == nil || a.Package != "hass-test" {
		t.Errorf("bluetooth actions = %+v", bt.Actions)
	}
}

func TestHomeAutomationHealthyFullStack(t *testing.T) {
	f := newHAFixture(t)
	f.install(t, "mqtt-test", "2.0.0", map[string]string{"node": "n1", "listen_addr": "192.0.2.10"},
		haContainer("mosquitto", "mqtt", "mqtt-test", "n1"))
	f.install(t, "hass-test", "1.2.0", map[string]string{"node": "n1", "bluetooth": "host-bluez", "mqtt_enabled": "true"},
		haContainer("home-assistant", "home-assistant", "hass-test", "n1"),
		haVolume("home-assistant-config", "home-assistant", "hass-test", "n1"),
		haVolume("home-assistant-backup-repo", "home-assistant", "hass-test", "n1"),
		haBackupPlan("home-assistant", "home-assistant", "hass-test", "n1", "home-assistant-config", "home-assistant-backup-repo"))
	f.install(t, "zigbee-test", "2.0.0", map[string]string{"node": "n1", "usb_device": haZigbeeAlias},
		haContainer("zigbee2mqtt", "zigbee", "zigbee-test", "n1", haZigbeeAlias))
	f.install(t, "zwave-test", "1.0.0", map[string]string{"node": "n1"},
		haContainer("zwave-js-ui", "zwave", "zwave-test", "n1", zwaveAlias))
	f.install(t, "matter-test", "1.0.0", map[string]string{"node": "n1"},
		haContainer("matter-server", "matter", "matter-test", "n1"))
	for _, name := range []string{"mosquitto", "home-assistant", "zigbee2mqtt", "zwave-js-ui", "matter-server"} {
		f.observe(t, resource.KindContainer, name, true, "healthy", "")
	}
	seq, err := f.h.s.DB.StartBackupRun("home-assistant", runtimedb.BackupRunBackup, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.h.s.DB.FinishBackupRun(seq, runtimedb.BackupRunSucceeded, ""); err != nil {
		t.Fatal(err)
	}

	v := f.get(t, "")
	for _, id := range []string{haMQTT, haHomeAssistant, haZigbee, haZWave, haMatter, haBluetooth} {
		c := v.comp(t, id)
		if c.State != haReady || c.Selection != "installed" {
			t.Errorf("%s = %s/%s %+v", id, c.State, c.Selection, c.Blockers)
		}
		for _, b := range c.Blockers {
			if b.Severity == haBlocking {
				t.Errorf("%s: blocking blocker on a healthy stack: %+v", id, b)
			}
		}
	}
	hass := v.comp(t, haHomeAssistant)
	if len(hass.Requires) != 1 || hass.Requires[0] != haMQTT {
		t.Errorf("home-assistant with mqtt_enabled requires %v", hass.Requires)
	}
	if hass.Backup == nil || len(hass.Backup.Uncovered) != 0 || hass.Backup.LastSuccessAt == nil || hass.Backup.Volumes[0] != "home-assistant-config" {
		t.Errorf("backup = %+v", hass.Backup)
	}
	if bt := v.comp(t, haBluetooth); bt.Mode != "host-bluez" {
		t.Errorf("bluetooth mode = %q", bt.Mode)
	}
	if z := v.comp(t, haZigbee); len(z.Radios) != 1 || z.Radios[0].RadioID != haZigbeeID || len(z.ExternalSteps) != 1 {
		t.Errorf("zigbee = %+v", z)
	}
	if th := v.comp(t, haThread); th.State != haUnavailable {
		t.Errorf("thread = %s", th.State)
	}
}

func TestHomeAutomationDegradedController(t *testing.T) {
	f := newHAFixture(t)
	f.install(t, "mqtt-test", "2.0.0", map[string]string{"node": "n1"}, haContainer("mosquitto", "mqtt", "mqtt-test", "n1"))
	f.install(t, "zigbee-test", "2.0.0", map[string]string{"node": "n1", "usb_device": haZigbeeAlias},
		haContainer("zigbee2mqtt", "zigbee", "zigbee-test", "n1", haZigbeeAlias))
	f.observe(t, resource.KindContainer, "mosquitto", true, "", "")
	f.observe(t, resource.KindContainer, "zigbee2mqtt", true, "unhealthy", "")

	z := f.get(t, "").comp(t, haZigbee)
	if z.State != haDegraded || z.blocker("app_unhealthy") == nil {
		t.Fatalf("zigbee = %s %+v", z.State, z.Blockers)
	}

	// The stick unplugged: the claim no longer resolves to a radio.
	f.radios = f.radios[1:]
	f.observe(t, resource.KindContainer, "zigbee2mqtt", false, "", "device /dev/serial/by-id/... not found")
	z = f.get(t, "").comp(t, haZigbee)
	if z.State != haDegraded || z.blocker("radio_missing") == nil || z.blocker("unhealthy") == nil {
		t.Fatalf("unplugged zigbee = %s %+v", z.State, z.Blockers)
	}

	// A failed newest attempt is named with its plan.
	doc, _ := json.Marshal(plan.StoredProposal{Summary: "upgrade zigbee", BasedOnHead: "x",
		Origin: &plan.CapabilityOrigin{Package: "zigbee-test", Version: "2.0.0"}})
	id, _ := f.h.s.DB.InsertPlan("root-admin", "", "upgrade zigbee", "low", string(doc))
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying, runtimedb.PlanFailed} {
		if err := f.h.s.DB.TransitionPlan(id, st, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	z = f.get(t, "").comp(t, haZigbee)
	if z.State != haDegraded || z.FailedPlan == nil || z.FailedPlan.ID != id || z.blocker("plan_failed") == nil {
		t.Fatalf("failed zigbee = %s %+v", z.State, z.FailedPlan)
	}
}

// Against the real catalog: each protocol component names its shipped
// package and is offered (matter-matterjs landed with #442).
func TestHomeAutomationRealCatalog(t *testing.T) {
	f := newHAFixture(t)
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	f.h.s.Catalog = c
	v := f.get(t, "?topology=managed&select=zigbee,zwave")
	if zw := v.comp(t, haZWave); zw.State != haReadyToPropose || len(zw.RadioCandidates) != 1 || zw.RadioCandidates[0] != zwaveID {
		t.Errorf("real zwave = %s %v %+v", zw.State, zw.RadioCandidates, zw.Blockers)
	}
	for id, want := range map[string]string{haMQTT: "mqtt-mosquitto", haHomeAssistant: "home-assistant", haExternalHA: "home-automation-hass",
		haZigbee: "zigbee-zigbee2mqtt", haZWave: "zwave-zwave-js-ui", haThread: "thread-otbr", haMatter: "matter-matterjs"} {
		if c := v.comp(t, id); c.Package != want || c.State == haUnavailable {
			t.Errorf("%s = %s %s", id, c.Package, c.State)
		}
	}
	if z := v.comp(t, haZigbee); !haContains(z.Requires, haMQTT) {
		t.Errorf("zigbee requires = %v", z.Requires)
	}
}
