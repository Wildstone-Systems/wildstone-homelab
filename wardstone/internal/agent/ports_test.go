package agent

import (
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
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
