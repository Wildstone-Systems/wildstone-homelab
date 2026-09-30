package agent

import (
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestEnsureContainerHostNetwork is the protocol-minor-22 op test: a HOST
// op reaches Docker as HostConfig.NetworkMode=host with no network
// endpoint, InspectContainer reports it back, and moving the same
// container back to the default is a replacement that clears it.
func TestEnsureContainerHostNetwork(t *testing.T) {
	fake := newFakeDocker(t)
	fake.addOwnedNetwork("wildstone")
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "hub", Image: testDigestImage,
			NetworkMode: wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST,
			Labels:      map[string]string{SpecHashLabel: "host-v1"},
		},
	}})
	if oe := res.GetError(); oe != nil {
		t.Fatalf("ensure host: %v", oe)
	}
	fake.mu.Lock()
	c := fake.containers["hub"]
	mode, nets := c.host.NetworkMode, c.networks
	fake.mu.Unlock()
	if mode != "host" {
		t.Errorf("HostConfig.NetworkMode = %q, want host", mode)
	}
	if len(nets) != 0 {
		t.Errorf("host-mode container was also attached to %v", nets)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: "hub"},
	}})
	if got := res.GetInspectContainer().GetNetworkMode(); got != "host" {
		t.Errorf("inspect network_mode = %q, want host", got)
	}

	// Back to the managed bridge: a new hash, so the container is
	// replaced, and the default leaves NetworkMode unset in the create
	// body exactly as it was before this field existed.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "hub", Image: testDigestImage, Network: "wildstone",
			Labels: map[string]string{SpecHashLabel: "bridge-v1"},
		},
	}})
	if !res.GetEnsureContainer().GetCreated() {
		t.Fatalf("move to bridge did not replace: %+v", res)
	}
	fake.mu.Lock()
	c = fake.containers["hub"]
	mode, nets = c.host.NetworkMode, c.networks
	fake.mu.Unlock()
	if mode != "" || len(nets) != 1 || nets[0] != "wildstone" {
		t.Errorf("bridge container: NetworkMode %q, networks %v; want unset and [wildstone]", mode, nets)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: "hub"},
	}})
	if got := res.GetInspectContainer().GetNetworkMode(); got != "bridge" {
		t.Errorf("inspect network_mode = %q, want Docker's verbatim bridge", got)
	}
}

// TestEnsureContainerHostNetworkRefusalsKeepTheOldContainer: host mode
// with ports, host mode with a network, and a mode this agent does not
// know are each INVALID — and refused before the replace path, so the
// container they would have replaced is still there and still running.
func TestEnsureContainerHostNetworkRefusalsKeepTheOldContainer(t *testing.T) {
	fake := newFakeDocker(t)
	fake.addOwnedNetwork("wildstone")
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "hub", Image: testDigestImage, Network: "wildstone",
			Labels: map[string]string{SpecHashLabel: "bridge-v1"},
		},
	}})
	if oe := res.GetError(); oe != nil {
		t.Fatalf("ensure bridge: %v", oe)
	}
	fake.mu.Lock()
	originalID := fake.containers["hub"].id
	fake.mu.Unlock()

	for name, op := range map[string]*wsv1.EnsureContainer{
		"host with ports": {
			NetworkMode: wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST,
			Ports:       []*wsv1.PortBinding{{HostPort: 8123, ContainerPort: 8123}},
		},
		"host with network": {
			NetworkMode: wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST,
			Network:     "wildstone",
		},
		"unknown mode": {NetworkMode: wsv1.ContainerNetworkMode(7)},
	} {
		op.Name, op.Image = "hub", testDigestImage
		op.Labels = map[string]string{SpecHashLabel: "changed"}
		res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: op}})
		if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
			t.Errorf("%s: got %+v, want INVALID", name, res)
			continue
		}
		fake.mu.Lock()
		c, ok := fake.containers["hub"]
		fake.mu.Unlock()
		if !ok || c.id != originalID || !c.running {
			t.Errorf("%s: the running container was disturbed before the refusal", name)
		}
	}
}

// TestAuditRecordsHostNetwork: host networking is host access, so the
// agent's own audit trail names it; the default adds nothing.
func TestAuditRecordsHostNetwork(t *testing.T) {
	_, fields := auditFields(&wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: "hub", Image: testDigestImage, NetworkMode: wsv1.ContainerNetworkMode_CONTAINER_NETWORK_MODE_HOST,
	}}})
	found := false
	for _, f := range fields {
		if f.Key == "network_mode" && f.Value.String() == "host" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit attrs %v do not record network_mode=host", fields)
	}
	_, fields = auditFields(&wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: "app", Image: testDigestImage,
	}}})
	for _, f := range fields {
		if f.Key == "network_mode" {
			t.Errorf("a default-network container logged %v", f)
		}
	}
}

// TestEnsureContainerHostGatewayAlias: the ingress proxy asked for the
// host-gateway alias reaches Docker with the literal "host-gateway"
// ExtraHosts entry, on the bridge, with nothing else granted (#433).
func TestEnsureContainerHostGatewayAlias(t *testing.T) {
	fake := newFakeDocker(t)
	fake.addOwnedNetwork("wildstone")
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "caddy", Image: testDigestImage, Network: "wildstone",
			HostAliases: []string{hostGatewayAlias},
			Labels:      map[string]string{SpecHashLabel: "gw-v1"},
		},
	}})
	if oe := res.GetError(); oe != nil {
		t.Fatalf("ensure: %v", oe)
	}
	fake.mu.Lock()
	c := fake.containers["caddy"]
	host, nets := c.host, c.networks
	fake.mu.Unlock()
	if len(host.ExtraHosts) != 1 || host.ExtraHosts[0] != "host.docker.internal:host-gateway" {
		t.Errorf("ExtraHosts = %v, want [host.docker.internal:host-gateway]", host.ExtraHosts)
	}
	if host.NetworkMode != "" || len(nets) != 1 || nets[0] != "wildstone" {
		t.Errorf("proxy left the bridge: NetworkMode %q, networks %v", host.NetworkMode, nets)
	}
	if len(host.CapAdd) != 0 || len(host.Devices) != 0 || len(host.PortBindings) != 0 {
		t.Errorf("the alias granted something: %+v", host)
	}
}
