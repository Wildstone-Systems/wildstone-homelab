package api

// P10's evidence (ADR-W030): a fake adapter proves events, queries,
// actions, timeout, cancellation, duplicate delivery, stale-epoch
// rejection and an empty valid answer — and an adapter credential can
// neither claim another connection's work nor reach general API
// authority.

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// fakeAdapter is the whole point of P10: a client that knows nothing
// about any application, exercising the contract Keystone actually
// offers. It claims work, answers it, and does so through the same HTTP
// surface a real container would.
type fakeAdapter struct {
	h     *harness
	token string
	name  string
}

func newFakeAdapter(t *testing.T, h *harness, name string) *fakeAdapter {
	ownExecutionFixture(t, h)
	t.Helper()
	p := h.s.Principals
	if err := p.Create(name, runtimedb.RoleIntegrationAdapter, ""); err != nil {
		t.Fatalf("creating the adapter principal: %v", err)
	}
	tok, err := p.Mint(name, "", 0)
	if err != nil {
		t.Fatalf("minting the adapter token: %v", err)
	}
	return &fakeAdapter{h: h, token: tok, name: name}
}

// claim polls without waiting: these tests queue work before claiming,
// so a wait window would only add seconds. The long poll itself is
// exercised by TestAdapterLongPollReturnsEmptyRatherThanFailing.
func (a *fakeAdapter) claim(t *testing.T) (runtimedb.IntegrationWork, bool) {
	t.Helper()
	var body struct {
		Work *runtimedb.IntegrationWork `json:"work"`
	}
	res := a.h.doToken(a.token, http.MethodPost, "/api/v1/integration-work/claim",
		map[string]any{"waitMillis": -1}, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d", res.StatusCode)
	}
	if body.Work == nil {
		return runtimedb.IntegrationWork{}, false
	}
	return *body.Work, true
}

func (a *fakeAdapter) report(t *testing.T, seq int64, resultType, result, observedAt string) *http.Response {
	t.Helper()
	return a.h.doToken(a.token, http.MethodPost, "/api/v1/integration-work/result", map[string]any{
		"seq": seq, "resultType": resultType, "result": result, "observedAt": observedAt,
	}, nil)
}

func setupConnection(t *testing.T, h *harness, name, adapterPrincipal string) {
	t.Helper()
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations", map[string]any{
		"name": name, "adapter": "fake", "ownership": "household", "owner": "household",
	}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("creating connection %s = %d", name, res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/"+name+"/adapter",
		map[string]any{"principal": adapterPrincipal}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("binding adapter to %s = %d", name, res.StatusCode)
	}
}

func queueWork(t *testing.T, h *harness, connection, kind, capability string, timeoutMillis int) int64 {
	t.Helper()
	var out struct {
		Seq int64 `json:"seq"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/"+connection+"/work",
		map[string]any{
			"kind": kind, "capability": capability, "correlationId": "bt-1",
			"request": `{"list":"groceries"}`, "timeoutMillis": timeoutMillis,
		}, &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("queueing %s = %d", capability, res.StatusCode)
	}
	return out.Seq
}

// The contract, end to end, through a fake adapter.
func TestFakeAdapterProvesTheContract(t *testing.T) {
	h := newHarness(t)
	adapter := newFakeAdapter(t, h, "pantry-adapter")
	setupConnection(t, h, "pantry", "pantry-adapter")

	t.Run("a query gets a real answer", func(t *testing.T) {
		seq := queueWork(t, h, "pantry", "query", "list.read", 5000)
		work, ok := adapter.claim(t)
		if !ok || work.Seq != seq {
			t.Fatalf("claimed %+v, want %d", work, seq)
		}
		if work.Connection != "pantry" || work.CorrelationID != "bt-1" {
			t.Fatalf("work = %+v", work)
		}
		if res := adapter.report(t, seq, "ok", `{"items":["milk"]}`, ""); res.StatusCode != http.StatusOK {
			t.Fatalf("report = %d", res.StatusCode)
		}
		got := readWork(t, h, seq)
		if got.ResultType != "ok" || got.State != runtimedb.WorkDone {
			t.Fatalf("work after answer = %+v", got)
		}
	})

	t.Run("an empty answer is a real answer", func(t *testing.T) {
		seq := queueWork(t, h, "pantry", "query", "list.read", 5000)
		work, _ := adapter.claim(t)
		if res := adapter.report(t, work.Seq, "empty", `{"items":[]}`, ""); res.StatusCode != http.StatusOK {
			t.Fatalf("report = %d", res.StatusCode)
		}
		got := readWork(t, h, seq)
		// The distinction the whole contract exists for: nothing on the
		// list is not the same as the app being down.
		if got.ResultType != runtimedb.ResultEmpty {
			t.Fatalf("an empty list came back as %q", got.ResultType)
		}
	})

	t.Run("unavailable and stale are distinguishable", func(t *testing.T) {
		down := queueWork(t, h, "pantry", "query", "list.read", 5000)
		w, _ := adapter.claim(t)
		adapter.report(t, w.Seq, "unavailable", `{}`, "")
		if readWork(t, h, down).ResultType != runtimedb.ResultUnavailable {
			t.Fatal("an outage was not reported as unavailable")
		}

		old := queueWork(t, h, "pantry", "query", "list.read", 5000)
		w2, _ := adapter.claim(t)
		observed := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		adapter.report(t, w2.Seq, "stale", `{"items":["milk"]}`, observed)
		got := readWork(t, h, old)
		if got.ResultType != runtimedb.ResultStale || got.ObservedAt.IsZero() {
			t.Fatalf("a stale answer lost its observation time: %+v", got)
		}
		// A stale answer with no observation time is refused: it would
		// be reported as if it were current.
		bad := queueWork(t, h, "pantry", "query", "list.read", 5000)
		w3, _ := adapter.claim(t)
		if res := adapter.report(t, w3.Seq, "stale", `{}`, ""); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("a stale result with no observation time = %d, want 400", res.StatusCode)
		}
		_ = bad
	})

	t.Run("duplicate delivery is recognised, not applied twice", func(t *testing.T) {
		seq := queueWork(t, h, "pantry", "query", "list.read", 5000)
		w, _ := adapter.claim(t)
		if res := adapter.report(t, w.Seq, "ok", `{"items":["eggs"]}`, ""); res.StatusCode != http.StatusOK {
			t.Fatalf("first report = %d", res.StatusCode)
		}
		if res := adapter.report(t, w.Seq, "ok", `{"items":["something else"]}`, ""); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("a duplicate delivery = %d, want 400", res.StatusCode)
		}
		if got := readWork(t, h, seq); got.ResultJSON != `{"items":["eggs"]}` {
			t.Fatalf("a duplicate delivery overwrote the answer: %+v", got)
		}
	})

	t.Run("a timeout expires rather than waiting forever", func(t *testing.T) {
		seq := queueWork(t, h, "pantry", "query", "list.read", 1)
		time.Sleep(5 * time.Millisecond)
		// The claim path sweeps expired work, so an adapter that has
		// been away does not wake up and answer a dead question.
		if _, ok := adapter.claim(t); ok {
			t.Fatal("expired work was claimed")
		}
		got := readWork(t, h, seq)
		if got.State != runtimedb.WorkExpired || got.ResultType != runtimedb.ResultUnavailable {
			t.Fatalf("expired work = %+v", got)
		}
	})

	t.Run("cancellation stops an answer landing", func(t *testing.T) {
		seq := queueWork(t, h, "pantry", "query", "list.read", 5000)
		w, _ := adapter.claim(t)
		if err := h.s.DB.CancelIntegrationWork(w.Seq, "the task was cancelled"); err != nil {
			t.Fatal(err)
		}
		if res := adapter.report(t, w.Seq, "ok", `{"items":["late"]}`, ""); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("an answer to cancelled work = %d, want 400", res.StatusCode)
		}
		if got := readWork(t, h, seq); got.State != runtimedb.WorkCancelled {
			t.Fatalf("cancelled work = %+v", got)
		}
	})

	t.Run("an action needs the connection's actions turned on", func(t *testing.T) {
		res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/pantry/work",
			map[string]any{"kind": "action", "capability": "list.add", "request": `{"item":"milk"}`}, nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("an action on a read-only connection = %d, want 400", res.StatusCode)
		}
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/pantry/widen",
			map[string]any{"actions": true}, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("widen = %d", res.StatusCode)
		}
		seq := queueWork(t, h, "pantry", "action", "list.add", 5000)
		w, ok := adapter.claim(t)
		if !ok || w.Seq != seq || w.Kind != runtimedb.WorkAction {
			t.Fatalf("claimed %+v", w)
		}
		adapter.report(t, w.Seq, "ok", `{"added":true}`, "")
	})
}

func readWork(t *testing.T, h *harness, seq int64) runtimedb.IntegrationWork {
	t.Helper()
	var w runtimedb.IntegrationWork
	res := h.do(runtimedb.RoleAdmin, http.MethodGet,
		"/api/v1/integration-work/"+strconv.FormatInt(seq, 10), nil, &w)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reading work %d = %d", seq, res.StatusCode)
	}
	return w
}

// A mail read's answer carries a Person's normalized messages; the work
// API never returns it (epic #326 S05: no full message body in any API
// response). Another domain's answer is unaffected.
func TestMailReadAnswersAreNotServedByTheWorkAPI(t *testing.T) {
	h := newHarness(t)
	adapter := newFakeAdapter(t, h, "mail-adapter")
	setupConnection(t, h, "household-mail", "mail-adapter")
	seq := queueWork(t, h, "household-mail", "query", "mail.changes", 5000)
	if _, ok := adapter.claim(t); !ok {
		t.Fatal("nothing to claim")
	}
	body := `{"outcome":"ok","messages":[{"content":{"text":"REPLACED-private-body"}}],"next":{"folders":[]}}`
	if res := adapter.report(t, seq, "ok", body, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("report = %d", res.StatusCode)
	}
	if got := readWork(t, h, seq); got.ResultType != "ok" || strings.Contains(got.ResultJSON, "REPLACED-private-body") {
		t.Fatalf("mail answer served: %+v", got)
	}
}

// An adapter credential cannot claim another connection's work, and
// cannot reach general API authority.
func TestAdapterCredentialIsConnectionBoundAndNarrow(t *testing.T) {
	h := newHarness(t)
	mine := newFakeAdapter(t, h, "pantry-adapter")
	theirs := newFakeAdapter(t, h, "calendar-adapter")
	setupConnection(t, h, "pantry", "pantry-adapter")
	setupConnection(t, h, "calendar", "calendar-adapter")

	queueWork(t, h, "pantry", "query", "list.read", 5000)

	// The other adapter gets nothing — an empty queue, not an error it
	// could probe with.
	if work, ok := theirs.claim(t); ok {
		t.Fatalf("an adapter claimed another connection's work: %+v", work)
	}
	// And it cannot answer work it did not claim.
	claimed, ok := mine.claim(t)
	if !ok {
		t.Fatal("the bound adapter got nothing")
	}
	if res := theirs.report(t, claimed.Seq, "ok", `{}`, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("answering another adapter's work = %d, want 404", res.StatusCode)
	}

	// The credential reaches nothing else. Every one of these is a
	// capability the two adapter verbs deliberately do not carry.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/resources", nil},
		{http.MethodGet, "/api/v1/integrations", nil},
		{http.MethodGet, "/api/v1/butler/sessions", nil},
		{http.MethodPost, "/api/v1/integrations", map[string]any{"name": "x", "adapter": "y", "ownership": "household", "owner": "z"}},
		{http.MethodPost, "/api/v1/integrations/pantry/widen", map[string]any{"actions": true}},
		{http.MethodPost, "/api/v1/integrations/pantry/work", map[string]any{"kind": "query", "capability": "c"}},
		{http.MethodGet, "/api/v1/butler/memory", nil},
	} {
		res := h.doToken(mine.token, tc.method, tc.path, tc.body, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("adapter %s %s = %d, want 403", tc.method, tc.path, res.StatusCode)
		}
	}
}

// Moving the lease fences the old adapter: work it claimed is refused
// when it finally answers.
func TestStaleEpochResultsAreRejected(t *testing.T) {
	h := newHarness(t)
	adapter := newFakeAdapter(t, h, "pantry-adapter")
	setupConnection(t, h, "pantry", "pantry-adapter")
	seq := queueWork(t, h, "pantry", "query", "list.read", 60000)

	work, ok := adapter.claim(t)
	if !ok {
		t.Fatal("nothing to claim")
	}
	// Placement moves, or the binding is withdrawn. Either way the
	// connection's lease advances.
	if _, err := h.s.DB.BumpConnectionEpoch("pantry"); err != nil {
		t.Fatal(err)
	}
	if res := adapter.report(t, work.Seq, "ok", `{"items":["late"]}`, ""); res.StatusCode != http.StatusConflict {
		t.Fatalf("a fenced adapter's answer = %d, want 409", res.StatusCode)
	}
	if got := readWork(t, h, seq); got.ResultType != "" {
		t.Fatalf("a fenced answer was applied: %+v", got)
	}
	// And it cannot claim under the old lease either.
	queueWork(t, h, "pantry", "query", "list.read", 60000)
	if _, ok := adapter.claim(t); !ok {
		// New work carries the NEW epoch, so the same adapter — still
		// bound — can claim it. Fencing supersedes in-flight work, it
		// does not ban the adapter.
		t.Fatal("a bound adapter could not claim work queued after the fence")
	}
}

// Discovery records what works; it does not authorize anything.
func TestCapabilityDiscoveryAuthorizesNothing(t *testing.T) {
	h := newHarness(t)
	newFakeAdapter(t, h, "pantry-adapter")
	setupConnection(t, h, "pantry", "pantry-adapter")

	var out struct {
		Note string `json:"note"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/pantry/capabilities",
		map[string]any{"kind": "action", "name": "list.clear", "version": "2.1", "available": true}, &out,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("recording a capability = %d", res.StatusCode)
	}
	// A discovered action is still not runnable: the connection's
	// actions switch is separate and still off.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/pantry/work",
		map[string]any{"kind": "action", "capability": "list.clear"}, nil,
	); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a discovered action ran without being enabled: %d", res.StatusCode)
	}
}

// A quiet period is a quiet period, not a failure: the long poll returns
// an empty answer and the adapter simply asks again.
func TestAdapterLongPollReturnsEmptyRatherThanFailing(t *testing.T) {
	h := newHarness(t)
	adapter := newFakeAdapter(t, h, "pantry-adapter")
	setupConnection(t, h, "pantry", "pantry-adapter")

	var body struct {
		Work *runtimedb.IntegrationWork `json:"work"`
	}
	start := time.Now()
	res := h.doToken(adapter.token, http.MethodPost, "/api/v1/integration-work/claim",
		map[string]any{"waitMillis": 300}, &body)
	waited := time.Since(start)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an idle poll = %d, want 200", res.StatusCode)
	}
	if body.Work != nil {
		t.Fatalf("an idle poll returned work: %+v", body.Work)
	}
	if waited < 250*time.Millisecond {
		t.Fatalf("the poll returned after %s; it did not wait", waited)
	}
	// And it is bounded: an adapter cannot ask to hold a connection open
	// indefinitely.
	start = time.Now()
	h.doToken(adapter.token, http.MethodPost, "/api/v1/integration-work/claim",
		map[string]any{"waitMillis": 60 * 60 * 1000}, &body)
	if time.Since(start) > maxAdapterPoll+5*time.Second {
		t.Fatal("an adapter held the connection past the server's own bound")
	}
}

// P11 at the API: a placement is declared, prepared, held, failed over,
// and removed only once its cleanup is proven.
func TestPlacementLifecycleThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	newFakeAdapter(t, h, "cal-adapter")
	setupConnection(t, h, "calendar", "cal-adapter")

	declare := func(node, role string, rank int) {
		t.Helper()
		var body struct {
			Note string `json:"note"`
		}
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/placement",
			map[string]any{"node": node, "role": role, "rank": rank}, &body); res.StatusCode != http.StatusOK {
			t.Fatalf("declaring %s = %d", node, res.StatusCode)
		}
		if !strings.Contains(body.Note, "not yet eligible") {
			t.Fatalf("declaring a node did not say it is not yet eligible: %q", body.Note)
		}
	}
	declare("n1", "primary", 0)
	declare("n2", "fallback", 1)

	// Nothing prepared: the answer is a wait, and nothing was created.
	var placed struct {
		Waiting bool   `json:"waiting"`
		Note    string `json:"note"`
		Holder  string `json:"holder"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/placement/place",
		map[string]any{}, &placed); res.StatusCode != http.StatusOK {
		t.Fatalf("place = %d", res.StatusCode)
	}
	if !placed.Waiting || !strings.Contains(placed.Note, "Nothing was created") {
		t.Fatalf("an outage with nothing prepared = %+v", placed)
	}

	ready := func(node string) {
		t.Helper()
		if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/placement/ready",
			map[string]any{
				"node": node, "containerRunning": true,
				"secretProvisioned": true, "readinessVerified": true,
			}, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("marking %s ready = %d", node, res.StatusCode)
		}
	}
	ready("n1")
	ready("n2")
	placed.Holder = ""
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/placement/place", map[string]any{}, &placed)
	if placed.Holder != "n1" {
		t.Fatalf("holder = %q, want the primary", placed.Holder)
	}

	// Removing the holder fences it immediately and leaves the
	// connection degraded until the cleanup is proven.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/placement/remove",
		map[string]any{"node": "n1"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("remove = %d", res.StatusCode)
	}
	var proof struct {
		Complete       bool   `json:"complete"`
		State          string `json:"state"`
		DegradedReason string `json:"degradedReason"`
	}
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/placement/proof",
		map[string]any{"node": "n1", "containerRemoved": true, "secretErased": false, "credentialRevoked": false}, &proof)
	if proof.Complete || proof.State != runtimedb.ConnectionDegraded || proof.DegradedReason == "" {
		t.Fatalf("partial cleanup = %+v", proof)
	}
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/placement/proof",
		map[string]any{"node": "n1", "containerRemoved": true, "secretErased": true, "credentialRevoked": true}, &proof)
	if !proof.Complete || proof.State != runtimedb.ConnectionReady {
		t.Fatalf("proven cleanup = %+v", proof)
	}

	// The lease moved to the fallback rather than to something invented.
	var view struct {
		Placements []runtimedb.Placement `json:"placements"`
	}
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/integrations/calendar/placement", nil, &view)
	if len(view.Placements) != 1 || view.Placements[0].Node != "n2" || !view.Placements[0].Holder {
		t.Fatalf("placements after removal = %+v", view.Placements)
	}
}

// A credential value cannot be onboarded through the API, and an
// adapter cannot reach the secret endpoints at all.
func TestSecretOnboardingRefusesValues(t *testing.T) {
	h := newHarness(t)
	adapter := newFakeAdapter(t, h, "cal-adapter")
	setupConnection(t, h, "calendar", "cal-adapter")

	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/secrets",
		map[string]any{
			"member": "sam", "secretRef": "https://sam:hunter2@dav.example.com", "upstreamReadOnly": true,
		}, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a credential-shaped reference = %d, want 400", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/calendar/secrets",
		map[string]any{"member": "sam", "secretRef": "calendar-sam-caldav", "upstreamReadOnly": true},
		nil); res.StatusCode != http.StatusOK {
		t.Fatalf("onboarding a reference = %d", res.StatusCode)
	}
	// The adapter's own credential reaches none of this.
	if res := h.doToken(adapter.token, http.MethodPost, "/api/v1/integrations/calendar/secrets",
		map[string]any{"member": "sam", "secretRef": "x", "upstreamReadOnly": true}, nil,
	); res.StatusCode != http.StatusForbidden {
		t.Fatalf("an adapter reached the secret endpoint: %d", res.StatusCode)
	}
	// And nothing in the placement view carries a value.
	body := h.rawBody(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/integrations/calendar/placement")
	if strings.Contains(body, "hunter2") {
		t.Fatalf("a credential value reached a response:\n%s", body)
	}
}
