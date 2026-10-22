package api

// Phase 1's evidence at the API (ADR-W039 §3.3, §3.7, §4.1): a new
// Agent is on the native runtime with a minted, bound credential and no
// token; the record reads back in the contract's shape; retiring
// revokes and restoring brings a native runtime back; an Agent that
// predates the record is credentialed at startup. Then the Owner's
// three acts on it: rotating the credential mints a successor and the
// predecessor stops working; revoking stops every credential; replacing
// the runtime revokes the old credential and touches nothing else about
// the Agent.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
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
	// The copy cannot poll or send as the source: its credential is
	// its own and the source's still resolves to the source.
	if rec.CredentialRef == readRuntime(t, h, "Kitchen Helper").CredentialRef {
		t.Error("the copy shares the source's runtime credential")
	}
	dup, _ := h.s.DB.GetButler("Kitchen Helper copy")
	if owner, _, _ := h.s.DB.RuntimePrincipalButler(rec.CredentialRef); owner != dup.Seq {
		t.Error("the copy's credential does not resolve to the copy")
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

// rotate asks for a fresh runtime credential and returns the response,
// failing the test on anything but 200.
func rotate(t *testing.T, h *harness, name string, body any) runtimeCredentialResponse {
	t.Helper()
	var out runtimeCredentialResponse
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/"+url.PathEscape(name)+"/runtime/credential", body, &out); res.StatusCode != http.StatusOK {
		t.Fatalf("rotating %s's runtime credential = %d", name, res.StatusCode)
	}
	return out
}

func TestOnlyExplicitRuntimeRevokeWithdrawsSubscriptions(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	b, err := h.s.DB.GetButler("health")
	if err != nil {
		t.Fatal(err)
	}
	subID, err := h.s.DB.InsertAgentSubscription(b.Seq, "event_type", `{}`, "", []string{"get_health"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.ApproveAgentSubscription(subID, "mika"); err != nil {
		t.Fatal(err)
	}
	_ = rotate(t, h, "health", nil)
	if sub, _ := h.s.DB.GetAgentSubscription(subID); !sub.Enabled {
		t.Fatal("credential rotation disabled the Agent's subscription")
	}
	var rec agentapi.RuntimeRecord
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime", map[string]any{
		"type": "hermes", "endpoint": "https://hermes-health.internal/",
		"negotiated": map[string]any{"delivery": "pull", "processingClass": "local_only"},
	}, &rec); res.StatusCode != http.StatusOK {
		t.Fatalf("runtime type change = %d", res.StatusCode)
	}
	if sub, _ := h.s.DB.GetAgentSubscription(subID); !sub.Enabled {
		t.Fatal("runtime type change disabled the Agent's subscription")
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/health/runtime/revoke", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("runtime revoke = %d", res.StatusCode)
	}
	if sub, _ := h.s.DB.GetAgentSubscription(subID); sub.Enabled {
		t.Fatal("explicit runtime revoke left the Agent's subscription enabled")
	}
	_ = rotate(t, h, "health", nil)
	if sub, _ := h.s.DB.GetAgentSubscription(subID); sub.Enabled {
		t.Fatal("credential rotation silently re-enabled a subscription withdrawn by runtime revoke")
	}
}

// authenticates requires the route gate's exact denial after successful
// authentication. Unauthorized, rate-limited, or server-error responses do not
// prove that a runtime credential authenticated.
func authenticates(h *harness, secret string) bool {
	res := h.doToken(secret, http.MethodGet, "/api/v1/whoami", nil, nil)
	return res.StatusCode == http.StatusForbidden
}

// Rotation is minting a successor and disabling the predecessor (§4.1):
// the new credential is `runtime-<agent>-<n+1>`, is current, and its
// secret authenticates; the old one is disabled, its tokens revoked, and
// a bearer it issued is refused from the next request.
func TestRotatingTheRuntimeCredentialMintsASuccessorAndDisablesThePredecessor(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")

	first := rotate(t, h, "health", map[string]any{})
	if first.CredentialRef != "runtime-health-2" {
		t.Fatalf("credentialRef = %q, want runtime-health-2", first.CredentialRef)
	}
	if !strings.HasPrefix(first.Secret, "wst_") {
		t.Fatalf("secret %q not returned intact (redaction must not eat the one legitimate secret reply)", first.Secret)
	}
	if first.Node != "" || first.Plan != "" {
		t.Errorf("a secret returned in the response also claims a node placement: %+v", first)
	}
	if !authenticates(h, first.Secret) {
		t.Error("the freshly minted runtime secret does not authenticate")
	}
	if p, _ := h.s.DB.GetPrincipal("runtime-health-1"); !p.Disabled {
		t.Error("rotating left the predecessor enabled")
	}
	if p, err := h.s.DB.GetPrincipal("runtime-health-2"); err != nil || p.Disabled || p.Role != runtimedb.RoleButler || p.ActingFor != "" {
		t.Errorf("successor = %+v %v, want an enabled butler-role principal acting for nobody", p, err)
	}
	rec := readRuntime(t, h, "health")
	if rec.CredentialRef != "runtime-health-2" || rec.Health != agentapi.HealthConnected || rec.Type != agentapi.RuntimeNative {
		t.Errorf("after rotating: %+v, want runtime-health-2 current and native still connected", rec)
	}
	b, _ := h.s.DB.GetButler("health")
	if owner, ok, _ := h.s.DB.RuntimePrincipalButler("runtime-health-2"); !ok || owner != b.Seq {
		t.Errorf("runtime-health-2 resolves to %d (ok=%v), want health (%d)", owner, ok, b.Seq)
	}

	// Rotating again: the bearer from the last rotation is refused from
	// the next request, with no cooperation from whoever holds it.
	second := rotate(t, h, "health", nil)
	if second.CredentialRef != "runtime-health-3" {
		t.Errorf("second rotation = %q, want runtime-health-3", second.CredentialRef)
	}
	if authenticates(h, first.Secret) {
		t.Error("a rotated-out runtime secret still authenticates")
	}
	if n, _ := h.s.DB.LiveTokensFor("runtime-health-2"); n != 0 {
		t.Errorf("%d live token(s) left on the rotated-out credential", n)
	}
	if !authenticates(h, second.Secret) {
		t.Error("the newest runtime secret does not authenticate")
	}
	if all, _ := h.s.DB.ListRuntimePrincipals(b.Seq); len(all) != 3 {
		t.Errorf("credentials held = %v, want the three ever minted, oldest first", all)
	}
}

// Revocation (§3.7) is immediate and total: every credential the runtime
// ever held is disabled and its bearers revoked, nothing can be minted
// for them, the runtime reads revoked — and the Agent is otherwise as it
// was. A rotation afterwards is how it comes back.
func TestRevokingTheRuntimeDisablesEveryCredentialAndRefusesToMint(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	if err := h.s.DB.GrantButlerAccess("health", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	issued := rotate(t, h, "health", nil)

	var rec agentapi.RuntimeRecord
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/health/runtime/revoke", nil, &rec); res.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", res.StatusCode)
	}
	if rec.Health != agentapi.HealthRevoked || rec.CredentialRef != "runtime-health-2" {
		t.Errorf("after revoking: %+v, want revoked with the credential still named", rec)
	}
	if authenticates(h, issued.Secret) {
		t.Error("a revoked runtime's secret still authenticates")
	}
	for _, name := range []string{"runtime-health-1", "runtime-health-2"} {
		if p, _ := h.s.DB.GetPrincipal(name); !p.Disabled {
			t.Errorf("%s is still enabled after revocation", name)
		}
		if n, _ := h.s.DB.LiveTokensFor(name); n != 0 {
			t.Errorf("%s still has %d live token(s)", name, n)
		}
		if _, err := h.s.Principals.Mint(name, "", 0); err == nil {
			t.Errorf("a token was minted for revoked %s", name)
		}
	}
	// The Agent is untouched: still live, still shared with sam.
	b, err := h.s.DB.GetButler("health")
	if err != nil || !b.Live() {
		t.Errorf("revoking the runtime changed the Agent: %+v %v", b, err)
	}
	if people, _ := h.s.DB.ListButlerAccess("health"); len(people) != 1 || people[0] != "sam" {
		t.Errorf("revoking the runtime changed who may talk to health: %v", people)
	}

	// Coming back is a rotation: the native runner is in-process, so a
	// fresh credential is all the re-registration it needs.
	again := rotate(t, h, "health", nil)
	if again.CredentialRef != "runtime-health-3" || !authenticates(h, again.Secret) {
		t.Errorf("rotation after revocation = %+v, want a working runtime-health-3", again)
	}
	if rec := readRuntime(t, h, "health"); rec.Health != agentapi.HealthConnected {
		t.Errorf("native after re-credentialing = %q, want connected", rec.Health)
	}
}

// A runtime change replaces the runtime field and nothing else (§3.7):
// moving off native revokes the old credential, stores the new
// endpoint and negotiated pair, and leaves the Agent unregistered until
// a credential is rotated for the new runtime and it authenticates.
// Moving back to native reconnects at once under the credential it
// holds. Reconfiguring the runtime it is already on revokes nothing.
func TestReplacingTheRuntimeRevokesTheOldCredentialAndKeepsTheAgent(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	if err := h.s.DB.GrantButlerAccess("health", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	before, _ := h.s.DB.GetButler("health")
	native := rotate(t, h, "health", nil)

	var rec agentapi.RuntimeRecord
	res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime", map[string]any{
		"type": "hermes", "endpoint": "https://hermes-health.internal/",
		"negotiated":          map[string]any{"delivery": "pull", "processingClass": "local_only"},
		"executionProfileRef": "sandboxed",
	}, &rec)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT native→hermes = %d", res.StatusCode)
	}
	if rec.Type != agentapi.RuntimeHermes || rec.Endpoint != "https://hermes-health.internal/" ||
		rec.Negotiated.Delivery != agentapi.DeliveryPull || rec.Negotiated.ProcessingClass != agentapi.ProcessingLocalOnly ||
		rec.ExecutionProfileRef != "sandboxed" {
		t.Errorf("hermes record = %+v", rec)
	}
	if rec.Health != agentapi.HealthUnregistered {
		t.Errorf("health after moving to hermes = %q, want unregistered until it authenticates", rec.Health)
	}
	if authenticates(h, native.Secret) {
		t.Error("the native runtime's secret still authenticates after the runtime was replaced")
	}
	for _, name := range []string{"runtime-health-1", "runtime-health-2"} {
		if p, _ := h.s.DB.GetPrincipal(name); !p.Disabled {
			t.Errorf("%s is still enabled after the runtime was replaced", name)
		}
	}
	after, _ := h.s.DB.GetButler("health")
	if after.Name != before.Name || after.Model != before.Model || after.AgentClass != before.AgentClass ||
		after.Surface != before.Surface || after.Directive != before.Directive || !after.Live() {
		t.Errorf("replacing the runtime changed the Agent:\n before %+v\n after  %+v", before, after)
	}
	if people, _ := h.s.DB.ListButlerAccess("health"); len(people) != 1 || people[0] != "sam" {
		t.Errorf("replacing the runtime changed who may talk to health: %v", people)
	}

	// The credential for the new runtime: a rotation, which leaves
	// health alone — hermes has not presented it yet.
	hermes := rotate(t, h, "health", nil)
	if hermes.CredentialRef != "runtime-health-3" || !authenticates(h, hermes.Secret) {
		t.Fatalf("hermes credential = %+v", hermes)
	}
	if rec := readRuntime(t, h, "health"); rec.Health != agentapi.HealthUnregistered {
		t.Errorf("rotating for hermes set health %q, want unregistered until it connects", rec.Health)
	}

	// Reconfiguring the runtime it is on: a new endpoint, same
	// credential, nothing revoked.
	res = h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime", map[string]any{
		"type": "hermes", "endpoint": "https://hermes-2.internal/",
		"negotiated":          map[string]any{"delivery": "pull", "processingClass": "local_only"},
		"executionProfileRef": "sandboxed",
	}, &rec)
	if res.StatusCode != http.StatusOK || rec.Endpoint != "https://hermes-2.internal/" {
		t.Fatalf("PUT hermes→hermes = %d %+v", res.StatusCode, rec)
	}
	if rec.CredentialRef != "runtime-health-3" || !authenticates(h, hermes.Secret) {
		t.Error("reconfiguring the same runtime revoked its credential")
	}

	// Back to native: the credential it holds is re-enabled — the
	// binding is for life — and the in-process runner is connected at
	// once. The hermes bearer does not come back with it.
	// Decoded into a fresh record: the JSON omits an empty endpoint,
	// and decoding into the hermes record would keep hermes's.
	var back agentapi.RuntimeRecord
	res = h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime", map[string]any{"type": "native"}, &back)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT hermes→native = %d", res.StatusCode)
	}
	// ADR-W039 §3.5 rule 3, #345: moving back to native re-derives the
	// processing class from the Agent's own assigned model (kitchen-box
	// is local) rather than leaving hermes's declared "local_only"
	// sitting there by coincidence or resetting to undeclared — a native
	// runtime is never asked to declare itself (ensureNativeRuntime).
	if back.Type != agentapi.RuntimeNative || back.Health != agentapi.HealthConnected || back.Endpoint != "" ||
		back.Negotiated.ProcessingClass != agentapi.ProcessingLocalOnly || back.CredentialRef != "runtime-health-3" {
		t.Errorf("native record = %+v, want connected under runtime-health-3 with no endpoint and processing class local_only", back)
	}
	if p, _ := h.s.DB.GetPrincipal("runtime-health-3"); p.Disabled {
		t.Error("moving back to native left the credential disabled")
	}
	if authenticates(h, hermes.Secret) {
		t.Error("the bearer issued to hermes still authenticates after the move back to native")
	}
	b, _ := h.s.DB.GetButler("health")
	if all, _ := h.s.DB.ListRuntimePrincipals(b.Seq); len(all) != 3 {
		t.Errorf("moving back to native minted again: %v", all)
	}
}

// What is refused, and that a refusal costs nothing: a native runtime
// with an endpoint, an unknown type, a caller without manage, an Agent
// that does not exist or is retired. None of them revokes anything.
func TestRuntimeChangesAreRefusedBeforeAnythingIsRevoked(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	issued := rotate(t, h, "health", nil)

	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"native with an endpoint", map[string]any{"type": "native", "endpoint": "https://x.internal/"}},
		{"an unknown type", map[string]any{"type": "skynet"}},
		{"an unknown delivery", map[string]any{"type": "hermes", "negotiated": map[string]any{"delivery": "carrier-pigeon"}}},
		{"an unknown class", map[string]any{"type": "hermes", "negotiated": map[string]any{"processingClass": "somewhere"}}},
		{"an unknown field", map[string]any{"type": "hermes", "credentialRef": "runtime-health-9"}},
	} {
		if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime", c.body, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", c.name, res.StatusCode)
		}
	}
	if !authenticates(h, issued.Secret) {
		t.Fatal("a refused runtime change revoked the credential")
	}
	if rec := readRuntime(t, h, "health"); rec.Type != agentapi.RuntimeNative || rec.Health != agentapi.HealthConnected {
		t.Errorf("a refused runtime change altered the record: %+v", rec)
	}

	// Manage is the verb, and only the Console holder has it.
	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler, runtimedb.RoleConcierge} {
		for _, rt := range []struct{ method, path string }{
			{http.MethodPut, "/api/v1/butlers/health/runtime"},
			{http.MethodPost, "/api/v1/butlers/health/runtime/credential"},
			{http.MethodPost, "/api/v1/butlers/health/runtime/revoke"},
		} {
			if res := h.do(role, rt.method, rt.path, map[string]any{"type": "hermes"}, nil); res.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s %s = %d, want 403", role, rt.method, rt.path, res.StatusCode)
			}
		}
	}
	if !authenticates(h, issued.Secret) {
		t.Error("a refused caller rotated the credential")
	}

	for _, rt := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/api/v1/butlers/nobody/runtime", map[string]any{"type": "hermes"}},
		{http.MethodPost, "/api/v1/butlers/nobody/runtime/credential", nil},
		{http.MethodPost, "/api/v1/butlers/nobody/runtime/revoke", nil},
	} {
		if res := h.do(runtimedb.RoleAdmin, rt.method, rt.path, rt.body, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", rt.method, rt.path, res.StatusCode)
		}
	}
	// A retired Agent's runtime is revoked with it; it is restored, not
	// re-credentialed in place.
	if res := h.do(runtimedb.RoleAdmin, http.MethodDelete, "/api/v1/butlers/health", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("retire = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/health/runtime/credential", nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("rotating a retired Agent's credential = %d, want 404", res.StatusCode)
	}
	if authenticates(h, issued.Secret) {
		t.Error("retiring left the runtime's bearer working")
	}
}

// With a node named, the secret is placed there as a provided secret and
// never appears in the response (§4.1: a packaged runtime reads it from
// its container's environment, as a chat adapter does). Its name is the
// Agent's, stable across rotations. A placement that fails leaves the
// Agent with the credential it had.
func TestRotatingOntoANodePlacesTheSecretInsteadOfReturningIt(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	b, _ := h.s.DB.GetButler("health")

	// Unwired agent plane: 503 before anything is minted.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/health/runtime/credential", map[string]any{"node": "n1"}, nil); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d, want 503", res.StatusCode)
	}
	if all, _ := h.s.DB.ListRuntimePrincipals(b.Seq); len(all) != 1 {
		t.Fatalf("a refused placement minted a credential: %v", all)
	}
	fake := &publisherFakeAgent{}
	h.s.Agents = fake

	placed := rotate(t, h, "health", map[string]any{"node": "n1"})
	if placed.Secret != "" {
		t.Fatal("a secret placed on a node was also returned")
	}
	if placed.CredentialRef != "runtime-health-2" || placed.Node != "n1" || placed.SecretName != "secret/runtime-health-token" || placed.Plan == "" {
		t.Errorf("placement = %+v", placed)
	}
	secret := fake.values["runtime-health-token"]
	if !strings.HasPrefix(secret, "wst_") || !authenticates(h, secret) {
		t.Errorf("the node did not receive a working bearer: %q", secret)
	}
	if p, _ := h.s.DB.GetPrincipal("runtime-health-1"); !p.Disabled {
		t.Error("placing a successor left the predecessor enabled")
	}
	if plan, err := h.s.DB.GetPlan(placed.Plan); err != nil || plan.State != runtimedb.PlanApplied {
		t.Errorf("the placement's plan = %+v %v, want applied", plan, err)
	}

	// The node refuses the write: the token never lands, the successor
	// is closed off, and the predecessor stays the working credential.
	fake.failPut = true
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butlers/health/runtime/credential", map[string]any{"node": "n1"}, nil); res.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed placement = %d, want 502", res.StatusCode)
	}
	if !authenticates(h, secret) {
		t.Error("a failed placement revoked the credential the Agent had")
	}
	if rec := readRuntime(t, h, "health"); rec.CredentialRef != "runtime-health-2" {
		t.Errorf("after a failed placement the current credential is %q, want runtime-health-2", rec.CredentialRef)
	}
	if p, _ := h.s.DB.GetPrincipal("runtime-health-3"); !p.Disabled {
		t.Error("the successor whose token never landed is still enabled")
	}
	if n, _ := h.s.DB.LiveTokensFor("runtime-health-3"); n != 0 {
		t.Errorf("%d live token(s) on a credential nothing holds", n)
	}
}

// The execution profile (#334, design doc §5) is a closed set the PUT
// validates like every other field on the runtime, and host
// administration carries its own gate: a native Agent's Tier 3 comes
// from the H7 opt-in tools, never from this profile, so it is refused
// there unless the Agent is Owner-class. Neither refusal revokes
// anything.
func TestExecutionProfileIsValidatedAndHostAdminIsGated(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health") // member-class, native

	// An unknown profile is refused like an unknown type or delivery.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
		map[string]any{"type": "native", "executionProfileRef": "root-shell"}, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown execution profile = %d, want 400", res.StatusCode)
	}
	if rec := readRuntime(t, h, "health"); rec.ExecutionProfileRef != "platform-only" {
		t.Errorf("a refused profile changed the record: %q", rec.ExecutionProfileRef)
	}

	// The closed set the migration's default and #334 both name.
	for _, p := range []string{"platform-only", "sandboxed"} {
		var rec agentapi.RuntimeRecord
		if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
			map[string]any{"type": "native", "executionProfileRef": p}, &rec); res.StatusCode != http.StatusOK {
			t.Errorf("PUT executionProfileRef=%s = %d, want 200", p, res.StatusCode)
		} else if rec.ExecutionProfileRef != p {
			t.Errorf("executionProfileRef = %q, want %s", rec.ExecutionProfileRef, p)
		}
	}

	// host-admin on a native, member-class Agent: refused, and nothing
	// about the Agent or its credential moves.
	issued := rotate(t, h, "health", nil)
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
		map[string]any{"type": "native", "executionProfileRef": "host-admin"}, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("host-admin on native+member = %d, want 400", res.StatusCode)
	}
	if rec := readRuntime(t, h, "health"); rec.ExecutionProfileRef == "host-admin" {
		t.Error("a refused host-admin change was applied anyway")
	}
	if !authenticates(h, issued.Secret) {
		t.Error("a refused execution-profile change revoked the credential")
	}

	// host-admin on native: allowed once the Agent is Owner-class.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/class",
		map[string]any{"agentClass": "owner"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("setting health owner-class = %d", res.StatusCode)
	}
	var rec agentapi.RuntimeRecord
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/health/runtime",
		map[string]any{"type": "native", "executionProfileRef": "host-admin"}, &rec); res.StatusCode != http.StatusOK {
		t.Errorf("host-admin on native+owner = %d, want 200", res.StatusCode)
	} else if rec.ExecutionProfileRef != "host-admin" {
		t.Errorf("executionProfileRef = %q, want host-admin", rec.ExecutionProfileRef)
	}

	// host-admin on a non-native runtime needs no Owner-class Agent at
	// all: the container is the same at every tier (§5.3), so the gate
	// is about who can claim Tier 3, not about the runtime type here.
	createAgent(t, h, "kitchen") // member-class, native
	if res := h.do(runtimedb.RoleAdmin, http.MethodPut, "/api/v1/butlers/kitchen/runtime",
		map[string]any{"type": "hermes", "executionProfileRef": "host-admin"}, &rec); res.StatusCode != http.StatusOK {
		t.Errorf("host-admin on hermes+member = %d, want 200", res.StatusCode)
	} else if rec.ExecutionProfileRef != "host-admin" || rec.Type != agentapi.RuntimeHermes {
		t.Errorf("kitchen's record = %+v", rec)
	}
}

// One Agent's credentials keep one stem: an Agent that was
// disambiguated at creation stays so at rotation, and the secret a
// packaged runtime reads is named after that stem.
func TestRotationKeepsAnAgentsCredentialStem(t *testing.T) {
	h := butlerConsole(t)
	createAgent(t, h, "health")
	createAgent(t, h, "Health")
	second := readRuntime(t, h, "Health")
	stem := runtimePrincipalStem(second.CredentialRef)
	if stem == "runtime-health" {
		t.Fatalf("the second Agent was not disambiguated: %q", second.CredentialRef)
	}
	rotated := rotate(t, h, "Health", nil)
	if rotated.CredentialRef != stem+"-2" {
		t.Errorf("rotated = %q, want %s-2", rotated.CredentialRef, stem)
	}
	if got := runtimeSecretName(rotated.CredentialRef); got != stem+"-token" {
		t.Errorf("secret name = %q, want %s-token", got, stem)
	}
	if got := runtimeSecretName("runtime-health-12"); got != "runtime-health-token" {
		t.Errorf("runtimeSecretName(runtime-health-12) = %q", got)
	}
}
