package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// secretsFakeAgent emulates the minor-7 secret arms plus stop/remove, so
// the API handlers can be exercised without a gateway. It records what
// was dispatched.
type secretsFakeAgent struct {
	secrets map[string]uint32
	removed []string
	stopped []string
}

func (f *secretsFakeAgent) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if f.secrets == nil {
		f.secrets = map[string]uint32{}
	}
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_EnsureSecret:
		name := o.EnsureSecret.GetName()
		_, existed := f.secrets[name]
		if !existed || o.EnsureSecret.GetRotate() {
			f.secrets[name]++
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_EnsureSecret{EnsureSecret: &wsv1.EnsureSecretResult{
			Existed: existed, Version: f.secrets[name],
		}}}, nil
	case *wsv1.Operation_PutSecret:
		f.secrets[o.PutSecret.GetName()]++
		return &wsv1.OpResult{Result: &wsv1.OpResult_PutSecret{PutSecret: &wsv1.PutSecretResult{
			Version: f.secrets[o.PutSecret.GetName()],
		}}}, nil
	case *wsv1.Operation_ListSecrets:
		res := &wsv1.ListSecretsResult{}
		for name, v := range f.secrets {
			res.Secrets = append(res.Secrets, &wsv1.SecretInfo{Name: name, Version: v})
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ListSecrets{ListSecrets: res}}, nil
	case *wsv1.Operation_StopContainer:
		f.stopped = append(f.stopped, o.StopContainer.GetName())
		return &wsv1.OpResult{}, nil
	case *wsv1.Operation_RemoveContainer:
		f.removed = append(f.removed, o.RemoveContainer.GetName())
		return &wsv1.OpResult{Result: &wsv1.OpResult_RemoveContainer{RemoveContainer: &wsv1.RemoveContainerResult{Existed: true}}}, nil
	default:
		return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
			Code: wsv1.ErrorCode_ERROR_CODE_INVALID, Message: "unexpected op in secrets test",
		}}}, nil
	}
}

func TestSecretsEndpointsAdminOnlyAndUnwired(t *testing.T) {
	h := newHarness(t)
	// No Agents wired: even the admin gets 503.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/nodes/n1/secrets", nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d, want 503", resp.StatusCode)
	}
	h.s.Agents = &secretsFakeAgent{}
	// manage verb: operator and butler are refused.
	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler} {
		if resp := h.do(role, http.MethodPost, "/api/v1/nodes/n1/secrets/tok", nil, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("role %s = %d, want 403", role, resp.StatusCode)
		}
	}
	// Bad name (charset, not path shape — "../" shapes never even route,
	// Go's mux cleans the path first): 400 before any dispatch.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/-lead", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name = %d, want 400", resp.StatusCode)
	}
}

func TestSecretsGenerateSetList(t *testing.T) {
	h := newHarness(t)
	fake := &secretsFakeAgent{}
	h.s.Agents = fake

	// Generate (no body value).
	var gen struct {
		Version uint32 `json:"version"`
		Existed bool   `json:"existed"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/immich-oidc", map[string]string{}, &gen); resp.StatusCode != http.StatusOK {
		t.Fatalf("generate = %d", resp.StatusCode)
	}
	if gen.Version != 1 || gen.Existed {
		t.Fatalf("generate = %+v", gen)
	}

	// Set with a value.
	var set struct {
		Version uint32 `json:"version"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/tunnel-token",
		map[string]string{"value": "KNOWN-FAKE-token"}, &set); resp.StatusCode != http.StatusOK {
		t.Fatalf("set = %d", resp.StatusCode)
	}
	if set.Version != 1 {
		t.Fatalf("set = %+v", set)
	}

	var list struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/nodes/n1/secrets", nil, &list); resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	if len(list.Secrets) != 2 {
		t.Fatalf("list = %+v", list)
	}
}

func TestSecretRotateRemovesDependentsWithProvenance(t *testing.T) {
	h := newHarness(t)
	fake := &secretsFakeAgent{}
	h.s.Agents = fake
	poked := false
	h.s.OnApproved = func() { poked = true }

	// Statefile: one dependent on n1, one non-dependent on n1, one
	// dependent on ANOTHER node (must not be touched by an n1 rotation).
	put := func(name, node string, env map[string]string) {
		t.Helper()
		if err := h.s.State.Put(resource.Resource{
			ApiVersion: resource.ApiVersion,
			Kind:       resource.KindContainer,
			Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: node},
			Container:  &resource.ContainerSpec{Image: "ghcr.io/x/y@" + testDigest, Env: env},
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("cloudflared", "n1", map[string]string{"TUNNEL_TOKEN": "secret/tunnel-token"})
	put("uptime-kuma", "n1", map[string]string{"TZ": "UTC"})
	put("cloudflared-b", "n2", map[string]string{"TUNNEL_TOKEN": "secret/tunnel-token"})
	if err := h.s.State.Commit("test", "seed", "seed"); err != nil {
		t.Fatal(err)
	}

	// Seed the secret, then rotate.
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/tunnel-token",
		map[string]string{"value": "KNOWN-FAKE-v1"}, nil)
	var rot struct {
		Version           uint32   `json:"version"`
		DependentsRemoved []string `json:"dependentsRemoved"`
		Plan              string   `json:"plan"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/tunnel-token/rotate", nil, &rot); resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d", resp.StatusCode)
	}
	if rot.Version != 2 {
		t.Fatalf("rotate version = %d, want 2", rot.Version)
	}
	if len(rot.DependentsRemoved) != 1 || rot.DependentsRemoved[0] != "cloudflared" {
		t.Fatalf("dependents = %v, want exactly [cloudflared] (n2's container is not n1's rotation)", rot.DependentsRemoved)
	}
	if len(fake.removed) != 1 || fake.removed[0] != "cloudflared" {
		t.Fatalf("removed = %v", fake.removed)
	}
	if !poked {
		t.Fatal("rotation did not poke the reconcile loop")
	}

	// Provenance: an applied system/secrets plan record naming the caller.
	rec, err := h.s.DB.GetPlan(rot.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Principal != secretsPrincipal || rec.ActingFor != "root-admin" || rec.State != runtimedb.PlanApplied {
		t.Fatalf("plan record = %+v", rec)
	}
	events, err := h.s.DB.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == "secret.rotated" && ev.Name == "tunnel-token" {
			found = true
		}
	}
	if !found {
		t.Fatal("no secret.rotated event")
	}
}
