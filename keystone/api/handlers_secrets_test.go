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
	// written records managed-file paths, so a test can assert that a
	// secret held inside a FILE is rewritten and not merely re-created
	// around.
	written []string
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
	case *wsv1.Operation_WriteManagedFile:
		f.written = append(f.written, o.WriteManagedFile.GetPath())
		return &wsv1.OpResult{}, nil
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

	// Seed the secret, then rotate. Seeding is itself a `set` with a
	// value, and a set now re-creates what reads the secret for the same
	// reason a rotation does — so clear what that removed before
	// asserting on the rotation's own effect.
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/tunnel-token",
		map[string]string{"value": "KNOWN-FAKE-v1"}, nil)
	if len(fake.removed) != 1 || fake.removed[0] != "cloudflared" {
		t.Fatalf("setting a value must re-create what reads it, removed = %v", fake.removed)
	}
	fake.removed = nil
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

// Setting a value must reach the thing that reads it.
//
// It did not, once. `wild secret set` wrote the value to the node and
// reported success while every container holding the previous one went
// on holding it — a container's environment is fixed when it is created,
// which this file's own header says about rotation and which set never
// acted on. An operator set the Davis admin password, was told it
// worked, and still could not sign in to the page it protects.
func TestSecretSetRecreatesWhatReadsIt(t *testing.T) {
	h := newHarness(t)
	fake := &secretsFakeAgent{}
	h.s.Agents = fake
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: "davis", Owner: "capability/mail", Node: "n1"},
		Container: &resource.ContainerSpec{Image: "ghcr.io/x/y@" + testDigest,
			Env: map[string]string{"ADMIN_PASSWORD": "secret/davis-admin-password"}},
	}); err != nil {
		t.Fatal(err)
	}
	// A container on the same node that does NOT read it stays running.
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: "roundcube", Owner: "capability/mail", Node: "n1"},
		Container:  &resource.ContainerSpec{Image: "ghcr.io/x/y@" + testDigest, Env: map[string]string{"TZ": "UTC"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "seed", "seed"); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Recreated int `json:"recreated"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/davis-admin-password",
		map[string]string{"value": "KNOWN-FAKE-CHOSEN"}, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("set = %d", resp.StatusCode)
	}
	if len(fake.removed) != 1 || fake.removed[0] != "davis" {
		t.Fatalf("removed = %v, want exactly [davis]", fake.removed)
	}
	if out.Recreated != 1 {
		t.Errorf("recreated = %d, want 1 — the caller should be told what was restarted", out.Recreated)
	}
}

// Generating a secret that already exists is the install-time "make sure
// this is here" call. It changes no value, so it must not restart a
// service that is running fine.
func TestSecretGenerateOnAnExistingSecretRestartsNothing(t *testing.T) {
	h := newHarness(t)
	fake := &secretsFakeAgent{}
	h.s.Agents = fake
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: "davis", Owner: "capability/mail", Node: "n1"},
		Container: &resource.ContainerSpec{Image: "ghcr.io/x/y@" + testDigest,
			Env: map[string]string{"ADMIN_PASSWORD": "secret/davis-admin-password"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "seed", "seed"); err != nil {
		t.Fatal(err)
	}
	// First generate creates it (and may re-create: nothing held a value).
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/davis-admin-password", nil, nil)
	fake.removed = nil
	// Second generate is a no-op on the value.
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/davis-admin-password", nil, nil)
	if len(fake.removed) != 0 {
		t.Fatalf("a no-op generate restarted %v", fake.removed)
	}
}

// Rotation rewrites the files a secret is written into, not only the
// containers whose env names it. Reading only the containers left
// rotation with the bug `set` had just been fixed for: the file keeps
// the old value and the re-created container starts against it, while
// the endpoint reports success.
func TestSecretRotateRewritesSecretBackedFiles(t *testing.T) {
	h := newHarness(t)
	fake := &secretsFakeAgent{}
	h.s.Agents = fake
	h.s.ManagedPrefix = "/etc/wildstone"
	put := func(r resource.Resource) {
		t.Helper()
		if err := h.s.State.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: "immich-config", Owner: "capability/photos", Node: "n1"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "immich.json", Content: `{"s":"secret/photos-oidc"}`},
	})
	put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindContainer,
		Metadata: resource.Metadata{Name: "immich-server", Owner: "capability/photos", Node: "n1"},
		Container: &resource.ContainerSpec{Image: "ghcr.io/x/y@" + testDigest,
			Mounts: []resource.Mount{{Source: "/etc/wildstone/photos", Target: "/etc/wildstone/photos"}}},
	})
	if err := h.s.State.Commit("test", "seed", "seed"); err != nil {
		t.Fatal(err)
	}
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/photos-oidc",
		map[string]string{"value": "KNOWN-FAKE-v1"}, nil)
	fake.written = nil
	fake.removed = nil

	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/nodes/n1/secrets/photos-oidc/rotate", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d", resp.StatusCode)
	}
	if len(fake.written) != 1 || fake.written[0] != "/etc/wildstone/photos/immich.json" {
		t.Fatalf("rotation must rewrite the file holding the secret, wrote %v", fake.written)
	}
	if len(fake.removed) != 1 || fake.removed[0] != "immich-server" {
		t.Fatalf("removed = %v, want the container that mounts it", fake.removed)
	}
}
