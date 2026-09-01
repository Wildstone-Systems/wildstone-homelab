package reconcile

// ADR-W005 A3/S3a coverage: OidcClient's batched-Authelia-reload behavior
// (the "trap" the ADR flags) and LdapGroup's declared/best-effort applier,
// exercised against the same real-gateway + minimal-protocol-test-agent
// harness the rest of this package's integration tests use.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func oidcClientResource(name, clientID, secretRef string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindOidcClient,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/identity", Node: "node-a"},
		OidcClient: &resource.OidcClientSpec{
			ClientId:     clientID,
			ClientName:   titleCase(clientID),
			RedirectUris: []string{"https://" + clientID + ".wildstone.dev/auth/login"},
			Scopes:       []string{"openid", "profile", "email", "groups"},
			AuthPolicy:   "two_factor",
			SecretRef:    "secret/" + clientID + "-oidc",
		},
	}
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func ldapGroupResource(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindLdapGroup,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/identity", Node: "node-a"},
		LdapGroup:  &resource.LdapGroupSpec{Name: name},
	}
}

// reloadCounter is a concurrency-safe call counter for ReloadAutheliaFn.
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

// TestReconcileTwoOidcClientsInOnePlanReloadsAutheliaExactlyOnce is the
// direct test of the ADR-W005 B3 batching trap: two OidcClient Create
// steps in a single plan must write two fragments but invoke
// ReloadAutheliaFn exactly once, never twice.
func TestReconcileTwoOidcClientsInOnePlanReloadsAutheliaExactlyOnce(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	rc := &reloadCounter{}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.ReloadAutheliaFn = rc.fn

	desired := []resource.Resource{
		oidcClientResource("immich", "immich", "secret/immich-oidc"),
		oidcClientResource("jellyfin", "jellyfin", "secret/jellyfin-oidc"),
	}
	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Applied || outcome.PendingApproval || outcome.Rolledback || outcome.Incident != "" {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if len(outcome.Plan.Steps) != 2 {
		t.Fatalf("expected 2 create steps, got %d: %+v", len(outcome.Plan.Steps), outcome.Plan.Steps)
	}

	immichFrag := agent.readOidcFragmentForTest(t, "immich")
	jellyfinFrag := agent.readOidcFragmentForTest(t, "jellyfin")
	if len(immichFrag) == 0 {
		t.Error("immich OidcClient fragment was not written")
	}
	if len(jellyfinFrag) == 0 {
		t.Error("jellyfin OidcClient fragment was not written")
	}
	wantImmich := renderOidcClientFragment(desired[0].OidcClient)
	if string(immichFrag) != string(wantImmich) {
		t.Errorf("immich fragment mismatch:\ngot:  %q\nwant: %q", immichFrag, wantImmich)
	}

	if got := rc.count(); got != 1 {
		t.Fatalf("ReloadAutheliaFn called %d times for a 2-OidcClient plan, want exactly 1", got)
	}
}

// TestReconcileOidcClientFragmentMatchesAutheliaShape checks the rendered
// fragment's structure against the shape documented in
// testing/fixtures/immich/authelia-oidc-client.yml: an
// identity_providers.oidc.clients entry with client_id, client_name,
// client_secret (a placeholder, never the real secret), public: false,
// authorization_policy, redirect_uris, and scopes.
func TestReconcileOidcClientFragmentMatchesAutheliaShape(t *testing.T) {
	spec := &resource.OidcClientSpec{
		ClientId:     "immich",
		ClientName:   "Immich",
		RedirectUris: []string{"https://photos.example.com/auth/login", "app.immich:///oauth-callback"},
		Scopes:       []string{"openid", "profile", "email", "groups"},
		AuthPolicy:   "two_factor",
		SecretRef:    "secret/immich-oidc",
	}
	got := string(renderOidcClientFragment(spec))

	wantLines := []string{
		"identity_providers:",
		"  oidc:",
		"    clients:",
		"      - client_id: immich",
		"        client_name: Immich",
		"        public: false",
		"        authorization_policy: two_factor",
		"        redirect_uris:",
		"          - https://photos.example.com/auth/login",
		"          - app.immich:///oauth-callback",
		"        scopes:",
		"          - openid",
		"          - profile",
		"          - email",
		"          - groups",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("rendered fragment missing expected line %q; full fragment:\n%s", line, got)
		}
	}
	if strings.Contains(got, "secret/immich-oidc") == false {
		t.Error("rendered fragment does not carry the secretRef reference at all")
	}
	if strings.Contains(got, "hunter2") {
		t.Error("rendered fragment must never contain a literal secret value")
	}
	// client_secret must be a placeholder, not a resolved value: the
	// secretRef string itself, not a real digest, appears in its place.
	if !strings.Contains(got, "client_secret: DIGEST-PLACEHOLDER:secret/immich-oidc") {
		t.Errorf("client_secret line does not carry the expected placeholder shape; full fragment:\n%s", got)
	}
}

// TestReconcileOidcClientReconvergeIsNoOpAndDoesNotReload re-reconciles an
// identical OidcClient plan: no steps, and — just as important for the
// batching contract — ReloadAutheliaFn must NOT be called at all on a
// no-op pass (there is nothing to batch a reload for).
func TestReconcileOidcClientReconvergeIsNoOpAndDoesNotReload(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	rc := &reloadCounter{}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.ReloadAutheliaFn = rc.fn

	desired := []resource.Resource{oidcClientResource("immich", "immich", "secret/immich-oidc")}
	first, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil || !first.Applied {
		t.Fatalf("first reconcile: outcome=%+v err=%v", first, err)
	}
	if got := rc.count(); got != 1 {
		t.Fatalf("expected exactly 1 reload after the initial create, got %d", got)
	}

	second, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if !second.Applied || len(second.Plan.Steps) != 0 {
		t.Fatalf("re-reconcile of identical OidcClient desired state produced steps: %+v", second.Plan.Steps)
	}
	if got := rc.count(); got != 1 {
		t.Fatalf("ReloadAutheliaFn called again on a no-op reconcile: total calls = %d, want still 1", got)
	}
}

// TestReconcileLdapGroupDeclaredAppliesWithoutError covers LdapGroup's
// declared/best-effort applier: apply succeeds with no external call, and
// observing it afterward (via a second identical Reconcile) reports it
// present from the managed cache — a no-op, not a repeated create.
func TestReconcileLdapGroupDeclaredAppliesWithoutError(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	desired := []resource.Resource{ldapGroupResource("photos-users")}

	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Applied || outcome.PendingApproval || outcome.Rolledback || outcome.Incident != "" {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if len(outcome.Plan.Steps) != 1 || outcome.Plan.Steps[0].Kind != resource.KindLdapGroup || outcome.Plan.Steps[0].Action != plan.ActionCreate {
		t.Fatalf("expected a single LdapGroup create step, got: %+v", outcome.Plan.Steps)
	}
	if len(outcome.Steps) != 1 || !outcome.Steps[0].Applied || !outcome.Steps[0].HealthOK {
		t.Fatalf("expected the LdapGroup step to apply and trivially health-gate, got: %+v", outcome.Steps)
	}
}

// TestReconcileLdapGroupObservesAsPresentOnReconverge confirms LdapGroup's
// declared-state observe: a second Reconcile call with the same desired
// group produces zero steps (it's observed as already present from the
// managed cache), never a repeated "create".
func TestReconcileLdapGroupObservesAsPresentOnReconverge(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	desired := []resource.Resource{ldapGroupResource("photos-users")}

	if outcome, err := r.Reconcile(context.Background(), "node-a", desired); err != nil || !outcome.Applied {
		t.Fatalf("initial reconcile: outcome=%+v err=%v", outcome, err)
	}

	second, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if !second.Applied || len(second.Plan.Steps) != 0 {
		t.Fatalf("expected a no-op reconcile, got: %+v", second)
	}
}

// TestReconcileOidcClientAndLdapGroupTogetherOneNodeMixedPlan sanity-checks
// that OidcClient and LdapGroup steps coexist correctly in a single mixed
// plan alongside a Container that (per the ADR-W005 ordering choice)
// depends on the identity wiring landing first.
func TestReconcileOidcClientAndLdapGroupTogetherOneNodeMixedPlan(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	rc := &reloadCounter{}
	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	r.ReloadAutheliaFn = rc.fn

	desired := []resource.Resource{
		containerResource("immich-server", testDigestImage),
		ldapGroupResource("photos-users"),
		oidcClientResource("immich", "immich", "secret/immich-oidc"),
	}
	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Applied || outcome.Incident != "" || outcome.Rolledback {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if len(outcome.Plan.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %d: %+v", len(outcome.Plan.Steps), outcome.Plan.Steps)
	}
	// ADR-W005 S3a ordering: LdapGroup, then OidcClient, then Container.
	wantOrder := []resource.Kind{resource.KindLdapGroup, resource.KindOidcClient, resource.KindContainer}
	for i, step := range outcome.Plan.Steps {
		if step.Kind != wantOrder[i] {
			t.Errorf("step %d: got kind %s, want %s", i, step.Kind, wantOrder[i])
		}
	}
	if running, ok := agent.containerExists("immich-server"); !ok || !running {
		t.Error("container was not created+running")
	}
	if got := rc.count(); got != 1 {
		t.Fatalf("expected exactly 1 Authelia reload for the single OidcClient step, got %d", got)
	}
}
