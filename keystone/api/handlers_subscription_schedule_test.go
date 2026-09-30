package api

// Scheduled Agent subscriptions over the public API (epic #326 S03,
// #456): one surface for Console, CLI and Butler. A Person sees and
// controls only the subscriptions bound to their own Personal source; an
// Agent may narrow (pause, disable) but never widen (resume, enable,
// cadence); an edit stops dispatch at once and is approved as that
// exact version through the ordinary Plan path.

import (
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

type scheduledAPI struct {
	h                      *harness
	owner, sam, kim, agent string
	subID                  int64
}

func newScheduledAPI(t *testing.T) *scheduledAPI {
	t.Helper()
	h := newHarness(t)
	a := &scheduledAPI{h: h}
	a.owner = h.console(runtimedb.RoleAdmin, "mika")
	a.sam = h.console(runtimedb.RoleOperator, "sam")
	a.kim = h.console(runtimedb.RoleOperator, "kim")
	tok, err := h.s.Principals.Mint("butler", "sam", 0)
	if err != nil {
		t.Fatal(err)
	}
	a.agent = tok
	if err := h.s.DB.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.CreateButler(runtimedb.Butler{
		Name: "alfred", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box",
	}, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.CreateConnection(runtimedb.IntegrationConnection{
		Name: "mail-sam", Adapter: "mail", Ownership: runtimedb.OwnershipPersonal, Owner: "sam", CreatedBy: "sam",
	}); err != nil {
		t.Fatal(err)
	}
	// Proposed the way the Agent's own propose path files it, approved
	// by the Owner over HTTP.
	view, err := proposals.FileSubscription(h.s.proposalDeps(), principal.Identity{Name: "butler", Role: runtimedb.RoleButler, ActingFor: "sam"},
		proposals.SubscriptionRequest{
			Butler: "alfred", MatchKind: "mail.changes", CapabilitySet: []string{"peek", "mail.message"},
			Trigger: runtimedb.SubscriptionTriggerSchedule, Source: "mail-sam",
		})
	if err != nil {
		t.Fatal(err)
	}
	if res := h.doToken(a.owner, http.MethodPost, "/api/v1/plans/"+view.ID+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("approving the scheduled subscription = %d", res.StatusCode)
	}
	subs, err := h.s.DB.ListAllAgentSubscriptions()
	if err != nil || len(subs) != 1 {
		t.Fatalf("subscriptions = %+v, %v", subs, err)
	}
	a.subID = subs[0].ID
	return a
}

func (a *scheduledAPI) path(suffix string) string {
	return "/api/v1/agent-subscriptions/" + itoa(a.subID) + suffix
}

func TestScheduledSubscriptionApprovedFromAPlanIsPersonBound(t *testing.T) {
	a := newScheduledAPI(t)
	var v subscriptionView
	if res := a.h.doToken(a.sam, http.MethodGet, a.path(""), nil, &v); res.StatusCode != http.StatusOK {
		t.Fatalf("sam reading their subscription = %d", res.StatusCode)
	}
	if v.Trigger != "schedule" || v.Cadence != "15m0s" || v.Person != "sam" || v.Source != "mail-sam" {
		t.Fatalf("view = %+v", v)
	}
	if v.Schedule == nil || v.Checkpoint == nil || v.Checkpoint.Version != 0 || v.AwaitingApproval {
		t.Fatalf("schedule/checkpoint = %+v / %+v", v.Schedule, v.Checkpoint)
	}
}

func TestScheduledSubscriptionIsVisibleOnlyToItsPersonAndOwners(t *testing.T) {
	a := newScheduledAPI(t)
	for who, want := range map[string]int{a.owner: 1, a.sam: 1, a.kim: 0} {
		var list struct {
			Subscriptions []subscriptionView `json:"subscriptions"`
		}
		if res := a.h.doToken(who, http.MethodGet, "/api/v1/agent-subscriptions", nil, &list); res.StatusCode != http.StatusOK {
			t.Fatalf("list = %d", res.StatusCode)
		}
		if len(list.Subscriptions) != want {
			t.Fatalf("listed %d, want %d", len(list.Subscriptions), want)
		}
	}
	if res := a.h.doToken(a.kim, http.MethodGet, a.path(""), nil, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another Person reading it = %d, want 404", res.StatusCode)
	}
	if res := a.h.doToken(a.kim, http.MethodPost, a.path("/control"), map[string]any{"action": "pause"}, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another Person pausing it = %d, want 404", res.StatusCode)
	}
}

func TestScheduledSubscriptionControlNarrowsForAgentsAndWidensOnlyForPeople(t *testing.T) {
	a := newScheduledAPI(t)
	var v subscriptionView
	if res := a.h.doToken(a.agent, http.MethodPost, a.path("/control"), map[string]any{"action": "pause", "reason": "asked to stop"}, &v); res.StatusCode != http.StatusOK {
		t.Fatalf("Agent pausing for its Person = %d", res.StatusCode)
	}
	if v.Schedule == nil || v.Schedule.Hold != runtimedb.SubscriptionHoldPaused {
		t.Fatalf("after pause: %+v", v.Schedule)
	}
	for _, action := range []string{"resume", "cadence", "enable"} {
		if res := a.h.doToken(a.agent, http.MethodPost, a.path("/control"), map[string]any{"action": action, "cadence": "30m"}, nil); res.StatusCode != http.StatusForbidden {
			t.Fatalf("Agent %s = %d, want 403", action, res.StatusCode)
		}
	}
	var resumed subscriptionView
	if res := a.h.doToken(a.sam, http.MethodPost, a.path("/control"), map[string]any{"action": "resume"}, &resumed); res.StatusCode != http.StatusOK || resumed.Schedule.Hold != "" {
		t.Fatalf("sam resuming = %d %+v", res.StatusCode, resumed.Schedule)
	}
	if res := a.h.doToken(a.sam, http.MethodPost, a.path("/control"), map[string]any{"action": "cadence", "cadence": "1m"}, nil); res.StatusCode != http.StatusConflict {
		t.Fatalf("a cadence below the floor = %d, want 409", res.StatusCode)
	}
	if res := a.h.doToken(a.sam, http.MethodPost, a.path("/control"), map[string]any{"action": "cadence", "cadence": "30m"}, &v); res.StatusCode != http.StatusOK || v.Cadence != "30m0s" {
		t.Fatalf("sam setting 30m = %d %q", res.StatusCode, v.Cadence)
	}
	if res := a.h.doToken(a.sam, http.MethodPost, a.path("/control"), map[string]any{"action": "disable"}, &v); res.StatusCode != http.StatusOK || v.Enabled {
		t.Fatalf("sam disabling = %d enabled=%v", res.StatusCode, v.Enabled)
	}
	if res := a.h.doToken(a.sam, http.MethodPost, a.path("/control"), map[string]any{"action": "enable"}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a non-Owner re-enabling = %d, want 403", res.StatusCode)
	}
	if res := a.h.doToken(a.owner, http.MethodPost, a.path("/control"), map[string]any{"action": "enable"}, &v); res.StatusCode != http.StatusOK || !v.Enabled {
		t.Fatalf("Owner re-enabling = %d enabled=%v", res.StatusCode, v.Enabled)
	}
}

func TestScheduledSubscriptionEditIsApprovedAsThatExactVersion(t *testing.T) {
	a := newScheduledAPI(t)
	if res := a.h.doToken(a.sam, http.MethodPut, a.path(""), map[string]any{"capabilities": []string{"peek", "calendar.create_event"}}, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("widening capabilities by edit = %d, want 400", res.StatusCode)
	}
	var edited struct {
		Subscription subscriptionView `json:"subscription"`
		Plan         struct {
			ID string `json:"id"`
		} `json:"plan"`
	}
	if res := a.h.doToken(a.sam, http.MethodPut, a.path(""), map[string]any{"match": map[string]any{"folders": []string{"INBOX", "School"}}}, &edited); res.StatusCode != http.StatusOK {
		t.Fatalf("sam editing = %d", res.StatusCode)
	}
	if !edited.Subscription.AwaitingApproval || edited.Subscription.Version != 2 || edited.Plan.ID == "" {
		t.Fatalf("edit = %+v", edited)
	}
	sub, err := a.h.s.DB.GetAgentSubscription(a.subID)
	if err != nil || sub.Dispatchable() {
		t.Fatalf("an unapproved edit is dispatchable: %+v %v", sub, err)
	}
	// sam cannot approve their own edit: only the Owner decides Plans.
	if res := a.h.doToken(a.sam, http.MethodPost, "/api/v1/plans/"+edited.Plan.ID+"/approve", nil, nil); res.StatusCode == http.StatusOK {
		t.Fatal("a Member approved a subscription edit")
	}
	if res := a.h.doToken(a.owner, http.MethodPost, "/api/v1/plans/"+edited.Plan.ID+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("Owner approving the edit = %d", res.StatusCode)
	}
	sub, err = a.h.s.DB.GetAgentSubscription(a.subID)
	if err != nil || !sub.Dispatchable() || sub.ApprovedVersion != 2 || sub.MatchJSON != `{"folders":["INBOX","School"]}` {
		t.Fatalf("after approval: %+v %v", sub, err)
	}
}

// TestSupersededSubscriptionEditPlanEnablesNothing: two edits, the
// older Plan approved last — it names version 2 of a row now at 3, and
// approval refuses rather than enabling content the Owner never saw.
func TestSupersededSubscriptionEditPlanEnablesNothing(t *testing.T) {
	a := newScheduledAPI(t)
	var first, second struct {
		Plan struct {
			ID string `json:"id"`
		} `json:"plan"`
	}
	a.h.doToken(a.sam, http.MethodPut, a.path(""), map[string]any{"audience": "aud_a"}, &first)
	a.h.doToken(a.sam, http.MethodPut, a.path(""), map[string]any{"audience": "aud_b"}, &second)
	if res := a.h.doToken(a.owner, http.MethodPost, "/api/v1/plans/"+first.Plan.ID+"/approve", nil, nil); res.StatusCode != http.StatusConflict {
		t.Fatalf("approving a superseded edit = %d, want 409", res.StatusCode)
	}
	if sub, _ := a.h.s.DB.GetAgentSubscription(a.subID); sub.Dispatchable() {
		t.Fatal("a superseded edit's approval enabled the row")
	}
}
