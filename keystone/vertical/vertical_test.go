// Package vertical is Spike S3's go/no-go gate (ADR-W005 A4 / S3c): it
// loads the two hand-written packages (packages/identity,
// packages/photos-immich), expands them through the real Capability
// Engine, writes the result to a real Statefile, and reconciles it through
// a REAL gateway + faked Docker/managed-filesystem — exactly the S1/E1/
// ADR-W004 B1 test style the rest of keystone/reconcile uses (see
// harness_test.go). It then asserts the ADR-W005 A4 equivalence: expanding
// identity+photos-immich and reconciling the result reproduces Immich's
// full resource set plus an Authelia OIDC-client fragment matching the
// golden fixture's shape, with the Authelia reload batched to exactly one
// call and zero hand-editing between package expansion and applied state.
//
// TestGoNoGoIdentityPhotosVertical is the test to run for the verdict; it
// logs a single "GATE VERDICT: GO" or "GATE VERDICT: NO-GO" line.
package vertical

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// testManagedPrefix mirrors keystone/reconcile's test convention (see
// reconcile_test.go's testManagedPrefix): an arbitrary managed-file root
// under which Route/OidcClient fragments are written.
const testManagedPrefix = "/wildstone-managed"

// repoRoot resolves the repository root from this test file's own path via
// runtime.Caller, so the test finds packages/ regardless of the working
// directory `go test` is invoked from (ADR-W005 S3c: "compute the repo path
// relative to the test file").
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file: <root>/keystone/vertical/vertical_test.go
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// oidcClientFragmentPath and routeFragmentPath duplicate the path
// convention documented in keystone/reconcile/oidcclient.go and route.go
// (managedPrefix/authelia/oidc-clients/<name>.yaml and
// managedPrefix/routes/<name>.caddy). They are not exported by
// keystone/reconcile, so the gate test reconstructs them from the
// documented, stable convention rather than depending on reconcile's
// unexported internals — the actual write/read of these paths always goes
// through the real Reconciler and the real (faked-Docker) op surface,
// never anything reimplemented here.
func oidcClientFragmentPath(name string) string {
	return filepath.ToSlash(filepath.Join(testManagedPrefix, "authelia", "oidc-clients", name+".yaml"))
}

func routeFragmentPath(name string) string {
	return filepath.ToSlash(filepath.Join(testManagedPrefix, "routes", name+".caddy"))
}

// reloadCounter is a concurrency-safe call counter for ReloadAutheliaFn,
// mirroring keystone/reconcile's own test helper of the same shape.
type reloadCounter struct {
	mu    sync.Mutex
	calls int
}

func (c *reloadCounter) fn(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil
}

func (c *reloadCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// autheliaOidcFragment is the minimal parsed shape of an Authelia
// identity_providers.oidc.clients fragment, used both to parse the
// Reconciler's rendered output and the golden fixture, so the gate
// compares structured fields (ADR-W005 A4) rather than raw text.
type autheliaOidcFragment struct {
	IdentityProviders struct {
		Oidc struct {
			Clients []autheliaOidcClient `yaml:"clients"`
		} `yaml:"oidc"`
	} `yaml:"identity_providers"`
}

type autheliaOidcClient struct {
	ClientID            string   `yaml:"client_id"`
	ClientName          string   `yaml:"client_name"`
	ClientSecret        string   `yaml:"client_secret"`
	Public              bool     `yaml:"public"`
	AuthorizationPolicy string   `yaml:"authorization_policy"`
	RedirectURIs        []string `yaml:"redirect_uris"`
	Scopes              []string `yaml:"scopes"`
}

func parseOidcFragment(t *testing.T, data []byte) autheliaOidcClient {
	t.Helper()
	var frag autheliaOidcFragment
	if err := yaml.Unmarshal(data, &frag); err != nil {
		t.Fatalf("parsing OIDC fragment: %v\nraw:\n%s", err, data)
	}
	if len(frag.IdentityProviders.Oidc.Clients) != 1 {
		t.Fatalf("expected exactly 1 client in fragment, got %d: %+v", len(frag.IdentityProviders.Oidc.Clients), frag)
	}
	return frag.IdentityProviders.Oidc.Clients[0]
}

func containsAll(haveSet []string, want ...string) bool {
	have := map[string]bool{}
	for _, s := range haveSet {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// resKey identifies a resource for the expansion<->statefile equivalence
// check, matching the (kind,name) identity every other part of this
// codebase uses.
type resKey struct {
	Kind resource.Kind
	Name string
}

func byKey(resources []resource.Resource) map[resKey]resource.Resource {
	m := make(map[resKey]resource.Resource, len(resources))
	for _, r := range resources {
		m[resKey{r.Kind, r.Metadata.Name}] = r
	}
	return m
}

// TestGoNoGoIdentityPhotosVertical is the ADR-W005 A4 gate: see the package
// doc. Read the log output for the GATE VERDICT line.
func TestGoNoGoIdentityPhotosVertical(t *testing.T) {
	root := repoRoot(t)

	// ---------------------------------------------------------------- a) load
	identityPkg, err := pkgspec.Load(filepath.Join(root, "packages", "identity", "package.yaml"))
	if err != nil {
		t.Fatalf("GATE VERDICT: NO-GO — loading packages/identity/package.yaml: %v", err)
	}
	photosPkg, err := pkgspec.Load(filepath.Join(root, "packages", "photos-immich", "package.yaml"))
	if err != nil {
		t.Fatalf("GATE VERDICT: NO-GO — loading packages/photos-immich/package.yaml: %v", err)
	}

	// ---------------------------------------------------------------- b) catalog + expand
	//
	// photos-immich's `requires` list (ADR-W005 A1's own package example)
	// includes ingress:public and storage:bulk alongside identity:oidc.
	// ingress:public and storage are structural traits the Engine
	// satisfies natively since E5 (ADR-W010 A1) — the S3c synthetic
	// "system-capabilities" scaffold package this test previously carried
	// is gone.
	catalog := capability.NewCatalog(identityPkg, photosPkg)
	engine := capability.NewEngine()

	identityResources, err := engine.Expand(identityPkg, map[string]string{
		"public_domain": "example.com",
		"node":          "node-a",
	}, catalog)
	if err != nil {
		t.Fatalf("GATE VERDICT: NO-GO — expanding packages/identity: %v", err)
	}
	photosResources, err := engine.Expand(photosPkg, map[string]string{
		"public_domain": "example.com",
		"node":          "node-a",
	}, catalog)
	if err != nil {
		t.Fatalf("GATE VERDICT: NO-GO — expanding packages/photos-immich: %v", err)
	}

	expanded := append(append([]resource.Resource{}, identityResources...), photosResources...)
	t.Logf("expanded %d resources from identity + %d resources from photos-immich", len(identityResources), len(photosResources))

	// ---------------------------------------------------------------- c) statefile
	store, err := statefile.New(t.TempDir())
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	for _, r := range expanded {
		if err := store.Put(r); err != nil {
			t.Fatalf("GATE VERDICT: NO-GO — statefile.Put(%s/%s): %v", r.Kind, r.Metadata.Name, err)
		}
	}
	if err := store.Commit("vertical-gate-test", "expand identity+photos-immich", "S3c go/no-go gate"); err != nil {
		t.Fatalf("statefile.Commit: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("statefile.Load: %v", err)
	}

	// No-hand-editing check (ADR-W005 A4): the resources round-tripped
	// through the Statefile must be exactly Expand's output, unmodified —
	// same (kind,name) set, same specs, field for field.
	expandedByKey := byKey(expanded)
	loadedByKey := byKey(loaded)
	if len(expandedByKey) != len(loadedByKey) {
		t.Fatalf("GATE VERDICT: NO-GO — statefile round-trip changed resource count: expanded %d, loaded %d", len(expandedByKey), len(loadedByKey))
	}
	for k, want := range expandedByKey {
		got, ok := loadedByKey[k]
		if !ok {
			t.Fatalf("GATE VERDICT: NO-GO — resource %s/%s present in Expand output but missing from Statefile", k.Kind, k.Name)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("GATE VERDICT: NO-GO — resource %s/%s differs between Expand output and Statefile round-trip (hand-editing or lossy serialization):\nexpanded: %+v\nloaded:   %+v", k.Kind, k.Name, want, got)
		}
	}
	t.Logf("statefile round-trip: %d/%d resources byte-for-byte identical to Expand's output — no hand-editing occurred", len(loaded), len(expanded))

	// ---------------------------------------------------------------- d) reconcile: real gateway + fake docker + managed FS
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	rc := &reloadCounter{}
	r := &reconcile.Reconciler{
		Applier:            &reconcile.GatewayApplier{Gateway: h.gw},
		ManagedPrefix:      testManagedPrefix,
		Approve:            approveEverything, // identity+photos-immich is a fresh, all-Create plan (Low); this policy also covers a Medium/High plan if diff-grading ever changes.
		ReloadAutheliaFn:   rc.fn,
		MaxApplyRetries:    2,
		RetryBackoff:       5 * time.Millisecond,
		HealthGateAttempts: 5,
		HealthGateBackoff:  10 * time.Millisecond,
	}

	// The desired set reconciled here is `loaded` — exactly what the
	// Statefile round-trip produced, i.e. exactly what Expand produced
	// (already asserted above) — never a hand-edited copy.
	outcome, err := r.Reconcile(context.Background(), "node-a", loaded)
	if err != nil {
		t.Fatalf("GATE VERDICT: NO-GO — reconcile: %v", err)
	}
	if outcome.PendingApproval {
		t.Fatalf("GATE VERDICT: NO-GO — plan required approval and none was granted: %+v", outcome.Plan)
	}
	if outcome.Rolledback {
		t.Fatalf("GATE VERDICT: NO-GO — a step's health gate failed and was rolled back: %+v", outcome.Steps)
	}
	if outcome.Incident != "" {
		t.Fatalf("GATE VERDICT: NO-GO — reconcile incident: %s", outcome.Incident)
	}
	if !outcome.Applied {
		t.Fatalf("GATE VERDICT: NO-GO — outcome not Applied: %+v", outcome)
	}
	t.Logf("reconcile plan: %s (overall risk %s), %d steps applied", outcome.Plan.Summary, outcome.Plan.OverallRisk, len(outcome.Steps))

	// ---------------------------------------------------------------- e) assert the gate
	wantContainers := []string{"immich-server", "immich-machine-learning", "immich-postgres", "immich-redis", "lldap", "authelia", "authelia-redis"}
	for _, name := range wantContainers {
		running, ok := agent.containerExists(name)
		if !ok || !running {
			t.Errorf("GATE VERDICT: NO-GO — container %q not created+running in the fake (ok=%v running=%v)", name, ok, running)
		}
	}

	wantVolumes := []string{"immich-upload", "immich-pgdata", "immich-redis-data", "lldap-data", "authelia-config", "authelia-redis-data"}
	for _, name := range wantVolumes {
		if !agent.volumeExists(name) {
			t.Errorf("GATE VERDICT: NO-GO — volume %q not created in the fake", name)
		}
	}

	photosRouteFrag, ok := agent.readManagedFileForTest(t, routeFragmentPath("photos"))
	if !ok || len(photosRouteFrag) == 0 {
		t.Error("GATE VERDICT: NO-GO — public photos Route fragment was not written")
	}
	authRouteFrag, ok := agent.readManagedFileForTest(t, routeFragmentPath("auth"))
	if !ok || len(authRouteFrag) == 0 {
		t.Error("GATE VERDICT: NO-GO — auth Route fragment was not written")
	}

	oidcFragData, ok := agent.readManagedFileForTest(t, oidcClientFragmentPath("immich"))
	if !ok || len(oidcFragData) == 0 {
		t.Fatal("GATE VERDICT: NO-GO — Authelia OIDC-client fragment for 'immich' was not written")
	}
	got := parseOidcFragment(t, oidcFragData)

	goldenPath := filepath.Join(root, "testing", "fixtures", "immich", "authelia-oidc-client.yml")
	goldenData, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	golden := parseOidcFragment(t, goldenData)

	if got.ClientID != "immich" {
		t.Errorf("GATE VERDICT: NO-GO — client_id = %q, want %q (golden: %q)", got.ClientID, "immich", golden.ClientID)
	}
	if got.AuthorizationPolicy != golden.AuthorizationPolicy {
		t.Errorf("GATE VERDICT: NO-GO — authorization_policy = %q, want %q (matching golden fixture)", got.AuthorizationPolicy, golden.AuthorizationPolicy)
	}
	if got.Public != golden.Public {
		t.Errorf("GATE VERDICT: NO-GO — public = %v, want %v (matching golden fixture)", got.Public, golden.Public)
	}
	if !containsAll(got.RedirectURIs, "https://photos.example.com/auth/login") {
		t.Errorf("GATE VERDICT: NO-GO — redirect_uris %v does not include the photos login URL", got.RedirectURIs)
	}
	if !containsAll(got.RedirectURIs, "app.immich:///oauth-callback") {
		t.Errorf("GATE VERDICT: NO-GO — redirect_uris %v does not include the Immich app callback scheme", got.RedirectURIs)
	}
	if !containsAll(got.Scopes, "openid", "profile", "email", "groups") {
		t.Errorf("GATE VERDICT: NO-GO — scopes %v do not match golden fixture's openid/profile/email/groups", got.Scopes)
	}
	if got.ClientSecret == "" || got.ClientSecret == golden.ClientSecret {
		t.Errorf("GATE VERDICT: NO-GO — client_secret %q is empty or literally matches the golden placeholder text (expected a distinct reference-derived placeholder)", got.ClientSecret)
	}
	// The rendered client_secret must never be a real/plausible secret
	// value — only a placeholder that embeds the secret *reference*
	// (ADR-W005 A2/A3).
	if got.ClientSecret != "DIGEST-PLACEHOLDER:secret/photos-oidc" {
		t.Errorf("GATE VERDICT: NO-GO — client_secret = %q, want a placeholder carrying the secret/photos-oidc reference, never a literal", got.ClientSecret)
	}

	if n := rc.count(); n != 1 {
		t.Errorf("GATE VERDICT: NO-GO — Authelia reloaded %d times for the whole batch, want exactly 1", n)
	} else {
		t.Logf("Authelia reload seam invoked exactly once for the whole batch (%d resources reconciled, 1 OidcClient step)", len(loaded))
	}

	if t.Failed() {
		t.Log("GATE VERDICT: NO-GO — see failures above")
		return
	}
	t.Log("GATE VERDICT: GO — identity + photos-immich expand, statefile-round-trip, and reconcile to the exact ADR-W005 A4 resource/fragment shape with zero hand-editing and exactly one batched Authelia reload")
}

// approveEverything is the test's approval policy: identity+photos-immich
// expanded fresh (no prior observed state) is entirely Create steps, which
// plan.Diff always grades Low (ADR-W004 A3 risk heuristic), so this policy
// is not actually exercised on the happy path — it exists so an Update/
// Delete step (Medium/High) occurring on a re-run would still apply rather
// than block on PendingApproval, matching a human operator who has already
// reviewed and approved this exact package-driven change.
func approveEverything(*plan.Plan) bool { return true }
