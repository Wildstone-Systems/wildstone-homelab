package automation

import (
	"context"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReviewedRoutineCannotRunAStaleStatefileCopy(t *testing.T) {
	original := rule("old-rule")
	e, db := harness(t, original, rule("unrelated"))
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	raw, err := resource.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "morning", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	// Simulate the migration's unchanged, already-active legacy authorization.
	if _, err = db.ExecForTest("UPDATE household_routines SET approved_revision=revision,state='enabled' WHERE name='morning'"); err != nil {
		t.Fatal(err)
	}
	if rules, err := e.Rules(); err != nil || len(rules) != 2 {
		t.Fatalf("grandfathered routine disappeared: %v %v", rules, err)
	}
	if _, err = db.ExecForTest("UPDATE household_routines SET state='trial' WHERE name='morning'"); err != nil {
		t.Fatal(err)
	}
	if rules, err := e.Rules(); err != nil || len(rules) != 2 {
		t.Fatalf("grandfathered trial disappeared: %v %v", rules, err)
	}
	pending, err := db.SubmitRoutinePlan("morning", "mika", "", string(runtimedb.ButlerAudienceShared), "notify", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, reject := range []bool{false, true} {
		if reject {
			if err = db.TransitionPlan(pending, runtimedb.PlanRejected, "mika"); err != nil {
				t.Fatal(err)
			}
		}
		if rules, err := e.Rules(); err != nil || len(rules) != 2 {
			t.Fatalf("unchanged legacy routine disappeared after submission/rejection: %v %v", rules, err)
		}
	}
	// Even a renamed edited draft must suppress its older Statefile identity.
	changed := rule("new-rule")
	changedRaw, err := resource.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.EditHouseholdRoutine("morning", string(changedRaw), "mika", "rename"); err != nil {
		t.Fatal(err)
	}
	for _, approve := range []bool{false, true} {
		if approve {
			id, err := db.SubmitRoutinePlan("morning", "mika", "", string(runtimedb.ButlerAudienceShared), "notify", string(changedRaw))
			if err != nil {
				t.Fatal(err)
			}
			if err = db.ApproveRoutinePlan(id, "mika"); err != nil {
				t.Fatal(err)
			}
		}
		actions := &fakeActions{}
		e.Actions = actions
		if err := e.act(context.Background(), Decision{Fired: true, Rule: "old-rule"}, runtimedb.HouseholdEvent{}, original); err == nil || len(actions.calls) != 0 {
			t.Fatalf("stale in-flight decision dispatched: %v %+v", err, actions.calls)
		}
		rules, err := e.Rules()
		if err != nil || len(rules) != 1 || rules[0].Metadata.Name != "unrelated" {
			t.Fatalf("stale reviewed rule executed (approved=%v): %v %v", approve, rules, err)
		}
	}
}

func TestRoutineEditDuringDispatchClaimPreventsTheEffect(t *testing.T) {
	original := rule("claim-race")
	e, db := harness(t, original)
	if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	raw, err := resource.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "claim-race", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecForTest("UPDATE household_routines SET approved_revision=revision,state='enabled' WHERE name='claim-race'"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecForTest(`CREATE TRIGGER edit_during_claim AFTER INSERT ON household_events WHEN NEW.type='automation.acted' BEGIN UPDATE household_routines SET state='draft',revision=revision+1 WHERE name='claim-race'; END`); err != nil {
		t.Fatal(err)
	}
	actions := &fakeActions{}
	e.Actions = actions
	err = e.act(context.Background(), Decision{Fired: true, Rule: "claim-race", EventSeq: 1, Action: "notifications.notify", Tier: "domestic"}, runtimedb.HouseholdEvent{Seq: 1, EventID: "race", OccurredAt: t0}, original)
	if err == nil || len(actions.calls) != 0 {
		t.Fatalf("claim-time edit dispatched old rule: %v %+v", err, actions.calls)
	}
	events, qerr := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Entity: "claim-race", Source: Source, Limit: 20})
	if qerr != nil {
		t.Fatal(qerr)
	}
	failed := false
	for _, event := range events {
		failed = failed || event.Type == TypeActionFailed
	}
	if !failed {
		t.Fatal("withheld action has no failure outcome")
	}

}

func TestRegistryLabelDoesNotSuppressUnrelatedRule(t *testing.T) {
	e, db := harness(t, rule("registry-label"), rule("actual-rule"))
	raw, err := resource.Marshal(rule("actual-rule"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "registry-label", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
		t.Fatal(err)
	}
	rules, err := e.Rules()
	if err != nil || len(rules) != 1 || rules[0].Metadata.Name != "registry-label" {
		t.Fatalf("unrelated rule suppressed: %v %v", rules, err)
	}
}

func TestRoutineEditAtGatewayOrExecutorPreventsDelivery(t *testing.T) {
	for _, boundary := range []string{"unchanged", "gateway", "executor", "redirect"} {
		t.Run(boundary, func(t *testing.T) {
			original := rule("boundary-race")
			original.Automation.Action.Params = map[string]string{"topic": "household", "message": "check in", "replies": "yes,no"}
			e, db := harness(t, original)
			if _, err := db.AcquireCoordinator("boundary-test"); err != nil {
				t.Fatal(err)
			}
			if err := db.RecordButlerMember("mika", runtimedb.RoleOwner, "", ""); err != nil {
				t.Fatal(err)
			}
			if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
				t.Fatal(err)
			}
			raw, err := resource.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.CreateHouseholdRoutine(runtimedb.HouseholdRoutine{Name: "boundary-race", Ownership: runtimedb.RoutineWildstone, Author: "mika", Document: string(raw)}); err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecForTest("UPDATE household_routines SET approved_revision=revision,state='enabled' WHERE name='boundary-race'"); err != nil {
				t.Fatal(err)
			}
			edit := func() {
				if _, err := db.EditHouseholdRoutine("boundary-race", string(raw)+"\n# changed", "mika", "changed"); err != nil {
					t.Fatal(err)
				}
			}
			delivered := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { delivered++; w.Write([]byte(`{}`)) }))
			defer target.Close()
			baseURL := target.URL
			if boundary == "redirect" {
				redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					edit()
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				}))
				defer redirect.Close()
				baseURL = redirect.URL
			}
			gateway := &domestic.Gateway{Authority: db, Declared: func(string, string) (string, bool) {
				if boundary == "gateway" {
					edit()
				}
				return domestic.TierDomestic, true
			}}
			gateway.Register(&domestic.Notifier{Authority: db, BaseURL: baseURL, ReplyURL: "https://home.test/reply", MintReply: func(context.Context) (string, error) {
				if boundary == "executor" {
					edit()
				}
				return "fixture", nil
			}})
			e.Actions = gateway
			if err = e.act(context.Background(), Decision{Fired: true, Rule: "boundary-race", EventSeq: 1, Action: "notifications.notify", Tier: "domestic"}, runtimedb.HouseholdEvent{Seq: 1, EventID: "boundary-race", OccurredAt: t0}, original); err != nil {
				t.Fatal(err)
			}
			want := 0
			if boundary == "unchanged" {
				want = 1
			}
			if delivered != want {
				t.Fatalf("%s delivered %d, want %d", boundary, delivered, want)
			}
			if boundary != "unchanged" {
				events, err := db.QueryHouseholdEvents(runtimedb.HouseholdQuery{Entity: "boundary-race", Source: Source, Limit: 20})
				if err != nil {
					t.Fatal(err)
				}
				failed := false
				for _, event := range events {
					failed = failed || event.Type == TypeActionFailed
				}
				if !failed {
					t.Fatal("withheld dispatch has no failure outcome")
				}
			}
		})
	}
}
