package runtimedb

// What a model route is, now that nothing selects between them
// (ADR-W031 A2, work package B7).
//
// A route used to be one of several a router might pick. It is now the
// model a Butler is ASSIGNED to, so the claims worth testing are about
// a route itself rather than about choosing one: its local/cloud kind
// is a boundary that cannot move, and its probe verdict decides what a
// session using it may do.
//
// The claims that went with the router — locality checked at selection,
// capacity, escalation, background-work ordering — are gone with it.
// Locality is now proven where it actually lives: material given to a
// Butler on a local model is not reachable by a Butler on a cloud one,
// in butlerscope_test.go and in the Part C acceptance suite.

import (
	"testing"
)

func routes(t *testing.T, db *DB) {
	t.Helper()
	for _, r := range []ModelRoute{
		{Name: "kitchen-box", Kind: RouteLocal, Provider: "ollama", Model: "llama",
			Readiness: RouteReady, Enabled: true},
		{Name: "anthropic", Kind: RouteCloud, Provider: "anthropic", Model: "sonnet",
			Readiness: RouteReady, Enabled: true},
	} {
		if err := db.UpsertModelRoute(r, "root-admin"); err != nil {
			t.Fatalf("UpsertModelRoute(%s): %v", r.Name, err)
		}
	}
}

// A route's kind is the data boundary. Changing it would move that
// boundary for every Butler assigned to the route by name — silently,
// and without any of them being edited.
func TestRouteKindIsImmutable(t *testing.T) {
	db := butlerTestDB(t)
	routes(t, db)
	if _, err := db.ExecForTest(
		`UPDATE model_routes SET kind = 'local' WHERE name = 'anthropic'`); err == nil {
		t.Fatal("a cloud route became a local one, moving the data boundary silently")
	}
}

// The probe verdict survives the router (ADR-W017 A4). It was never
// about which model to pick: it is about what a chosen one is trusted
// with.
func TestOnlyAProbedRouteCarriesActionAuthority(t *testing.T) {
	db := butlerTestDB(t)

	// Unprobed is not ready. It has not been checked, which is
	// different from having been checked and passed.
	if err := db.UpsertModelRoute(ModelRoute{
		Name: "new-box", Kind: RouteLocal, Provider: "ollama", Enabled: true,
		Readiness: RouteUnprobed,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	fresh, err := db.ListModelRoutes()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range fresh {
		if r.Name == "new-box" && r.CarriesActionAuthority() {
			t.Fatal("an unprobed route was trusted with tools that do things")
		}
	}

	// QA-only has been checked and found wanting: it answers questions
	// and performs nothing.
	if err := db.RecordRouteReadiness("new-box", RouteQAOnly, "it invented a tool name"); err != nil {
		t.Fatal(err)
	}
	after, _ := db.ListModelRoutes()
	for _, r := range after {
		if r.Name != "new-box" {
			continue
		}
		if r.CarriesActionAuthority() {
			t.Fatal("a QA-only route carries action authority")
		}
		if r.ReadinessDetail == "" {
			t.Fatal("a narrowed route does not say why it was narrowed")
		}
	}

	// A route reported anything but ready has to say why, or an
	// operator reads "unavailable" with nothing to act on.
	if err := db.RecordRouteReadiness("new-box", RouteUnavailable, ""); err == nil {
		t.Fatal("a route was reported unavailable with no reason")
	}

	// And a ready one does carry it.
	if err := db.RecordRouteReadiness("new-box", RouteReady, ""); err != nil {
		t.Fatal(err)
	}
	ready, _ := db.ListModelRoutes()
	for _, r := range ready {
		if r.Name == "new-box" && !r.CarriesActionAuthority() {
			t.Fatal("a probed, ready route carries no action authority")
		}
	}
}

// A disabled route is not a candidate for assignment. This is the
// property B1's schema trigger depends on: a Butler cannot be pointed
// at a route that has been turned off.
func TestADisabledRouteStaysDisabled(t *testing.T) {
	db := butlerTestDB(t)
	routes(t, db)
	if err := db.SetModelRouteEnabled("anthropic", false, "root-admin"); err != nil {
		t.Fatal(err)
	}
	all, err := db.ListModelRoutes()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.Name == "anthropic" {
			if r.Enabled {
				t.Fatal("a disabled route reports itself enabled")
			}
			if r.CarriesActionAuthority() {
				t.Fatal("a disabled route carries action authority")
			}
		}
	}
}
