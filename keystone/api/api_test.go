package api

import (
	"bytes"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// harness stands up a real Server over a temp statefile repo + runtime DB
// and returns bearer tokens for one principal of each role.
type harness struct {
	t      *testing.T
	srv    *httptest.Server
	s      *Server
	tokens map[runtimedb.Role]string
}

// migrated is the schema, built once for this whole test binary. This
// package stands up a server — and so a database — 184 times, and
// replaying thirty-nine migrations into each of them is what ran the
// -race suite out of its clock (#231). Each harness still gets its own
// private file; it is now a copy rather than a re-enactment.
var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	if err != nil {
		t.Fatalf("runtimedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	state, err := statefile.New(filepath.Join(t.TempDir(), "statefile"))
	if err != nil {
		t.Fatalf("statefile.New: %v", err)
	}
	p := &principal.Store{DB: db}
	s := New(p, db, state, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	h := &harness{t: t, srv: srv, s: s, tokens: map[runtimedb.Role]string{}}
	for name, role := range map[string]runtimedb.Role{
		"root-admin": runtimedb.RoleAdmin, "op": runtimedb.RoleOperator, "butler": runtimedb.RoleButler,
		// The two chat adapters (ADR-W026 A3): converse and nothing
		// else. Their names are the CAPABILITY they were minted for,
		// because since ADR-W033 B7 a bot's credential is what
		// identifies its Connection — one Agent, one transport. A
		// generic `concierge` token names no bot, so it names no
		// Connection, and the inbound door cannot tell which Agent it
		// arrived at.
		chatAdapterPrincipalPrefix + "chat-telegram-family": runtimedb.RoleConcierge,
		chatAdapterPrincipalPrefix + "chat-telegram-admin":  runtimedb.RoleValet,
	} {
		if err := p.Create(name, role, ""); err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		tok, err := p.Mint(name, "", 0)
		if err != nil {
			t.Fatalf("Mint(%s): %v", name, err)
		}
		h.tokens[role] = tok
	}
	return h
}

// person puts a household member on record.
//
// Since migration 0040 a task's author and owner, a memory record's
// author and a notice's recipient are foreign keys to a Person, so a
// fixture cannot file any of them for "sam" without saying who sam is.
// Machine principals are not people and are deliberately not recorded
// here: `root-admin`, `op` and the adapters keep appearing in these
// tests as actors, which is what they are.
//
// Named person rather than member because harness.member is the portal
// tests' REQUEST helper — one issues a call as somebody, this one says
// who somebody is.
// They are ADMITTED as well as recorded, because the portal's door asks
// Active(): a record alone is a person the household has heard of and
// not let in, and a fixture that leaves them there is testing a
// half-onboarded member nobody meant to create. Tests about admission
// itself record and admit by hand.
func (h *harness) person(handles ...string) {
	h.t.Helper()
	for _, handle := range handles {
		if err := h.s.DB.RecordButlerMember(handle, runtimedb.RoleMember, "", ""); err != nil {
			h.t.Fatalf("recording %q: %v", handle, err)
		}
		if err := h.s.DB.AdmitButlerMember(handle, "fixture"); err != nil {
			h.t.Fatalf("admitting %q: %v", handle, err)
		}
	}
}

// console records handle as a household member and returns a token for
// role that ACTS FOR them — a Console caller.
//
// A Butler turn is had by a person (#240): it can write memory, file
// work and raise a notice, and every one of those now names a Person
// rather than whatever the caller authenticated as. A bare machine
// token has nobody behind it, so the endpoint refuses it — which is why
// every fixture that asks a Butler mints one of these instead.
func (h *harness) console(role runtimedb.Role, handle string) string {
	h.t.Helper()
	h.person(handle)
	tok, err := h.s.Principals.Mint(principalNameFor(h.t, role), handle, 0)
	if err != nil {
		h.t.Fatalf("minting a %s token acting for %q: %v", role, handle, err)
	}
	return tok
}

func principalNameFor(t *testing.T, role runtimedb.Role) string {
	t.Helper()
	switch role {
	case runtimedb.RoleAdmin:
		return "root-admin"
	case runtimedb.RoleOperator:
		return "op"
	case runtimedb.RoleButler:
		return "butler"
	case runtimedb.RoleConcierge:
		return "concierge"
	case runtimedb.RoleValet:
		return "valet"
	}
	t.Fatalf("no principal in the harness holds the role %q", role)
	return ""
}

// do issues a request with the given role's token ("" = unauthenticated)
// and decodes the JSON body into out (skipped when out is nil).
func (h *harness) do(role runtimedb.Role, method, path string, body any, out any) *http.Response {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, h.srv.URL+path, &buf)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	if role != "" {
		req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatalf("%s %s: decoding body: %v", method, path, err)
		}
	}
	return resp
}

func containerYAML(t *testing.T, name, image string, env map[string]string) string {
	t.Helper()
	data, err := resource.Marshal(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: "n1"},
		Container:  &resource.ContainerSpec{Image: image, Env: env},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(data)
}

func TestHealthzUnauthenticated(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.srv.URL + "/api/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
}

func TestAuthnRequired(t *testing.T) {
	h := newHarness(t)
	resp := h.do("", "GET", "/api/v1/whoami", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated whoami = %d, want 401", resp.StatusCode)
	}
}

func TestWhoamiAndRBAC(t *testing.T) {
	h := newHarness(t)
	var id principal.Identity
	if resp := h.do(runtimedb.RoleButler, "GET", "/api/v1/whoami", nil, &id); resp.StatusCode != 200 {
		t.Fatalf("whoami = %d", resp.StatusCode)
	}
	if id.Name != "butler" || id.Role != runtimedb.RoleButler {
		t.Fatalf("identity = %+v", id)
	}

	// The load-bearing RBAC rows over real HTTP: butler can read and
	// propose but can never approve or manage; operator cannot manage.
	for _, c := range []struct {
		role   runtimedb.Role
		method string
		path   string
		want   int
	}{
		{runtimedb.RoleButler, "POST", "/api/v1/plans/plan-1/approve", http.StatusForbidden},
		{runtimedb.RoleButler, "GET", "/api/v1/audit", http.StatusForbidden},
		{runtimedb.RoleButler, "GET", "/api/v1/principals", http.StatusForbidden},
		{runtimedb.RoleOperator, "GET", "/api/v1/principals", http.StatusForbidden},
		{runtimedb.RoleOperator, "GET", "/api/v1/resources", http.StatusOK},
	} {
		resp := h.do(c.role, c.method, c.path, nil, nil)
		if resp.StatusCode != c.want {
			t.Errorf("%s %s as %s = %d, want %d", c.method, c.path, c.role, resp.StatusCode, c.want)
		}
	}

	// Denied requests still leave audit rows (ADR-W007 B5).
	records, err := h.s.DB.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for _, r := range records {
		if r.Status == http.StatusForbidden {
			denied++
		}
	}
	if denied < 4 {
		t.Fatalf("denied audit rows = %d, want >= 4", denied)
	}
}

func TestProposeApproveFlow(t *testing.T) {
	h := newHarness(t)
	doc := containerYAML(t, "immich", "ghcr.io/immich-app/immich-server@"+testDigest, nil)

	var rec runtimedb.PlanRecord
	resp := h.do(runtimedb.RoleButler, "POST", "/api/v1/plans",
		proposalRequest{Summary: "deploy immich", Put: []string{doc}}, &rec)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("propose = %d", resp.StatusCode)
	}
	if rec.State != runtimedb.PlanProposed || rec.Principal != "butler" {
		t.Fatalf("proposed record = %+v", rec)
	}

	// The plan travels as a structured object, not JSON escaped inside a
	// JSON string, and the Statefile head it was computed against
	// survives redaction — it is an audit identifier, not a secret.
	var view planView
	if resp := h.do(runtimedb.RoleOperator, "GET", "/api/v1/plans/"+rec.ID, nil, &view); resp.StatusCode != http.StatusOK {
		t.Fatalf("get plan = %d", resp.StatusCode)
	}
	if view.Plan == nil || len(view.Plan.Steps) != 1 {
		t.Fatalf("plan not served structured: %+v", view.Plan)
	}
	if step := view.Plan.Steps[0]; step.BlastRadius == "" || step.Name != "immich" {
		t.Fatalf("plan step = %+v", step)
	}

	// Approve as operator: statefile gains the resource with a structured
	// commit, head marker advances, plan record resolves.
	var approved runtimedb.PlanRecord
	resp = h.do(runtimedb.RoleOperator, "POST", "/api/v1/plans/"+rec.ID+"/approve", nil, &approved)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", resp.StatusCode)
	}
	if approved.State != runtimedb.PlanApproved || approved.ResolvedBy != "op" {
		t.Fatalf("approved record = %+v", approved)
	}
	resources, err := h.s.State.Load()
	if err != nil || len(resources) != 1 || resources[0].Metadata.Name != "immich" {
		t.Fatalf("statefile after approve: %+v, %v", resources, err)
	}

	// Now that the Statefile has a commit, a fresh proposal records the
	// head it was computed against — and that hash must reach the
	// reviewer intact. It is a 40-hex git object id, exactly the shape
	// value-redaction eats, and losing it takes the one field that says
	// WHICH desired state the plan was diffed from.
	var second planView
	if resp := h.do(runtimedb.RoleButler, "POST", "/api/v1/plans",
		proposalRequest{Summary: "second", Put: []string{containerYAML(t, "jelly", "ghcr.io/example/jelly@"+testDigest, nil)}},
		&second); resp.StatusCode != http.StatusCreated {
		t.Fatalf("second propose = %d", resp.StatusCode)
	}
	if second.BasedOnHead == "" || strings.Contains(second.BasedOnHead, "REDACTED") {
		t.Fatalf("basedOnHead = %q", second.BasedOnHead)
	}
	head, err := h.s.State.Head()
	if err != nil || head == "" {
		t.Fatalf("statefile head: %q, %v", head, err)
	}
	marker, err := h.s.DB.StatefileHead()
	if err != nil || marker != head {
		t.Fatalf("statefile_head marker = %q, want %q (%v)", marker, head, err)
	}

	// The events stream saw both transitions.
	events, err := h.s.DB.EventsAfter(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "plan.proposed") || !strings.Contains(joined, "plan.approved") {
		t.Fatalf("event types = %v", types)
	}

	// The resource now serves back with its digest pin intact.
	var views []resourceView
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/resources", nil, &views)
	if len(views) != 1 || !strings.Contains(views[0].YAML, testDigest) {
		t.Fatalf("served resource lost its digest pin: %+v", views)
	}
}

func TestStalePlanSuperseded(t *testing.T) {
	h := newHarness(t)
	docA := containerYAML(t, "app-a", "reg.example/a@"+testDigest, nil)
	docB := containerYAML(t, "app-b", "reg.example/b@"+testDigest, nil)

	var planA, planB runtimedb.PlanRecord
	h.do(runtimedb.RoleOperator, "POST", "/api/v1/plans",
		proposalRequest{Summary: "a", Put: []string{docA}}, &planA)
	h.do(runtimedb.RoleOperator, "POST", "/api/v1/plans",
		proposalRequest{Summary: "b", Put: []string{docB}}, &planB)

	// Approving A moves the statefile; B was computed against the old
	// head and must supersede, not apply.
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+planA.ID+"/approve", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("approve A = %d", resp.StatusCode)
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+planB.ID+"/approve", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("approve stale B = %d, want 409", resp.StatusCode)
	}
	rec, err := h.s.DB.GetPlan(planB.ID)
	if err != nil || rec.State != runtimedb.PlanSuperseded {
		t.Fatalf("plan B = %+v, %v; want superseded", rec, err)
	}
}

func TestRejectFlow(t *testing.T) {
	h := newHarness(t)
	doc := containerYAML(t, "app", "reg.example/app@"+testDigest, nil)
	var rec runtimedb.PlanRecord
	h.do(runtimedb.RoleButler, "POST", "/api/v1/plans",
		proposalRequest{Summary: "sneaky", Put: []string{doc}}, &rec)
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+rec.ID+"/reject", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("reject = %d", resp.StatusCode)
	}
	resources, _ := h.s.State.Load()
	if len(resources) != 0 {
		t.Fatalf("rejected plan reached the statefile: %+v", resources)
	}
	// A rejected plan cannot be approved later.
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+rec.ID+"/approve", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("approve after reject = %d, want 409", resp.StatusCode)
	}
}

func TestProposalValidation(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]proposalRequest{
		"empty":      {Summary: "x"},
		"no summary": {Put: []string{containerYAML(t, "a", "r@"+testDigest, nil)}},
		"bad yaml":   {Summary: "x", Put: []string{"kind: Nope\nmetadata: {name: z}"}},
		"bad delete": {Summary: "x", Delete: []deletionRef{{Kind: "Nope", Name: "z"}}},
		"unpinned":   {Summary: "x", Put: []string{containerYAML(t, "a", "registry.example/app:latest", nil)}},
	} {
		resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans", body, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: propose = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestMintedTokenWorksAndAdminLockoutGuard(t *testing.T) {
	h := newHarness(t)
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/principals",
		map[string]string{"name": "wild-cli", "role": "operator"}, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create principal = %d", resp.StatusCode)
	}
	var mint mintResponse
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/principals/wild-cli/tokens",
		map[string]int{"ttlSeconds": 3600}, &mint)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint = %d", resp.StatusCode)
	}
	if !strings.HasPrefix(mint.Token, "wst_") {
		t.Fatalf("minted token %q not returned intact (redaction must not eat the one legitimate secret reply)", mint.Token)
	}
	// The minted token authenticates.
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+mint.Token)
	wres, err := http.DefaultClient.Do(req)
	if err != nil || wres.StatusCode != 200 {
		t.Fatalf("whoami with minted token: %v %d", err, wres.StatusCode)
	}
	wres.Body.Close()

	// An admin cannot disable themself (lockout guard).
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/principals/root-admin/disable", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-disable = %d, want 400", resp.StatusCode)
	}
	// But can disable others, whose tokens then stop working.
	if resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/principals/wild-cli/disable", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("disable = %d", resp.StatusCode)
	}
	dres, err := http.DefaultClient.Do(req)
	if err != nil || dres.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled principal's token = %v %d, want 401", err, dres.StatusCode)
	}
	dres.Body.Close()
}

// TestRedactionProperty is the ADR-W007 B3 property test: secret-shaped
// values planted in every serializable surface (resource env, event
// messages/attrs, plan summaries) never survive any API response, while
// secret/<name> references and digest pins always do.
func TestRedactionProperty(t *testing.T) {
	h := newHarness(t)
	secrets := []string{
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", // hex
		"aaaabbbbccccddddeeeeffffgggg",                                     // base64-shaped, deliberately low-entropy (gitleaks)
	}
	env := map[string]string{
		"DB_PASSWORD": secrets[0],
		"API_KEY":     secrets[1],
		"OIDC_REF":    "secret/immich-oidc",
	}
	// Since E4, a proposal carrying literal secrets is 422'd by the
	// policy engine before it ever becomes state — so a secret-bearing
	// resource can only be planted by writing the Statefile directly
	// (simulating a pre-E4 tree or a hand edit). The redaction property
	// must hold for what's IN state regardless of how it got there.
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: "immich", Owner: "capability/test", Node: "n1"},
		Container:  &resource.ContainerSpec{Image: "ghcr.io/immich@" + testDigest, Env: env},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "", "seed secret-bearing resource"); err != nil {
		t.Fatal(err)
	}

	// The plan record's secret surface: a planted secret in the summary
	// (free text is not policy-checked; redaction must still catch it).
	doc := containerYAML(t, "clean-app", "ghcr.io/clean@"+testDigest, nil)
	var rec runtimedb.PlanRecord
	if resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/plans",
		proposalRequest{Summary: "deploy with planted secrets " + secrets[0], Put: []string{doc}}, &rec); resp.StatusCode != 201 {
		t.Fatalf("propose = %d", resp.StatusCode)
	}
	h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+rec.ID+"/approve", nil, nil)
	if _, err := h.s.DB.AppendEvent(runtimedb.Event{
		Type: "test", Message: "token: " + secrets[0], Attrs: map[string]string{"apikey": secrets[1]},
	}); err != nil {
		t.Fatal(err)
	}

	paths := []string{
		"/api/v1/resources",
		"/api/v1/resources/Container/immich",
		"/api/v1/events?limit=100",
		"/api/v1/plans",
		"/api/v1/plans/" + rec.ID,
		"/api/v1/audit",
		"/api/v1/principals",
	}
	for _, path := range paths {
		req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+h.tokens[runtimedb.RoleAdmin])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		raw := new(bytes.Buffer)
		if _, err := raw.ReadFrom(resp.Body); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		body := raw.String()
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s leaked a planted secret: %s", path, body)
			}
		}
		if strings.Contains(body, "immich-oidc") && !strings.Contains(body, "secret/immich-oidc") {
			t.Errorf("GET %s mangled the secret reference: %s", path, body)
		}
		if !json.Valid(raw.Bytes()) {
			t.Errorf("GET %s: redaction produced invalid JSON: %s", path, body)
		}
	}
}

func TestAnonymousRateLimit(t *testing.T) {
	h := newHarness(t)
	// Drain the strict anonymous bucket with bad-token requests; well
	// under the authenticated budget so real principals are unaffected.
	limited := false
	for i := 0; i < anonBurst+2; i++ {
		req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer wst_"+strings.Repeat("0", 64))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("anonymous bucket never limited a token-guessing burst")
	}
	// An authenticated principal still gets through.
	if resp := h.do(runtimedb.RoleOperator, "GET", "/api/v1/whoami", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("authenticated request rate-limited alongside anonymous: %d", resp.StatusCode)
	}
}

func TestEventsLongPoll(t *testing.T) {
	h := newHarness(t)
	start := time.Now()
	done := make(chan []runtimedb.Event, 1)
	go func() {
		var events []runtimedb.Event
		h.do(runtimedb.RoleOperator, "GET", "/api/v1/events?waitMs=5000", nil, &events)
		done <- events
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := h.s.DB.AppendEvent(runtimedb.Event{Type: "poke"}); err != nil {
		t.Fatal(err)
	}
	select {
	case events := <-done:
		if len(events) != 1 || events[0].Type != "poke" {
			t.Fatalf("long-poll returned %+v", events)
		}
		if time.Since(start) > 4*time.Second {
			t.Fatal("long-poll only returned at deadline, not on event arrival")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("long-poll never returned")
	}
}

func TestNoUnmediatedWritePath(t *testing.T) {
	// Ground rule 4 as a route-surface assertion: no PUT/DELETE routes
	// exist, and POSTing a resource-shaped body anywhere outside plan
	// proposal 404s/405s.
	h := newHarness(t)
	for _, c := range []struct{ method, path string }{
		{"PUT", "/api/v1/resources/Container/x"},
		{"DELETE", "/api/v1/resources/Container/x"},
		{"POST", "/api/v1/resources"},
	} {
		req, _ := http.NewRequest(c.method, h.srv.URL+c.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+h.tokens[runtimedb.RoleAdmin])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 404/405 (no unmediated write path)", c.method, c.path, resp.StatusCode)
		}
	}
}

// TestPolicyRejectsProposal pins ADR-W009 A1 on the API path: a proposal
// violating a standing invariant is 422'd before any plan record exists,
// and the rejection is evented.
func TestPolicyRejectsProposal(t *testing.T) {
	h := newHarness(t)
	doc, err := resource.Marshal(resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindRoute,
		Metadata:   resource.Metadata{Name: "cam", Owner: "capability/test", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: "cam.example.com", Upstream: "10.0.0.9:80",
			Auth: "none", Classification: "family_public",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans",
		proposalRequest{Summary: "expose camera", Put: []string{string(doc)}}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("policy-violating propose = %d, want 422", resp.StatusCode)
	}
	plans, err := h.s.DB.ListPlans(10)
	if err != nil || len(plans) != 0 {
		t.Fatalf("plan records after rejection = %+v, %v; want none", plans, err)
	}
	events, err := h.s.DB.EventsAfter(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == "policy.rejected" && strings.Contains(e.Message, "public-requires-auth") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no policy.rejected event: %+v", events)
	}
}
