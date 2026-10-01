package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// Follow-ups to #446/#447: what can never work is blocked structurally,
// what Wildstone cannot see is said so, and the radios offered are only
// ones a package can actually be handed.

const (
	haThreadAlias = "/dev/serial/by-id/usb-Thread_Stick-if00"
	haThreadID    = "serial-by-id:usb-Thread_Stick-if00"
)

func threadRadio() *wsv1.Radio {
	return &wsv1.Radio{Id: haThreadID, Kind: wsv1.RadioKind_RADIO_KIND_THREAD, KernelName: "ttyACM1", DevPath: "/dev/ttyACM1", ByIdPaths: []string{haThreadAlias}}
}

// realCatalog swaps in the shipped packages (thread-otbr and
// matter-matterjs included).
func (f *haFixture) realCatalog(t *testing.T) {
	t.Helper()
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	f.h.s.Catalog = c
}

// Matter and Thread answer Home Assistant on loopback only, so they are
// blocked — not merely described — wherever no Home Assistant on their
// own machine can reach them.
func TestHomeAutomationHostLocalComponents(t *testing.T) {
	f := newHAFixture(t)
	f.realCatalog(t)
	f.radios = append(f.radios, threadRadio())

	v := f.get(t, "?topology=external&select=matter,thread")
	for id, endpoint := range map[string]string{haMatter: "ws://127.0.0.1:5580/ws", haThread: "http://127.0.0.1:8081"} {
		c := v.comp(t, id)
		b := c.blocker("requires_managed_home_assistant")
		if c.State != haBlocked || b == nil || b.Severity != haBlocking || b.Evidence[0].Value != endpoint {
			t.Fatalf("external %s = %s %+v", id, c.State, c.Blockers)
		}
		if len(b.Actions) != 1 || b.Actions[0].Kind != "choose_topology" || b.Actions[0].Topology != haTopologyManaged {
			t.Errorf("external %s actions = %+v", id, b.Actions)
		}
	}
	// The same household asking about a managed setup: nothing in the way.
	v = f.get(t, "?topology=managed&select=matter,thread")
	for _, id := range []string{haMatter, haThread} {
		if c := v.comp(t, id); c.State != haReadyToPropose || c.blocker("requires_managed_home_assistant") != nil || c.blocker("dependency_on_other_node") != nil {
			t.Errorf("managed %s = %s %+v", id, c.State, c.Blockers)
		}
	}

	// The orchestration orders it, then stops on it rather than proposing.
	o := f.orch(t, "preview", haOrchRequest{Topology: haTopologyExternal, Select: []string{haMatter}}, http.StatusOK)
	if len(o.Sequence) != 2 || o.Sequence[1].Component != haMatter || o.Sequence[1].Action != haStepResolve {
		t.Fatalf("external matter sequence = %s", stepIDs(o.Sequence))
	}
	if o.Sequence[1].blocker("requires_managed_home_assistant") == nil {
		t.Errorf("matter step = %+v", o.Sequence[1])
	}

	// No topology asked, but an existing Home Assistant is what is
	// installed: still blocked.
	cfg := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: "home-assistant-wildstone", Owner: "capability/home-automation", Package: "home-automation-hass", Node: "n1"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "wildstone.yaml", Content: "x"}}
	f.install(t, "home-automation-hass", "1.0.0", map[string]string{"node": "n1"}, cfg)
	if m := f.get(t, "?select=matter").comp(t, haMatter); m.State != haBlocked || m.blocker("requires_managed_home_assistant") == nil {
		t.Errorf("installed-external matter = %s %+v", m.State, m.Blockers)
	}

	// A Wildstone-run Home Assistant on another machine cannot reach them
	// either: the component must go where Home Assistant is.
	f2 := newHAFixture(t)
	if err := f2.h.s.DB.RecordNodeJoin("n2"); err != nil {
		t.Fatal(err)
	}
	f2.live = append(f2.live, LiveNode{NodeID: "n2", ProtocolMinor: 25, Since: time.Now()})
	f2.install(t, "hass-test", "1.2.0", map[string]string{"node": "n2"}, haContainer("home-assistant", "home-assistant", "hass-test", "n2"))
	m := f2.get(t, "?topology=managed&select=matter&node=n1").comp(t, haMatter)
	b := m.blocker("dependency_on_other_node")
	if m.State != haBlocked || m.Node != "n1" || b == nil || b.Actions[0].Kind != "choose_node" || b.Actions[0].Node != "n2" {
		t.Fatalf("matter away from home assistant = %s %+v", m.State, m.Blockers)
	}
}

// A Bluetooth adapter is used through BlueZ and never handed to a
// container: it is listed, but never a radio to assign; its presence is
// the bluetooth_adapter readiness check.
func TestHomeAutomationBluetoothAdapterIsNotARadioChoice(t *testing.T) {
	f := newHAFixture(t)
	f.install(t, "hass-test", "1.2.0", map[string]string{"node": "n1"}, haContainer("home-assistant", "home-assistant", "hass-test", "n1"))
	f.observe(t, resource.KindContainer, "home-assistant", true, "", "")
	f.radios = append(f.radios, &wsv1.Radio{Id: "serial-by-id:usb-unknown-if00", Kind: wsv1.RadioKind_RADIO_KIND_UNKNOWN,
		KernelName: "ttyACM0", DevPath: "/dev/ttyACM0", ByIdPaths: []string{"/dev/serial/by-id/usb-unknown-if00"}})
	v := f.get(t, "?select=bluetooth&bluetooth=host-bluez")
	for _, r := range v.Nodes[0].RadioProbe.Radios {
		for _, s := range r.SuitableFor {
			if s.Component == haBluetooth {
				t.Errorf("radio %s offered to bluetooth: %+v", r.ID, r.SuitableFor)
			}
		}
		if r.Kind == "bluetooth" && len(r.SuitableFor) != 0 {
			t.Errorf("adapter %s suitable for %+v", r.ID, r.SuitableFor)
		}
	}
	bt := v.comp(t, haBluetooth)
	if len(bt.RadioCandidates) != 0 || bt.State != haReadyToPropose {
		t.Errorf("bluetooth = %s candidates %v %+v", bt.State, bt.RadioCandidates, bt.Blockers)
	}
	if a := bt.action("reconfigure_package"); a == nil || len(a.RadioIDs) != 0 {
		t.Errorf("bluetooth actions = %+v", bt.Actions)
	}
	// The adapter missing is a readiness blocker carrying its check.
	for _, c := range f.checks {
		if c.Check == "bluetooth_adapter" {
			c.State = wsv1.ReadinessState_READINESS_STATE_MISSING
		}
	}
	bt = f.get(t, "?select=bluetooth&bluetooth=host-bluez").comp(t, haBluetooth)
	b := bt.blocker("bluetooth_adapter_missing")
	if bt.State != haBlocked || b == nil || b.Check == nil || *b.Check != (haCheckRef{Check: "bluetooth_adapter", Subject: "hci0"}) {
		t.Fatalf("adapterless bluetooth = %s %+v", bt.State, bt.Blockers)
	}
}

// Wildstone cannot read a radio's firmware: Thread carries a standing
// warning with the exact manual steps until it is installed, and never
// claims to have checked.
func TestHomeAutomationThreadRCPFirmwareWarning(t *testing.T) {
	f := newHAFixture(t)
	f.realCatalog(t)
	f.radios = append(f.radios, threadRadio())
	th := f.get(t, "?topology=managed&select=thread").comp(t, haThread)
	b := th.blocker("thread_rcp_firmware_unverified")
	if th.State != haReadyToPropose || b == nil || b.Severity != haWarning || b.Evidence[0].State != "unverified" {
		t.Fatalf("thread = %s %+v", th.State, th.Blockers)
	}
	if len(b.Actions) != 1 || b.Actions[0].Kind != "manual_step" {
		t.Fatalf("rcp actions = %+v", b.Actions)
	}
	text := strings.Join(b.Actions[0].Instructions, "\n")
	for _, want := range []string{"OpenThread RCP", "460800", "docker logs otbr", "RCP version", "n1"} {
		if !strings.Contains(text, want) {
			t.Errorf("rcp instructions lack %q:\n%s", want, text)
		}
	}
	if !haContains(th.RadioCandidates, haThreadID) {
		t.Errorf("thread candidates = %v", th.RadioCandidates)
	}
	// The Home Assistant side names the loopback address, never the LAN one.
	if len(th.ExternalSteps) == 0 || !strings.Contains(strings.Join(th.ExternalSteps[0].Instructions, " "), "http://127.0.0.1:8081") {
		t.Errorf("otbr step = %+v", th.ExternalSteps)
	}

	f.install(t, "thread-otbr", "1.0.0", map[string]string{"node": "n1", "radio_device": haThreadAlias, "backbone_interface": "eth0"},
		haContainer("otbr", "thread", "thread-otbr", "n1", haThreadAlias))
	if th = f.get(t, "").comp(t, haThread); th.blocker("thread_rcp_firmware_unverified") != nil {
		t.Errorf("installed thread still warns: %+v", th.Blockers)
	}
}

// The LAN-facing interface params (thread-otbr's required
// backbone_interface, matter-matterjs's optional primary_interface) are
// filled from the node's default-route interface; with none, the
// required one is asked for and the optional one left to its default.
func TestHomeAutomationOrchestrateInterfaceParams(t *testing.T) {
	f := newOrchFixture(t)
	f.realCatalog(t)
	f.radios = append(f.radios, threadRadio())
	f.install(t, "home-assistant", "1.0.0", map[string]string{"node": "n1"}, haContainer("home-assistant", "home-assistant", "home-assistant", "n1"))
	f.observe(t, resource.KindContainer, "home-assistant", true, "healthy", "")

	o := f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haThread}}, http.StatusOK)
	if o.Next.Component != haThread || o.Next.Params["backbone_interface"] != "eth0" || o.Next.Params["radio_device"] != haThreadAlias ||
		o.Next.RadioID != haThreadID || o.Next.blocker("param_needed") != nil {
		t.Fatalf("thread step = %+v", o.Next)
	}
	if o.Next.blocker("thread_rcp_firmware_unverified") == nil {
		t.Errorf("thread step lost the RCP warning: %+v", o.Next.Blockers)
	}
	o = f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haMatter}}, http.StatusOK)
	if o.Next.Component != haMatter || o.Next.Params["primary_interface"] != "eth0" || o.Next.blocker("param_needed") != nil {
		t.Fatalf("matter step = %+v", o.Next)
	}

	// No interface carries the default route (and every interface is
	// otherwise ready, so only the param is in question).
	f.noDefaultRoute = true
	for _, c := range f.checks {
		c.State = wsv1.ReadinessState_READINESS_STATE_READY
	}
	o = f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haThread}}, http.StatusOK)
	b := o.Next.blocker("param_needed")
	if o.Next.ID != "thread:resolve_blockers" || b == nil || b.Evidence[0].Value != "backbone_interface" ||
		b.Actions[0].Kind != "provide_option" || o.Next.Params["backbone_interface"] != "" {
		t.Fatalf("thread without a default route = %+v", o.Next)
	}
	o = f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haMatter}}, http.StatusOK)
	if o.Next.Component != haMatter || o.Next.Params["primary_interface"] != "" || o.Next.blocker("param_needed") != nil {
		t.Fatalf("matter without a default route = %+v", o.Next)
	}
	// The household supplies it as an option.
	o = f.orch(t, "preview", haOrchRequest{Topology: haTopologyManaged, Select: []string{haThread},
		Options: map[string]map[string]string{haThread: {"backbone_interface": "eth0"}}}, http.StatusOK)
	if o.Next.Params["backbone_interface"] != "eth0" || o.Next.blocker("param_needed") != nil {
		t.Fatalf("thread with the interface given = %+v", o.Next)
	}
}

// An unidentified radio is offered to every radio-claiming component;
// choosing it for two is refused server-side, whatever the Console does,
// and nothing is filed.
func TestHomeAutomationOrchestrateUnidentifiedRadioChosenTwice(t *testing.T) {
	f := newOrchFixture(t)
	f.realCatalog(t)
	unknownID := "serial-by-id:usb-Generic_Stick-if00"
	f.radios = []*wsv1.Radio{{Id: unknownID, Kind: wsv1.RadioKind_RADIO_KIND_UNKNOWN, KernelName: "ttyACM0",
		DevPath: "/dev/ttyACM0", ByIdPaths: []string{"/dev/serial/by-id/usb-Generic_Stick-if00"}}}
	v := f.get(t, "?topology=managed&select=zigbee,zwave,thread")
	for _, id := range []string{haZigbee, haZWave, haThread} {
		if c := v.comp(t, id); len(c.RadioCandidates) != 1 || c.RadioCandidates[0] != unknownID {
			t.Fatalf("%s candidates = %v", id, c.RadioCandidates)
		}
	}
	req := haOrchRequest{Topology: haTopologyManaged, Select: []string{haZWave, haThread},
		Radios: map[string]string{haZWave: unknownID, haThread: unknownID}}
	for _, op := range []string{"preview", "propose"} {
		o := f.orch(t, op, req, http.StatusConflict)
		if len(o.Rejected) != 1 || o.Rejected[0].Code != "radio_conflict" || o.Next.RadioID != "" {
			t.Fatalf("%s: rejected = %+v next = %+v", op, o.Rejected, o.Next)
		}
	}
	if f.planCount(t) != 0 {
		t.Fatal("a radio chosen twice was proposed")
	}
}
