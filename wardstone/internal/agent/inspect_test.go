package agent

import (
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestInspectContainerAndSpecHashReplace pins the protocol-minor-5
// surface (ADR-W008 R2c): InspectContainer's ownership/readback
// semantics, and ensureContainer's spec-hash-label replace rule with its
// image-only N−1 fallback — over the real gRPC session + fake docker.
func TestInspectContainerAndSpecHashReplace(t *testing.T) {
	fake := newFakeDocker(t)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Absent: NOT_FOUND.
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: "ghost"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("inspect absent = %+v, want NOT_FOUND", res)
	}

	// Create with a spec-hash label, then inspect it back.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "app", Image: testDigestImage,
			Labels: map[string]string{SpecHashLabel: "hash-v1"},
		},
	}})
	if !res.GetEnsureContainer().GetCreated() {
		t.Fatalf("ensure: %+v", res)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: "app"},
	}})
	ic := res.GetInspectContainer()
	if ic == nil || !ic.GetRunning() || ic.GetImage() != testDigestImage ||
		ic.GetLabels()[SpecHashLabel] != "hash-v1" {
		t.Fatalf("inspect = %+v", res)
	}

	// Same image + same hash: no replace.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "app", Image: testDigestImage,
			Labels: map[string]string{SpecHashLabel: "hash-v1"},
		},
	}})
	if res.GetEnsureContainer().GetCreated() {
		t.Fatalf("same-hash ensure recreated: %+v", res)
	}

	// Same image, DIFFERENT hash (an env-only spec change): replace —
	// exactly what the pre-R2c image-only rule missed.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{
			Name: "app", Image: testDigestImage,
			Env:    map[string]string{"NEW": "on"},
			Labels: map[string]string{SpecHashLabel: "hash-v2"},
		},
	}})
	if !res.GetEnsureContainer().GetCreated() {
		t.Fatalf("changed-hash ensure did not replace: %+v", res)
	}

	// N−1 fallback: an op with NO hash label compares images only, so an
	// old Keystone keeps its exact previous idempotency behavior.
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{Name: "app", Image: testDigestImage},
	}})
	if res.GetEnsureContainer().GetCreated() {
		t.Fatalf("hashless same-image ensure recreated: %+v", res)
	}

	// Foreign: FORBIDDEN, ownership checked before anything is revealed.
	fake.addForeign("intruder")
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: "intruder"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("inspect foreign = %+v, want FORBIDDEN", res)
	}
}
