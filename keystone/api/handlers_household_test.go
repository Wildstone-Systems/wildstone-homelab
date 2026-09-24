package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime/native"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/household"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// publisherFakeAgent records the value of every PutSecret so a test can
// present the placed token back to the API — the whole point of minting
// is that the node ends up holding a credential that works.
type publisherFakeAgent struct {
	secretsFakeAgent
	values  map[string]string
	failPut bool
}

func (f *publisherFakeAgent) Apply(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if ps, ok := op.GetOp().(*wsv1.Operation_PutSecret); ok {
		if f.failPut {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
				Code: wsv1.ErrorCode_ERROR_CODE_UNSPECIFIED, Message: "disk full",
			}}}, nil
		}
		if f.values == nil {
			f.values = map[string]string{}
		}
		f.values[ps.PutSecret.GetName()] = ps.PutSecret.GetValue()
	}
	return f.secretsFakeAgent.Apply(ctx, node, op)
}

func envelope(id string) household.Envelope {
	return household.Envelope{
		EventID: id, OccurredAt: time.Now().Add(-time.Minute), Subject: "mika",
		Type: "person.left_home", Payload: []byte(`{"zone":"home"}`),
	}
}

// addPublisher creates a publisher principal directly and returns its
// token, for tests that need one without going through minting.
func (h *harness) addPublisher(name string) string {
	h.t.Helper()
	if err := h.s.Principals.Create(name, runtimedb.RolePublisher, ""); err != nil {
		h.t.Fatal(err)
	}
	tok, err := h.s.Principals.Mint(name, "", 0)
	if err != nil {
		h.t.Fatal(err)
	}
	h.tokens[runtimedb.RolePublisher] = tok
	return tok
}

func TestHouseholdEmitStampsSourceAndDedupes(t *testing.T) {
	h := newHarness(t)
	h.addPublisher("publisher-home")

	// A ULID-shaped id: the shape the value redactor would otherwise
	// strip from every response. It must round-trip intact.
	const ulid = "01J6ZK8Q2M7Y4N3R5T9V1W0X2A"
	var r1 household.Receipt
	if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", envelope(ulid), &r1); resp.StatusCode != http.StatusCreated {
		t.Fatalf("emit = %d, want 201", resp.StatusCode)
	}
	if r1.Seq == 0 || r1.Duplicate || r1.EventID != ulid {
		t.Fatalf("receipt = %+v", r1)
	}
	// A retry of the same delivery: 200, same seq, nothing written.
	var r2 household.Receipt
	if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", envelope(ulid), &r2); resp.StatusCode != http.StatusOK {
		t.Fatalf("retry = %d, want 200", resp.StatusCode)
	}
	if !r2.Duplicate || r2.Seq != r1.Seq {
		t.Fatalf("retry receipt = %+v", r2)
	}

	// The stored source is the token's principal. A "source" field in the
	// body is not part of the envelope and is ignored — a publisher
	// cannot speak for another.
	if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events",
		map[string]any{"eventId": "ha-2", "occurredAt": time.Now(), "type": "person.arrived_home", "source": "publisher-pantry"}, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("emit with source in body = %d", resp.StatusCode)
	}
	var events []runtimedb.HouseholdEvent
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/household/events", nil, &events); resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (the retry must not have written)", len(events))
	}
	for _, e := range events {
		if e.Source != "publisher-home" {
			t.Fatalf("event %s source = %q, want publisher-home", e.EventID, e.Source)
		}
	}
	if events[0].Subject != "mika" || string(events[0].Payload) != `{"zone":"home"}` || events[0].EventID != ulid {
		t.Fatalf("event = %+v", events[0])
	}

	// Cursor: after the first seq, only the second comes back.
	var tail []runtimedb.HouseholdEvent
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/household/events?after="+itoa(events[0].Seq), nil, &tail)
	if len(tail) != 1 || tail[0].EventID != "ha-2" {
		t.Fatalf("after cursor = %+v", tail)
	}
	// An empty stream past the end is [] not null, with a short long-poll.
	var empty []runtimedb.HouseholdEvent
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/household/events?after="+itoa(events[1].Seq)+"&waitMs=50", nil, &empty)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("past the end = %v, want []", empty)
	}
}

func TestHouseholdEmitValidation(t *testing.T) {
	h := newHarness(t)
	h.addPublisher("publisher-home")
	for name, body := range map[string]any{
		"flat type":  map[string]any{"eventId": "x", "occurredAt": time.Now(), "type": "left_home"},
		"no id":      map[string]any{"occurredAt": time.Now(), "type": "a.b"},
		"no time":    map[string]any{"eventId": "x", "type": "a.b"},
		"array body": map[string]any{"eventId": "x", "occurredAt": time.Now(), "type": "a.b", "payload": []int{1}},
		"not json":   "nope",
	} {
		var out map[string]any
		if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", body, &out); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, resp.StatusCode, out)
		}
	}
}

// The RBAC rows over real HTTP: a publisher emits and nothing else; the
// butler and operator read but never emit; the admin does both.
func TestHouseholdRBAC(t *testing.T) {
	h := newHarness(t)
	h.addPublisher("publisher-home")
	for _, c := range []struct {
		role   runtimedb.Role
		method string
		path   string
		body   any
		want   int
	}{
		{runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", envelope("p-1"), http.StatusCreated},
		{runtimedb.RolePublisher, http.MethodGet, "/api/v1/household/events", nil, http.StatusForbidden},
		{runtimedb.RolePublisher, http.MethodGet, "/api/v1/resources", nil, http.StatusForbidden},
		{runtimedb.RolePublisher, http.MethodGet, "/api/v1/whoami", nil, http.StatusForbidden},
		{runtimedb.RolePublisher, http.MethodPost, "/api/v1/plans", map[string]any{}, http.StatusForbidden},
		{runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/publishers/home", map[string]string{"node": "n1"}, http.StatusForbidden},
		{runtimedb.RoleButler, http.MethodPost, "/api/v1/household/events", envelope("b-1"), http.StatusForbidden},
		{runtimedb.RoleOperator, http.MethodPost, "/api/v1/household/events", envelope("o-1"), http.StatusForbidden},
		{runtimedb.RoleButler, http.MethodGet, "/api/v1/household/events", nil, http.StatusOK},
		{runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/events", envelope("a-1"), http.StatusCreated},
		{"", http.MethodPost, "/api/v1/household/events", envelope("anon"), http.StatusUnauthorized},
	} {
		if resp := h.do(c.role, c.method, c.path, c.body, nil); resp.StatusCode != c.want {
			t.Errorf("%s %s %s = %d, want %d", c.role, c.method, c.path, resp.StatusCode, c.want)
		}
	}
}

func TestMintPublisherPlacesAWorkingTokenAndRotates(t *testing.T) {
	h := newHarness(t)
	// Unwired agent plane: 503 before anything is created.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/publishers/home", map[string]string{"node": "n1"}, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d, want 503", resp.StatusCode)
	}
	if _, err := h.s.DB.GetPrincipal("publisher-home"); err == nil {
		t.Fatal("principal created before the agent plane was checked")
	}
	fake := &publisherFakeAgent{}
	h.s.Agents = fake
	poked := 0
	h.s.OnApproved = func() { poked++ }

	// A shim container on n1 references the publisher secret; it must be
	// re-created on rotation. One on another node must not be touched.
	for _, c := range []struct{ name, node string }{{"home-shim", "n1"}, {"other-shim", "n2"}} {
		if err := h.s.State.Put(resource.Resource{
			ApiVersion: resource.ApiVersion, Kind: resource.KindContainer,
			Metadata:  resource.Metadata{Name: c.name, Owner: "capability/home", Node: c.node},
			Container: &resource.ContainerSpec{Image: "ghcr.io/x/y@" + testDigest, Env: map[string]string{"WILDSTONE_TOKEN": "secret/home-publisher"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.State.Commit("test", "seed", "seed"); err != nil {
		t.Fatal(err)
	}

	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler} {
		if resp := h.do(role, http.MethodPost, "/api/v1/household/publishers/home", map[string]string{"node": "n1"}, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s mint = %d, want 403", role, resp.StatusCode)
		}
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/publishers/home", map[string]string{}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no node = %d, want 400", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/publishers/Bad_Name", map[string]string{"node": "n1"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad capability = %d, want 400", resp.StatusCode)
	}

	var out struct {
		Principal string   `json:"principal"`
		Secret    string   `json:"secret"`
		Version   uint32   `json:"version"`
		Removed   []string `json:"dependentsRemoved"`
		Plan      string   `json:"plan"`
		Token     string   `json:"token"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/publishers/home", map[string]string{"node": "n1"}, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("mint = %d", resp.StatusCode)
	}
	if out.Principal != "publisher-home" || out.Secret != "secret/home-publisher" || out.Version != 1 || out.Plan == "" {
		t.Fatalf("mint = %+v", out)
	}
	if out.Token != "" {
		t.Fatal("the token appeared in the response; it must only ever exist on the node")
	}
	if len(out.Removed) != 1 || out.Removed[0] != "home-shim" || len(fake.removed) != 1 {
		t.Fatalf("dependents = %v / removed %v, want just home-shim", out.Removed, fake.removed)
	}
	if poked != 1 {
		t.Fatalf("reconcile poked %d times, want 1", poked)
	}

	// The value placed on the node is a live publisher credential: it can
	// emit, and it cannot read.
	first := fake.values["home-publisher"]
	if first == "" {
		t.Fatal("no token placed on the node")
	}
	h.tokens[runtimedb.RolePublisher] = first
	if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", envelope("m-1"), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("placed token emit = %d, want 201", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RolePublisher, http.MethodGet, "/api/v1/household/events", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("placed token read = %d, want 403", resp.StatusCode)
	}
	var events []runtimedb.HouseholdEvent
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/household/events", nil, &events)
	if len(events) != 1 || events[0].Source != "publisher-home" {
		t.Fatalf("events = %+v", events)
	}

	// Mint again: rotation. The old token stops working, the new one
	// works, the secret version advances, the shim is re-created again.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/publishers/home", map[string]string{"node": "n1"}, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("re-mint = %d", resp.StatusCode)
	}
	second := fake.values["home-publisher"]
	if second == "" || second == first || out.Version != 2 {
		t.Fatalf("rotation: first=%q second=%q version=%d", first[:8], second[:8], out.Version)
	}
	h.tokens[runtimedb.RolePublisher] = first
	if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", envelope("m-2"), nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token after rotation = %d, want 401", resp.StatusCode)
	}
	h.tokens[runtimedb.RolePublisher] = second
	if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", envelope("m-2"), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("new token after rotation = %d, want 201", resp.StatusCode)
	}
	principals, _ := h.s.DB.ListPrincipals()
	n := 0
	for _, p := range principals {
		if p.Name == "publisher-home" {
			n++
			if p.Role != runtimedb.RolePublisher {
				t.Fatalf("role = %s", p.Role)
			}
		}
	}
	if n != 1 {
		t.Fatalf("publisher-home principals = %d, want 1 (re-mint must reuse)", n)
	}

	// A name collision with a principal of another role is refused.
	if err := h.s.Principals.Create("publisher-pantry", runtimedb.RoleOperator, ""); err != nil {
		t.Fatal(err)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/publishers/pantry", map[string]string{"node": "n1"}, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("collision = %d, want 409", resp.StatusCode)
	}
}

// A placement failure leaves no live credential behind: the token that
// never reached the node is revoked, and the plan record says failed.
func TestMintPublisherPlacementFailureRevokes(t *testing.T) {
	h := newHarness(t)
	fake := &publisherFakeAgent{failPut: true}
	h.s.Agents = fake
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/household/publishers/home", map[string]string{"node": "n1"}, nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed placement = %d, want 502", resp.StatusCode)
	}
	plans, err := h.s.DB.ListPlans(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].State != runtimedb.PlanFailed {
		t.Fatalf("plans = %+v", plans)
	}
	// No token for the principal authenticates — every one minted is revoked.
	// (There is no token-list API; minting again must succeed and the
	// count of live tokens is not observable, so assert via the DB.)
	rows, err := h.s.DB.RevokeTokensFor("publisher-home")
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d live token(s) survived a failed placement", rows)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A household rule is proposed, policy-checked, and approved through the
// ONE plan pipeline (ADR-W024 A1): the Butler may draft it, a human
// signs it, and a rule that reaches for an infrastructure tier is
// refused at proposal regardless of who wrote it.
func TestAutomationTravelsThePlanPipeline(t *testing.T) {
	h := newHarness(t)
	ruleYAML := func(tier string) string {
		data, err := resource.Marshal(resource.Resource{
			ApiVersion: resource.ApiVersion, Kind: resource.KindAutomation,
			Metadata: resource.Metadata{Name: "breakfast-check", Owner: "household/mika"},
			Automation: &resource.AutomationSpec{
				Description: "Ask about breakfast when leaving without a meal logged",
				Trigger:     resource.AutomationTrigger{EventType: "person.left_home", Subject: "mika"},
				Conditions:  []resource.AutomationCondition{{Kind: "eventAbsent", EventType: "pantry.meal_logged", Window: "6h", SameSubject: true}},
				Action:      resource.AutomationAction{Tool: "notifications.notify", Tier: tier, Params: map[string]string{"message": "Breakfast?"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	// Infrastructure tier: 422 from policy, no plan record.
	var body map[string]any
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/plans",
		proposalRequest{Summary: "restart on arrival", Put: []string{ruleYAML("high")}}, &body); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("high-tier rule = %d (%v), want 422", resp.StatusCode, body)
	}

	// Valid Automation documents still require the exact-version routine path.
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/plans",
		proposalRequest{Summary: "breakfast check", Put: []string{ruleYAML("domestic")}}, &body); resp.StatusCode != http.StatusBadRequest || !strings.Contains(fmt.Sprint(body), "exact-version routine Plan") {
		t.Fatalf("legacy proposal was not refused clearly: %d %v", resp.StatusCode, body)
	}

}

// Replay runs a rule over history with no side effects (ADR-W024 A7): an
// approved rule by name, or a draft by document, from any read principal
// — and a new event pokes the engine.
func TestAutomationReplay(t *testing.T) {
	h := newHarness(t)
	h.addPublisher("publisher-home")
	pokes := 0
	h.s.OnHouseholdEvent = func() { pokes++ }

	base := time.Now().Add(-3 * time.Hour)
	send := func(id, typ, subject string, at time.Time) {
		t.Helper()
		env := household.Envelope{EventID: id, OccurredAt: at, Type: typ, Subject: subject}
		if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", env, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("emit %s = %d", id, resp.StatusCode)
		}
	}
	send("meal-1", "pantry.meal_logged", "mika", base)
	send("left-1", "person.left_home", "mika", base.Add(30*time.Minute))
	send("left-2", "person.left_home", "mika", base.Add(2*time.Hour+30*time.Minute))
	// A retried delivery does not poke: nothing new happened.
	h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events",
		household.Envelope{EventID: "left-2", OccurredAt: base.Add(2*time.Hour + 30*time.Minute), Type: "person.left_home", Subject: "mika"}, nil)
	if pokes != 3 {
		t.Fatalf("pokes = %d, want 3 (one per new event, none for the retry)", pokes)
	}

	draft := `apiVersion: wildstone.dev/v1
kind: Automation
metadata: { name: breakfast-check, owner: household/mika }
spec:
  description: Ask about breakfast when leaving without a meal logged
  trigger: { eventType: person.left_home, subject: mika }
  conditions:
    - { kind: eventAbsent, eventType: pantry.meal_logged, window: 1h, sameSubject: true }
  action: { tool: notifications.notify, tier: domestic, params: { message: "Breakfast?" } }
`
	var out replayResponse
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/household/automations/replay",
		replayRequest{Rule: draft}, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("replay draft = %d", resp.StatusCode)
	}
	if !out.Draft || out.Total != 2 || out.Fired != 1 || len(out.Violations) != 0 {
		t.Fatalf("draft replay = %+v", out)
	}
	if out.Verdicts[0].Fired || !out.Verdicts[1].Fired || out.Verdicts[1].EventID != "left-2" {
		t.Fatalf("verdicts = %+v", out.Verdicts)
	}
	// Nothing was written and nothing moved.
	var events []runtimedb.HouseholdEvent
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/household/events", nil, &events)
	if len(events) != 3 {
		t.Fatalf("replay wrote to the stream: %d events", len(events))
	}

	// A draft that policy would refuse still replays, and says so.
	bad := strings.Replace(draft, "tier: domestic", "tier: high", 1)
	h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/household/automations/replay", replayRequest{Rule: bad}, &out)
	if len(out.Violations) != 1 || out.Violations[0].Rule != "automation-action-tier" {
		t.Fatalf("policy on draft = %+v", out.Violations)
	}
	// An invalid draft is a 400; so is asking for both or neither.
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/household/automations/replay",
		replayRequest{Rule: strings.Replace(draft, "person.left_home", "left_home", 1)}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid draft = %d", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/household/automations/replay", replayRequest{}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("neither = %d", resp.StatusCode)
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/household/automations/replay", replayRequest{Name: "nope"}, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown name = %d", resp.StatusCode)
	}
	// A publisher cannot replay.
	if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/automations/replay", replayRequest{Rule: draft}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("publisher replay = %d", resp.StatusCode)
	}

	// Approve the rule (disabled), then replay it by name: a disabled
	// rule replays as if enabled, and the response says it is off.
	disabled := strings.Replace(draft, "  trigger:", "  disabled: true\n  trigger:", 1)
	seedLegacyAutomationFixture(t, h, disabled)
	if resp := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/household/automations/replay", replayRequest{Name: "breakfast-check"}, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("replay by name = %d", resp.StatusCode)
	}
	if out.Draft || out.Enabled || out.Total != 2 || out.Fired != 1 {
		t.Fatalf("named replay = %+v", out)
	}
}

// TestHouseholdQuestionAndAnswerLoop is the H4 deliverable end to end
// (ADR-W024): a rule fires, the domestic gateway sends a question with
// answer buttons, a household device taps one, and the answer lands back
// on the household stream correlated to the question that asked it — no
// chat surface, no polling, no second channel.
func TestHouseholdQuestionAndAnswerLoop(t *testing.T) {
	h := newHarness(t)
	h.addPublisher("publisher-home")

	// A fake ntfy that keeps the last publish.
	var published struct {
		Topic   string `json:"topic"`
		Message string `json:"message"`
		Actions []struct {
			Label   string            `json:"label"`
			URL     string            `json:"url"`
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		} `json:"actions"`
	}
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &published); err != nil {
			t.Errorf("ntfy body: %v", err)
		}
		w.Write([]byte(`{"id":"x"}`)) //nolint:errcheck
	}))
	defer ntfy.Close()

	// The reply token is an ordinary short-lived publisher token.
	if err := h.s.Principals.Create("publisher-replies", runtimedb.RolePublisher, ""); err != nil {
		t.Fatal(err)
	}
	ownExecutionFixture(t, h)
	gw := &domestic.Gateway{Authority: h.s.DB}
	gw.Register(&domestic.Notifier{Authority: h.s.DB,
		BaseURL:  ntfy.URL,
		ReplyURL: h.srv.URL + "/api/v1/household/events",
		MintReply: func(context.Context) (string, error) {
			return h.s.Principals.Mint("publisher-replies", "", time.Hour)
		},
	})
	h.s.Automation.Actions = gw

	rule := `apiVersion: wildstone.dev/v1
kind: Automation
metadata: { name: mail-check, owner: household/mika }
spec:
  description: Ask whether the mail was collected on arriving home
  trigger: { eventType: person.arrived_home, subject: mika }
  conditions:
    - { kind: cooldown, window: 20h }
  action:
    tool: notifications.notify
    tier: domestic
    params:
      topic: "household-{{ .Subject }}"
      message: "Did you get the mail?"
      replies: "Yes,No"
`
	seedLegacyAutomationFixture(t, h, rule)

	// The engine adopts the stream it starts on, so prime it with a
	// marker before the trigger the test cares about.
	emit := func(id, typ string, at time.Time) {
		t.Helper()
		env := household.Envelope{EventID: id, OccurredAt: at, Type: typ, Subject: "mika"}
		if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", env, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("emit %s = %d", id, resp.StatusCode)
		}
	}
	emit("marker", "household.marker", time.Now().Add(-time.Minute))
	if _, err := h.s.Automation.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Mika gets home. The rule fires and asks.
	emit("arrived-1", "person.arrived_home", time.Now().Add(-time.Minute))
	out, err := h.s.Automation.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Fired != 1 {
		t.Fatalf("tick = %+v", out)
	}
	if published.Topic != "household-mika" || published.Message != "Did you get the mail?" || len(published.Actions) != 2 {
		t.Fatalf("published = %+v", published)
	}

	// The household taps "Yes". The button is a plain HTTP request a
	// phone can make — so the test makes exactly that request, with
	// nothing but what the button carries.
	yes := published.Actions[0]
	req, err := http.NewRequest(yes.Method, yes.URL, strings.NewReader(yes.Body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range yes.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reply button = %d, want 201", resp.StatusCode)
	}

	// The answer is a fact on the same stream, correlated to the trigger
	// that asked, published by the reply principal and nobody else.
	var events []runtimedb.HouseholdEvent
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/household/events", nil, &events)
	var answered *runtimedb.HouseholdEvent
	for i := range events {
		if events[i].Type == domestic.AnsweredEventType {
			answered = &events[i]
		}
	}
	if answered == nil {
		t.Fatalf("no %s on the stream: %+v", domestic.AnsweredEventType, events)
	}
	if answered.Source != "publisher-replies" || answered.Subject != "mika" || answered.Entity != "mail-check" ||
		answered.CorrelationID != "arrived-1" || !strings.Contains(string(answered.Payload), `"answer":"Yes"`) {
		t.Fatalf("answer = %+v", answered)
	}
	// Everything the trigger caused carries the trigger's id as its
	// correlation, so the exchange reads as one thread: the decision,
	// the action, and the answer. The trigger itself is the origin and
	// carries no correlation of its own.
	thread := map[string]bool{}
	for _, e := range events {
		if e.CorrelationID == "arrived-1" {
			thread[e.Type] = true
		}
	}
	for _, want := range []string{"automation.fired", "automation.acted", domestic.AnsweredEventType} {
		if !thread[want] {
			t.Fatalf("no %s correlated to the trigger; thread = %v", want, thread)
		}
	}
	if len(thread) != 3 {
		t.Fatalf("correlated thread = %v, want exactly the decision, the action, and the answer", thread)
	}

	// Tapping the same button twice is one answer, not two.
	req2, _ := http.NewRequest(yes.Method, yes.URL, strings.NewReader(yes.Body))
	for k, v := range yes.Headers {
		req2.Header.Set(k, v)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second tap = %d, want 200 (a duplicate, not a new answer)", resp2.StatusCode)
	}
}

// TestHomeAssistantWireShapeIsAccepted posts the exact body the rendered
// Home Assistant adapter sends (ADR-W024 H5) — Jinja resolved the way
// Home Assistant would resolve it, as raw JSON on the wire rather than
// through this repo's own envelope type.
//
// The package test in keystone/vertical proves the configuration renders
// and agrees with what it declares; nothing there proves the intake
// accepts it. Both halves have to hold, and they are written in
// different places by different people, so this is where they meet.
func TestHomeAssistantWireShapeIsAccepted(t *testing.T) {
	h := newHarness(t)
	token := h.addPublisher("publisher-home-automation")

	// Home Assistant's isoformat(): microseconds and a "+00:00" offset,
	// not Go's "Z".
	occurred := time.Now().Add(-30 * time.Second).UTC().Format("2006-01-02T15:04:05.000000-07:00")
	if !strings.HasSuffix(occurred, "+00:00") {
		t.Fatalf("test fixture is not shaped like Home Assistant's isoformat(): %q", occurred)
	}
	body := `{"eventId": "person.mika:person.arrived_home:1757102400", "occurredAt": "` + occurred + `",
	          "type": "person.arrived_home", "subject": "mika",
	          "payload": {"entity": "person.mika", "from": "not_home", "to": "home"}}`

	post := func() *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/household/events", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post()
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("Home Assistant's wire shape = %d, want 201", resp.StatusCode)
	}
	// Home Assistant's rest_command has no retry of its own, but a
	// household running two instances, or an operator replaying, must
	// not double-count an arrival. The id is derived from the state
	// transition, so a repeat is the same fact.
	resp = post()
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repeat delivery = %d, want 200 (a duplicate, not a second arrival)", resp.StatusCode)
	}

	var events []runtimedb.HouseholdEvent
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/household/events", nil, &events)
	if len(events) != 1 {
		t.Fatalf("stream has %d events, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.Type != "person.arrived_home" || e.Subject != "mika" || e.Source != "publisher-home-automation" {
		t.Fatalf("event = %+v", e)
	}
	// Compact on the way back out: the encoder normalises the JSON Home
	// Assistant sent, and the fields are what matter, not its spacing.
	if !strings.Contains(string(e.Payload), `"entity":"person.mika"`) ||
		!strings.Contains(string(e.Payload), `"from":"not_home"`) {
		t.Fatalf("payload = %s", e.Payload)
	}
	// The event id survives the response encoder intact — it is the
	// household's dedupe key, and a redacted one is useless.
	if e.EventID != "person.mika:person.arrived_home:1757102400" {
		t.Fatalf("event id = %q", e.EventID)
	}
}

// phrasingProvider is a fake LLM that records what it was asked and
// returns a scripted line.
type phrasingProvider struct {
	requests []provider.ChatRequest
	reply    string
}

func (p *phrasingProvider) Name() string { return "fake" }
func (p *phrasingProvider) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	p.requests = append(p.requests, req)
	return &provider.ChatResponse{Text: p.reply}, nil
}

// TestBreakfastEscalationEndToEnd is the ADR-W024 §5 flow, whole: Home
// Assistant's departure event arrives, the rule finds no meal logged,
// the engine gathers the last few breakfasts, the Butler phrases one
// informed question from them, and the domestic gateway sends it — with
// the Butler having had no tools, no say in whether the household was
// contacted, and no way to change who was asked.
func TestBreakfastEscalationEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.addPublisher("publisher-home")

	var published struct {
		Topic   string `json:"topic"`
		Message string `json:"message"`
	}
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &published) //nolint:errcheck
		w.Write([]byte(`{"id":"x"}`))    //nolint:errcheck
	}))
	defer ntfy.Close()

	ownExecutionFixture(t, h)
	gw := &domestic.Gateway{Authority: h.s.DB}
	gw.Register(&domestic.Notifier{Authority: h.s.DB, BaseURL: ntfy.URL})
	llm := &phrasingProvider{reply: "You usually have porridge about now — did you eat before heading out?"}
	phraser := &native.Phraser{Provider: llm}

	h.s.Automation.Actions = gw
	h.s.Automation.Escalator = automation.EscalatorFunc(func(ctx context.Context, req automation.EscalationRequest) (string, error) {
		facts := make([]native.Fact, 0, len(req.Facts))
		for _, f := range req.Facts {
			facts = append(facts, native.Fact{Label: f.Label, Content: f.Content})
		}
		return phraser.Phrase(ctx, native.PhraseRequest{
			Rule: req.Rule, Instruction: req.Instruction, Fallback: req.Fallback, Facts: facts,
		})
	})

	rule := `apiVersion: wildstone.dev/v1
kind: Automation
metadata: { name: breakfast-check, owner: household/mika }
spec:
  description: Ask about breakfast when leaving home without a meal logged
  trigger: { eventType: person.left_home, subject: mika }
  conditions:
    - { kind: eventAbsent, eventType: pantry.meal_logged, window: 6h, sameSubject: true }
  context:
    - { kind: recentEvents, eventType: pantry.meal_logged, sameSubject: true, limit: 3 }
  escalation:
    prompt: "Ask, in one sentence, whether today matched the usual breakfast."
    into: message
  action:
    tool: notifications.notify
    tier: domestic
    params:
      topic: "household-{{ .Subject }}"
      message: "Did you have breakfast?"
`
	seedLegacyAutomationFixture(t, h, rule)

	emit := func(id, typ string, at time.Time) {
		t.Helper()
		env := household.Envelope{EventID: id, OccurredAt: at, Type: typ, Subject: "mika",
			Payload: json.RawMessage(`{"meal":"porridge"}`)}
		if resp := h.do(runtimedb.RolePublisher, http.MethodPost, "/api/v1/household/events", env, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("emit %s = %d", id, resp.StatusCode)
		}
	}
	now := time.Now()
	emit("meal-1", "pantry.meal_logged", now.Add(-48*time.Hour))
	emit("meal-2", "pantry.meal_logged", now.Add(-24*time.Hour))
	if _, err := h.s.Automation.Tick(t.Context()); err != nil { // adopt
		t.Fatal(err)
	}
	emit("left-1", "person.left_home", now.Add(-time.Minute))

	out, err := h.s.Automation.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Fired != 1 {
		t.Fatalf("tick = %+v", out)
	}

	// The household got the Butler's sentence, at the address the RULE
	// chose — not one the model had any part in.
	if published.Topic != "household-mika" || published.Message != llm.reply {
		t.Fatalf("published = %+v", published)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("asked the model %d times", len(llm.requests))
	}
	req := llm.requests[0]
	// The containment claim, end to end: no tools were on the table.
	if len(req.Tools) != 0 {
		t.Fatalf("the escalation offered %d tool(s)", len(req.Tools))
	}
	// The packet carried the rule's instruction and the two breakfasts
	// it asked for, as untrusted data.
	user := req.Messages[0].Text
	if !strings.Contains(user, "Ask, in one sentence") || !strings.Contains(user, "Did you have breakfast?") {
		t.Fatalf("packet lost the instruction or the fallback:\n%s", user)
	}
	if !strings.Contains(user, "<untrusted_data") || strings.Count(user, "porridge") < 2 {
		t.Fatalf("packet did not carry the gathered breakfasts as untrusted data:\n%s", user)
	}

	// And the whole exchange is one correlated thread on the household
	// stream: the decision, the escalation, and the action.
	var events []runtimedb.HouseholdEvent
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/household/events", nil, &events)
	thread := map[string]bool{}
	for _, e := range events {
		if e.CorrelationID == "left-1" {
			thread[e.Type] = true
		}
	}
	for _, want := range []string{"automation.fired", "automation.escalated", "automation.acted"} {
		if !thread[want] {
			t.Fatalf("no %s correlated to the departure; thread = %v", want, thread)
		}
	}
}

// Existing Statefile Automations remain executable after upgrade. These fixtures
// exercise that compatibility; new authoring is covered by exact-version review tests.
func seedLegacyAutomationFixture(t *testing.T, h *harness, document string) {
	t.Helper()
	res, err := resource.Unmarshal([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.State.Put(res); err != nil {
		t.Fatal(err)
	}
	if err = h.s.State.Commit("test", "legacy-fixture", "Existing Automation before routine review"); err != nil {
		t.Fatal(err)
	}
}
