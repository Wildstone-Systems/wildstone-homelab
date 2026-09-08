package runtimedb

import (
	"strings"
	"testing"
)

// The bug this is written from: ListPlans deliberately blanks plan_json,
// and installedVersions() read it anyway. Every unmarshal failed, the
// version map was always empty, and the Console's "this capability is
// behind the catalog" (F91) reported nothing on any real deployment —
// shipped, tested against a fake, and inert.
//
// This asserts against the real schema, which is the only place the
// difference is visible.
func TestListPlansOmitsJSONAndAppliedPlanJSONReturnsIt(t *testing.T) {
	db := open(t)
	doc := `{"summary":"enable capability x","origin":{"package":"pkg-x","version":"1.2.3"}}`
	id, err := db.InsertPlan("admin", "", "enable capability x", "low", doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []PlanState{PlanApproved, PlanApplying, PlanApplied} {
		if err := db.TransitionPlan(id, to, "admin"); err != nil {
			t.Fatal(err)
		}
	}

	// The listing does not carry it. That is deliberate, and stated here
	// so a caller cannot rediscover it by accident.
	recs, err := db.ListPlans(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want one plan, got %d", len(recs))
	}
	if recs[0].PlanJSON != "" {
		t.Error("ListPlans is documented as omitting plan_json; something now depends on it carrying one")
	}

	// This is where origins come from.
	docs, err := db.AppliedPlanJSON(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("want one applied plan document, got %d", len(docs))
	}
	if !strings.Contains(docs[0], `"package":"pkg-x"`) {
		t.Errorf("the stored origin did not come back: %s", docs[0])
	}
}

// Only applied plans: a proposal that failed or was superseded never
// described what is RUNNING.
func TestAppliedPlanJSONIgnoresPlansThatNeverApplied(t *testing.T) {
	db := open(t)
	if _, err := db.InsertPlan("admin", "", "proposed only", "low", `{"origin":{"package":"never"}}`); err != nil {
		t.Fatal(err)
	}
	failed, err := db.InsertPlan("admin", "", "failed one", "low", `{"origin":{"package":"failed"}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []PlanState{PlanApproved, PlanApplying, PlanFailed} {
		if err := db.TransitionPlan(failed, to, "admin"); err != nil {
			t.Fatal(err)
		}
	}

	docs, err := db.AppliedPlanJSON(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Errorf("only applied plans describe what is running, got %v", docs)
	}
}

// Newest first, so a caller walking backwards lets the newest install win.
func TestAppliedPlanJSONIsNewestFirst(t *testing.T) {
	db := open(t)
	for _, name := range []string{"first", "second"} {
		id, err := db.InsertPlan("admin", "", name, "low", `{"origin":{"package":"`+name+`"}}`)
		if err != nil {
			t.Fatal(err)
		}
		for _, to := range []PlanState{PlanApproved, PlanApplying, PlanApplied} {
			if err := db.TransitionPlan(id, to, "admin"); err != nil {
				t.Fatal(err)
			}
		}
	}
	docs, err := db.AppliedPlanJSON(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || !strings.Contains(docs[0], "second") {
		t.Fatalf("want newest first, got %v", docs)
	}
}
