package runtimedb

// P14's evidence (ADR-W030): local-only information never reaches a
// cloud route, fallback never widens tools, an unready or QA-only
// fallback cannot inherit action authority, and deterministic work
// continues with every model unavailable.

import (
	"errors"
	"strings"
	"testing"
)

func routes(t *testing.T, db *DB) {
	t.Helper()
	for _, r := range []ModelRoute{
		{Name: "kitchen-box", Kind: RouteLocal, Provider: "ollama", Model: "llama3",
			Readiness: RouteReady, MaxConcurrency: 1, Priority: 10, Enabled: true},
		{Name: "anthropic", Kind: RouteCloud, Provider: "anthropic", Model: "opus",
			Readiness: RouteReady, MaxConcurrency: 4, Priority: 200, Enabled: true},
	} {
		if err := db.UpsertModelRoute(r, "root-admin"); err != nil {
			t.Fatalf("UpsertModelRoute(%s): %v", r.Name, err)
		}
	}
}

// The first and absolute rule: local-only material reaches a local route
// and nothing else, however busy or broken that route is.
func TestLocalOnlyNeverReachesACloudRoute(t *testing.T) {
	db := butlerTestDB(t)
	routes(t, db)

	// The local route is the natural choice anyway.
	sel, err := db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-1"})
	if err != nil {
		t.Fatalf("SelectModelRoute: %v", err)
	}
	if sel.Route.Kind != RouteLocal {
		t.Fatalf("local-only material went to a %s route", sel.Route.Kind)
	}

	// Now it is busy — its concurrency is one and that slot is taken.
	_, err = db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-2"})
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("a busy local route escalated to the cloud: %v", err)
	}
	if !strings.Contains(err.Error(), "must not leave the house") {
		t.Fatalf("the refusal does not say why: %v", err)
	}

	// And with the local route down entirely, it still does not
	// escalate.
	db.ReleaseModelRoute(sel.LeaseSeq)
	if err := db.RecordRouteReadiness("kitchen-box", RouteUnavailable, "the box is off"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-3"}); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("a dead local route escalated to the cloud: %v", err)
	}
	// Material that is allowed out does escalate, which is what makes
	// the previous case a decision rather than an accident.
	if sel, err := db.SelectModelRoute(RouteRequest{
		Locality: DataCloudAllowed, Holder: "task-4", Attempt: 1,
	}); err != nil || sel.Route.Kind != RouteCloud {
		t.Fatalf("cloud-allowed material did not escalate: %+v, %v", sel, err)
	}
}

// A fallback to an unready or QA-only route carries no action
// authority, and the caller is told to recompute from the verdict.
func TestFallbackNeverWidensAuthority(t *testing.T) {
	db := butlerTestDB(t)
	if err := db.UpsertModelRoute(ModelRoute{
		Name: "kitchen-box", Kind: RouteLocal, Provider: "ollama",
		Readiness: RouteQAOnly, MaxConcurrency: 2, Priority: 10, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRouteReadiness("kitchen-box", RouteQAOnly, "it invented a tool name"); err != nil {
		t.Fatal(err)
	}
	sel, err := db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Verdict != VerdictQAOnlyConst {
		t.Fatalf("a QA-only route produced verdict %q", sel.Verdict)
	}
	if sel.Route.CarriesActionAuthority() {
		t.Fatal("a QA-only route carries action authority")
	}
	if sel.Reason == "" {
		t.Fatal("a narrowed session does not say why it is narrowed")
	}

	// An unprobed route is not a ready one. It has not been checked,
	// which is different from having been checked and passed.
	if err := db.UpsertModelRoute(ModelRoute{
		Name: "new-box", Kind: RouteLocal, Provider: "ollama", MaxConcurrency: 1,
		Priority: 5, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	sel, err = db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-2"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Route.Name != "new-box" {
		t.Fatalf("selection = %s, want the higher-priority route", sel.Route.Name)
	}
	if sel.Verdict != VerdictQAOnlyConst {
		t.Fatal("an unprobed route was trusted with action authority")
	}
	// A route reported anything but ready has to say why.
	if err := db.RecordRouteReadiness("new-box", RouteUnavailable, ""); err == nil {
		t.Fatal("a route was reported unavailable with no reason")
	}
}

// Escalation is bounded: the house does not try every model it has to
// answer one question.
func TestEscalationIsBounded(t *testing.T) {
	db := butlerTestDB(t)
	routes(t, db)
	_, err := db.SelectModelRoute(RouteRequest{
		Locality: DataCloudAllowed, Holder: "task-1", Attempt: 3, MaxEscalations: 2,
	})
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("escalation past the bound = %v", err)
	}
	if !strings.Contains(err.Error(), "decision for a person") {
		t.Fatalf("the bound's refusal does not say who decides: %v", err)
	}
}

// Background processing is local-first and does not escalate: nobody is
// waiting for it.
func TestBackgroundWorkStaysLocal(t *testing.T) {
	db := butlerTestDB(t)
	routes(t, db)
	// The local route's only slot goes to an interactive request.
	if _, err := db.SelectModelRoute(RouteRequest{
		Locality: DataCloudAllowed, Job: JobInteractive, Holder: "person",
	}); err != nil {
		t.Fatal(err)
	}
	// Background work waits rather than going to the cloud, even though
	// the material is allowed there.
	_, err := db.SelectModelRoute(RouteRequest{
		Locality: DataCloudAllowed, Job: JobBackground, Holder: "indexer",
	})
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("background work escalated to a cloud route: %v", err)
	}
	if !strings.Contains(err.Error(), "local routes only") {
		t.Fatalf("the refusal does not explain the local-first policy: %v", err)
	}
}

// A capacity slot is freed when the caller finishes, and when it goes
// away without finishing.
func TestRouteCapacityIsReleasedAndSwept(t *testing.T) {
	db := butlerTestDB(t)
	routes(t, db)
	sel, err := db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-2"}); !errors.Is(err, ErrNoRoute) {
		t.Fatal("a one-slot route answered twice at once")
	}
	if err := db.ReleaseModelRoute(sel.LeaseSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-3"}); err != nil {
		t.Fatalf("a released slot was not reusable: %v", err)
	}

	// A caller that never came back does not hold the only slot forever.
	if _, err := db.ExecForTest(
		`UPDATE model_route_leases SET expires_at = '2000-01-01T00:00:00Z' WHERE released_at = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SelectModelRoute(RouteRequest{Locality: DataLocalOnly, Holder: "task-4"}); err != nil {
		t.Fatalf("an abandoned lease held the slot: %v", err)
	}
}

// Locality that was not stated is treated as the strict one.
func TestUnstatedLocalityIsTreatedAsLocalOnly(t *testing.T) {
	db := butlerTestDB(t)
	if err := db.UpsertModelRoute(ModelRoute{
		Name: "anthropic", Kind: RouteCloud, Provider: "anthropic",
		Readiness: RouteReady, MaxConcurrency: 4, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	// Only a cloud route exists, and an unlabelled request does not
	// reach it: defaulting the other way would make every unlabelled
	// request eligible to leave the house.
	if _, err := db.SelectModelRoute(RouteRequest{Holder: "task-1"}); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("an unlabelled request reached a cloud route: %v", err)
	}
}

// A route's kind is the data boundary and cannot be edited.
func TestRouteKindIsImmutable(t *testing.T) {
	db := butlerTestDB(t)
	routes(t, db)
	if _, err := db.ExecForTest(
		`UPDATE model_routes SET kind = 'local' WHERE name = 'anthropic'`); err == nil {
		t.Fatal("a cloud route became a local one, moving the data boundary silently")
	}
}

// Every route down is not a broken house: deterministic work continues,
// and only model-dependent work waits.
func TestEveryRouteDownIsAWaitNotAFailure(t *testing.T) {
	db := butlerTestDB(t)
	routes(t, db)
	for _, name := range []string{"kitchen-box", "anthropic"} {
		if err := db.RecordRouteReadiness(name, RouteUnavailable, "down"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := db.SelectModelRoute(RouteRequest{Locality: DataCloudAllowed, Holder: "task-1"})
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("selection with everything down = %v, want ErrNoRoute", err)
	}
	// ErrNoRoute is a wait, and the caller can tell it apart from a
	// failure — which is what lets a routine run its deterministic steps
	// and skip only its phrasing.
	if !strings.Contains(err.Error(), "no model route") {
		t.Fatalf("the error does not read as an absence: %v", err)
	}
}
