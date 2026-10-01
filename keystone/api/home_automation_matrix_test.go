package api

// The failure half of the hardware-free home-automation matrix (#452):
// restarts, disconnects, crashes, protocol-minor skew and broken host
// prerequisites, each driven through the same API → plan → approval →
// reconcile loop → fakenode path as home_automation_e2e_test.go, and each
// checked at the layer that is supposed to catch it.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/fakenode"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// quiet ticks and requires that the pass changed nothing on the node and
// proposed nothing.
func (st *haStack) quiet(label string) {
	st.t.Helper()
	mark := st.fleet.Mark()
	out := st.tick()
	if out.Failed != "" || out.DriftProposed != "" || len(out.PlanIDs) > 0 {
		st.t.Fatalf("%s: tick %+v", label, out)
	}
	if ch := changes(st.fleet.Since(mark)); len(ch) > 0 {
		st.t.Fatalf("%s: the node was changed: %v", label, ch)
	}
}

func (st *haStack) complete(label string, req haOrchRequest) {
	st.t.Helper()
	if o := st.orch("preview", req, http.StatusOK); !o.Complete {
		st.t.Fatalf("%s: not complete: next %s (%s) %+v", label, o.Next.ID, o.Next.Reason, o.Next.Blockers)
	}
}

func (st *haStack) createdCounts() map[string]int {
	out := map[string]int{}
	for _, n := range st.fleet.ContainerNames(st.node) {
		out[n] = st.createdCount(n)
	}
	return out
}

// sameContainers checks that no container that existed before was
// replaced; new ones may have appeared.
func (st *haStack) sameContainers(label string, before map[string]int) {
	st.t.Helper()
	for n, c := range st.createdCounts() {
		if before[n] > 0 && before[n] != c {
			st.t.Errorf("%s: container %s was re-created (%d → %d)", label, n, before[n], c)
		}
	}
}

func (st *haStack) setCheck(name, subject string, state wsv1.ReadinessState, value string) {
	st.fleet.Do(st.node, func(n *fakenode.Node) {
		c := n.Check(name, subject)
		if c == nil {
			st.t.Fatalf("no %s check on %s", name, st.node)
		}
		c.State, c.Value = state, value
	})
}

// approveDrift approves the drift plan a tick proposed and lets the loop
// carry it out.
func (st *haStack) approveDrift(id string) {
	st.t.Helper()
	if id == "" {
		st.t.Fatal("no drift plan was proposed")
	}
	st.approve(id)
	out := st.tick()
	if got := st.plan(id).State; got != runtimedb.PlanApplied {
		st.t.Fatalf("drift plan %s = %s: %+v", id, got, out)
	}
}

func withoutStep(steps []string, drop string) []string {
	var out []string
	for _, s := range steps {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// survivesRestartAndFailure runs the complete reference stack through a
// control-plane restart, a host reboot, an agent disconnect, a crashed
// service and a node lost half way through applying a change.
func (st *haStack) survivesRestartAndFailure(req haOrchRequest) {
	t := st.t
	t.Helper()
	created := st.createdCounts()

	// The control plane restarts: the new loop's first pass changes
	// nothing and the setup is still complete.
	st.restartKeystone()
	st.quiet("keystone restart")
	st.complete("keystone restart", req)

	// The machine reboots: Docker brings every service back by its
	// restart policy, and Wildstone re-creates nothing.
	st.fleet.Reboot(st.node)
	st.quiet("host reboot")
	st.complete("host reboot", req)
	st.sameContainers("host reboot", created)

	// The agent disconnects: every component names the offline node,
	// nothing can be proposed, and the loop neither touches the node nor
	// reports a failure.
	st.fleet.SetConnected(st.node, false)
	o := st.orch("preview", req, http.StatusOK)
	if o.Complete || o.Next.Action != haStepResolve || o.Next.blocker("node_offline") == nil {
		t.Fatalf("offline: next %+v", o.Next)
	}
	plans := st.planCount()
	st.orch("propose", req, http.StatusConflict)
	if st.planCount() != plans {
		t.Fatal("a plan was filed for an offline node")
	}
	st.quiet("offline")
	st.fleet.SetConnected(st.node, true)
	st.quiet("reconnect")
	st.complete("reconnect", req)
	st.sameContainers("disconnect", created)

	// A service dies and stays down. The loop sees it; restarting it is
	// a change only an approval authorizes (ADR-W008), so the setup is
	// not complete until that drift plan is approved — and then the same
	// container is started, not replaced.
	st.fleet.Crash(st.node, "matter-server")
	out := st.tick()
	if c := mustContainer(st, "matter-server"); c.Running || out.DriftProposed == "" {
		t.Fatalf("crash: running %v, tick %+v", c.Running, out)
	}
	if o := st.orch("preview", req, http.StatusOK); o.Complete || o.Next.Component != haMatter {
		t.Fatalf("crashed matter: next %+v", o.Next)
	}
	st.approveDrift(out.DriftProposed)
	st.running("matter-server")
	st.sameContainers("crash", created)
	st.complete("crash repaired", req)

	// The household switches Bluetooth to the host's adapter, and the
	// node drops off half way through applying it: the approved plan
	// stays applying, the running Home Assistant is untouched, and the
	// reconnect finishes the job without another approval.
	req.Modes[haBluetooth] = "host-bluez"
	req.ConfirmedSteps = withoutStep(req.ConfirmedSteps, "bluetooth/esphome_bluetooth_proxies")
	pv := st.orch("preview", req, http.StatusOK)
	if pv.Next.ID != "bluetooth:reconfigure_package:home-assistant" || pv.Next.Params["bluetooth"] != "host-bluez" ||
		pv.Next.Params["mqtt_enabled"] != "true" {
		t.Fatalf("switch to host-bluez: next %s params %v", pv.Next.ID, pv.Next.Params)
	}
	req.ExpectStep = pv.Next.ID
	id, _ := st.orch("propose", req, http.StatusCreated).Plan.(string)
	req.ExpectStep = ""
	st.approve(id)
	st.fleet.SetConnected(st.node, false)
	if out := st.tick(); out.Failed != "" || st.plan(id).State != runtimedb.PlanApplying {
		t.Fatalf("offline apply: plan %s, tick %+v", st.plan(id).State, out)
	}
	st.sameContainers("offline apply", created)
	st.fleet.SetConnected(st.node, true)
	if out := st.tick(); st.plan(id).State != runtimedb.PlanApplied {
		t.Fatalf("reconnect apply: plan %s, tick %+v", st.plan(id).State, out)
	}
	ha := mustContainer(st, "home-assistant")
	dbus := false
	for _, m := range ha.Spec.GetMounts() {
		dbus = dbus || (m.GetSource() == "/run/dbus" && m.GetReadOnly())
	}
	if !dbus || strings.Join(ha.Spec.GetCapAdd(), ",") != "NET_ADMIN,NET_RAW" || ha.Created != created["home-assistant"]+1 {
		t.Fatalf("host-bluez home-assistant: dbus %v, capAdd %v, created %d", dbus, ha.Spec.GetCapAdd(), ha.Created)
	}
	st.complete("host-bluez", req)
	st.quiet("host-bluez converged")
}

// refusedAtApply proposes and approves the next step, which the
// reconciler must refuse with want before touching anything that runs;
// the aggregate then carries the refusal as evidence.
func (st *haStack) refusedAtApply(req haOrchRequest, wantStep, want string) {
	t := st.t
	t.Helper()
	created := st.createdCounts()
	o := st.orch("preview", req, http.StatusOK)
	if o.Next.ID != wantStep {
		t.Fatalf("next = %s (%s), want %s", o.Next.ID, o.Next.Reason, wantStep)
	}
	req.ExpectStep = o.Next.ID
	id, _ := st.orch("propose", req, http.StatusCreated).Plan.(string)
	st.approve(id)
	out := st.tick()
	if st.plan(id).State != runtimedb.PlanFailed || !strings.Contains(out.Failed, want) || !strings.Contains(out.Failed, "upgrade wardstoned") {
		t.Fatalf("%s: plan %s, tick failed %q", wantStep, st.plan(id).State, out.Failed)
	}
	st.sameContainers(wantStep, created)
	st.running(st.services(created)...)
	o = st.orch("preview", referenceRequestLike(req), http.StatusOK)
	if o.Next.Action != haStepResolve || o.Next.blocker("plan_failed") == nil || !haEvidenceSays(o.Next.Blockers, want) {
		t.Fatalf("after the refusal: next %s %+v", o.Next.ID, o.Next.Blockers)
	}
}

// refusedBeforePlan checks the next step is blocked with code, whose
// evidence says detail, and that proposing files nothing and changes
// nothing.
func (st *haStack) refusedBeforePlan(req haOrchRequest, wantStep, code, detail string) haOrchestration {
	t := st.t
	t.Helper()
	o := st.orch("preview", req, http.StatusOK)
	found := false
	for _, b := range o.Next.Blockers {
		found = found || (b.Code == code && haEvidenceSays([]haBlocker{b}, detail))
	}
	if o.Next.ID != wantStep || !found {
		t.Fatalf("next = %s, want %s with %s saying %q: %+v", o.Next.ID, wantStep, code, detail, o.Next.Blockers)
	}
	plans, mark := st.planCount(), st.fleet.Mark()
	st.orch("propose", req, http.StatusConflict)
	if st.planCount() != plans || len(changes(st.fleet.Since(mark))) > 0 {
		t.Fatal("a refused step was proposed or reached the node")
	}
	return o
}

// upgradeAgent moves the node's agent to minor; when an approved change
// was refused for the old one, the loop proposes the repair and an
// approval carries it out.
func (st *haStack) upgradeAgent(minor uint32, repair bool) {
	st.t.Helper()
	st.fleet.Do(st.node, func(n *fakenode.Node) { n.ProtocolMinor = minor })
	if repair {
		st.approveDrift(st.tick().DriftProposed)
	}
}

// Protocol-minor skew, as a rolling agent upgrade through every minor
// the managed stack depends on. An agent older than a step needs is
// refused at the layer that knows — the aggregate, for the radio and
// readiness probes, before anything is proposed; the reconciler, for a
// container or route field, at apply — naming the minor and the upgrade,
// with every running service untouched. Each upgrade then lets the setup
// continue through ordinary approvals.
func TestHomeAutomationE2EProtocolSkew(t *testing.T) {
	t.Parallel()
	st := newHAStack(t, func(n *fakenode.Node) { n.ProtocolMinor = 21 })
	st.installBase()
	// Zigbee pulls in the broker and Home Assistant; host-bluez Bluetooth
	// needs the newest minor of all.
	req := selectOnly(referenceRequest(), haZigbee, haBluetooth)
	req.Modes[haBluetooth] = "host-bluez"

	// No readiness probe below 25, so no host address to fill in: the
	// household is asked for it rather than given a guess.
	o := st.orch("preview", req, http.StatusOK)
	if o.Next.ID != "mqtt:resolve_blockers" || o.Next.blocker("param_needed") == nil {
		t.Fatalf("old agent, no address: next %s %+v", o.Next.ID, o.Next.Blockers)
	}
	req.Options[haMQTT] = map[string]string{"listen_addr": "192.168.1.10"}
	if _, state := st.advance(req); state != runtimedb.PlanApplied {
		t.Fatalf("mqtt on minor 21: %s", state)
	}

	st.refusedAtApply(req, "home-assistant:install_package:home-assistant",
		"needs wardstone protocol minor 22 (networkMode: host) and the agent on node ha1 speaks minor 21")
	if st.has("home-assistant") {
		t.Fatal("home-assistant was started on a minor-21 agent")
	}

	// Minor 22 could run the container, but not yet the host Route in
	// front of it: the loop's next pass over the committed install stops
	// at the Route with the same kind of refusal, and the aggregate
	// carries it.
	st.fleet.Do(st.node, func(n *fakenode.Node) { n.ProtocolMinor = 22 })
	routeRefusal := "routes to host/8123, which needs wardstone protocol minor 23, and the agent on node ha1 speaks minor 22"
	if out := st.tick(); !strings.Contains(out.Failed, routeRefusal) {
		t.Fatalf("minor 22: tick %q", out.Failed)
	}
	if o := st.orch("preview", req, http.StatusOK); o.Next.Action != haStepResolve || !haEvidenceSays(o.Next.Blockers, routeRefusal) {
		t.Fatalf("minor 22: next %s %+v", o.Next.ID, o.Next.Blockers)
	}

	st.upgradeAgent(23, true)
	st.refusedBeforePlan(req, "zigbee:resolve_blockers", "agent_outdated", "the radio inventory needs 24")
	st.upgradeAgent(24, false)
	st.refusedBeforePlan(req, "zigbee:resolve_blockers", "agent_outdated", "the readiness probe needs 25")
	st.upgradeAgent(25, false)
	if _, state := st.advance(req); state != runtimedb.PlanApplied {
		t.Fatalf("zigbee on minor 25: %s", state)
	}
	st.refusedAtApply(req, "bluetooth:reconfigure_package:home-assistant",
		"needs wardstone protocol minor 26 (a read-only mount of the host's /run/dbus")
	for _, m := range mustContainer(st, "home-assistant").Spec.GetMounts() {
		if m.GetSource() == "/run/dbus" {
			t.Fatal("the proxy-mode container was replaced on a minor-25 agent")
		}
	}

	st.upgradeAgent(26, true)
	if o := st.orch("preview", req, http.StatusOK); o.Next.Action != haStepExternal {
		t.Fatalf("after the last upgrade: next %s (%s) %+v", o.Next.ID, o.Next.Reason, o.Next.Blockers)
	}
	st.quiet("upgraded")
}

// A deliberately broken host is reported where it can be seen: the
// orchestration names the missing check, its evidence and the exact fix
// before any plan exists; a plan forced past it through the capability
// endpoint fails at apply with the running services untouched, and the
// aggregate carries both. Fixing the host lets the setup finish.
func TestHomeAutomationE2EBrokenPrerequisites(t *testing.T) {
	t.Parallel()
	missing := wsv1.ReadinessState_READINESS_STATE_MISSING
	ready := wsv1.ReadinessState_READINESS_STATE_READY
	st := newHAStack(t, func(n *fakenode.Node) {
		// thread-otbr's preflight (before_server) refuses to start
		// otbr-agent without IPv6 forwarding, and the container exits.
		n.Starts = func(n *fakenode.Node, spec *wsv1.EnsureContainer) (bool, string) {
			if spec.GetName() == "otbr" {
				return n.Check("ipv6_forwarding", "").GetState() == wsv1.ReadinessState_READINESS_STATE_READY, ""
			}
			return true, ""
		}
	})
	st.setCheck("ipv6", "", missing, "1")
	st.setCheck("interface_ipv6", "eth0", missing, "")
	st.setCheck("ipv6_forwarding", "", missing, "0")
	st.setCheck("dbus_system_bus", "", missing, "")
	st.installBase()
	req := selectOnly(referenceRequest(), haMatter, haThread, haBluetooth)
	req.Modes[haBluetooth] = "host-bluez"

	// Home Assistant needs none of it (it installs with Bluetooth
	// proxies); Matter waits for IPv6, Thread for IPv6 and forwarding,
	// host BlueZ for D-Bus.
	o := st.advanceAll(req)
	if !st.has("home-assistant") || st.has("matter-server") {
		t.Fatalf("containers %v", st.fleet.ContainerNames(st.node))
	}
	o = st.refusedBeforePlan(req, "matter:resolve_blockers", "ipv6_missing", "")
	for _, s := range o.Sequence {
		want := map[string]string{haThread: "ipv6_forwarding_missing", haBluetooth: "dbus_system_bus_missing"}[s.Component]
		if want != "" && (s.State != haBlocked || s.blocker(want) == nil) {
			t.Errorf("%s = %s %+v", s.Component, s.State, s.Blockers)
		}
	}
	if b := o.Next.blocker("interface_ipv6_missing"); b == nil || b.Evidence[0].Subject != "eth0" {
		t.Fatalf("no interface evidence: %+v", o.Next.Blockers)
	}

	st.setCheck("ipv6", "", ready, "0")
	st.setCheck("interface_ipv6", "eth0", ready, "2001:db8:10::10/64 fe80::10/64")
	if _, state := st.advance(req); state != runtimedb.PlanApplied || !st.has("matter-server") {
		t.Fatalf("matter after the IPv6 fix: %s", state)
	}

	// Thread: the missing sysctl, its value, and the exact fix.
	o = st.refusedBeforePlan(req, "thread:resolve_blockers", "ipv6_forwarding_missing", "")
	b := o.Next.blocker("ipv6_forwarding_missing")
	if b.Evidence[0].Subject != "/proc/sys/net/ipv6/conf/all/forwarding" || b.Evidence[0].Value != "0" ||
		!haActionSays(b.Actions, "manual_step", "net.ipv6.conf.all.forwarding=1") || !haActionSays(b.Actions, "retry_probe", "") {
		t.Fatalf("forwarding blocker: %+v", b)
	}

	// Forced past it through the capability endpoint: the border router
	// refuses to start, the health gate fails the plan, and the aggregate
	// reports the host check and the failed plan together.
	created := st.createdCounts()
	id := st.enable("thread-otbr", map[string]string{"node": st.node, "radio_device": e2eThreadPath, "backbone_interface": "eth0"})
	st.approve(id)
	if out := st.tick(); st.plan(id).State != runtimedb.PlanFailed || !strings.Contains(out.Failed, "health gate failed") {
		t.Fatalf("forced thread: plan %s, tick %q", st.plan(id).State, out.Failed)
	}
	if c, ok := st.fleet.Container(st.node, "otbr"); ok && c.Running {
		t.Fatal("otbr is running without IPv6 forwarding")
	}
	st.sameContainers("forced thread", created)
	st.running(st.services(created)...)
	o = st.orch("preview", req, http.StatusOK)
	if o.Next.ID != "thread:resolve_blockers" || o.Next.blocker("ipv6_forwarding_missing") == nil || o.Next.blocker("plan_failed") == nil {
		t.Fatalf("forced thread: next %s %+v", o.Next.ID, o.Next.Blockers)
	}
	// The host is fixed; the loop proposes the repair, an approval
	// carries it out, and the old failure stops holding the setup.
	st.setCheck("ipv6_forwarding", "", ready, "1")
	st.approveDrift(st.tick().DriftProposed)
	st.running("otbr")

	// Host BlueZ without a D-Bus system bus: refused before a plan, and,
	// forced through the capability endpoint, refused by the reconciler
	// before the proxy-mode container is touched.
	o = st.refusedBeforePlan(req, "bluetooth:resolve_blockers", "dbus_system_bus_missing", "")
	if !haActionSays(o.Next.blocker("dbus_system_bus_missing").Actions, "manual_step", "systemctl enable --now dbus") {
		t.Fatalf("no D-Bus remedy: %+v", o.Next.Blockers)
	}
	created = st.createdCounts()
	id = st.enable("home-assistant", map[string]string{"node": st.node, "public_domain": "home.example.internal", "bluetooth": "host-bluez"})
	st.approve(id)
	if out := st.tick(); st.plan(id).State != runtimedb.PlanFailed || !strings.Contains(out.Failed, "dbus_system_bus missing") ||
		!strings.Contains(out.Failed, "before the existing container is touched") {
		t.Fatalf("forced host-bluez: plan %s, tick %q", st.plan(id).State, out.Failed)
	}
	st.sameContainers("forced host-bluez", created)
	st.running("home-assistant")
	for _, m := range mustContainer(st, "home-assistant").Spec.GetMounts() {
		if m.GetSource() == "/run/dbus" {
			t.Fatal("the proxy-mode container was replaced")
		}
	}
	// D-Bus started: the committed change goes through on approval.
	st.setCheck("dbus_system_bus", "", ready, "socket")
	st.approveDrift(st.tick().DriftProposed)
	if o := st.orch("preview", req, http.StatusOK); o.Next.Action != haStepExternal {
		t.Fatalf("every prerequisite fixed: next %s (%s) %+v", o.Next.ID, o.Next.Reason, o.Next.Blockers)
	}
}

func haEvidenceSays(bs []haBlocker, text string) bool {
	for _, b := range bs {
		for _, ev := range b.Evidence {
			if strings.Contains(ev.Detail, text) {
				return true
			}
		}
	}
	return false
}

func (st *haStack) has(name string) bool {
	_, ok := st.fleet.Container(st.node, name)
	return ok
}

// services are the long-running containers among names (a Job's
// container is expected to have exited).
func (st *haStack) services(names map[string]int) []string {
	var out []string
	for n := range names {
		if c, ok := st.fleet.Container(st.node, n); ok && c.Spec.GetRestartPolicy() != "no" {
			out = append(out, n)
		}
	}
	return out
}

func haActionSays(actions []haAction, kind, text string) bool {
	for _, a := range actions {
		if a.Kind != kind {
			continue
		}
		if text == "" || strings.Contains(strings.Join(a.Instructions, "\n"), text) {
			return true
		}
	}
	return false
}

// Catalog-wide wiring: every home-automation component resolves to a
// shipped package the orchestration can drive without anyone typing a
// value it could have read from the node.
func TestHomeAutomationCatalogWiring(t *testing.T) {
	cat, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range haComponents {
		var pkgs []string
		for _, p := range cat.List() {
			if p.Metadata.Capability != spec.Capability {
				continue
			}
			pkgs = append(pkgs, p.Metadata.Name)
			if len(spec.ModeParams) > 0 {
				d, ok := modeParam(spec, p)
				if !ok {
					t.Errorf("%s: package %s has no %v param", spec.ID, p.Metadata.Name, spec.ModeParams)
					continue
				}
				for _, m := range spec.OnModes {
					if !strings.Contains("|"+strings.TrimPrefix(d.Validate, "enum:")+"|", "|"+m+"|") {
						t.Errorf("%s: %s.%s does not offer mode %s (%s)", spec.ID, p.Metadata.Name, d.Name, m, d.Validate)
					}
				}
				continue
			}
			radio := ""
			for _, name := range haRadioParams {
				if declaresParam(p, name) && radio == "" {
					radio = name
				}
			}
			if spec.RadioClaims && radio == "" {
				t.Errorf("%s: package %s takes no radio (%v)", spec.ID, p.Metadata.Name, haRadioParams)
			}
			for _, d := range p.Params {
				if !d.Required || d.Default != "" {
					continue
				}
				// What the orchestration fills: deployment facts, the
				// chosen radio's stable path, the node's own address and
				// its default-route interface.
				if d.Source != "" || d.Name == radio || d.Validate == "private_ipv4" || d.Validate == "interface_name" {
					continue
				}
				// The external-Home-Assistant adapter asks for entity
				// names only that Home Assistant knows (param_needed is
				// the designed answer there); the managed stack must ask
				// for nothing.
				if spec.Topology == haTopologyExternal {
					continue
				}
				t.Errorf("%s: %s.%s is required and nothing fills it", spec.ID, p.Metadata.Name, d.Name)
			}
		}
		if len(pkgs) == 0 {
			t.Errorf("%s: no shipped package provides capability %s", spec.ID, spec.Capability)
		}
	}
}
