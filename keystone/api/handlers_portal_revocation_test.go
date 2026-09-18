package api

// Does the portal door actually revoke?
//
// auth_portal.go says membership IS the permission, "so removing
// somebody from the group revokes them on their next request". main has
// since corrected exactly that claim for the Console, on the grounds
// that this deployment cannot remove anyone from a group — the identity
// capability's provisioning job is additive by construction and "never
// removes a membership".
//
// If that is true, offboarding a household member leaves their portal
// access standing. This test asks.

import (
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestOffboardingAMemberRevokesTheirPortalAccess(t *testing.T) {
	h := homeHarness(t)

	// A household member, admitted and using the dashboard.
	if err := h.s.DB.RecordButlerMember("sandy", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("sandy", "owner"); err != nil {
		t.Fatal(err)
	}
	res := h.member(http.MethodGet, "/api/v1/home/summary", "sandy", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an admitted member cannot reach the dashboard: %d", res.StatusCode)
	}

	// The household offboards them. Nothing removes them from the
	// directory group, because nothing in this system can.
	if _, err := h.s.DB.OffboardMember("sandy", "owner", "owner"); err != nil {
		t.Fatal(err)
	}
	m, err := h.s.DB.GetButlerMember("sandy")
	if err != nil {
		t.Fatal(err)
	}
	if m.Active() {
		t.Fatal("offboarding did not deactivate the member record")
	}

	// Their browser still holds the session it signed in with, so it
	// still presents the group. This is the request that matters.
	res = h.member(http.MethodGet, "/api/v1/home/summary", "sandy", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Error("an OFFBOARDED member still reaches the household dashboard: " +
			"the door trusts a group nothing can remove them from")
	}

	// And the same for the surfaces that hold their files and their
	// assistant.
	for _, path := range []string{
		"/api/v1/filedrop/list?namespace=shared",
		"/api/v1/assistant/agents",
	} {
		res = h.member(http.MethodGet, path, "sandy", "filedrop_users,assistant_users", nil, "")
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("an offboarded member still reaches %s", path)
		}
	}
}

// A household that keeps no Person records is not locked out.
//
// Requiring a record would refuse everybody on a deployment that has
// not created them — including this one — and a door that refuses the
// whole family is not a safer door. With no record, the directory group
// decides exactly as it did before.
func TestAMemberWithNoRecordIsStillLetIn(t *testing.T) {
	h := homeHarness(t)
	res := h.member(http.MethodGet, "/api/v1/home/summary", "nobody-on-file", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("a member with no household record was refused: %d", res.StatusCode)
	}
}

// Disabling revokes too, not only offboarding: Active() is all three
// conditions, and the door asks Active().
func TestADisabledMemberIsRefused(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.DB.RecordButlerMember("maya", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("maya", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.DisableButlerMember("maya", "owner"); err != nil {
		t.Fatal(err)
	}
	res := h.member(http.MethodGet, "/api/v1/home/summary", "maya", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Error("a disabled member still reaches the dashboard")
	}
}

// The case-insensitive match may only ever REFUSE.
//
// Handles are compared exactly everywhere else, so "Sandy" and "sandy"
// are two records. An exact lookup here would miss the record whenever
// the proxy asserted a different capitalisation and admit somebody the
// household had just offboarded — the permissive branch is the one an
// attacker aims for.
func TestACaseDifferentSpellingCannotSlipPastRevocation(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.DB.RecordButlerMember("sandy", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("sandy", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.OffboardMember("sandy", "owner", "owner"); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"sandy", "Sandy", "SANDY", " Sandy "} {
		res := h.member(http.MethodGet, "/api/v1/home/summary", spelling, "homepage_users", nil, "")
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("%q got in after being offboarded as \"sandy\"", spelling)
		}
	}
}
