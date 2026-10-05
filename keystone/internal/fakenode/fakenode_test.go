package fakenode

import (
	"context"
	"strings"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

const pinned = "ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func ensure(spec *wsv1.EnsureContainer) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: spec}}
}

// The rules the real agent enforces on its side are enforced here too,
// or a test built on the fake would pass a plan the node refuses.
func TestAgentSideRules(t *testing.T) {
	p, err := LoadProfile("../../../testing/fixtures/home-automation/reference-node.json")
	if err != nil {
		t.Fatal(err)
	}
	f := New()
	f.AddProfile(p)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		op   *wsv1.Operation
		want string
	}{
		"mutable tag":     {ensure(&wsv1.EnsureContainer{Name: "a", Image: "eclipse-mosquitto:2"}), "not digest-pinned"},
		"host with ports": {ensure(&wsv1.EnsureContainer{Name: "a", Image: pinned, NetworkMode: wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST, Ports: []*wsv1.PortBinding{{HostPort: 1}}}), "excludes ports"},
		"absent device":   {ensure(&wsv1.EnsureContainer{Name: "a", Image: pinned, Devices: []*wsv1.DeviceMapping{{HostPath: "/dev/serial/by-id/gone"}}}), "does not exist on this node"},
		"absent secret":   {ensure(&wsv1.EnsureContainer{Name: "a", Image: pinned, Env: map[string]string{"P": "secret/nope"}}), "secret/nope"},
	} {
		res, err := f.Apply(ctx, p.ID, tc.op)
		if err != nil || !strings.Contains(res.GetError().GetMessage(), tc.want) {
			t.Errorf("%s: %v %v", name, res.GetError(), err)
		}
	}
	if len(f.ContainerNames(p.ID)) != 0 {
		t.Fatal("a refused ensure created a container")
	}

	// A present by-id path from the fixture is accepted; an op newer than
	// the agent's minor is answered as an older agent answers it.
	res, _ := f.Apply(ctx, p.ID, ensure(&wsv1.EnsureContainer{Name: "z", Image: pinned,
		Devices: []*wsv1.DeviceMapping{{HostPath: "/dev/serial/by-id/usb-0658_0200-if00", ContainerPath: "/dev/zwave"}}}))
	if res.GetEnsureContainer() == nil || !res.GetEnsureContainer().GetCreated() {
		t.Fatalf("ensure = %v", res)
	}
	f.Do(p.ID, func(n *Node) { n.ProtocolMinor = 25 })
	res, _ = f.Apply(ctx, p.ID, &wsv1.Operation{Op: &wsv1.Operation_HomeAutomationReadiness{HomeAutomationReadiness: &wsv1.HomeAutomationReadiness{}}})
	if !strings.Contains(res.GetError().GetMessage(), "unknown operation") {
		t.Fatalf("readiness on minor 25 = %v", res)
	}
	f.SetConnected(p.ID, false)
	if _, err := f.Apply(ctx, p.ID, ensure(&wsv1.EnsureContainer{Name: "z", Image: pinned})); err == nil || !strings.Contains(err.Error(), "is not connected") {
		t.Fatalf("disconnected = %v", err)
	}
}
