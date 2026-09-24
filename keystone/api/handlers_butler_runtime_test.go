package api

// Phase 1's evidence at the API (ADR-W039 §3.3, §3.7, §4.1): a new
// Agent is on the native runtime with a minted, bound credential and no
// token; the record reads back in the contract's shape; retiring
// revokes and restoring brings a native runtime back; an Agent that
// predates the record is credentialed at startup.

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func createAgent(t *testing.T, h *harness, name string) {
	t.Helper()
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers", map[string]any{
		"name": name, "agentClass": "member", "model": "kitchen-box",
	}, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("creating %s = %d", name, res.StatusCode)
	}
}

func readRuntime(t *testing.T, h *harness, name string) agentapi.RuntimeRecord {
	t.Helper()
	var rec agentapi.RuntimeRecord
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers/"+url.PathEscape(name)+"/runtime", nil, &rec); res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/runtime = %d", name, res.StatusCode)
	}
	return rec
}

func TestANewAgentRunsNativeUnderAMintedRuntimePrincipal(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")

	rec := readRuntime(t, h, "health")
	if rec.Type != agentapi.RuntimeNative {
		t.Errorf("runtime type = %q, want native", rec.Type)
	}
	if rec.Health != agentapi.HealthConnected {
		t.Errorf("health = %q, want connected — the native runtime is in-process", rec.Health)
	}
	if rec.CredentialRef != "runtime-health-1" {
		t.Errorf("credentialRef = %q, want runtime-health-1", rec.CredentialRef)
	}
	if rec.Endpoint != "" {
		t.Errorf("a native runtime has an endpoint %q", rec.Endpoint)
	}
	if rec.Negotiated.Delivery != agentapi.DeliveryPush || rec.ExecutionProfileRef != "platform-only" {
		t.Errorf("negotiated defaults = %+v, profile %q", rec.Negotiated, rec.ExecutionProfileRef)
	}

	// The credential is a real principal: butler role, acting for
	// nobody, bound to this Agent, and enabled.
	p, err := h.s.DB.GetPrincipal("runtime-health-1")
	if err != nil {
		t.Fatalf("the runtime principal was not minted: %v", err)
	}
	if p.Role != runtimedb.RoleButler || p.ActingFor != "" || p.Disabled {
		t.Errorf("runtime principal = %+v, want an enabled butler-role principal acting for nobody", p)
	}
	b, _ := h.s.DB.GetButler("health")
	if owner, ok, _ := h.s.DB.RuntimePrincipalButler("runtime-health-1"); !ok || owner != b.Seq {
		t.Errorf("runtime-health-1 resolves to %d (ok=%v), want %s (%d)", owner, ok, b.Name, b.Seq)
	}
	// And no token was issued for it: nothing presents it yet, and a
	// bearer nobody holds is only a secret waiting to leak.
	if n, _ := h.s.DB.LiveTokensFor("runtime-health-1"); n != 0 {
		t.Errorf("%d live token(s) were issued for the runtime principal; this slice issues none", n)
	}
}

func TestReadingARuntimeNeedsAnAgentAndACaller(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers/nobody/runtime", nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown agent's runtime = %d, want 404", res.StatusCode)
	}
	if res := h.do("", http.MethodGet, "/api/v1/butlers/health/runtime", nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", res.StatusCode)
	}
	// Reading is a read: a chat adapter, which holds converse only,
	// cannot see it.
	if res := h.do(runtimedb.RoleConcierge, http.MethodGet, "/api/v1/butlers/health/runtime", nil, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("concierge = %d, want 403", res.StatusCode)
	}
}

// Retiring revokes (§3.7): the credential is disabled and the runtime
// says so. Restoring a native runtime re-enables the same credential —
// the binding is for life, so there is nothing to re-mint.
func TestRetiringRevokesTheRuntimeAndRestoringBringsItBack(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	if res := h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/butlers/health", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("retire = %d", res.StatusCode)
	}
	if rec := readRuntime(t, h, "health"); rec.Health != agentapi.HealthRevoked || rec.CredentialRef != "runtime-health-1" {
		t.Errorf("after retiring: %+v, want revoked with the credential still named", rec)
	}
	if p, _ := h.s.DB.GetPrincipal("runtime-health-1"); !p.Disabled {
		t.Error("retiring left the runtime principal enabled; every call must fail closed from the next request")
	}
	if _, err := h.s.Principals.Mint("runtime-health-1", "", 0); err == nil {
		t.Error("a token was minted for a revoked runtime principal")
	}

	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/health/restore", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d", res.StatusCode)
	}
	if rec := readRuntime(t, h, "health"); rec.Health != agentapi.HealthConnected || rec.CredentialRef != "runtime-health-1" {
		t.Errorf("after restoring: %+v, want connected under the same credential", rec)
	}
	if p, _ := h.s.DB.GetPrincipal("runtime-health-1"); p.Disabled {
		t.Error("restoring left the runtime principal disabled")
	}
	b, _ := h.s.DB.GetButler("health")
	if all, _ := h.s.DB.ListRuntimePrincipals(b.Seq); len(all) != 1 {
		t.Errorf("restore minted a second credential: %v", all)
	}
}

// A copy is its own Agent with its own credential; a name that is not
// a valid principal name is slugged into one.
func TestADuplicateGetsItsOwnRuntimePrincipal(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "Kitchen Helper")
	if rec := readRuntime(t, h, "Kitchen Helper"); rec.CredentialRef != "runtime-kitchen-helper-1" {
		t.Errorf("credentialRef = %q, want runtime-kitchen-helper-1", rec.CredentialRef)
	}
	var copied map[string]any
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/"+url.PathEscape("Kitchen Helper")+"/duplicate",
		map[string]any{}, &copied); res.StatusCode != http.StatusCreated {
		t.Fatalf("duplicate = %d", res.StatusCode)
	}
	rec := readRuntime(t, h, "Kitchen Helper copy")
	if rec.Type != agentapi.RuntimeNative || rec.Health != agentapi.HealthConnected {
		t.Errorf("the copy's runtime = %+v", rec)
	}
	if rec.CredentialRef != "runtime-kitchen-helper-copy-1" {
		t.Errorf("the copy's credentialRef = %q, want runtime-kitchen-helper-copy-1", rec.CredentialRef)
	}
	original, _ := h.s.DB.GetButler("Kitchen Helper")
	if owner, _, _ := h.s.DB.RuntimePrincipalButler("runtime-kitchen-helper-1"); owner != original.Seq {
		t.Error("duplicating moved the original's credential")
	}
}

// Two names that slug alike get distinct credentials, and the second
// folds its seq in rather than failing to be credentialed at all.
func TestSlugCollisionsStillCredentialBothAgents(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	createAgent(t, h, "Health")
	first := readRuntime(t, h, "health")
	second := readRuntime(t, h, "Health")
	if first.CredentialRef != "runtime-health-1" {
		t.Errorf("first = %q", first.CredentialRef)
	}
	if second.CredentialRef == "" || second.CredentialRef == first.CredentialRef {
		t.Errorf("second = %q, want a distinct credential", second.CredentialRef)
	}
	b, _ := h.s.DB.GetButler("Health")
	if owner, ok, _ := h.s.DB.RuntimePrincipalButler(second.CredentialRef); !ok || owner != b.Seq {
		t.Errorf("%s resolves to %d (ok=%v), want Health (%d)", second.CredentialRef, owner, ok, b.Seq)
	}
}

// An Agent that predates 0081 has a native record and no credential
// until the process starts; the backfill mints one and is idempotent.
func TestStartupCredentialsAgentsThatPredateTheRecord(t *testing.T) {
	h := butlerConsole(t)
	// Made below the API, the way an Agent from before 0081 reaches
	// the backfill: a record with no credential, and a retired one.
	for _, name := range []string{"alfred", "hobson"} {
		if _, err := h.s.DB.CreateButler(runtimedb.Butler{
			Name: name, Surface: runtimedb.ButlerSurfaceFamily,
			AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box",
		}, "mika"); err != nil {
			t.Fatal(err)
		}
		b, _ := h.s.DB.GetButler(name)
		if err := h.s.DB.SetButlerRuntime(b.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative}, "migration"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.DB.RetireButler("hobson", "mika"); err != nil {
		t.Fatal(err)
	}
	hobson, _ := h.s.DB.GetButler("hobson")
	if err := h.s.DB.SetRuntimeHealth(hobson.Seq, agentapi.HealthRevoked); err != nil {
		t.Fatal(err)
	}
	if rec := readRuntime(t, h, "alfred"); rec.CredentialRef != "" {
		t.Fatalf("alfred is credentialed before the backfill: %q", rec.CredentialRef)
	}

	if err := h.s.BackfillRuntimePrincipals(); err != nil {
		t.Fatal(err)
	}
	if rec := readRuntime(t, h, "alfred"); rec.CredentialRef != "runtime-alfred-1" || rec.Health != agentapi.HealthConnected {
		t.Errorf("after the backfill alfred = %+v", rec)
	}
	if rec := readRuntime(t, h, "hobson"); rec.CredentialRef != "" || rec.Health != agentapi.HealthRevoked {
		t.Errorf("the backfill credentialed a retired agent: %+v", rec)
	}
	if err := h.s.BackfillRuntimePrincipals(); err != nil {
		t.Fatal(err)
	}
	alfred, _ := h.s.DB.GetButler("alfred")
	if all, _ := h.s.DB.ListRuntimePrincipals(alfred.Seq); len(all) != 1 {
		t.Errorf("a second backfill minted again: %v", all)
	}
}

// The credential is a runtime's and nobody else's: the names the
// platform picks stay inside the principal name discipline for any
// Butler name a person can type.
func TestRuntimePrincipalNamesFollowTheNameDiscipline(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		name string
		want string
	}{
		{"health", "runtime-health-1"},
		{"Kitchen Helper", "runtime-kitchen-helper-1"},
		{"  odd -- spacing!! ", "runtime-odd-spacing-1"},
		{"日本語", "runtime-agent7-1"},
		{"a-very-long-name-that-goes-on-and-on-and-on-past-any-sensible-label-length", "runtime-a-very-long-name-that-goes-on-and-on-1"},
	} {
		got := runtimePrincipalName(runtimedb.Butler{Seq: 7, Name: c.name}, 1, false)
		if got != c.want {
			t.Errorf("runtimePrincipalName(%q) = %q, want %q", c.name, got, c.want)
		}
		if err := h.s.Principals.Create(got, runtimedb.RoleButler, ""); err != nil {
			t.Errorf("%q is not a valid principal name: %v", got, err)
		}
	}
	if got := runtimePrincipalName(runtimedb.Butler{Seq: 7, Name: "health"}, 2, true); got != "runtime-health.7-2" {
		t.Errorf("disambiguated = %q", got)
	}
}
