package api

// The Console's entitlement surface (ADR-W033 design §4, §18.2; slice 8).

import (
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// groupID finds one of the seeded groups by the role it plays.
func seedGroup(t *testing.T, h *harness, seedKey string) string {
	t.Helper()
	g, err := h.s.DB.GroupBySeedKey(seedKey)
	if err != nil {
		t.Fatalf("GroupBySeedKey(%s): %v", seedKey, err)
	}
	return g.ID
}

// The screen answers who it reaches, not only what the rules are.
func TestServiceAccessResolvesWhoItReaches(t *testing.T) {
	h := newHarness(t)
	h.person("tom", "sam")
	family := seedGroup(t, h, runtimedb.SeedFamily)

	var body serviceAccess
	res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/services/photos/access", nil, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET access = %d", res.StatusCode)
	}
	if len(body.Reaches) != 0 {
		t.Errorf("a service with no decision reaches %v; the absence of a decision "+
			"is a refusal, not a permission", body.Reaches)
	}

	// Allow the default group, which admission put both of them in.
	res = h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/services/photos/access",
		map[string]any{"subjectKind": "group", "subject": family, "effect": "allow"}, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST access = %d", res.StatusCode)
	}
	if len(body.Reaches) != 2 {
		t.Errorf("reaches = %v, want both people", body.Reaches)
	}

	// Refuse one by name. The point of showing the resolved answer is
	// that an Owner sees this without working it out.
	res = h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/services/photos/access",
		map[string]any{"subjectKind": "person", "subject": personIDOf(t, h, "sam"), "effect": "restrict"}, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST restrict = %d", res.StatusCode)
	}
	if len(body.Reaches) != 1 || body.Reaches[0] != "tom" {
		t.Errorf("reaches = %v after restricting sam, want just tom", body.Reaches)
	}
}

// Removing a decision is not refusing, and the API keeps the three
// states distinct.
func TestRemovingADecisionThroughTheAPIIsNotRefusing(t *testing.T) {
	h := newHarness(t)
	h.person("sam")
	family := seedGroup(t, h, runtimedb.SeedFamily)
	sam := personIDOf(t, h, "sam")

	var body serviceAccess
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/services/photos/access",
		map[string]any{"subjectKind": "group", "subject": family, "effect": "allow"}, &body)
	h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/services/photos/access",
		map[string]any{"subjectKind": "person", "subject": sam, "effect": "restrict"}, &body)
	if len(body.Reaches) != 0 {
		t.Fatalf("setup: the restriction did not hold: %v", body.Reaches)
	}

	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/services/photos/access",
		map[string]any{"subjectKind": "person", "subject": sam, "effect": ""}, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST remove = %d", res.StatusCode)
	}
	if len(body.Reaches) != 1 {
		t.Errorf("removing the restriction did not return sam to what their groups "+
			"say: %v", body.Reaches)
	}
	if len(body.People) != 0 {
		t.Errorf("the removed decision is still on record: %v", body.People)
	}
}

// A bad effect is the caller's input, not a server fault.
func TestAnUnknownEffectIsRefusedAsInput(t *testing.T) {
	h := newHarness(t)
	h.person("sam")
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/services/photos/access",
		map[string]any{"subjectKind": "person", "subject": personIDOf(t, h, "sam"), "effect": "maybe"}, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with effect=maybe = %d, want 400", res.StatusCode)
	}
}

// Group membership is editable, and grants nothing by itself.
func TestPersonGroupMembershipIsEditable(t *testing.T) {
	h := newHarness(t)
	h.person("tom")
	adults := seedGroup(t, h, runtimedb.SeedAdults)

	var groups []personGroup
	res := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/person-groups/"+adults+"/members",
		map[string]any{"member": "tom", "in": true}, &groups)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST membership = %d", res.StatusCode)
	}
	if !groupHolds(groups, adults, "tom") {
		t.Errorf("tom was not added to Adults: %v", groups)
	}

	// And it entitled them to nothing.
	var access serviceAccess
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/services/photos/access", nil, &access)
	if len(access.Reaches) != 0 {
		t.Errorf("joining a group reached %v; membership is an identity primitive "+
			"and what it authorizes is each resource's own decision", access.Reaches)
	}

	res = h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/person-groups/"+adults+"/members",
		map[string]any{"member": "tom", "in": false}, &groups)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST removal = %d", res.StatusCode)
	}
	if groupHolds(groups, adults, "tom") {
		t.Errorf("tom was not taken out of Adults: %v", groups)
	}
}

// Naming somebody who does not exist is a 404, not a silent no-op.
//
// A screen that reported success for a name nothing matched would leave
// an Owner believing they had granted something.
func TestAddingAnUnknownPersonToAGroupIsRefused(t *testing.T) {
	h := newHarness(t)
	h.person("tom")
	adults := seedGroup(t, h, runtimedb.SeedAdults)
	res := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/household/person-groups/"+adults+"/members",
		map[string]any{"member": "nobody", "in": true}, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("POST for an unknown person = %d, want 404", res.StatusCode)
	}
}

func personIDOf(t *testing.T, h *harness, handle string) string {
	t.Helper()
	members, err := h.s.DB.ButlerMembersByHandleFold(handle)
	if err != nil || len(members) != 1 {
		t.Fatalf("looking up %s: %v %v", handle, members, err)
	}
	return members[0].PersonID
}

func groupHolds(groups []personGroup, id, member string) bool {
	for _, g := range groups {
		if g.ID != id {
			continue
		}
		for _, m := range g.Members {
			if m == member {
				return true
			}
		}
	}
	return false
}

// The launcher leaves out a card for a service this member cannot use
// — and that is PRESENTATION, never the gate (§18.2).
//
// The pairing matters: hiding the card and denying the URL are two
// different mechanisms, and a household that could reach a service by
// typing its address while the card was hidden would have an
// entitlement system that only looked like one.
func TestTheLauncherLeavesOutAServiceAMemberCannotUse(t *testing.T) {
	h := newHarness(t)
	h.person("tom", "sam")
	desired := []resource.Resource{
		{
			Kind:      resource.KindLdapGroup,
			Metadata:  resource.Metadata{Name: "docs-users", Owner: "capability/documents", Node: "n1"},
			LdapGroup: &resource.LdapGroupSpec{Name: "docs_users"},
		},
	}

	// Nothing declared for `feeds`, so its card is drawn for everybody:
	// an upgrade that added a field must not empty a launcher.
	if reach := h.s.launcherReach("tom", desired); reach == nil {
		t.Fatal("no reach map at all; a household with declared groups should get one")
	} else if _, decided := reach["feeds"]; decided {
		t.Error("a capability nobody declared an audience for was decided about")
	}

	// `documents` declared one, and nobody is entitled yet.
	reach := h.s.launcherReach("tom", desired)
	if reach["documents"] {
		t.Error("a service with no entitlement was drawn; default deny applies to " +
			"the launcher as well as the door")
	}

	family := seedGroup(t, h, runtimedb.SeedFamily)
	if err := h.s.DB.SetEntitlement("documents", runtimedb.EntitlementSubjectGroup,
		family, runtimedb.EntitlementAllow, "tom"); err != nil {
		t.Fatal(err)
	}
	if !h.s.launcherReach("tom", desired)["documents"] {
		t.Error("an entitled member did not get the card")
	}

	// Restricting sam hides it from them and nobody else.
	if err := h.s.DB.SetEntitlement("documents", runtimedb.EntitlementSubjectPerson,
		personIDOf(t, h, "sam"), runtimedb.EntitlementRestrict, "tom"); err != nil {
		t.Fatal(err)
	}
	if h.s.launcherReach("sam", desired)["documents"] {
		t.Error("a restricted member still had the card drawn")
	}
	if !h.s.launcherReach("tom", desired)["documents"] {
		t.Error("restricting one member hid the card from another")
	}
}

// An unresolvable member draws everything.
//
// The opposite of what an authorization check would do, deliberately:
// hiding a card the household CAN use is a bug they report as the app
// disappearing, while showing one they cannot is a link that politely
// refuses. Only one of those is recoverable from the page it happens on.
func TestAnUnresolvableMemberStillGetsALauncher(t *testing.T) {
	h := newHarness(t)
	desired := []resource.Resource{
		{
			Kind:      resource.KindLdapGroup,
			Metadata:  resource.Metadata{Name: "docs-users", Owner: "capability/documents", Node: "n1"},
			LdapGroup: &resource.LdapGroupSpec{Name: "docs_users"},
		},
	}
	if reach := h.s.launcherReach("stranger", desired); reach != nil {
		t.Errorf("a name with no Person produced a filter %v; the launcher must "+
			"draw everything and let the door decide", reach)
	}
	if reach := h.s.launcherReach("", desired); reach != nil {
		t.Errorf("an empty member produced a filter %v", reach)
	}
}

// The filter is applied to the CARDS, not merely computed.
//
// The two tests above exercise the map; this one exercises the thing
// that reads it, and the difference is not academic. Reading a missing
// key as "not entitled" would empty the launcher of every service
// nobody declared an audience for — which is most of the catalog — and
// no test of the map alone can see that.
func TestTheCardFilterIsAppliedAndSparesTheUndeclared(t *testing.T) {
	h := newHarness(t)
	h.person("tom")
	desired := []resource.Resource{
		{
			Kind:      resource.KindLdapGroup,
			Metadata:  resource.Metadata{Name: "docs-users", Owner: "capability/documents", Node: "n1"},
			LdapGroup: &resource.LdapGroupSpec{Name: "docs_users"},
		},
		portalRoute("docs", "documents", "docs.example.com"),
		portalRoute("feeds", "feeds", "feeds.example.com"),
	}

	apps, _, _ := h.s.homeApps("tom", desired)
	var sawDocs, sawFeeds bool
	for _, a := range apps {
		switch a.Capability {
		case "documents":
			sawDocs = true
		case "feeds":
			sawFeeds = true
		}
	}
	if sawDocs {
		t.Error("a card was drawn for a declared service nobody is entitled to")
	}
	if !sawFeeds {
		t.Error("a card was hidden for a service nobody declared an audience for; " +
			"absence of a decision must mean draw it, or the launcher empties")
	}

	family := seedGroup(t, h, runtimedb.SeedFamily)
	if err := h.s.DB.SetEntitlement("documents", runtimedb.EntitlementSubjectGroup,
		family, runtimedb.EntitlementAllow, "tom"); err != nil {
		t.Fatal(err)
	}
	apps, _, _ = h.s.homeApps("tom", desired)
	sawDocs = false
	for _, a := range apps {
		if a.Capability == "documents" {
			sawDocs = true
		}
	}
	if !sawDocs {
		t.Error("the card did not come back once the member was entitled")
	}
}

// A deployment where nothing declares a directory group filters nothing.
//
// Not the same as the case above: there the map exists and one
// capability is absent from it; here there is no map at all, which is
// what every deployment looks like until a package declares an
// audience. Getting this wrong would empty the launcher on upgrade.
func TestADeploymentWithNoDeclaredGroupsFiltersNothing(t *testing.T) {
	h := newHarness(t)
	h.person("tom")
	desired := []resource.Resource{portalRoute("feeds", "feeds", "feeds.example.com")}
	if reach := h.s.launcherReach("tom", desired); reach != nil {
		t.Errorf("a deployment with no declared groups produced a filter %v", reach)
	}
}

func portalRoute(name, capability, host string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindRoute,
		Metadata: resource.Metadata{Name: name, Owner: "capability/" + capability, Node: "n1"},
		Route: &resource.RouteSpec{
			Host: host, Upstream: "container/" + name + ":80",
			Auth: "authelia_oidc", Classification: "family_public",
		},
	}
}
