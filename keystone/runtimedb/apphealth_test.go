package runtimedb

import "testing"

// healthy means CONVERGED — the container matches desired state. That is
// the reconciler's question. Whether the app inside can serve anybody is
// a different one, and only the app can answer it.
//
// open-webui was converged and refusing connections at the same time,
// for several minutes, while every surface read 0 unhealthy.
func TestObservedCarriesTheAppsOwnOpinionSeparately(t *testing.T) {
	db := open(t)
	if err := db.UpsertObserved(Observed{
		Kind: "Container", Name: "open-webui", Node: "n1",
		Healthy: true, AppHealth: "unhealthy",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListObserved()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one row, got %d", len(got))
	}
	if !got[0].Healthy {
		t.Error("converged must stay converged: the app's opinion is a separate fact")
	}
	if got[0].AppHealth != "unhealthy" {
		t.Errorf("want the app's own opinion preserved, got %q", got[0].AppHealth)
	}
}

// Most images declare no healthcheck. Empty is NO OPINION, and a reader
// that treated it as a problem would paint most of a homelab red.
func TestNoHealthcheckIsEmptyNotUnhealthy(t *testing.T) {
	db := open(t)
	if err := db.UpsertObserved(Observed{Kind: "Container", Name: "plain", Node: "n1", Healthy: true}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListObserved()
	if err != nil {
		t.Fatal(err)
	}
	if got[0].AppHealth != "" {
		t.Errorf("want empty, got %q", got[0].AppHealth)
	}
}

// The row is upserted every tick, so a recovering app has to stop being
// reported as unhealthy.
func TestAppHealthIsOverwrittenOnTheNextObservation(t *testing.T) {
	db := open(t)
	for _, h := range []string{"starting", "healthy"} {
		if err := db.UpsertObserved(Observed{
			Kind: "Container", Name: "open-webui", Node: "n1", Healthy: true, AppHealth: h,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.ListObserved()
	if err != nil {
		t.Fatal(err)
	}
	if got[0].AppHealth != "healthy" {
		t.Errorf("want the newest observation, got %q", got[0].AppHealth)
	}
}
