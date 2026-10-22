package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/catalog"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// The home-automation orchestration API (#447), driven end to end over
// HTTP: an empty deployment walked through MQTT → Home Assistant →
// Zigbee → Z-Wave one ordinary plan at a time, with every approval made
// through the Plans endpoint and every "apply" played by the test as the
// reconciler would.

// haOrchCatalog mirrors the shipped packages' params — a broker needing a
// private listen address, Home Assistant's mqtt_enabled and bluetooth,
// controllers taking a radio by-id through usb_device — without the
// identity and ingress requirements that are not this slice's subject.
func haOrchCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	img := `"ghcr.io/example/app@` + testDigest + `"`
	pkgs := map[string]string{
		"mqtt-orch": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: mqtt-orch, version: 2.0.0, capability: mqtt }
provides:
  - { trait: mqtt, mode: broker, exports: { MqttHost: "192.168.1.10" } }
params:
  - { name: node, required: true, source: node }
  - { name: listen_addr, required: true, validate: private_ipv4 }
  - { name: listen_port, default: "1883", validate: port }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: mosquitto, node: "{{ .Params.node }}" }
    spec:
      image: ` + img + `
      env: { LISTEN: "{{ .Params.listen_addr }}:{{ .Params.listen_port }}" }
`,
		"hass-orch": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: hass-orch, version: 2.0.0, capability: home-assistant }
requires:
  - { trait: mqtt, mode: broker, when: mqtt_enabled }
params:
  - { name: node, required: true, source: node }
  - { name: mqtt_enabled, default: "false", validate: "enum:true|false" }
  - { name: bluetooth, default: "off", validate: "enum:proxy|host-bluez|off" }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: home-assistant, node: "{{ .Params.node }}" }
    spec:
      image: ` + img + `
      env: { MQTT: "{{ .Params.mqtt_enabled }}", BLUETOOTH: "{{ .Params.bluetooth }}" }
`,
		"external-orch": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: external-orch, version: 1.0.0, capability: home-automation }
params:
  - { name: node, required: true, source: node }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: ConfigFile
    metadata: { name: home-assistant-wildstone, node: "{{ .Params.node }}" }
    spec: { filename: wildstone.yaml, content: "x" }
`,
		"zigbee-orch": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: zigbee-orch, version: 2.0.1, capability: zigbee }
requires:
  - { trait: mqtt, mode: broker }
params:
  - { name: node, required: true, source: node }
  - { name: usb_device, default: "" }
  - { name: coordinator_url, default: "tcp://192.168.1.60:6638" }
  - { name: coordinator_adapter, default: zstack, validate: "enum:zstack|ember|deconz" }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: zigbee2mqtt, node: "{{ .Params.node }}" }
    spec:
      image: ` + img + `
      env: { ADAPTER: "{{ .Params.coordinator_adapter }}" }
      devices: {{ with .Params.usb_device }}[{ source: "{{ . }}", target: /dev/zigbee }]{{ else }}[]{{ end }}
`,
		"zwave-orch": `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: zwave-orch, version: 1.0.0, capability: zwave }
params:
  - { name: node, required: true, source: node }
  - { name: usb_device, required: true }
  - { name: listen_addr, required: true, validate: private_ipv4 }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: zwave-js-ui, node: "{{ .Params.node }}" }
    spec:
      image: ` + img + `
      env: { LISTEN: "{{ .Params.listen_addr }}" }
      devices: [{ source: "{{ .Params.usb_device }}", target: /dev/zwave }]
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

func newOrchFixture(t *testing.T) *haFixture {
	f := newHAFixture(t)
	f.addr = "192.168.1.10/24"
	f.h.s.Catalog = haOrchCatalog(t)
	return f
}

// orch calls preview or propose and checks the status and the answer's
// contract.
func (f *haFixture) orch(t *testing.T, op string, req haOrchRequest, want int) haOrchestration {
	t.Helper()
	var o haOrchestration
	resp := f.h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/home-automation/"+op, req, &o)
	if resp.StatusCode != want {
		t.Fatalf("%s = %d, want %d (next %+v)", op, resp.StatusCode, want, o.Next)
	}
	if want == http.StatusBadRequest {
		return o
	}
	if o.Next.ID == "" || o.Next.Action == "" || o.Sequence == nil || o.ExternalSteps == nil || o.Next.Blockers == nil {
		t.Fatalf("%s: answer breaks the contract: %+v", op, o)
	}
	current := 0
	for _, st := range o.Sequence {
		if st.Status == "current" {
			current++
		}
		if st.Blockers == nil || st.Actions == nil {
			t.Errorf("%s: step %s has null blockers/actions", op, st.ID)
		}
	}
	if current > 1 {
		t.Fatalf("%s: %d current steps", op, current)
	}
	return o
}

func (f *haFixture) planCount(t *testing.T) int {
	t.Helper()
	plans, err := f.h.s.DB.ListPlans(0)
	if err != nil {
		t.Fatal(err)
	}
	return len(plans)
}

func (f *haFixture) desired(t *testing.T, kind resource.Kind, name string) bool {
	t.Helper()
	cur, err := f.h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range cur {
		if r.Kind == kind && r.Metadata.Name == name {
			return true
		}
	}
	return false
}

// approveAndApply approves through the Plans endpoint, as the household
// would, then plays the reconciler: applying, applied.
func (f *haFixture) approveAndApply(t *testing.T, planID string) {
	t.Helper()
	if resp := f.h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+planID+"/approve", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve %s = %d", planID, resp.StatusCode)
	}
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApplying, runtimedb.PlanApplied} {
		if err := f.h.s.DB.TransitionPlan(planID, st, "reconciler"); err != nil {
			t.Fatal(err)
		}
	}
}

// restart stands up a fresh Server over the same database and Statefile:
// nothing the orchestration knows survives except what they hold.
func (f *haFixture) restart(t *testing.T) {
	t.Helper()
	old := f.h.s
	s := New(old.Principals, old.DB, old.State, nil)
	s.Catalog, s.LiveNodes, s.Agents = old.Catalog, old.LiveNodes, old.Agents
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	f.h = &harness{t: t, srv: srv, s: s, tokens: f.h.tokens}
}

func stepIDs(seq []haOrchStep) string {
	var ids []string
	for _, st := range seq {
		ids = append(ids, st.Component+"="+st.Action+"/"+st.Status)
	}
	return strings.Join(ids, " ")
}

// proposeNext previews, proposes exactly that step and returns its plan
// id, checking that nothing reached desired state.
func (f *haFixture) proposeNext(t *testing.T, req haOrchRequest, wantStep string) string {
	t.Helper()
	pv := f.orch(t, "preview", req, http.StatusOK)
	if pv.Next.ID != wantStep {
		t.Fatalf("next = %s (%s), want %s; blockers %+v", pv.Next.ID, pv.Next.Reason, wantStep, pv.Next.Blockers)
	}
	if pv.Preview == nil {
		t.Fatalf("preview of %s has no dry-run plan", wantStep)
	}
	head, _ := f.h.s.State.Head()
	before := f.planCount(t)
	req.ExpectStep = pv.Next.ID
	var o struct {
		haOrchestration
		Plan struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"plan"`
	}
	resp := f.h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/home-automation/propose", req, &o)
	if resp.StatusCode != http.StatusCreated || o.Plan.ID == "" || o.Plan.State != string(runtimedb.PlanProposed) {
		t.Fatalf("propose %s = %d %+v", wantStep, resp.StatusCode, o.Plan)
	}
	if o.Next.Action != haStepAwaitApproval || o.Next.PlanID != o.Plan.ID {
		t.Fatalf("after propose next = %+v", o.Next)
	}
	if head2, _ := f.h.s.State.Head(); head2 != head || f.planCount(t) != before+1 {
		t.Fatalf("propose changed desired state or filed %d plans", f.planCount(t)-before)
	}
	return o.Plan.ID
}

func TestHomeAutomationOrchestrateManagedFromEmpty(t *testing.T) {
	f := newOrchFixture(t)
	req := haOrchRequest{Topology: haTopologyManaged, Select: []string{haZigbee, haZWave}}

	// Empty deployment: MQTT first, with the node and the node's own LAN
	// address filled in, and the rest of the sequence behind it.
	o := f.orch(t, "preview", req, http.StatusOK)
	if o.Next.ID != "mqtt:install_package:mqtt-orch" || o.Next.Params["node"] != "n1" || o.Next.Params["listen_addr"] != "192.168.1.10" {
		t.Fatalf("first step = %+v", o.Next)
	}
	if got := stepIDs(o.Sequence); got != "mqtt=install_package/current home-assistant=install_package/upcoming zigbee=install_package/upcoming zwave=install_package/upcoming" {
		t.Fatalf("sequence = %s", got)
	}
	if o.Complete || f.planCount(t) != 0 {
		t.Fatalf("a preview filed a plan or claimed completion")
	}

	mqttPlan := f.proposeNext(t, req, "mqtt:install_package:mqtt-orch")
	if f.desired(t, resource.KindContainer, "mosquitto") {
		t.Fatal("the broker reached desired state without approval")
	}
	// Proposing again files nothing: the next step is the open plan.
	again := f.orch(t, "propose", req, http.StatusConflict)
	if again.Next.ID != "mqtt:await_approval:"+mqttPlan || f.planCount(t) != 1 {
		t.Fatalf("second propose = %+v (%d plans)", again.Next, f.planCount(t))
	}
	// A stale expectation is refused rather than proposing something else.
	stale := req
	stale.ExpectStep = "mqtt:install_package:mqtt-orch"
	if o := f.orch(t, "propose", stale, http.StatusConflict); o.Next.Action != haStepAwaitApproval {
		t.Fatalf("stale propose = %+v", o.Next)
	}

	// Approved and applied but not yet observed: still waiting, and Home
	// Assistant is not proposed before its broker answers.
	f.approveAndApply(t, mqttPlan)
	if !f.desired(t, resource.KindContainer, "mosquitto") {
		t.Fatal("approval did not commit the broker")
	}
	if o := f.orch(t, "propose", req, http.StatusConflict); o.Next.ID != "mqtt:await_observation" || f.planCount(t) != 1 {
		t.Fatalf("unobserved broker: next = %+v", o.Next)
	}
	f.observe(t, resource.KindContainer, "mosquitto", true, "", "")

	// Home Assistant comes with its MQTT login, because Zigbee was chosen.
	o = f.orch(t, "preview", req, http.StatusOK)
	if o.Next.ID != "home-assistant:install_package:hass-orch" || o.Next.Params["mqtt_enabled"] != "true" || o.Next.Params["node"] != "n1" {
		t.Fatalf("second step = %+v", o.Next)
	}
	if o.Sequence[0].Status != "done" {
		t.Errorf("mqtt step = %+v", o.Sequence[0])
	}

	// The control plane restarts: the answer is rebuilt from the database
	// and the Statefile alone.
	f.restart(t)
	hassPlan := f.proposeNext(t, req, "home-assistant:install_package:hass-orch")
	f.restart(t)
	if o := f.orch(t, "preview", req, http.StatusOK); o.Next.ID != "home-assistant:await_approval:"+hassPlan {
		t.Fatalf("after restart next = %+v", o.Next)
	}
	f.approveAndApply(t, hassPlan)
	f.observe(t, resource.KindContainer, "home-assistant", true, "healthy", "")

	// Zigbee: the one Zigbee stick is taken without anyone typing a path.
	zigbeePlan := f.proposeNext(t, req, "zigbee:install_package:zigbee-orch")
	pv := f.orch(t, "preview", req, http.StatusOK)
	if pv.Next.PlanID != zigbeePlan {
		t.Fatalf("next = %+v", pv.Next)
	}
	rec, err := f.h.s.DB.GetPlan(zigbeePlan)
	if err != nil || !strings.Contains(rec.PlanJSON, haZigbeeAlias) {
		t.Fatalf("zigbee plan does not carry the stick's by-id path: %v", err)
	}
	f.approveAndApply(t, zigbeePlan)
	f.observe(t, resource.KindContainer, "zigbee2mqtt", true, "", "")

	o = f.orch(t, "preview", req, http.StatusOK)
	if o.Next.Params["usb_device"] != zwaveAlias || o.Next.RadioID != zwaveID || o.Next.Params["listen_addr"] != "192.168.1.10" {
		t.Fatalf("zwave step = %+v", o.Next)
	}
	zwavePlan := f.proposeNext(t, req, "zwave:install_package:zwave-orch")
	f.approveAndApply(t, zwavePlan)
	f.observe(t, resource.KindContainer, "zwave-js-ui", true, "", "")

	// Every package in: the Home-Assistant-side steps come back as one
	// stable external step, the same on every call, filing nothing.
	plans := f.planCount(t)
	var first haOrchestration
	for i := 0; i < 3; i++ {
		o = f.orch(t, "preview", req, http.StatusOK)
		if i == 0 {
			first = o
		}
		if o.Next.ID != first.Next.ID || o.Complete {
			t.Fatalf("external step moved: %s vs %s", o.Next.ID, first.Next.ID)
		}
	}
	if first.Next.Action != haStepExternal || len(first.Next.ExternalSteps) != 2 ||
		first.Next.ID != "external_step:zigbee/ha_mqtt_integration,zwave/ha_zwave_integration" {
		t.Fatalf("external next = %+v", first.Next)
	}
	for _, st := range first.Sequence {
		if st.Status != "done" {
			t.Errorf("step %s = %s", st.ID, st.Status)
		}
	}
	if o := f.orch(t, "propose", req, http.StatusConflict); o.Next.Action != haStepExternal || f.planCount(t) != plans {
		t.Fatalf("propose during external step = %+v", o.Next)
	}

	// Confirming one leaves the other; confirming both completes.
	req.ConfirmedSteps = []string{"zigbee/ha_mqtt_integration"}
	if o := f.orch(t, "preview", req, http.StatusOK); o.Next.ID != "external_step:zwave/ha_zwave_integration" {
		t.Fatalf("one confirmed: %s", o.Next.ID)
	}
	req.ConfirmedSteps = append(req.ConfirmedSteps, "zwave/ha_zwave_integration")
	o = f.orch(t, "preview", req, http.StatusOK)
	if !o.Complete || o.Next.Action != haStepDone || f.planCount(t) != plans {
		t.Fatalf("all confirmed: %+v", o.Next)
	}
	for _, es := range o.ExternalSteps {
		if !es.Due || !es.Confirmed {
			t.Errorf("external step %+v", es)
		}
	}
}

// A plan the household rejects is not retried behind its back, and is not
// a dead end: the step is simply offered again.
func TestHomeAutomationOrchestrateRejectedPlan(t *testing.T) {
	f := newOrchFixture(t)
	req := haOrchRequest{Topology: haTopologyManaged, Select: []string{haZigbee}}
	id := f.proposeNext(t, req, "mqtt:install_package:mqtt-orch")
	if resp := f.h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+id+"/reject", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("reject = %d", resp.StatusCode)
	}
	o := f.orch(t, "preview", req, http.StatusOK)
	if o.Next.ID != "mqtt:install_package:mqtt-orch" || f.planCount(t) != 1 {
		t.Fatalf("after reject next = %+v", o.Next)
	}
}

func TestHomeAutomationOrchestrateRadios(t *testing.T) {
	f := newOrchFixture(t)
	f.install(t, "mqtt-orch", "2.0.0", map[string]string{"node": "n1", "listen_addr": "192.168.1.10"}, haContainer("mosquitto", "mqtt", "mqtt-orch", "n1"))
	f.install(t, "hass-orch", "2.0.0", map[string]string{"node": "n1", "mqtt_enabled": "true"}, haContainer("home-assistant", "home-assistant", "hass-orch", "n1"))
	f.observe(t, resource.KindContainer, "mosquitto", true, "", "")
	f.observe(t, resource.KindContainer, "home-assistant", true, "", "")
	unknownID := "serial-by-id:usb-1a86_USB_Serial-if00"
	f.radios = append(f.radios, &wsv1.Radio{Id: unknownID, Kind: wsv1.RadioKind_RADIO_KIND_UNKNOWN, KernelName: "ttyACM0",
		DevPath: "/dev/ttyACM0", ByIdPaths: []string{"/dev/serial/by-id/usb-1a86_USB_Serial-if00"}})
	req := haOrchRequest{Topology: haTopologyManaged, Select: []string{haZigbee, haZWave}}

	// Two candidates (the identified stick and an unidentified serial
	// radio): the household chooses, from a picker.
	o := f.orch(t, "preview", req, http.StatusOK)
	b := o.Next.blocker("radio_choice_needed")
	if o.Next.ID != "zigbee:resolve_blockers" || b == nil || len(b.Actions) != 1 || b.Actions[0].Kind != "select_radio" || len(b.Actions[0].RadioIDs) != 2 {
		t.Fatalf("ambiguous zigbee = %+v", o.Next)
	}
	if f.orch(t, "propose", req, http.StatusConflict); f.planCount(t) != 2 {
		t.Fatal("a blocked step was proposed")
	}

	// A radio that is not a candidate at all is stale: 409 with the fresh
	// answer, never proposed.
	for _, bad := range []string{"serial-by-id:gone", zwaveID} {
		req.Radios = map[string]string{haZigbee: bad}
		o = f.orch(t, "preview", req, http.StatusConflict)
		if len(o.Rejected) != 1 || o.Rejected[0].Code != "radio_stale" || o.Next.Component != haZigbee {
			t.Fatalf("stale radio %s = %+v %+v", bad, o.Rejected, o.Next)
		}
		f.orch(t, "propose", req, http.StatusConflict)
	}
	// One radio for two components is a conflict.
	req.Radios = map[string]string{haZigbee: unknownID, haZWave: unknownID}
	if o = f.orch(t, "preview", req, http.StatusConflict); len(o.Rejected) != 1 || o.Rejected[0].Code != "radio_conflict" {
		t.Fatalf("conflict = %+v", o.Rejected)
	}

	// A valid choice fills the param with the stable path.
	req.Radios = map[string]string{haZigbee: haZigbeeID}
	o = f.orch(t, "preview", req, http.StatusOK)
	if o.Next.ID != "zigbee:install_package:zigbee-orch" || o.Next.Params["usb_device"] != haZigbeeAlias || o.Preview == nil {
		t.Fatalf("chosen zigbee = %+v", o.Next)
	}
	// The stick is taken by something else before the propose: stale.
	seedDesired(t, f.h, deviceContainerResource("legacy-z2m", "legacy", "n1", haZigbeeAlias))
	if o = f.orch(t, "propose", req, http.StatusConflict); len(o.Rejected) != 1 || o.Rejected[0].Code != "radio_stale" {
		t.Fatalf("taken stick = %+v", o.Rejected)
	}
	if f.planCount(t) != 2 {
		t.Fatal("a stale radio was proposed")
	}

	// Explicitly no radio: Zigbee's network coordinator, with a closed
	// option; Z-Wave cannot go without one.
	req.Radios = map[string]string{haZigbee: ""}
	req.Options = map[string]map[string]string{haZigbee: {"coordinator_url": "tcp://192.168.1.60:6638", "coordinator_adapter": "ember"}}
	o = f.orch(t, "preview", req, http.StatusOK)
	if o.Next.ID != "zigbee:install_package:zigbee-orch" || o.Next.Params["usb_device"] != "" || o.Next.Params["coordinator_adapter"] != "ember" {
		t.Fatalf("network zigbee = %+v", o.Next)
	}
	req.Radios = map[string]string{haZWave: ""}
	f.orch(t, "preview", req, http.StatusBadRequest)
}

func TestHomeAutomationOrchestrateSettingsAndModes(t *testing.T) {
	f := newOrchFixture(t)
	f.install(t, "mqtt-orch", "2.0.0", map[string]string{"node": "n1", "listen_addr": "192.168.1.10"}, haContainer("mosquitto", "mqtt", "mqtt-orch", "n1"))
	f.install(t, "hass-orch", "2.0.0", map[string]string{"node": "n1", "bluetooth": "off"}, haContainer("home-assistant", "home-assistant", "hass-orch", "n1"))
	f.observe(t, resource.KindContainer, "mosquitto", true, "", "")
	f.observe(t, resource.KindContainer, "home-assistant", true, "", "")

	// Home Assistant was installed without its MQTT login: choosing Zigbee
	// reconfigures it first, carrying every installed param forward.
	o := f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haZigbee}}, http.StatusOK)
	if o.Next.ID != "home-assistant:reconfigure_package:hass-orch" || o.Next.Params["mqtt_enabled"] != "true" ||
		o.Next.Params["bluetooth"] != "off" || o.Next.Params["node"] != "n1" || o.Preview == nil {
		t.Fatalf("reconfigure = %+v", o.Next)
	}

	// Bluetooth needs a mode, chosen from a closed list.
	o = f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haBluetooth}}, http.StatusOK)
	if o.Next.ID != "bluetooth:resolve_blockers" || o.Next.blocker("mode_needed") == nil {
		t.Fatalf("modeless bluetooth = %+v", o.Next)
	}
	o = f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haBluetooth}, Modes: map[string]string{haBluetooth: "proxy"}}, http.StatusOK)
	if o.Next.ID != "bluetooth:reconfigure_package:hass-orch" || o.Next.Params["bluetooth"] != "proxy" || o.Next.Params["node"] != "n1" {
		t.Fatalf("proxy bluetooth = %+v", o.Next)
	}

	for name, bad := range map[string]haOrchRequest{
		"no topology":           {Select: []string{haZigbee}},
		"managed-only part":     {Topology: haTopologyExternal, Select: []string{haBluetooth}},
		"unknown component":     {Topology: haTopologyManaged, Select: []string{"x10"}},
		"bad mode":              {Topology: haTopologyManaged, Modes: map[string]string{haBluetooth: "on"}},
		"reserved option":       {Topology: haTopologyManaged, Options: map[string]map[string]string{haZigbee: {"node": "n2"}}},
		"dependency option":     {Topology: haTopologyManaged, Options: map[string]map[string]string{haHomeAssistant: {"mqtt_enabled": "false"}}},
		"undeclared option":     {Topology: haTopologyManaged, Options: map[string]map[string]string{haZigbee: {"password": "x"}}},
		"radio for a non-radio": {Topology: haTopologyManaged, Radios: map[string]string{haMQTT: haZigbeeID}},
		"bad confirmation":      {Topology: haTopologyManaged, ConfirmedSteps: []string{"zigbee"}},
	} {
		t.Run(name, func(t *testing.T) { f.orch(t, "preview", bad, http.StatusBadRequest) })
	}
}

func TestHomeAutomationOrchestrateExternal(t *testing.T) {
	f := newOrchFixture(t)
	req := haOrchRequest{Topology: haTopologyExternal, Select: []string{haZigbee}}
	o := f.orch(t, "preview", req, http.StatusOK)
	if got := stepIDs(o.Sequence); got != "mqtt=install_package/current home-automation=install_package/upcoming zigbee=install_package/upcoming" {
		t.Fatalf("external sequence = %s", got)
	}
	f.install(t, "mqtt-orch", "2.0.0", map[string]string{"node": "n1", "listen_addr": "192.168.1.10"}, haContainer("mosquitto", "mqtt", "mqtt-orch", "n1"))
	cfg := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: "home-assistant-wildstone", Owner: "capability/home-automation", Package: "external-orch", Node: "n1"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "wildstone.yaml", Content: "x"}}
	f.install(t, "external-orch", "1.0.0", map[string]string{"node": "n1"}, cfg)
	f.install(t, "zigbee-orch", "2.0.1", map[string]string{"node": "n1", "usb_device": haZigbeeAlias},
		haContainer("zigbee2mqtt", "zigbee", "zigbee-orch", "n1", haZigbeeAlias))
	for _, n := range []string{"mosquitto", "zigbee2mqtt"} {
		f.observe(t, resource.KindContainer, n, true, "", "")
	}
	f.observe(t, resource.KindConfigFile, "home-assistant-wildstone", false, "", "secret home-automation-publisher is not set on n1")

	// The publisher credential is a step Wildstone can see: it comes
	// first, with the Console action that does it; confirming cannot
	// skip it.
	req.ConfirmedSteps = []string{"home-automation/publisher_token"}
	o = f.orch(t, "preview", req, http.StatusOK)
	if o.Next.ID != "external_step:home-automation/publisher_token" || len(o.Next.Actions) != 1 || o.Next.Actions[0].Kind != "mint_publisher" {
		t.Fatalf("publisher step = %+v", o.Next)
	}
	if err := f.h.s.Principals.Create(publisherPrincipalPrefix+"home-automation", runtimedb.RolePublisher, ""); err != nil {
		t.Fatal(err)
	}
	f.observe(t, resource.KindConfigFile, "home-assistant-wildstone", true, "", "")
	o = f.orch(t, "preview", req, http.StatusOK)
	if o.Next.ID != "external_step:home-automation/include_package_file,zigbee/ha_mqtt_integration" {
		t.Fatalf("unverified steps = %+v", o.Next)
	}
	req.ConfirmedSteps = append(req.ConfirmedSteps, "home-automation/include_package_file", "zigbee/ha_mqtt_integration")
	if o = f.orch(t, "preview", req, http.StatusOK); !o.Complete {
		t.Fatalf("external complete = %+v", o.Next)
	}
}

// Against the shipped catalog: the first step on an empty deployment is
// the real broker package, previewed through the real pipeline.
func TestHomeAutomationOrchestrateRealCatalog(t *testing.T) {
	f := newOrchFixture(t)
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	f.h.s.Catalog = c
	o := f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haZigbee, haZWave}}, http.StatusOK)
	if o.Next.ID != "mqtt:install_package:mqtt-mosquitto" || o.Next.Params["listen_addr"] != "192.168.1.10" || o.Preview == nil {
		t.Fatalf("real first step = %+v", o.Next)
	}
	if f.planCount(t) != 0 {
		t.Fatal("preview filed a plan")
	}
}

func (st haOrchStep) blocker(code string) *haBlocker {
	for i := range st.Blockers {
		if st.Blockers[i].Code == code {
			return &st.Blockers[i]
		}
	}
	return nil
}
