package api

// The design's four page states, as conditions rather than a dropdown.

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// routeRes is one installed, routed app.
func routeRes(name, container, node string) resource.Resource {
	return resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: name, Owner: "capability/" + name, Package: name, Node: node},
		Route: &resource.RouteSpec{
			Host: name + ".example.com", Upstream: "container/" + container + ":80",
			Auth: "authelia_oidc", Classification: "family_public",
		},
	}
}

// A normal day says nothing. A band that congratulates a household on a
// working house is a band they learn to scroll past.
func TestANormalDaySaysNothing(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.State.Put(routeRes("photos", "immich-server", "n1")); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.UpsertObserved(runtimedb.Observed{
		Kind: "Container", Name: "immich-server", Healthy: true,
	}); err != nil {
		t.Fatal(err)
	}
	got := h.summary(t, "sandy")
	for _, c := range got.Conditions {
		if c.Kind == "bad" || c.Kind == "warn" {
			t.Errorf("a working house reported %q: %s", c.Kind, c.Title)
		}
	}
}

// An app that is down is named, and the band leads with what is not
// lost.
func TestAnAppThatIsDownIsNamedWithItsConsequence(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.State.Put(routeRes("photos", "immich-server", "n1")); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.UpsertObserved(runtimedb.Observed{
		Kind: "Container", Name: "immich-server", Healthy: false, Detail: "exited",
	}); err != nil {
		t.Fatal(err)
	}
	// The node is connected, so this is the app being down rather than
	// the machine being away.
	if err := h.s.DB.RecordNodeJoin("n1"); err != nil {
		t.Fatal(err)
	}
	h.s.LiveNodes = func() []LiveNode { return []LiveNode{{NodeID: "n1"}} }

	got := h.summary(t, "sandy")
	var found *condition
	for i, c := range got.Conditions {
		if strings.Contains(c.Title, "Photos") {
			found = &got.Conditions[i]
		}
	}
	if found == nil {
		t.Fatalf("the down app is not in the band: %+v", got.Conditions)
	}
	if !strings.Contains(found.Title, "isn't answering") {
		t.Errorf("title = %q", found.Title)
	}
	if strings.Contains(found.Title, "machine") {
		t.Errorf("title = %q — the machine is up; this is the app", found.Title)
	}
	// Lead with what is not lost.
	if !strings.Contains(found.Detail, "still there") {
		t.Errorf("detail = %q, want the consequence rather than only the fault", found.Detail)
	}
}

// An app on a machine that is away is a DIFFERENT fact, said
// differently — and the difference is whether retrying is worth it.
func TestAnAppOnAnAbsentMachineSaysSoInstead(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.State.Put(routeRes("cameras", "frigate", "kitchen-pi")); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.UpsertObserved(runtimedb.Observed{
		Kind: "Container", Name: "frigate", Healthy: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.RecordNodeJoin("kitchen-pi"); err != nil {
		t.Fatal(err)
	}
	// Nothing is connected.
	h.s.LiveNodes = func() []LiveNode { return nil }

	got := h.summary(t, "sandy")
	var found *condition
	for i, c := range got.Conditions {
		if strings.Contains(c.Title, "Cameras") {
			found = &got.Conditions[i]
		}
	}
	if found == nil {
		t.Fatalf("nothing said about the unreachable app: %+v", got.Conditions)
	}
	if !strings.Contains(found.Title, "machine it runs on is offline") {
		t.Errorf("title = %q, want the machine's absence named", found.Title)
	}
	// And no hostname: §7 gives the household surface none.
	for _, c := range got.Conditions {
		if strings.Contains(c.Title+c.Detail, "kitchen-pi") {
			t.Errorf("the band names a machine: %q", c.Title+c.Detail)
		}
	}
}

// With no live view of the fleet, nothing is claimed to be offline:
// saying a machine is away when it may not be is worse than saying only
// that the app is not answering.
func TestWithoutALiveViewNoMachineIsCalledOffline(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.State.Put(routeRes("cameras", "frigate", "kitchen-pi")); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.UpsertObserved(runtimedb.Observed{Kind: "Container", Name: "frigate", Healthy: false}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.RecordNodeJoin("kitchen-pi"); err != nil {
		t.Fatal(err)
	}
	h.s.LiveNodes = nil

	for _, c := range h.summary(t, "sandy").Conditions {
		if strings.Contains(c.Title, "offline") {
			t.Errorf("a machine was called offline with no live view: %q", c.Title)
		}
	}
}

// Zero routed services is the normal state of a correct fresh install,
// and must read as working rather than as broken.
func TestAFreshInstallReadsAsWorking(t *testing.T) {
	h := homeHarness(t)
	got := h.summary(t, "sandy")
	if len(got.Conditions) == 0 {
		t.Fatal("a fresh install says nothing at all")
	}
	first := got.Conditions[0]
	if first.Kind == "bad" {
		t.Errorf("a correct fresh install is reported as a fault: %q", first.Title)
	}
	if !strings.Contains(first.Detail, "working as intended") {
		t.Errorf("detail = %q, want it to say the install is fine", first.Detail)
	}
}

// The tailnet posture is said once, and can be dismissed.
func TestTheTailnetPostureIsSaidOnceAndIsDismissible(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.State.Put(routeRes("photos", "immich-server", "n1")); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindIngress,
		Metadata: resource.Metadata{Name: "ingress", Owner: "capability/ingress", Node: "n1"},
		Ingress:  &resource.IngressSpec{TLS: "tailscale", TailnetName: "house.ts.net"},
	}); err != nil {
		t.Fatal(err)
	}
	var found *condition
	got := h.summary(t, "sandy")
	for i, c := range got.Conditions {
		if c.ID == "tailnet" {
			found = &got.Conditions[i]
		}
	}
	if found == nil {
		t.Fatalf("nothing said about the tailnet posture: %+v", got.Conditions)
	}
	if !found.Dismissible {
		t.Error("a standing fact that cannot be dismissed becomes a banner people read past")
	}
	if found.Kind == "bad" || found.Kind == "warn" {
		t.Errorf("the posture is reported as a problem: %q", found.Kind)
	}
}
