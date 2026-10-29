package agent

import (
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

func TestDockerSizeToBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"512m": 512 << 20,
		"4g":   4 << 30,
		"1k":   1 << 10,
		"100b": 100,
		"1024": 1024,
	} {
		got, oe := dockerSizeToBytes(in)
		if oe != nil {
			t.Errorf("%q: unexpected error: %v", in, oe.GetMessage())
			continue
		}
		if got != want {
			t.Errorf("dockerSizeToBytes(%q) = %d, want %d", in, got, want)
		}
	}
	if got, oe := dockerSizeToBytes(""); oe != nil || got != 0 {
		t.Errorf("empty input: got %d, %v", got, oe)
	}
	for _, bad := range []string{"0", "-1", "abc", "5gg", "5.5g", "g"} {
		if _, oe := dockerSizeToBytes(bad); oe == nil {
			t.Errorf("%q: expected an error", bad)
		} else if oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
			t.Errorf("%q: code %v, want INVALID", bad, oe.GetCode())
		}
	}
}

func TestCpusToNanoCPUs(t *testing.T) {
	for in, want := range map[string]int64{
		"2":    2_000_000_000,
		"0.5":  500_000_000,
		"1.25": 1_250_000_000,
	} {
		got, oe := cpusToNanoCPUs(in)
		if oe != nil {
			t.Errorf("%q: unexpected error: %v", in, oe.GetMessage())
			continue
		}
		if got != want {
			t.Errorf("cpusToNanoCPUs(%q) = %d, want %d", in, got, want)
		}
	}
	if got, oe := cpusToNanoCPUs(""); oe != nil || got != 0 {
		t.Errorf("empty input: got %d, %v", got, oe)
	}
	for _, bad := range []string{"0", "-1", "abc"} {
		if _, oe := cpusToNanoCPUs(bad); oe == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestCheckCapDrop(t *testing.T) {
	for _, ok := range []string{"ALL", "NET_ADMIN", "SYS_ADMIN"} {
		if oe := checkCapDrop(ok); oe != nil {
			t.Errorf("%s refused: %v", ok, oe.GetMessage())
		}
	}
	for _, bad := range []string{"", "net_admin", "CAP_NET_ADMIN", "Net_Admin"} {
		if oe := checkCapDrop(bad); oe == nil {
			t.Errorf("%q was allowed", bad)
		}
	}
	// The CAP_ spelling is reported as invalid with the fix named, the
	// same courtesy checkCapability extends.
	if oe := checkCapDrop("CAP_SYS_ADMIN"); oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Errorf("CAP_SYS_ADMIN: got %v, want INVALID", oe)
	}
}

// TestEnsureContainerCarriesHardeningToDocker is the H1 wardstone op test:
// a plan using the hardening fields reaches Docker's HostConfig exactly
// as asked, unlike devices/capAdd — dropping SYS_ADMIN and capping
// resources never needs a host-shaped allowlist.
func TestEnsureContainerCarriesHardeningToDocker(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	gid := int64(1001)
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "hermes", Image: testDigestImage,
			RuntimeIdentity: &wsv1.ContainerRuntimeIdentity{Uid: 1000, Gid: &gid},
			ReadOnlyRootfs:  true,
			CapDrop:         []string{"ALL"},
			NoNewPrivileges: true,
			Resources:       &wsv1.ContainerResources{Memory: "4g", Cpus: "2", PidsLimit: 512},
			ShmSize:         "1g",
		},
	}})
	if oe := res.GetError(); oe != nil {
		t.Fatalf("ensure: %v", oe)
	}
	fake.mu.Lock()
	hc := fake.containers["hermes"].host
	fake.mu.Unlock()
	if !hc.ReadonlyRootfs {
		t.Error("ReadonlyRootfs was not set")
	}
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %+v", hc.CapDrop)
	}
	if len(hc.SecurityOpt) != 1 || hc.SecurityOpt[0] != "no-new-privileges:true" {
		t.Errorf("SecurityOpt = %+v", hc.SecurityOpt)
	}
	if hc.Memory != 4<<30 {
		t.Errorf("Memory = %d, want %d", hc.Memory, int64(4)<<30)
	}
	if hc.NanoCpus != 2_000_000_000 {
		t.Errorf("NanoCpus = %d, want 2000000000", hc.NanoCpus)
	}
	if hc.PidsLimit != 512 {
		t.Errorf("PidsLimit = %d, want 512", hc.PidsLimit)
	}
	if hc.ShmSize != 1<<30 {
		t.Errorf("ShmSize = %d, want %d", hc.ShmSize, int64(1)<<30)
	}
	if got := fake.containers["hermes"].user; got != "1000:1001" {
		t.Errorf("Docker User = %q, want 1000:1001", got)
	}
	inspect := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{InspectContainer: &wsv1.InspectContainer{Name: "hermes"}}}).GetInspectContainer()
	if !inspect.GetHardeningObserved() || inspect.GetRuntimeIdentity().GetUid() != 1000 || inspect.GetRuntimeIdentity().GetGid() != 1001 ||
		!inspect.GetReadOnlyRootfs() || !inspect.GetNoNewPrivileges() || inspect.GetMemoryBytes() != 4<<30 ||
		inspect.GetNanoCpus() != 2_000_000_000 || inspect.GetPidsLimit() != 512 || inspect.GetShmSizeBytes() != 1<<30 {
		t.Errorf("inspect hardening readback = %+v", inspect)
	}

	// A malformed resource value fails the op before anything is created,
	// the same up-front-check discipline devices and capAdd already get.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "bad-resources", Image: testDigestImage,
			Resources: &wsv1.ContainerResources{Memory: "not-a-size"},
		},
	}})
	if oe := res.GetError(); oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Fatalf("bad memory string: got %v, want INVALID", res.GetError())
	}
	fake.mu.Lock()
	_, created := fake.containers["bad-resources"]
	fake.mu.Unlock()
	if created {
		t.Error("a container was created despite the malformed resources")
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: "bad-user", Image: testDigestImage, RuntimeIdentity: &wsv1.ContainerRuntimeIdentity{Uid: -1},
	}}})
	if oe := res.GetError(); oe == nil || oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Fatalf("negative uid: got %v, want INVALID", res.GetError())
	}
}

// TestEnsureContainerHardeningFieldsAreOptional: a spec that sets none of
// the H1 fields must produce a HostConfig with none of them set either —
// the no-churn property that keeps an existing fleet from being replaced
// once for nothing when this feature ships.
func TestEnsureContainerHardeningFieldsAreOptional(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{Name: "plain", Image: testDigestImage},
	}})
	if oe := res.GetError(); oe != nil {
		t.Fatalf("ensure: %v", oe)
	}
	fake.mu.Lock()
	hc := fake.containers["plain"].host
	fake.mu.Unlock()
	if fake.containers["plain"].user != "" || hc.ReadonlyRootfs || len(hc.CapDrop) != 0 || len(hc.SecurityOpt) != 0 ||
		hc.Memory != 0 || hc.NanoCpus != 0 || hc.PidsLimit != 0 || hc.ShmSize != 0 {
		t.Errorf("a spec with no hardening fields set some anyway: %+v", hc)
	}
}

func TestManagedHardeningMatchesEveryConfiguredField(t *testing.T) {
	gid := int64(1001)
	op := &wsv1.EnsureContainer{
		RuntimeIdentity: &wsv1.ContainerRuntimeIdentity{Uid: 1000, Gid: &gid},
		ReadOnlyRootfs:  true, CapDrop: []string{"ALL"}, NoNewPrivileges: true,
		Resources: &wsv1.ContainerResources{Memory: "4g", Cpus: "2", PidsLimit: 512}, ShmSize: "1g",
	}
	base := func() *containerInspect {
		ci := &containerInspect{}
		ci.Config.User = "1000:1001"
		ci.HostConfig.ReadonlyRootfs = true
		ci.HostConfig.CapDrop = []string{"ALL"}
		ci.HostConfig.SecurityOpt = []string{"no-new-privileges:true"}
		ci.HostConfig.Memory, ci.HostConfig.NanoCpus, ci.HostConfig.PidsLimit, ci.HostConfig.ShmSize = 4<<30, 2_000_000_000, 512, 1<<30
		return ci
	}
	if !managedHardeningMatches(base(), op, "1000:1001", 4<<30, 2_000_000_000, 512, 1<<30) {
		t.Fatal("matching hardening was reported drifted")
	}
	for name, mutate := range map[string]func(*containerInspect){
		"uid":               func(c *containerInspect) { c.Config.User = "1002:1001" },
		"gid":               func(c *containerInspect) { c.Config.User = "1000:1002" },
		"read-only":         func(c *containerInspect) { c.HostConfig.ReadonlyRootfs = false },
		"cap-drop":          func(c *containerInspect) { c.HostConfig.CapDrop = []string{"NET_RAW"} },
		"no-new-privileges": func(c *containerInspect) { c.HostConfig.SecurityOpt = nil },
		"memory":            func(c *containerInspect) { c.HostConfig.Memory-- },
		"cpu":               func(c *containerInspect) { c.HostConfig.NanoCpus-- },
		"pids":              func(c *containerInspect) { c.HostConfig.PidsLimit-- },
		"shm":               func(c *containerInspect) { c.HostConfig.ShmSize-- },
	} {
		ci := base()
		mutate(ci)
		if managedHardeningMatches(ci, op, "1000:1001", 4<<30, 2_000_000_000, 512, 1<<30) {
			t.Errorf("%s drift was reported matching", name)
		}
	}
}

func TestEnsureContainerReplacesForgedCurrentHardening(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	op := &wsv1.EnsureContainer{Name: "app", Image: testDigestImage, Labels: map[string]string{SpecHashLabel: "current"}, Resources: &wsv1.ContainerResources{Memory: "4g"}}
	ensure := func() *wsv1.EnsureContainerResult {
		res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: op}})
		if res.GetError() != nil {
			t.Fatalf("ensure: %v", res.GetError())
		}
		return res.GetEnsureContainer()
	}
	if !ensure().GetCreated() {
		t.Fatal("initial ensure did not create")
	}
	fake.mu.Lock()
	fake.containers["app"].host.Memory = 2 << 30
	if fake.containers["app"].labels[SpecHashLabel] != "current" {
		t.Fatal("test did not preserve the current spec hash")
	}
	fake.mu.Unlock()
	if !ensure().GetCreated() {
		t.Fatal("hardening drift with a current label was not replaced")
	}
	fake.mu.Lock()
	got := fake.containers["app"].host.Memory
	fake.mu.Unlock()
	if got != 4<<30 {
		t.Fatalf("replacement memory = %d, want %d", got, int64(4)<<30)
	}
}
