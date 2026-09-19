package loop

// Projecting entitlements into the directory (ADR-W033 design §4; slice
// 8, #209).
//
// The boundary is the point of this file. Projection may take back what
// it placed and may not touch anything else, and every test here is
// about one side of that line.

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// household sets up a Person, a capability with a directory group, and
// the desired resources that describe them.
func household(t *testing.T, db *runtimedb.DB, handle string) (personID string, desired []resource.Resource) {
	t.Helper()
	if err := db.RecordButlerMember(handle, runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember(handle, "root-admin"); err != nil {
		t.Fatal(err)
	}
	members, err := db.ButlerMembersByHandleFold(handle)
	if err != nil || len(members) != 1 {
		t.Fatalf("looking up %s: %v %v", handle, members, err)
	}
	return members[0].PersonID, []resource.Resource{
		{
			Kind:      resource.KindLdapGroup,
			Metadata:  resource.Metadata{Name: "photos-users", Owner: "capability/photos", Node: "n1"},
			LdapGroup: &resource.LdapGroupSpec{Name: "photos_users"},
		},
		{
			Kind:     resource.KindLdapUser,
			Metadata: resource.Metadata{Name: handle, Owner: "system", Node: "n1"},
			LdapUser: &resource.LdapUserSpec{
				Username: handle, Email: handle + "@example.com",
				Groups: []string{"console_users"},
			},
		},
	}
}

func projectedUser(t *testing.T, out []resource.Resource, handle string) *resource.LdapUserSpec {
	t.Helper()
	for _, r := range out {
		if r.Kind == resource.KindLdapUser && r.LdapUser != nil && r.LdapUser.Username == handle {
			return r.LdapUser
		}
	}
	t.Fatalf("no LdapUser for %s in %v", handle, out)
	return nil
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// An entitlement becomes a directory membership.
func TestAnEntitlementBecomesADirectoryMembership(t *testing.T) {
	_, _, db, _ := harness(t)
	person, desired := household(t, db, "mika")

	// Not entitled yet: nothing is projected, and the declared group
	// the installer put there is untouched.
	out, drift, err := projectEntitlements(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 0 {
		t.Errorf("unexpected drift: %v", drift)
	}
	u := projectedUser(t, out, "mika")
	if has(u.Groups, "photos_users") {
		t.Error("a membership was projected with no entitlement behind it")
	}
	if !has(u.Groups, "console_users") {
		t.Error("projection dropped a group the Statefile declared")
	}

	// Entitle the default group, which admission already put them in.
	family, err := db.GroupBySeedKey(runtimedb.SeedFamily)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEntitlement("photos", runtimedb.EntitlementSubjectGroup,
		family.ID, runtimedb.EntitlementAllow, "mika"); err != nil {
		t.Fatal(err)
	}

	out, _, err = projectEntitlements(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	u = projectedUser(t, out, "mika")
	if !has(u.Groups, "photos_users") {
		t.Errorf("the entitlement did not reach the directory: %v", u.Groups)
	}
	// And Wildstone recorded that it placed it, which is what lets it
	// take the membership back later.
	placed, err := db.PlacedMemberships()
	if err != nil {
		t.Fatal(err)
	}
	if !has(placed[person], "photos_users") {
		t.Errorf("the membership was projected without being claimed: %v", placed)
	}
}

// Withdrawing an entitlement revokes the membership it justified.
func TestWithdrawingAnEntitlementRevokesTheMembership(t *testing.T) {
	_, _, db, _ := harness(t)
	_, desired := household(t, db, "mika")
	family, err := db.GroupBySeedKey(runtimedb.SeedFamily)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEntitlement("photos", runtimedb.EntitlementSubjectGroup,
		family.ID, runtimedb.EntitlementAllow, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := projectEntitlements(db, desired); err != nil {
		t.Fatal(err)
	}

	// The household changes its mind.
	if err := db.RemoveEntitlement("photos", runtimedb.EntitlementSubjectGroup, family.ID); err != nil {
		t.Fatal(err)
	}
	out, _, err := projectEntitlements(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	u := projectedUser(t, out, "mika")
	if !has(u.Revoke, "photos_users") {
		t.Errorf("the membership was not revoked after the entitlement went away: "+
			"groups=%v revoke=%v", u.Groups, u.Revoke)
	}
	if has(u.Groups, "photos_users") {
		t.Error("the membership is in both columns at once")
	}
}

// THE boundary (#209, and the decision this slice reverses): Wildstone
// removes what it placed and reports what it did not.
//
// LdapGroupSpec recorded the objection to a control plane owning
// memberships in so many words:
//
//	a system that thinks it does is one that removes your sister from
//	photos_users because a YAML file did not mention her.
//
// She is in the directory here and in nothing Wildstone ever claimed, so
// she cannot appear in a revocation list whatever the entitlements say.
func TestProjectionNeverRevokesAMembershipItDidNotPlace(t *testing.T) {
	_, _, db, _ := harness(t)
	person, desired := household(t, db, "mika")

	// Somebody added this by hand in LLDAP's own UI. Wildstone has no
	// record of it, which is exactly what "by hand" looks like from
	// here — projected_memberships is empty.
	placed, err := db.PlacedMemberships()
	if err != nil {
		t.Fatal(err)
	}
	if len(placed[person]) != 0 {
		t.Fatalf("setup: something was already claimed: %v", placed)
	}

	out, _, err := projectEntitlements(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	u := projectedUser(t, out, "mika")
	if len(u.Revoke) != 0 {
		t.Errorf("projection asked to revoke %v with nothing claimed; a membership "+
			"Wildstone never placed is not its to remove", u.Revoke)
	}
}

// A declared group outranks the projection.
//
// console_users is put there by the installer and by the members API,
// never by an entitlement. A pass that revoked it because no entitlement
// implied it would lock the Owner out of the Console — which is also the
// one surface that could put them back.
func TestADeclaredGroupIsNeverRevoked(t *testing.T) {
	_, _, db, _ := harness(t)
	person, desired := household(t, db, "mika")

	// Pretend a previous pass had claimed it too, which is the only way
	// it could reach a revocation list at all.
	if err := db.ClaimMemberships(person, []string{"console_users"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := projectEntitlements(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	u := projectedUser(t, out, "mika")
	if has(u.Revoke, "console_users") {
		t.Error("projection revoked a group the Statefile declares; withdrawing an " +
			"entitlement must not undo what the household stated separately")
	}
	if !has(u.Groups, "console_users") {
		t.Error("the declared group is gone from the groups column")
	}
}

// An ambiguous handle projects nothing, and says so.
//
// auth_console.go's rule: a directory account and a Person are joined by
// nothing but a string, and a name collision must never make two
// subjects one. Projection PLACES memberships, so it is using that match
// in the direction the rule warns about — and refuses the case the rule
// is about rather than guessing.
func TestAnAmbiguousHandleProjectsNothing(t *testing.T) {
	_, _, db, _ := harness(t)
	_, desired := household(t, db, "mika")

	// A second active Person answering to the same name, differing only
	// in case — which butler_members allows and LLDAP folds together.
	if err := db.RecordButlerMember("Mika", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("Mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	family, err := db.GroupBySeedKey(runtimedb.SeedFamily)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEntitlement("photos", runtimedb.EntitlementSubjectGroup,
		family.ID, runtimedb.EntitlementAllow, "mika"); err != nil {
		t.Fatal(err)
	}

	out, drift, err := projectEntitlements(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	u := projectedUser(t, out, "mika")
	if has(u.Groups, "photos_users") {
		t.Error("projection placed a membership on a guess about which Person a " +
			"directory account means")
	}
	if len(drift) != 1 || drift[0].Account != "mika" {
		t.Fatalf("the ambiguity was not reported: %v", drift)
	}
}

// A directory account with no Person keeps whatever it has.
//
// The installer makes accounts Wildstone does not manage, and a
// household may make others. Not drift, not an error, and not something
// to strip memberships from.
func TestADirectoryAccountWithNoPersonIsLeftAlone(t *testing.T) {
	_, _, db, _ := harness(t)
	desired := []resource.Resource{
		{
			Kind:      resource.KindLdapGroup,
			Metadata:  resource.Metadata{Name: "photos-users", Owner: "capability/photos", Node: "n1"},
			LdapGroup: &resource.LdapGroupSpec{Name: "photos_users"},
		},
		{
			Kind:     resource.KindLdapUser,
			Metadata: resource.Metadata{Name: "kiosk", Owner: "system", Node: "n1"},
			LdapUser: &resource.LdapUserSpec{
				Username: "kiosk", Email: "kiosk@example.com",
				Groups: []string{"photos_users"},
			},
		},
	}
	out, drift, err := projectEntitlements(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 0 {
		t.Errorf("an unmanaged account was reported as drift: %v", drift)
	}
	u := projectedUser(t, out, "kiosk")
	if len(u.Revoke) != 0 || !has(u.Groups, "photos_users") {
		t.Errorf("an account with no Person was altered: groups=%v revoke=%v", u.Groups, u.Revoke)
	}
}

// Projection is reconciling: a membership removed by hand comes back.
//
// #209 requires "a drifted directory group is corrected on the next
// reconcile". This is the half that corrects — the membership is still
// entitled, so every pass asserts it again whatever the directory looks
// like. The job's own add-if-missing does the rest.
func TestAStillEntitledMembershipIsAssertedEveryPass(t *testing.T) {
	_, _, db, _ := harness(t)
	_, desired := household(t, db, "mika")
	family, err := db.GroupBySeedKey(runtimedb.SeedFamily)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEntitlement("photos", runtimedb.EntitlementSubjectGroup,
		family.ID, runtimedb.EntitlementAllow, "mika"); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 3; pass++ {
		out, _, err := projectEntitlements(db, desired)
		if err != nil {
			t.Fatal(err)
		}
		u := projectedUser(t, out, "mika")
		if !has(u.Groups, "photos_users") {
			t.Fatalf("pass %d stopped asserting the membership: %v", pass, u.Groups)
		}
		if len(u.Revoke) != 0 {
			t.Fatalf("pass %d wanted to revoke a still-entitled membership: %v", pass, u.Revoke)
		}
	}
}

// Desired state is never edited — projection works on a copy.
//
// The Statefile says what the household and its packages declared. The
// projection is derived and recomputed every tick; writing it back would
// make the Statefile disagree with itself the moment an entitlement
// changed without a plan.
func TestProjectionDoesNotEditDesiredState(t *testing.T) {
	_, _, db, _ := harness(t)
	_, desired := household(t, db, "mika")
	family, err := db.GroupBySeedKey(runtimedb.SeedFamily)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEntitlement("photos", runtimedb.EntitlementSubjectGroup,
		family.ID, runtimedb.EntitlementAllow, "mika"); err != nil {
		t.Fatal(err)
	}
	before := projectedUser(t, desired, "mika")
	if _, _, err := projectEntitlements(db, desired); err != nil {
		t.Fatal(err)
	}
	if has(before.Groups, "photos_users") || len(before.Revoke) != 0 {
		t.Errorf("projection wrote into desired state: groups=%v revoke=%v",
			before.Groups, before.Revoke)
	}
}

// No directory groups declared means nothing to project INTO, and
// desired state comes back untouched.
//
// It matters more than it reads: a node whose resources have not loaded
// yet must not look like a household that withdrew every entitlement,
// which would revoke the lot.
func TestNoDirectoryGroupsProjectsNothing(t *testing.T) {
	_, _, db, _ := harness(t)
	person, desired := household(t, db, "mika")
	if err := db.ClaimMemberships(person, []string{"photos_users"}); err != nil {
		t.Fatal(err)
	}
	// Drop the LdapGroup, keeping the user.
	var onlyUser []resource.Resource
	for _, r := range desired {
		if r.Kind != resource.KindLdapGroup {
			onlyUser = append(onlyUser, r)
		}
	}
	out, _, err := projectEntitlements(db, onlyUser)
	if err != nil {
		t.Fatal(err)
	}
	u := projectedUser(t, out, "mika")
	if len(u.Revoke) != 0 {
		t.Errorf("an empty resource set revoked %v; nothing to project into is not "+
			"the same as every entitlement withdrawn", u.Revoke)
	}
}

// Structural: a revocation is always a subset of what was claimed.
//
// The behavioural tests above each exercise one path through this, and
// none of them can state the invariant itself — projection never sees
// the directory, so "it did not revoke what it could not name" is a
// property of the function rather than of a scenario. This says it
// directly, because it is the sentence the whole slice rests on.
func TestARevocationIsAlwaysSomethingWildstonePlaced(t *testing.T) {
	cases := []struct {
		name            string
		claimed, wanted []string
		declared        map[string]bool
		want            []string
	}{
		{"nothing claimed, nothing revoked",
			nil, []string{"photos_users"}, nil, nil},
		{"claimed and no longer wanted",
			[]string{"photos_users"}, nil, nil, []string{"photos_users"}},
		{"claimed and still wanted",
			[]string{"photos_users"}, []string{"photos_users"}, nil, nil},
		{"claimed, unwanted, but declared",
			[]string{"console_users"}, nil, map[string]bool{"console_users": true}, nil},
		{"several, mixed",
			[]string{"a_users", "b_users", "c_users"}, []string{"b_users"},
			map[string]bool{"c_users": true}, []string{"a_users"}},
	}
	for _, tc := range cases {
		got := revocations(tc.claimed, tc.wanted, tc.declared)
		if len(got) != len(tc.want) {
			t.Errorf("%s: revocations = %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: revocations = %v, want %v", tc.name, got, tc.want)
				break
			}
		}
		// The invariant, restated over every case: nothing is revoked
		// that was not claimed. A rewrite that started from the
		// directory's contents instead would fail here.
		for _, g := range got {
			if !has(tc.claimed, g) {
				t.Errorf("%s: revoked %q, which was never claimed", tc.name, g)
			}
		}
	}
}

// Installing a capability creates its entitlements; removing it makes
// them dormant; reinstalling brings them back (§17.3, §18.2).
//
// The whole lifecycle in one test, because the three are one mechanism:
// desired state is read every tick, and what is in it decides.
func TestACapabilitysEntitlementsFollowItsInstallation(t *testing.T) {
	_, _, db, _ := harness(t)
	person, desired := household(t, db, "mika")
	// A second capability, so that uninstalling photos is
	// distinguishable from a desired set that has not loaded. With one
	// capability the two look identical, and the pass deliberately does
	// nothing in that case rather than withdrawing everything.
	desired = append(desired, resource.Resource{
		Kind:      resource.KindLdapGroup,
		Metadata:  resource.Metadata{Name: "docs-users", Owner: "capability/documents", Node: "n1"},
		LdapGroup: &resource.LdapGroupSpec{Name: "docs_users"},
	})
	accessGroups := map[string][]string{
		"photos":    {runtimedb.SeedFamily},
		"documents": {runtimedb.SeedFamily},
	}

	// Installed: the declared audience becomes an entitlement, and the
	// Person admission put in `Family` reaches it.
	if err := seedCapabilityEntitlements(db, accessGroups, desired); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.EntitledTo(person, "photos"); err != nil || !ok {
		t.Fatalf("the declared audience did not become an entitlement (%v, %v)", ok, err)
	}

	// The household restricts somebody by name — a decision no manifest
	// could have stated.
	other := personOfHandle(t, db, "giulia")
	if err := db.SetEntitlement("photos", runtimedb.EntitlementSubjectPerson,
		other, runtimedb.EntitlementRestrict, "mika"); err != nil {
		t.Fatal(err)
	}

	// Uninstalled: nothing in desired state owns the capability any
	// more. The rows go dormant rather than away.
	var withoutPhotos []resource.Resource
	for _, r := range desired {
		if r.Metadata.Owner != "capability/photos" {
			withoutPhotos = append(withoutPhotos, r)
		}
	}
	if err := seedCapabilityEntitlements(db, accessGroups, withoutPhotos); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.EntitledTo(person, "photos"); err != nil || ok {
		t.Error("an uninstalled service still admitted somebody")
	}
	rows, err := db.ListEntitlements("photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("uninstalling left %d rows, want both kept as dormant", len(rows))
	}

	// Reinstalled: both decisions come back, the household's included.
	if err := seedCapabilityEntitlements(db, accessGroups, desired); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.EntitledTo(person, "photos"); err != nil || !ok {
		t.Error("reinstalling did not restore the group's assignment")
	}
	if ok, err := db.EntitledTo(other, "photos"); err != nil || ok {
		t.Error("reinstalling un-restricted a Person the household had refused")
	}
}

// An empty desired set withdraws nothing.
//
// A loop that has not loaded yet, or a statefile that momentarily reads
// empty, must not look like a household that uninstalled everything —
// that would make every service dormant and withdraw all access on one
// tick.
func TestAnEmptyDesiredSetDoesNotUninstallEverything(t *testing.T) {
	_, _, db, _ := harness(t)
	person, desired := household(t, db, "mika")
	accessGroups := map[string][]string{"photos": {runtimedb.SeedFamily}}
	if err := seedCapabilityEntitlements(db, accessGroups, desired); err != nil {
		t.Fatal(err)
	}

	if err := seedCapabilityEntitlements(db, accessGroups, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.EntitledTo(person, "photos"); err != nil || !ok {
		t.Error("an empty resource set made every service dormant; nothing loaded " +
			"is not the same as nothing installed")
	}
}

// personOfHandle records and admits a Person, returning their id.
func personOfHandle(t *testing.T, db *runtimedb.DB, handle string) string {
	t.Helper()
	if err := db.RecordButlerMember(handle, runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember(handle, "root-admin"); err != nil {
		t.Fatal(err)
	}
	members, err := db.ButlerMembersByHandleFold(handle)
	if err != nil || len(members) != 1 {
		t.Fatalf("looking up %s: %v %v", handle, members, err)
	}
	return members[0].PersonID
}
