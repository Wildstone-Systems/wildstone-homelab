package agent

import (
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// Docker needs both Config.ExposedPorts and HostConfig.PortBindings. Without
// ExposedPorts it accepts the create request but publishes no host ports.
func TestEnsureContainerPublishesDeclaredPorts(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "proxy", Image: testDigestImage,
			Ports: []*wsv1.PortBinding{
				{HostIp: "100.64.0.46", HostPort: 7443, ContainerPort: 7443},
				{HostPort: 53, ContainerPort: 53, Protocol: wsv1.PortProtocol_PORT_PROTOCOL_UDP},
			},
		},
	}})
	if oe := res.GetError(); oe != nil {
		t.Fatalf("ensure: %v", oe)
	}
	fake.mu.Lock()
	c := fake.containers["proxy"]
	fake.mu.Unlock()
	if c == nil {
		t.Fatal("Docker did not receive the container")
	}
	for _, key := range []string{"7443/tcp", "53/udp"} {
		if _, ok := c.exposedPorts[key]; !ok {
			t.Errorf("Docker create omitted ExposedPorts[%q]", key)
		}
		if len(c.host.PortBindings[key]) != 1 {
			t.Errorf("Docker create omitted PortBindings[%q]", key)
		}
	}
	if got := c.host.PortBindings["7443/tcp"]; len(got) != 1 {
		t.Fatalf("expected one TCP binding, got %v", got)
	} else if got[0].HostIP != "100.64.0.46" {
		t.Errorf("host binding IP = %q", got[0].HostIP)
	}
}

func TestEnsureContainerCleansUpFailedPortStart(t *testing.T) {
	fake := newFakeDocker(t)
	fake.startErrorOnce = true
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	op := &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: "proxy", Image: testDigestImage,
		Ports: []*wsv1.PortBinding{{HostIp: "100.64.0.46", HostPort: 45443, ContainerPort: 45443}},
	}}}
	if res := dispatch(t, h, "node-a", op); res.GetError() == nil {
		t.Fatal("expected Docker port bind failure")
	}
	fake.mu.Lock()
	_, leftBehind := fake.containers["proxy"]
	fake.mu.Unlock()
	if leftBehind {
		t.Fatal("failed Docker start left a partial container for retry")
	}
	if res := dispatch(t, h, "node-a", op); res.GetError() != nil {
		t.Fatalf("retry after port conflict cleared: %v", res.GetError())
	}
}

func TestInspectAndEnsureRepairMissingPublishedPorts(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	op := &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: "proxy", Image: testDigestImage,
		Labels: map[string]string{SpecHashLabel: "wanted"},
		Ports:  []*wsv1.PortBinding{{HostIp: "100.64.0.46", HostPort: 45443, ContainerPort: 45443}},
	}}}
	first := dispatch(t, h, "node-a", op)
	if first.GetError() != nil {
		t.Fatalf("first ensure: %v", first.GetError())
	}
	fake.mu.Lock()
	oldID := fake.containers["proxy"].id
	fake.containers["proxy"].suppressPorts = true
	fake.mu.Unlock()

	observed := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: "proxy"},
	}})
	if observed.GetError() != nil {
		t.Fatalf("inspect: %v", observed.GetError())
	}
	if got := observed.GetInspectContainer().GetLabels()[SpecHashLabel]; got != "" {
		t.Fatalf("malformed container reported converged spec hash %q", got)
	}
	repaired := dispatch(t, h, "node-a", op)
	if repaired.GetError() != nil {
		t.Fatalf("repair: %v", repaired.GetError())
	}
	fake.mu.Lock()
	current := fake.containers["proxy"]
	fake.mu.Unlock()
	if current == nil || current.id == oldID {
		t.Fatalf("malformed container was not replaced: old=%s current=%v", oldID, current)
	}
}
