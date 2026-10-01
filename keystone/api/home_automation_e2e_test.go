package api

// The managed home-automation stack, end to end and hardware-free (#452).
//
// Everything here is the real control plane: the shipped catalog
// (packages/), the orchestration API (#447) choosing each next step, the
// ordinary capability pipeline filing it, the Plans endpoint approving
// it, and the reconcile loop (keystone/reconcile/loop over the real
// Reconciler) converging it — dispatching every operation to
// internal/fakenode, an in-memory fleet that answers the Wardstone
// protocol as an agent at a given protocol minor would, from the node
// fixture in testing/fixtures/home-automation. No Docker, no radios, no
// multicast or IPv6 on the machine running the test.
//
// What each layer is trusted for is tested where it lives; this file
// proves they compose: an empty Statefile reaches a ready reference stack
// through approved plans only, survives restarts and failures, refuses
// across protocol-minor skew, and reports a broken host prerequisite at
// the layer that can see it. The Docker-backed image contracts (real
// Mosquitto, Home Assistant, Z-Wave JS UI, OTBR, Matter.js Server) stay
// behind WILDSTONE_DOCKER_TESTS; testing/fixtures/home-automation/README.md
// lists both tiers and how to run them.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/fakenode"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile/loop"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

const (
	haFixtureDir = "../../testing/fixtures/home-automation"

	// The reference node's radios (reference-node.json).
	e2eZWaveRadio  = "serial-by-id:usb-0658_0200-if00"
	e2eZWavePath   = "/dev/serial/by-id/usb-0658_0200-if00"
	e2eThreadRadio = "serial-by-id:usb-Nabu_Casa_Home_Assistant_Connect_ZBT-1_REPLACED-SERIAL-if00-port0"
	e2eThreadPath  = "/dev/serial/by-id/usb-Nabu_Casa_Home_Assistant_Connect_ZBT-1_REPLACED-SERIAL-if00-port0"
)

// e2ePackages is the reference stack's package sequence, in the order the
// orchestration must propose it.
var e2ePackages = []string{"mqtt-mosquitto", "home-assistant", "zigbee-zigbee2mqtt", "zwave-zwave-js-ui", "matter-matterjs", "thread-otbr"}

// e2eContainers is the container each of those packages runs, same order.
var e2eContainers = []string{"mosquitto", "home-assistant", "zigbee2mqtt", "zwave-js-ui", "matter-server", "otbr"}

// e2eExternal is every Home-Assistant-side step the reference selection
// leaves for the household, in sequence order.
var e2eExternal = []string{"zigbee/ha_mqtt_integration", "zwave/ha_zwave_integration", "matter/ha_matter_integration",
	"thread/ha_otbr_integration", "thread/thread_credentials", "bluetooth/esphome_bluetooth_proxies"}

// selectOnly narrows a request to some of its components, keeping only
// their radios, modes and options.
func selectOnly(req haOrchRequest, ids ...string) haOrchRequest {
	out := haOrchRequest{Topology: req.Topology, Select: ids, Radios: map[string]string{}, Modes: map[string]string{},
		Options: map[string]map[string]string{}}
	for _, id := range ids {
		if r, ok := req.Radios[id]; ok {
			out.Radios[id] = r
		}
		if m, ok := req.Modes[id]; ok {
			out.Modes[id] = m
		}
		if o, ok := req.Options[id]; ok {
			out.Options[id] = o
		}
	}
	return out
}

// advance proposes the step a preview names, approves it and lets the
// loop run it, returning the plan's id and final state.
func (st *haStack) advance(req haOrchRequest) (string, runtimedb.PlanState) {
	st.t.Helper()
	pv := st.orch("preview", req, http.StatusOK)
	if pv.Next.Action != haStepInstall && pv.Next.Action != haStepReconfigure {
		st.t.Fatalf("nothing to advance: next %s (%s) %+v", pv.Next.ID, pv.Next.Reason, pv.Next.Blockers)
	}
	req.ExpectStep = pv.Next.ID
	id, _ := st.orch("propose", req, http.StatusCreated).Plan.(string)
	st.approve(id)
	st.tick()
	return id, st.plan(id).State
}

// advanceAll advances while the next step is a package step and returns
// the answer it stopped at.
func (st *haStack) advanceAll(req haOrchRequest) haOrchestration {
	st.t.Helper()
	for i := 0; i <= len(e2ePackages)+1; i++ {
		o := st.orch("preview", req, http.StatusOK)
		if o.Next.Action != haStepInstall && o.Next.Action != haStepReconfigure {
			return o
		}
		if id, state := st.advance(req); state != runtimedb.PlanApplied {
			st.t.Fatalf("%s: plan %s %s", o.Next.ID, id, state)
		}
	}
	st.t.Fatal("the sequence did not stop")
	return haOrchestration{}
}

// referenceRequest is what the Console sends for the reference topology:
// every protocol, Zigbee through a network coordinator, the Z-Wave stick
// and the Thread RCP picked from the inventory, Bluetooth via proxies.
func referenceRequest() haOrchRequest {
	return haOrchRequest{Topology: haTopologyManaged,
		Select:  []string{haZigbee, haZWave, haMatter, haThread, haBluetooth},
		Modes:   map[string]string{haBluetooth: "proxy"},
		Radios:  map[string]string{haZigbee: "", haZWave: e2eZWaveRadio, haThread: e2eThreadRadio},
		Options: map[string]map[string]string{haZigbee: {"coordinator_url": "tcp://192.168.1.60:6638", "coordinator_adapter": "ember"}},
	}
}

// haStack is one deployment: an API server, its reconcile loop and the
// fleet both dispatch to, over one database and Statefile.
type haStack struct {
	t     *testing.T
	h     *harness
	fleet *fakenode.Fleet
	loop  *loop.Loop
	node  string
}

// newHAStack stands up the reference node; mutate, when set, changes its
// facts (protocol minor, a readiness check) before anything runs.
func newHAStack(t *testing.T, mutate func(n *fakenode.Node)) *haStack {
	t.Helper()
	p, err := fakenode.LoadProfile(filepath.Join(haFixtureDir, "reference-node.json"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	st := &haStack{t: t, h: newHarness(t), fleet: fakenode.New(), node: p.ID}
	st.fleet.AddProfile(p)
	if mutate != nil {
		st.fleet.Do(p.ID, mutate)
	}
	if err := st.h.s.DB.RecordNodeJoin(p.ID); err != nil {
		t.Fatal(err)
	}
	st.wire(st.h.s, cat)
	st.newLoop()
	return st
}

func (st *haStack) wire(s *Server, cat *catalog.Catalog) {
	s.Catalog = cat
	s.Agents = st.fleet
	s.LiveNodes = func() []LiveNode {
		var out []LiveNode
		for id, minor := range st.fleet.Live() {
			out = append(out, LiveNode{NodeID: id, AgentVersion: "fakenode", ProtocolMinor: minor, Since: time.Unix(0, 0)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
		return out
	}
}

// newLoop builds the reconcile service as cmd/keystone does, including
// the managed-state seed from healthy observed rows (F76).
func (st *haStack) newLoop() {
	rec := &reconcile.Reconciler{Applier: st.fleet, ManagedPrefix: "/etc/wildstone",
		AgentProtocolMinor: st.fleet.ProtocolMinor,
		MaxApplyRetries:    1, RetryBackoff: time.Millisecond,
		HealthGateAttempts: 2, HealthGateBackoff: time.Millisecond,
		JobGateAttempts: 2, JobGateBackoff: time.Millisecond}
	rows, err := st.h.s.DB.ListObserved()
	if err != nil {
		st.t.Fatal(err)
	}
	byNode := map[string][]reconcile.ManagedRef{}
	for _, o := range rows {
		if o.Healthy {
			byNode[o.Node] = append(byNode[o.Node], reconcile.ManagedRef{Kind: resource.Kind(o.Kind), Name: o.Name})
		}
	}
	for n, refs := range byNode {
		rec.SeedManaged(n, refs)
	}
	st.loop = &loop.Loop{State: st.h.s.State, DB: st.h.s.DB, Rec: rec, Connected: st.fleet.Connected,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// restartKeystone replaces the API server and the reconcile loop with
// fresh ones over the same database and Statefile: nothing in memory
// survives, as after `systemctl restart keystone`.
func (st *haStack) restartKeystone() {
	old := st.h.s
	s := New(old.Principals, old.DB, old.State, nil)
	st.wire(s, old.Catalog)
	srv := httptest.NewServer(s.Handler())
	st.t.Cleanup(srv.Close)
	st.h = &harness{t: st.t, srv: srv, s: s, tokens: st.h.tokens}
	st.newLoop()
}

func (st *haStack) tick() loop.TickOutcome {
	st.t.Helper()
	out, err := st.loop.Tick(context.Background())
	if err != nil {
		st.t.Fatalf("tick: %v", err)
	}
	return out
}

// enable files a plan through the ordinary capability endpoint — the
// path a household (or a Butler) takes around the orchestration.
func (st *haStack) enable(pkg string, params map[string]string) string {
	st.t.Helper()
	var body struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	resp := st.h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities", enableRequest{Package: pkg, Params: params}, &body)
	if resp.StatusCode != http.StatusCreated || body.ID == "" {
		st.t.Fatalf("enable %s = %d %s", pkg, resp.StatusCode, body.Error)
	}
	return body.ID
}

func (st *haStack) approve(id string) {
	st.t.Helper()
	if resp := st.h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+id+"/approve", nil, nil); resp.StatusCode != http.StatusOK {
		st.t.Fatalf("approve %s = %d", id, resp.StatusCode)
	}
}

func (st *haStack) plan(id string) runtimedb.PlanRecord {
	st.t.Helper()
	rec, err := st.h.s.DB.GetPlan(id)
	if err != nil {
		st.t.Fatal(err)
	}
	return rec
}

func (st *haStack) planCount() int {
	st.t.Helper()
	plans, err := st.h.s.DB.ListPlans(0)
	if err != nil {
		st.t.Fatal(err)
	}
	return len(plans)
}

// installBase puts in place the platform layer the home-automation
// packages require — ingress:private for Home Assistant's route,
// identity:oidc for the Zigbee and Z-Wave admin UIs, and the domain the
// routes are named under — through ordinary approved plans converged by
// the loop.
func (st *haStack) installBase() {
	st.t.Helper()
	for _, step := range []struct {
		pkg    string
		params map[string]string
	}{
		{"ingress-tailnet", map[string]string{"node": st.node, "admin_domain": "home.example.internal", "listen_addr": "100.64.0.1"}},
		{"identity", map[string]string{"node": st.node, "public_domain": "home.example.internal"}},
	} {
		id := st.enable(step.pkg, step.params)
		st.approve(id)
		out := st.tick()
		if got := st.plan(id).State; got != runtimedb.PlanApplied {
			st.t.Fatalf("%s plan %s = %s: %+v", step.pkg, id, got, out)
		}
	}
}

// orch calls preview or propose and checks the answer's contract.
func (st *haStack) orch(op string, req haOrchRequest, want int) haOrchestration {
	st.t.Helper()
	var o struct {
		haOrchestration
		Error string `json:"error"`
		Plan  struct {
			ID string `json:"id"`
		} `json:"plan"`
	}
	resp := st.h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/home-automation/"+op, req, &o)
	if resp.StatusCode != want {
		st.t.Fatalf("%s = %d, want %d (%s; next %s: %s %+v)", op, resp.StatusCode, want, o.Error, o.Next.ID, o.Next.Reason, o.Next.Blockers)
	}
	if o.Next.ID == "" || o.Sequence == nil || o.Next.Blockers == nil {
		st.t.Fatalf("%s: answer breaks the contract: %+v", op, o.haOrchestration)
	}
	if o.Plan.ID != "" {
		o.haOrchestration.Plan = o.Plan.ID
	}
	return o.haOrchestration
}

func (st *haStack) view(query string) haView {
	st.t.Helper()
	var v haView
	if resp := st.h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/home-automation"+query, nil, &v); resp.StatusCode != http.StatusOK {
		st.t.Fatalf("GET %s = %d", query, resp.StatusCode)
	}
	return v
}

// changes lists the ops that changed the node.
func changes(ops []fakenode.Op) []string {
	var out []string
	for _, op := range ops {
		if op.Changed {
			out = append(out, op.Kind+" "+op.Name)
		}
	}
	return out
}

func creates(ops []fakenode.Op) []string {
	var out []string
	for _, op := range ops {
		if op.Kind == "EnsureContainer" && op.Created {
			out = append(out, op.Name)
		}
	}
	return out
}

// installStep previews the next step, which must install pkg, proposes
// exactly it, checks that nothing reached the node or desired state
// before approval, approves through the Plans endpoint and lets the loop
// converge it. It returns the plan id.
func (st *haStack) installStep(req haOrchRequest, pkg string) string {
	st.t.Helper()
	pv := st.orch("preview", req, http.StatusOK)
	if pv.Next.Action != haStepInstall || pv.Next.Package != pkg || pv.Preview == nil {
		st.t.Fatalf("next = %s (%s), want install of %s; blockers %+v", pv.Next.ID, pv.Next.Reason, pkg, pv.Next.Blockers)
	}
	head, _ := st.h.s.State.Head()
	mark := st.fleet.Mark()
	req.ExpectStep = pv.Next.ID
	o := st.orch("propose", req, http.StatusCreated)
	id, _ := o.Plan.(string)
	if id == "" || o.Next.Action != haStepAwaitApproval || o.Next.PlanID != id {
		st.t.Fatalf("propose %s: plan %q, next %+v", pkg, id, o.Next)
	}
	// A second propose files nothing: the next step is the open plan.
	if again := st.orch("propose", req, http.StatusConflict); again.Next.PlanID != id {
		st.t.Fatalf("second propose: next %+v", again.Next)
	}
	if head2, _ := st.h.s.State.Head(); head2 != head {
		st.t.Fatalf("proposing %s changed desired state before approval", pkg)
	}
	if ch := changes(st.fleet.Since(mark)); len(ch) > 0 {
		st.t.Fatalf("proposing %s changed the node before approval: %v", pkg, ch)
	}

	st.approve(id)
	if o := st.orch("preview", referenceRequestLike(req), http.StatusOK); o.Next.Action != haStepAwaitApply || o.Next.PlanID != id {
		st.t.Fatalf("approved, not applied: next %+v", o.Next)
	}
	out := st.tick()
	if rec := st.plan(id); rec.State != runtimedb.PlanApplied {
		st.t.Fatalf("%s plan %s = %s: %+v", pkg, id, rec.State, out)
	}
	return id
}

// referenceRequestLike drops ExpectStep, which only a propose carries.
func referenceRequestLike(req haOrchRequest) haOrchRequest {
	req.ExpectStep = ""
	return req
}

// installReferenceStack drives an empty deployment to the complete
// reference stack and returns the request with every external step
// confirmed.
func (st *haStack) installReferenceStack() haOrchRequest {
	st.t.Helper()
	st.installBase()
	req := referenceRequest()
	for _, pkg := range e2ePackages {
		st.installStep(req, pkg)
	}
	req.ConfirmedSteps = append([]string{}, e2eExternal...)
	if o := st.orch("preview", req, http.StatusOK); !o.Complete {
		st.t.Fatalf("reference stack not complete: next %s (%s)", o.Next.ID, o.Next.Reason)
	}
	return req
}

// running checks every reference container is up on the node.
func (st *haStack) running(names ...string) {
	st.t.Helper()
	for _, n := range names {
		c, ok := st.fleet.Container(st.node, n)
		if !ok || !c.Running {
			st.t.Errorf("container %s: present %v, running %v", n, ok, c.Running)
		}
	}
}

func (st *haStack) createdCount(name string) int {
	c, _ := st.fleet.Container(st.node, name)
	return c.Created
}

func TestHomeAutomationE2EReferenceStackFromEmpty(t *testing.T) {
	t.Parallel()
	st := newHAStack(t, nil)
	if cur, err := st.h.s.State.Load(); err != nil || len(cur) != 0 {
		t.Fatalf("not an empty Statefile: %d resources, %v", len(cur), err)
	}
	st.installBase()
	req := referenceRequest()

	o := st.orch("preview", req, http.StatusOK)
	if got := stepIDs(o.Sequence); got != "mqtt=install_package/current home-assistant=install_package/upcoming "+
		"zigbee=install_package/upcoming zwave=install_package/upcoming matter=install_package/upcoming "+
		"thread=install_package/upcoming bluetooth=reconfigure_package/upcoming" {
		t.Fatalf("sequence = %s", got)
	}
	// The node's own facts fill the params: its LAN address, its
	// default-route interface, the chosen radios' stable paths.
	if p := o.Next.Params; p["node"] != st.node || p["listen_addr"] != "192.168.1.10" {
		t.Fatalf("mqtt params = %v", p)
	}

	var restartedAt string
	for i, pkg := range e2ePackages {
		pv := st.orch("preview", req, http.StatusOK)
		switch pkg {
		case "zwave-zwave-js-ui":
			if pv.Next.Params["usb_device"] != e2eZWavePath || pv.Next.RadioID != e2eZWaveRadio {
				t.Fatalf("zwave params = %v", pv.Next.Params)
			}
		case "thread-otbr":
			if pv.Next.Params["radio_device"] != e2eThreadPath || pv.Next.Params["backbone_interface"] != "eth0" {
				t.Fatalf("thread params = %v", pv.Next.Params)
			}
			// What no probe can see is said, without holding the step.
			if b := pv.Next.blocker("thread_rcp_firmware_unverified"); b == nil || b.Severity == haBlocking {
				t.Fatalf("thread step does not warn about the RCP firmware: %+v", pv.Next.Blockers)
			}
		case "matter-matterjs":
			if pv.Next.Params["primary_interface"] != "eth0" {
				t.Fatalf("matter params = %v", pv.Next.Params)
			}
		}
		st.installStep(req, pkg)
		st.running(e2eContainers[:i+1]...)

		// Half way, the control plane restarts: the next step is rebuilt
		// from the database and the Statefile alone, and the new loop's
		// first pass changes nothing on the node.
		if pkg == "zigbee-zigbee2mqtt" {
			before := st.orch("preview", req, http.StatusOK).Next.ID
			st.restartKeystone()
			mark := st.fleet.Mark()
			if out := st.tick(); out.Failed != "" {
				t.Fatalf("first tick after restart: %s", out.Failed)
			}
			if c := creates(st.fleet.Since(mark)); len(c) > 0 {
				t.Fatalf("restart re-created %v", c)
			}
			restartedAt = st.orch("preview", req, http.StatusOK).Next.ID
			if restartedAt != before {
				t.Fatalf("after restart next = %s, want %s", restartedAt, before)
			}
		}
	}

	// Services came up in dependency order: the broker before anything
	// that logs in to it, Home Assistant before the integrations it
	// consumes.
	var order []string
	seen := map[string]bool{}
	for _, c := range creates(st.fleet.Log()) {
		for _, want := range e2eContainers {
			if c == want && !seen[c] {
				seen[c] = true
				order = append(order, c)
			}
		}
	}
	if strings.Join(order, " ") != strings.Join(e2eContainers, " ") {
		t.Fatalf("creation order = %v, want %v", order, e2eContainers)
	}

	st.checkShapes()

	// Every package in: the Home-Assistant-side steps, the same answer on
	// every call, filing nothing, until the household confirms them.
	plans := st.planCount()
	o = st.orch("preview", req, http.StatusOK)
	if o.Next.ID != haStepExternal+":"+strings.Join(e2eExternal, ",") || o.Complete {
		t.Fatalf("external next = %s", o.Next.ID)
	}
	if again := st.orch("preview", req, http.StatusOK); again.Next.ID != o.Next.ID {
		t.Fatalf("external step moved: %s", again.Next.ID)
	}
	st.orch("propose", req, http.StatusConflict)
	req.ConfirmedSteps = append([]string{}, e2eExternal...)
	o = st.orch("preview", req, http.StatusOK)
	if !o.Complete || o.Next.Action != haStepDone || st.planCount() != plans {
		t.Fatalf("all confirmed: %+v", o.Next)
	}
	for _, s := range o.Sequence {
		if s.Status != "done" {
			t.Errorf("step %s = %s", s.ID, s.Status)
		}
	}
	v := st.view("?topology=managed&select=zigbee,zwave,matter,thread,bluetooth")
	for _, c := range v.Components {
		if c.Selection != "none" && c.State != haReady && c.State != haExternalStep {
			t.Errorf("component %s = %s (%s)", c.ID, c.State, c.Reason)
		}
	}

	// Converged: another pass creates nothing.
	mark := st.fleet.Mark()
	if out := st.tick(); out.Failed != "" || out.DriftProposed != "" {
		t.Fatalf("steady-state tick: %+v", out)
	}
	if c := creates(st.fleet.Since(mark)); len(c) > 0 {
		t.Fatalf("steady state re-created %v", c)
	}

	st.survivesRestartAndFailure(req)
}

var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// checkShapes asserts what each reference container was created with:
// the host-network services on the host, radios by their stable path,
// nothing published that should not be.
func (st *haStack) checkShapes() {
	st.t.Helper()
	host := wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST
	devices := func(c fakenode.Container) map[string]string {
		m := map[string]string{}
		for _, d := range c.Spec.GetDevices() {
			m[d.GetHostPath()] = d.GetContainerPath()
		}
		return m
	}
	for _, name := range st.fleet.ContainerNames(st.node) {
		c, _ := st.fleet.Container(st.node, name)
		if !digestPinned.MatchString(c.Spec.GetImage()) {
			st.t.Errorf("%s runs %s, not a digest-pinned image", name, c.Spec.GetImage())
		}
	}
	for _, name := range []string{"home-assistant", "matter-server", "otbr"} {
		c, _ := st.fleet.Container(st.node, name)
		if c.Spec.GetNetworkMode() != host || len(c.Spec.GetPorts()) > 0 {
			st.t.Errorf("%s: network mode %v, ports %v; want the host's network, nothing published", name, c.Spec.GetNetworkMode(), c.Spec.GetPorts())
		}
	}
	mq, _ := st.fleet.Container(st.node, "mosquitto")
	if ps := mq.Spec.GetPorts(); len(ps) != 1 || ps[0].GetHostIp() != "192.168.1.10" || ps[0].GetHostPort() != 1883 {
		st.t.Errorf("mosquitto ports = %v, want 192.168.1.10:1883 only", ps)
	}
	if d := devices(mustContainer(st, "zwave-js-ui")); d[e2eZWavePath] != "/dev/zwave" || len(d) != 1 {
		st.t.Errorf("zwave-js-ui devices = %v", d)
	}
	if d := devices(mustContainer(st, "otbr")); len(d) != 2 || d[e2eThreadPath] != "/dev/ttyRCP" || !haHasKey(d, "/dev/net/tun") {
		st.t.Errorf("otbr devices = %v", d)
	}
	if d := devices(mustContainer(st, "zigbee2mqtt")); len(d) != 0 {
		st.t.Errorf("zigbee2mqtt (network coordinator) devices = %v", d)
	}
	ha := mustContainer(st, "home-assistant")
	if ha.Spec.GetEnv()["WILDSTONE_MQTT_HOST"] == "" || len(ha.Spec.GetCapAdd()) != 0 {
		st.t.Errorf("home-assistant: MQTT env %q, capAdd %v", ha.Spec.GetEnv()["WILDSTONE_MQTT_HOST"], ha.Spec.GetCapAdd())
	}
	for _, m := range ha.Spec.GetMounts() {
		if m.GetSource() == "/run/dbus" {
			st.t.Errorf("home-assistant in proxy mode mounts the host's D-Bus")
		}
	}
}

func haHasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

func mustContainer(st *haStack, name string) fakenode.Container {
	st.t.Helper()
	c, ok := st.fleet.Container(st.node, name)
	if !ok {
		st.t.Fatalf("no container %s on %s", name, st.node)
	}
	return c
}
